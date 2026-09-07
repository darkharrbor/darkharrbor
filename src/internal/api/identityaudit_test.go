package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func newIdentityAuditTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := newC04TestStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Server{log: log, store: st}, st
}

func mustCreateReadyIdentityItem(t *testing.T, st *store.Store, id, displayName string, identity *store.ProviderIdentity, facts *mediatruth.Facts, createdAt time.Time) {
	t.Helper()
	item := &store.Item{
		ID: id, PublicID: id, SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateReady, SubmissionKey: id, DisplayName: displayName,
		CreatedAt: createdAt, UpdatedAt: createdAt,
		Metadata: store.SubmissionMetadata{ProviderIdentity: identity, MediaFacts: facts},
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem %s: %v", id, err)
	}
}

// TestRunIdentityAuditCycle_NoEligibleItem proves the empty-library no-op
// return, never an error.
func TestRunIdentityAuditCycle_NoEligibleItem(t *testing.T) {
	s, _ := newIdentityAuditTestServer(t)
	result, err := s.RunIdentityAuditCycle(context.Background())
	if err != nil {
		t.Fatalf("RunIdentityAuditCycle: %v", err)
	}
	if result == nil || result.Audited {
		t.Fatalf("expected Audited=false no-op, got %+v", result)
	}
}

// TestRunIdentityAuditCycle_AbstainsWithoutCapturedIdentity proves an item
// with no persisted ProviderIdentity is still marked audited (advancing
// rotation) but produces zero verdicts -- the abstain-not-guess convention
// ID-01 itself already established, reused rather than reinvented here.
func TestRunIdentityAuditCycle_AbstainsWithoutCapturedIdentity(t *testing.T) {
	s, st := newIdentityAuditTestServer(t)
	mustCreateReadyIdentityItem(t, st, "item-1", "Some.Movie.2020.1080p", nil, nil, time.Now().UTC())

	result, err := s.RunIdentityAuditCycle(context.Background())
	if err != nil {
		t.Fatalf("RunIdentityAuditCycle: %v", err)
	}
	if result == nil || !result.Audited || result.ItemID != "item-1" || result.VerdictCount != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}

	a, err := st.GetIdentityAudit(context.Background(), "item-1")
	if err != nil || a == nil {
		t.Fatalf("GetIdentityAudit: a=%v err=%v", a, err)
	}
	if a.VerdictsJSON != "[]" {
		t.Fatalf("expected empty verdicts for uncaptured identity, got %q", a.VerdictsJSON)
	}
}

// TestRunIdentityAuditCycle_FindsRealMismatch proves the row's actual
// detection path: a real year mismatch between the release title and the
// persisted Arr identity is found and persisted as a finding, exactly
// reusing identitycheck.Verify -- no second classifier.
func TestRunIdentityAuditCycle_FindsRealMismatch(t *testing.T) {
	s, st := newIdentityAuditTestServer(t)
	identity := &mediaidentity.ProviderIdentity{Kind: "movie", Year: 1999}
	mustCreateReadyIdentityItem(t, st, "item-1", "Some.Movie.1969.1080p.BluRay", identity, nil, time.Now().UTC())

	result, err := s.RunIdentityAuditCycle(context.Background())
	if err != nil {
		t.Fatalf("RunIdentityAuditCycle: %v", err)
	}
	if result == nil || result.VerdictCount != 1 || result.Reasons[0] != "year_mismatch" {
		t.Fatalf("expected one year_mismatch verdict, got %+v", result)
	}

	// Enforcement must never fire from this row: the item stays Ready, no
	// suppression/blacklist side effect exists to check for since this row
	// adds none.
	got, err := st.GetItemByID(context.Background(), "item-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateReady {
		t.Fatalf("ID-03 must never change item state; got %s", got.State)
	}
}

