package store

import (
	"context"
	"testing"
	"time"
)

func newTestStoreForErrorJournal(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/error-journal.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	s := New(db)
	now := time.Now().UTC()
	if err := s.CreateItem(ctx, &Item{
		ID: "item-1", PublicID: "item-1", SourceType: SourceTypeNZB, ClientKind: ClientKindSAB,
		Category: "tv", State: StateReady, SubmissionKey: "item-1", DisplayName: "item-1",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create item: %v", err)
	}
	return s, ctx
}

// TestAppendErrorJournal_RoundTrip proves a single append is readable back
// with all four fields intact.
func TestAppendErrorJournal_RoundTrip(t *testing.T) {
	s, ctx := newTestStoreForErrorJournal(t)
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	if err := s.AppendErrorJournal(ctx, "item-1", ErrorJournalEntry{
		Class: "transient_here", Rung: "nntp_primary", Provider: "newshosting", At: at,
	}, 20); err != nil {
		t.Fatalf("AppendErrorJournal: %v", err)
	}
	got, err := s.GetErrorJournal(ctx, "item-1")
	if err != nil {
		t.Fatalf("GetErrorJournal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].Class != "transient_here" || got[0].Rung != "nntp_primary" || got[0].Provider != "newshosting" || !got[0].At.Equal(at) {
		t.Fatalf("unexpected entry: %+v", got[0])
	}
}

// TestAppendErrorJournal_BoundedRing proves the ring is capped at
// maxEntries with the OLDEST entries dropped first (DG-05: bounded state).
func TestAppendErrorJournal_BoundedRing(t *testing.T) {
	s, ctx := newTestStoreForErrorJournal(t)
	for i := 0; i < 25; i++ {
		if err := s.AppendErrorJournal(ctx, "item-1", ErrorJournalEntry{
			Class: "transient_here",
			Rung:  "r",
			At:    time.Unix(int64(i), 0).UTC(),
		}, 20); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := s.GetErrorJournal(ctx, "item-1")
	if err != nil {
		t.Fatalf("GetErrorJournal: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("expected ring capped at 20, got %d", len(got))
	}
	// Oldest 5 (unix 0..4) must have been dropped; the ring keeps the most
	// recent 20 (unix 5..24), oldest-first.
	if got[0].At.Unix() != 5 {
		t.Fatalf("expected oldest retained entry at unix 5, got %d", got[0].At.Unix())
	}
	if got[19].At.Unix() != 24 {
		t.Fatalf("expected newest entry at unix 24, got %d", got[19].At.Unix())
	}
}

// TestAppendErrorJournal_DefaultMaxEntries proves a non-positive maxEntries
// falls back to DefaultErrorJournalMaxEntries rather than failing or
// producing an unbounded ring (never-fail-startup-shaped config contract).
func TestAppendErrorJournal_DefaultMaxEntries(t *testing.T) {
	s, ctx := newTestStoreForErrorJournal(t)
	for i := 0; i < DefaultErrorJournalMaxEntries+5; i++ {
		if err := s.AppendErrorJournal(ctx, "item-1", ErrorJournalEntry{
			Class: "unknown", Rung: "r", At: time.Unix(int64(i), 0).UTC(),
		}, 0); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := s.GetErrorJournal(ctx, "item-1")
	if err != nil {
		t.Fatalf("GetErrorJournal: %v", err)
	}
	if len(got) != DefaultErrorJournalMaxEntries {
		t.Fatalf("expected ring capped at default %d, got %d", DefaultErrorJournalMaxEntries, len(got))
	}
}

// TestAppendErrorJournal_AbstainsOnMalformedInput proves empty item ID and
// empty class are both silent no-ops (fail-closed, never guesses/panics).
func TestAppendErrorJournal_AbstainsOnMalformedInput(t *testing.T) {
	s, ctx := newTestStoreForErrorJournal(t)
	if err := s.AppendErrorJournal(ctx, "", ErrorJournalEntry{Class: "transient_here"}, 20); err != nil {
		t.Fatalf("expected nil error for empty item id, got %v", err)
	}
	if err := s.AppendErrorJournal(ctx, "item-1", ErrorJournalEntry{Class: ""}, 20); err != nil {
		t.Fatalf("expected nil error for empty class, got %v", err)
	}
	got, err := s.GetErrorJournal(ctx, "item-1")
	if err != nil {
		t.Fatalf("GetErrorJournal: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero entries after abstained appends, got %d", len(got))
	}
}

// TestAppendErrorJournal_NonexistentItemAbstains proves a nonexistent item
// ID never returns an error (best-effort reporting contract, matching
// reportFinalRung's own "reporter failure never masks the original stream
// error" precedent).
func TestAppendErrorJournal_NonexistentItemAbstains(t *testing.T) {
	s, ctx := newTestStoreForErrorJournal(t)
	if err := s.AppendErrorJournal(ctx, "does-not-exist", ErrorJournalEntry{Class: "transient_here", Rung: "r"}, 20); err != nil {
		t.Fatalf("expected nil error for nonexistent item, got %v", err)
	}
}

// TestAppendErrorJournal_DoesNotClobberOtherMetadata proves the targeted
// json_set touches only $.error_journal, leaving an unrelated existing
// metadata_json field (nntp_zero_fill_count) untouched -- the same
// concurrency-safety property IncrementNNTPZeroFill itself relies on.
func TestAppendErrorJournal_DoesNotClobberOtherMetadata(t *testing.T) {
	s, ctx := newTestStoreForErrorJournal(t)
	if _, err := s.IncrementNNTPZeroFill(ctx, "item-1"); err != nil {
		t.Fatalf("IncrementNNTPZeroFill: %v", err)
	}
	if err := s.AppendErrorJournal(ctx, "item-1", ErrorJournalEntry{Class: "transient_here", Rung: "r"}, 20); err != nil {
		t.Fatalf("AppendErrorJournal: %v", err)
	}
	item, err := s.GetItemByID(ctx, "item-1")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Metadata.NNTPZeroFillCount != 1 {
		t.Fatalf("expected zero-fill count to survive journal append untouched, got %d", item.Metadata.NNTPZeroFillCount)
	}
	if len(item.Metadata.ErrorJournal) != 1 {
		t.Fatalf("expected 1 journal entry on item, got %d", len(item.Metadata.ErrorJournal))
	}
}

// TestErrorJournalAggregateStats proves the DB-wide aggregate correctly
// sums totals and per-class counts across multiple items.
func TestErrorJournalAggregateStats(t *testing.T) {
	s, ctx := newTestStoreForErrorJournal(t)
	now := time.Now().UTC()
	if err := s.CreateItem(ctx, &Item{
		ID: "item-2", PublicID: "item-2", SourceType: SourceTypeNZB, ClientKind: ClientKindSAB,
		Category: "tv", State: StateReady, SubmissionKey: "item-2", DisplayName: "item-2",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create item-2: %v", err)
	}
	entries := []struct {
		item  string
		class string
	}{
		{"item-1", "transient_here"},
		{"item-1", "transient_here"},
		{"item-1", "permanent_here"},
		{"item-2", "transient_here"},
	}
	for _, e := range entries {
		if err := s.AppendErrorJournal(ctx, e.item, ErrorJournalEntry{Class: e.class, Rung: "r"}, 20); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	agg, err := s.ErrorJournalAggregateStats(ctx)
	if err != nil {
		t.Fatalf("ErrorJournalAggregateStats: %v", err)
	}
	if agg.Total != 4 {
		t.Fatalf("expected total 4, got %d", agg.Total)
	}
	if agg.ByClass["transient_here"] != 3 {
		t.Fatalf("expected 3 transient_here, got %d", agg.ByClass["transient_here"])
	}
	if agg.ByClass["permanent_here"] != 1 {
		t.Fatalf("expected 1 permanent_here, got %d", agg.ByClass["permanent_here"])
	}
}

// TestErrorJournal_RestartPersistence proves the journal survives closing
// and reopening the database (simulated process restart), matching the
// established convention (e.g. TestIdentityAudit_RestartPersistence).
func TestErrorJournal_RestartPersistence(t *testing.T) {
	path := t.TempDir() + "/error-journal-restart.db"
	ctx := context.Background()
	db1, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := RunMigrationsFS(db1, EmbeddedMigrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s1 := New(db1)
	now := time.Now().UTC()
	if err := s1.CreateItem(ctx, &Item{
		ID: "item-1", PublicID: "item-1", SourceType: SourceTypeNZB, ClientKind: ClientKindSAB,
		Category: "tv", State: StateReady, SubmissionKey: "item-1", DisplayName: "item-1",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create item: %v", err)
	}
	if err := s1.AppendErrorJournal(ctx, "item-1", ErrorJournalEntry{
		Class: "transient_here", Rung: "nntp_primary", Provider: "newshosting", At: now,
	}, 20); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	db2, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	s2 := New(db2)
	got, err := s2.GetErrorJournal(ctx, "item-1")
	if err != nil {
		t.Fatalf("GetErrorJournal after restart: %v", err)
	}
	if len(got) != 1 || got[0].Provider != "newshosting" {
		t.Fatalf("journal did not survive restart byte-identical: %+v", got)
	}
}
