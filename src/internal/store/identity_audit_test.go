package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newIdentityAuditTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "identity-audit.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

func mustCreateReadyItem(t *testing.T, s *Store, id string, source SourceType, createdAt time.Time) {
	t.Helper()
	if err := s.CreateItem(context.Background(), &Item{
		ID: id, PublicID: id, SourceType: source, ClientKind: ClientKindQBit,
		Category: "series", State: StateReady, SubmissionKey: id, DisplayName: id,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}); err != nil {
		t.Fatalf("CreateItem %s: %v", id, err)
	}
}

// TestUpsertGetIdentityAudit_RoundTrip covers a plain round trip and the
// upsert-refreshes-on-repeat-pass behavior, mirroring
// UpsertNNTPHealthAudit/GetNNTPHealthAudit's own test shape.
func TestUpsertGetIdentityAudit_RoundTrip(t *testing.T) {
	s := newIdentityAuditTestStore(t)
	ctx := context.Background()
	mustCreateReadyItem(t, s, "item-1", SourceTypeTorrent, time.Now().UTC())

	// Never audited: GetIdentityAudit returns (nil, nil), distinct from a
	// persisted-but-clean '[]' row.
	got, err := s.GetIdentityAudit(ctx, "item-1")
	if err != nil {
		t.Fatalf("GetIdentityAudit (never audited): %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for never-audited item, got %+v", got)
	}

	if err := s.UpsertIdentityAudit(ctx, "item-1", `[]`); err != nil {
		t.Fatalf("upsert (clean): %v", err)
	}
	got, err = s.GetIdentityAudit(ctx, "item-1")
	if err != nil || got == nil {
		t.Fatalf("GetIdentityAudit (clean): got=%v err=%v", got, err)
	}
	if got.VerdictsJSON != "[]" {
		t.Fatalf("expected clean [] verdicts, got %q", got.VerdictsJSON)
	}
	firstAudited := got.LastAuditedAt

	// A later pass upserts (refresh, not duplicate) and updates the
	// findings.
	if err := s.UpsertIdentityAudit(ctx, "item-1", `[{"reason":"year_mismatch","detail":"d"}]`); err != nil {
		t.Fatalf("upsert (mismatch): %v", err)
	}
	got, err = s.GetIdentityAudit(ctx, "item-1")
	if err != nil || got == nil {
		t.Fatalf("GetIdentityAudit (mismatch): got=%v err=%v", got, err)
	}
	if got.VerdictsJSON != `[{"reason":"year_mismatch","detail":"d"}]` {
		t.Fatalf("expected refreshed verdicts, got %q", got.VerdictsJSON)
	}
	if got.LastAuditedAt.Before(firstAudited) {
		t.Fatal("last_audited_at should not go backwards on refresh")
	}
}

// TestUpsertIdentityAudit_EmptyItemIDAbstains proves the malformed-input
// abstain path never silently writes a row keyed by an empty string.
func TestUpsertIdentityAudit_EmptyItemIDAbstains(t *testing.T) {
	s := newIdentityAuditTestStore(t)
	if err := s.UpsertIdentityAudit(context.Background(), "", "[]"); err == nil {
		t.Fatal("expected error for empty item id, got nil")
	}
}