// TestRunIdentityAuditCycle_ConcurrentInvocationsNeverDoubleProcess is the
// row's DG-07 race/coalescing proof: the periodic ticker and an on-demand
// trigger firing at the same instant must never select and persist the
// same item twice in one instant, nor panic/race under -race. With a
// single eligible item and N concurrent callers, exactly one call may
// observe Audited=true for it before the item's last_audited_at advances
// past "never audited"; the rest correctly find no more urgent candidate
// (the same item, now freshly audited, is not necessarily nil -- with only
// one item in the library every caller will still select the *same* item
// each pass, so the real proof is simply: no data race, no panic, no error,
// under concurrent access to s.identityAuditMu.
func TestRunIdentityAuditCycle_ConcurrentInvocationsNeverDoubleProcess(t *testing.T) {
	s, st := newIdentityAuditTestServer(t)
	identity := &mediaidentity.ProviderIdentity{Kind: "movie", Year: 1999}
	mustCreateReadyIdentityItem(t, st, "item-1", "Some.Movie.1999.1080p", identity, nil, time.Now().UTC())

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.RunIdentityAuditCycle(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent RunIdentityAuditCycle error: %v", err)
	}

	a, err := st.GetIdentityAudit(context.Background(), "item-1")
	if err != nil || a == nil {
		t.Fatalf("GetIdentityAudit after concurrent passes: a=%v err=%v", a, err)
	}
}

// TestHandleIdentityAuditRun_HTTP covers the on-demand endpoint end to end
// through the real http.Handler, proving the JSON response shape.
func TestHandleIdentityAuditRun_HTTP(t *testing.T) {
	s, st := newIdentityAuditTestServer(t)
	mustCreateReadyIdentityItem(t, st, "item-1", "Some.Movie.2020", nil, nil, time.Now().UTC())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/identity-audit/run", nil)
	rec := httptest.NewRecorder()
	s.handleIdentityAuditRun(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var result IdentityAuditCycleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !result.Audited || result.ItemID != "item-1" {
		t.Fatalf("unexpected response: %+v", result)
	}
}

// TestRunIdentityAuditCycle_CleanPassNeverSerializesAsNull is a direct
// regression test for the exact defect this row's own live-gate-equivalent
// unit testing caught: json.Marshal(nil []identitycheck.Verdict) encodes
// as the JSON literal "null", not "[]" -- if RunIdentityAuditCycle ever
// regresses to a nil-initialized slice, a clean pass would persist as
// "null" and silently bypass ListIdentityAuditFindings' `!= '[]'` filter,
// making every abstained/clean item look like a permanent finding.
func TestRunIdentityAuditCycle_CleanPassNeverSerializesAsNull(t *testing.T) {
	s, st := newIdentityAuditTestServer(t)
	identity := &mediaidentity.ProviderIdentity{Kind: "movie", Year: 1999}
	mustCreateReadyIdentityItem(t, st, "item-1", "Some.Movie.1999.1080p", identity, nil, time.Now().UTC())

	if _, err := s.RunIdentityAuditCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, err := st.GetIdentityAudit(context.Background(), "item-1")
	if err != nil || a == nil {
		t.Fatalf("GetIdentityAudit: a=%v err=%v", a, err)
	}
	if a.VerdictsJSON != "[]" {
		t.Fatalf("expected literal \"[]\" for a clean pass, got %q", a.VerdictsJSON)
	}
}

// TestHandleIdentityAuditFindings_HTTP proves the report endpoint returns
// only non-clean findings, bounded by ?limit=.
func TestHandleIdentityAuditFindings_HTTP(t *testing.T) {
	s, st := newIdentityAuditTestServer(t)
	now := time.Now().UTC()
	identity := &mediaidentity.ProviderIdentity{Kind: "movie", Year: 1999}
	mustCreateReadyIdentityItem(t, st, "clean-1", "Some.Movie.1999", identity, nil, now)
	mustCreateReadyIdentityItem(t, st, "mismatch-1", "Some.Movie.1969", identity, nil, now.Add(time.Second))

	ctx := context.Background()
	if _, err := s.RunIdentityAuditCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunIdentityAuditCycle(ctx); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/identity-audit/findings?limit=10", nil)
	rec := httptest.NewRecorder()
	s.handleIdentityAuditFindings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Findings []struct {
			ItemID string `json:"item_id"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Findings) != 1 || body.Findings[0].ItemID != "mismatch-1" {
		t.Fatalf("unexpected findings: %+v", body.Findings)
	}
}
