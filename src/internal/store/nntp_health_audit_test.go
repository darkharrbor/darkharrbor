package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStoreForHealthAudit(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "health-audit.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

func mustCreateReadyNNTPItem(t *testing.T, st *Store, id string, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateItem(ctx, &Item{
		ID: id, PublicID: id, SourceType: SourceTypeNZB, ClientKind: ClientKindSAB,
		Category: "movies", State: StateReady, SubmissionKey: id, DisplayName: id,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}); err != nil {
		t.Fatalf("create item %s: %v", id, err)
	}
}

// UpsertNNTPHealthAudit must round-trip every field exactly, including the
// decayed boolean and the bounded dead-region JSON blob.
func TestUpsertGetNNTPHealthAudit_RoundTrip(t *testing.T) {
	st := newTestStoreForHealthAudit(t)
	ctx := context.Background()
	mustCreateReadyNNTPItem(t, st, "item-1", time.Now().UTC())

	deadJSON := `[{"file_index":0,"segment_number":42,"message_id":"abc@example"}]`
	if err := st.UpsertNNTPHealthAudit(ctx, "item-1", 8, 6, 0.75, true, deadJSON); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.GetNNTPHealthAudit(ctx, "item-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected a persisted row, got nil")
	}
	if got.SegmentsSampled != 8 || got.SegmentsPresent != 6 || got.Completeness != 0.75 || !got.Decayed {
		t.Fatalf("unexpected round-trip: %+v", got)
	}
	if got.DeadRegionsJSON != deadJSON {
		t.Fatalf("dead regions json mismatch: got %q want %q", got.DeadRegionsJSON, deadJSON)
	}
	if got.LastAuditedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("expected non-zero timestamps: %+v", got)
	}
}

// A second upsert for the same item must overwrite, not duplicate (upsert
// on the item_id primary key), and must refresh last_audited_at.
func TestUpsertNNTPHealthAudit_OverwritesOnRepeatPass(t *testing.T) {
	st := newTestStoreForHealthAudit(t)
	ctx := context.Background()
	mustCreateReadyNNTPItem(t, st, "item-1", time.Now().UTC())

	first := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return first })
	if err := st.UpsertNNTPHealthAudit(ctx, "item-1", 8, 8, 1.0, false, "[]"); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}

	second := first.Add(time.Hour)
	st.SetClock(func() time.Time { return second })
	if err := st.UpsertNNTPHealthAudit(ctx, "item-1", 8, 3, 0.375, true, `[{"file_index":0,"segment_number":1,"message_id":"x"}]`); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	got, err := st.GetNNTPHealthAudit(ctx, "item-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SegmentsPresent != 3 || !got.Decayed {
		t.Fatalf("expected overwritten values, got %+v", got)
	}
	if !got.LastAuditedAt.Equal(second) {
		t.Fatalf("expected last_audited_at refreshed to %v, got %v", second, got.LastAuditedAt)
	}
}

// GetNNTPHealthAudit on a never-audited item returns (nil, nil), not an
// error -- callers must be able to distinguish "no evidence yet" from a
// real failure.
func TestGetNNTPHealthAudit_NeverAudited_ReturnsNilNil(t *testing.T) {
	st := newTestStoreForHealthAudit(t)
	got, err := st.GetNNTPHealthAudit(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// NextHealthAuditItem must prefer a never-audited item over one already
// audited, regardless of creation order.
func TestNextHealthAuditItem_NeverAuditedSortsFirst(t *testing.T) {
	st := newTestStoreForHealthAudit(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mustCreateReadyNNTPItem(t, st, "audited-old", now)
	mustCreateReadyNNTPItem(t, st, "never-audited", now.Add(time.Second))

	if err := st.UpsertNNTPHealthAudit(ctx, "audited-old", 8, 8, 1.0, false, "[]"); err != nil {
		t.Fatalf("seed audit: %v", err)
	}

	got, err := st.NextHealthAuditItem(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if got == nil || got.ID != "never-audited" {
		t.Fatalf("expected never-audited item first, got %+v", got)
	}
}

// Among already-audited items, the least-recently-audited one comes first
// (round-robin over the whole library across successive calls, given a
// fixed per-tick cadence).
func TestNextHealthAuditItem_LeastRecentlyAuditedFirst(t *testing.T) {
	st := newTestStoreForHealthAudit(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i, id := range []string{"item-a", "item-b", "item-c"} {
		mustCreateReadyNNTPItem(t, st, id, now.Add(time.Duration(i)*time.Second))
	}

	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return base.Add(3 * time.Hour) })
	if err := st.UpsertNNTPHealthAudit(ctx, "item-a", 8, 8, 1.0, false, "[]"); err != nil {
		t.Fatal(err)
	}
	st.SetClock(func() time.Time { return base.Add(1 * time.Hour) })
	if err := st.UpsertNNTPHealthAudit(ctx, "item-b", 8, 8, 1.0, false, "[]"); err != nil {
		t.Fatal(err)
	}
	st.SetClock(func() time.Time { return base.Add(2 * time.Hour) })
	if err := st.UpsertNNTPHealthAudit(ctx, "item-c", 8, 8, 1.0, false, "[]"); err != nil {
		t.Fatal(err)
	}

	got, err := st.NextHealthAuditItem(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if got == nil || got.ID != "item-b" {
		t.Fatalf("expected item-b (audited earliest), got %+v", got)
	}
}

// NextHealthAuditItem must exclude non-Ready, non-NZB, and TorBox-usenet-
// cache items -- the exact same lane scope as ListReadyNNTPItems.
func TestNextHealthAuditItem_ExcludesWrongStateOrLane(t *testing.T) {
	st := newTestStoreForHealthAudit(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := st.CreateItem(ctx, &Item{
		ID: "not-ready", PublicID: "not-ready", SourceType: SourceTypeNZB, ClientKind: ClientKindSAB,
		Category: "movies", State: StateResolving, SubmissionKey: "not-ready", DisplayName: "not-ready",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateItem(ctx, &Item{
		ID: "torrent-item", PublicID: "torrent-item", SourceType: SourceTypeTorrent, ClientKind: ClientKindQBit,
		Category: "movies", State: StateReady, SubmissionKey: "torrent-item", DisplayName: "torrent-item",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	remoteID := "tb-123"
	if err := st.CreateItem(ctx, &Item{
		ID: "torbox-nzb", PublicID: "torbox-nzb", SourceType: SourceTypeNZB, ClientKind: ClientKindSAB,
		Category: "movies", State: StateReady, SubmissionKey: "torbox-nzb", DisplayName: "torbox-nzb",
		RemoteID: &remoteID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.NextHealthAuditItem(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no eligible item, got %+v", got)
	}
}

// A DG-07-style bounded-search assertion: NextHealthAuditItem must not
// error or hang against an empty items table.
func TestNextHealthAuditItem_EmptyLibrary_ReturnsNilNil(t *testing.T) {
	st := newTestStoreForHealthAudit(t)
	got, err := st.NextHealthAuditItem(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}