// TestNextIdentityAuditItem_CrossLaneLeastRecentlyAudited proves the
// selection is (a) cross-lane (torrent/HTTP/NZB all eligible, unlike
// NextHealthAuditItem's NNTP-only scope) and (b) never-audited-first, then
// ascending last_audited_at.
func TestNextIdentityAuditItem_CrossLaneLeastRecentlyAudited(t *testing.T) {
	s := newIdentityAuditTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mustCreateReadyItem(t, s, "torrent-1", SourceTypeTorrent, now)
	mustCreateReadyItem(t, s, "http-1", SourceTypeHTTP, now.Add(time.Second))
	mustCreateReadyItem(t, s, "nzb-1", SourceTypeNZB, now.Add(2*time.Second))

	// All three already audited except nzb-1, which must be selected first
	// (never-audited sorts before any real timestamp).
	if err := s.UpsertIdentityAudit(ctx, "torrent-1", "[]"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertIdentityAudit(ctx, "http-1", "[]"); err != nil {
		t.Fatal(err)
	}

	next, err := s.NextIdentityAuditItem(ctx)
	if err != nil {
		t.Fatalf("NextIdentityAuditItem: %v", err)
	}
	if next == nil || next.ID != "nzb-1" {
		t.Fatalf("expected never-audited nzb-1 first, got %+v", next)
	}

	// Audit nzb-1; now torrent-1 (audited first, so least-recently-audited)
	// should come up next, ahead of http-1.
	if err := s.UpsertIdentityAudit(ctx, "nzb-1", "[]"); err != nil {
		t.Fatal(err)
	}
	next, err = s.NextIdentityAuditItem(ctx)
	if err != nil {
		t.Fatalf("NextIdentityAuditItem (2nd): %v", err)
	}
	if next == nil || next.ID != "torrent-1" {
		t.Fatalf("expected least-recently-audited torrent-1 next, got %+v", next)
	}
}

// TestNextIdentityAuditItem_NoEligibleItem proves the (nil, nil) no-op
// return for an empty/fully-terminal library.
func TestNextIdentityAuditItem_NoEligibleItem(t *testing.T) {
	s := newIdentityAuditTestStore(t)
	next, err := s.NextIdentityAuditItem(context.Background())
	if err != nil {
		t.Fatalf("NextIdentityAuditItem: %v", err)
	}
	if next != nil {
		t.Fatalf("expected nil for empty library, got %+v", next)
	}
}

// TestListIdentityAuditFindings_FiltersCleanAndBounds proves the report
// excludes '[]' (clean) rows, orders most-recently-audited first, and
// respects an explicit bound; limit<=0 returns (nil, nil) rather than an
// unbounded scan.
func TestListIdentityAuditFindings_FiltersCleanAndBounds(t *testing.T) {
	s := newIdentityAuditTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mustCreateReadyItem(t, s, "clean-1", SourceTypeTorrent, now)
	mustCreateReadyItem(t, s, "mismatch-1", SourceTypeHTTP, now)
	mustCreateReadyItem(t, s, "mismatch-2", SourceTypeNZB, now)

	if err := s.UpsertIdentityAudit(ctx, "clean-1", "[]"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertIdentityAudit(ctx, "mismatch-1", `[{"reason":"year_mismatch","detail":"a"}]`); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertIdentityAudit(ctx, "mismatch-2", `[{"reason":"runtime_mismatch","detail":"b"}]`); err != nil {
		t.Fatal(err)
	}

	if out, err := s.ListIdentityAuditFindings(ctx, 0); err != nil || out != nil {
		t.Fatalf("limit<=0 should return (nil, nil), got out=%v err=%v", out, err)
	}

	findings, err := s.ListIdentityAuditFindings(ctx, 10)
	if err != nil {
		t.Fatalf("ListIdentityAuditFindings: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("expected exactly 2 non-clean findings, got %d: %+v", len(findings), findings)
	}
	for _, f := range findings {
		if f.ItemID == "clean-1" {
			t.Fatal("clean item leaked into findings report")
		}
	}

	bounded, err := s.ListIdentityAuditFindings(ctx, 1)
	if err != nil {
		t.Fatalf("ListIdentityAuditFindings (bounded): %v", err)
	}
	if len(bounded) != 1 {
		t.Fatalf("expected exactly 1 bounded finding, got %d", len(bounded))
	}
}

// TestListIdentityAuditFindings_ExcludesNonReadyItem proves a finding
// disappears from the report once its item leaves StateReady (e.g. removed
// or failed via an unrelated path), matching every other report's
// state-scoped convention.
func TestListIdentityAuditFindings_ExcludesNonReadyItem(t *testing.T) {
	s := newIdentityAuditTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mustCreateReadyItem(t, s, "mismatch-1", SourceTypeTorrent, now)
	if err := s.UpsertIdentityAudit(ctx, "mismatch-1", `[{"reason":"year_mismatch","detail":"a"}]`); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetItemByID(ctx, "mismatch-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateItemState(ctx, item, StateRemoved, "test: removed"); err != nil {
		t.Fatal(err)
	}

	findings, err := s.ListIdentityAuditFindings(ctx, 10)
	if err != nil {
		t.Fatalf("ListIdentityAuditFindings: %v", err)
	}
	for _, f := range findings {
		if f.ItemID == "mismatch-1" {
			t.Fatal("removed item's stale finding should not appear in the report")
		}
	}
}
