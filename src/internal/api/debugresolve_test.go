package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func newTestStoreForDebugResolve(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "debugresolve.db")
	db, err := store.Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	return store.New(db)
}

func doDebugResolveRequest(s *Server, itemID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/debug/resolve/"+itemID, nil)
	req.SetPathValue("item_id", itemID)
	rec := httptest.NewRecorder()
	s.handleDebugResolve(rec, req)
	return rec
}

// TestHandleDebugResolve_MissingItemID proves DG-03's malformed-input
// abstain: an empty path segment never guesses an item, it refuses.
func TestHandleDebugResolve_MissingItemID(t *testing.T) {
	s := &Server{store: newTestStoreForDebugResolve(t), log: slog.Default()}
	rec := doDebugResolveRequest(s, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestHandleDebugResolve_StoreUnavailable proves a nil store abstains
// rather than panicking.
func TestHandleDebugResolve_StoreUnavailable(t *testing.T) {
	s := &Server{log: slog.Default()}
	rec := doDebugResolveRequest(s, "item1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestHandleDebugResolve_UnknownItem proves a nonexistent item 404s rather
// than fabricating a response.
func TestHandleDebugResolve_UnknownItem(t *testing.T) {
	s := &Server{store: newTestStoreForDebugResolve(t), log: slog.Default()}
	rec := doDebugResolveRequest(s, "does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func mustCreateDebugItem(t *testing.T, st *store.Store, item *store.Item) {
	t.Helper()
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
}

// TestHandleDebugResolve_TorrentLane_NoGovernor proves the no-governor
// branch never fabricates lease numbers.
func TestHandleDebugResolve_TorrentLane_NoGovernor(t *testing.T) {
	st := newTestStoreForDebugResolve(t)
	at := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID: "t1", PublicID: "pubt1", SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateReady, SubmissionKey: "subt1", DisplayName: "movie",
		CreatedAt: at, UpdatedAt: at,
	}
	mustCreateDebugItem(t, st, item)

	s := &Server{store: st, log: slog.Default(), providerOrder: []string{"torbox", "realdebrid"}}
	rec := doDebugResolveRequest(s, "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["lane"] != "torrent" {
		t.Fatalf("lane = %v, want torrent", body["lane"])
	}
	if body["lease"] != nil {
		t.Fatalf("lease = %v, want nil (no governor configured)", body["lease"])
	}
	cands, ok := body["candidates"].([]any)
	if !ok || len(cands) != 2 {
		t.Fatalf("candidates = %v, want 2 entries", body["candidates"])
	}
}

// TestHandleDebugResolve_TorrentLane_WithGovernor proves lease
// availability and predicted-rung reflect the real, live, non-blocking
// governor state -- no lease is ever acquired by this request.
func TestHandleDebugResolve_TorrentLane_WithGovernor(t *testing.T) {
	st := newTestStoreForDebugResolve(t)
	at := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID: "t2", PublicID: "pubt2", SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateReady, SubmissionKey: "subt2", DisplayName: "movie2",
		CreatedAt: at, UpdatedAt: at,
	}
	mustCreateDebugItem(t, st, item)

	gov := accountgov.New("test")
	gov.SetCapacity(TorrentCDNGovOp, 2)
	s := &Server{store: st, log: slog.Default(), providerOrder: []string{"torbox"}, torrentGov: gov}

	rec := doDebugResolveRequest(s, "t2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	lease, ok := body["lease"].(map[string]any)
	if !ok {
		t.Fatalf("lease = %v, want a lease object", body["lease"])
	}
	if lease["capacity"].(float64) != 2 {
		t.Fatalf("capacity = %v, want 2", lease["capacity"])
	}
	if lease["has_headroom"] != true {
		t.Fatalf("has_headroom = %v, want true (0 in use of 2 capacity)", lease["has_headroom"])
	}
	if body["predicted_rung"] != "primary-provider" {
		t.Fatalf("predicted_rung = %v, want primary-provider", body["predicted_rung"])
	}
	// Governor's real InUse/Waiting counters must be unaffected -- this
	// endpoint never acquires a lease.
	if gov.InUse(TorrentCDNGovOp) != 0 {
		t.Fatalf("InUse = %d, want 0 (dry-run must never acquire a lease)", gov.InUse(TorrentCDNGovOp))
	}
}

// TestHandleDebugResolve_NZBLane_DisclosesNoGovernor proves the NNTP lane's
// established no-governor limitation (HR1.4/NS-6.1) is disclosed honestly
// rather than a number being fabricated.
func TestHandleDebugResolve_NZBLane_DisclosesNoGovernor(t *testing.T) {
	st := newTestStoreForDebugResolve(t)
	at := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID: "n1", PublicID: "pubn1", SourceType: store.SourceTypeNZB, ClientKind: store.ClientKindSAB,
		Category: "tv", State: store.StateReady, SubmissionKey: "subn1", DisplayName: "show",
		CreatedAt: at, UpdatedAt: at,
	}
	mustCreateDebugItem(t, st, item)

	s := &Server{store: st, log: slog.Default(), usenetOrder: []string{"newshosting", "torbox-nntp"}}
	rec := doDebugResolveRequest(s, "n1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	leaseStr, ok := body["lease"].(string)
	if !ok || !strings.Contains(leaseStr, "not applicable") {
		t.Fatalf("lease = %v, want a disclosed not-applicable string", body["lease"])
	}
}

// TestHandleDebugResolve_LooksUpByPublicID proves public-ID lookup works
// when the caller does not have the internal ID.
func TestHandleDebugResolve_LooksUpByPublicID(t *testing.T) {
	st := newTestStoreForDebugResolve(t)
	at := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID: "internal3", PublicID: "public3", SourceType: store.SourceTypeHTTP, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateResolving, SubmissionKey: "subh3", DisplayName: "movie3",
		CreatedAt: at, UpdatedAt: at,
	}
	mustCreateDebugItem(t, st, item)

	s := &Server{store: st, log: slog.Default()}
	rec := doDebugResolveRequest(s, "public3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["item_id"] != "internal3" {
		t.Fatalf("item_id = %v, want internal3 (resolved via public_id lookup)", body["item_id"])
	}
}

// TestHandleDebugResolve_NoSecretLeak is this row's DG-04/LG-10 coverage:
// the response must never contain an upstream URL scheme marker or DH's
// own signed tok= query value, across every lane.
func TestHandleDebugResolve_NoSecretLeak(t *testing.T) {
	st := newTestStoreForDebugResolve(t)
	at := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	provider := "torbox"
	items := []*store.Item{
		{ID: "s1", PublicID: "p1", SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit,
			Category: "movies", State: store.StateReady, SubmissionKey: "ss1", DisplayName: "movie",
			Provider: &provider, CreatedAt: at, UpdatedAt: at},
		{ID: "s2", PublicID: "p2", SourceType: store.SourceTypeNZB, ClientKind: store.ClientKindSAB,
			Category: "tv", State: store.StateReady, SubmissionKey: "ss2", DisplayName: "show",
			CreatedAt: at, UpdatedAt: at},
		{ID: "s3", PublicID: "p3", SourceType: store.SourceTypeHTTP, ClientKind: store.ClientKindQBit,
			Category: "movies", State: store.StateReady, SubmissionKey: "ss3", DisplayName: "movie3",
			CreatedAt: at, UpdatedAt: at},
	}
	for _, it := range items {
		mustCreateDebugItem(t, st, it)
	}

	gov := accountgov.New("test")
	gov.SetCapacity(TorrentCDNGovOp, 4)
	s := &Server{
		store: st, log: slog.Default(),
		providerOrder: []string{"torbox", "realdebrid"},
		usenetOrder:   []string{"newshosting", "torbox-nntp"},
		torrentGov:    gov,
	}

	for _, it := range items {
		rec := doDebugResolveRequest(s, it.ID)
		if rec.Code != http.StatusOK {
			t.Fatalf("item %s: status = %d, body = %s", it.ID, rec.Code, rec.Body.String())
		}
		out := rec.Body.String()
		if strings.Contains(out, "://") || strings.Contains(out, "tok=") {
			t.Fatalf("item %s: response leaked an upstream URL/token marker: %s", it.ID, out)
		}
	}
}
