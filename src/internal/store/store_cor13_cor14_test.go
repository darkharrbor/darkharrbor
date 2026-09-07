package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStoreCOR(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cor_test.db")
	db, err := Open(context.Background(), dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db)
}

func createItemAt(t *testing.T, s *Store, id string, state ItemState, at time.Time, strmPath *string) *Item {
	t.Helper()
	item := &Item{
		ID:            id,
		PublicID:      id,
		SourceType:    SourceTypeTorrent,
		ClientKind:    ClientKindQBit,
		Category:      "tv",
		State:         state,
		SubmissionKey: id,
		DisplayName:   id,
		StrmPath:      strmPath,
		CreatedAt:     at,
		UpdatedAt:     at,
	}
	if err := s.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem(%s): %v", id, err)
	}
	return item
}

// COR-13: the removed-item prune must never select materialized items --
// the items row + strm_blobs are the only playback identity behind library
// .strm files (imported torrents 404'd ~30 days after import before this).
func TestPrunableRemoved_MaterializedItemsAreNeverPrunable(t *testing.T) {
	s := newTestStoreCOR(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-45 * 24 * time.Hour)

	// Materialization receipt via strm_blobs (DB authority).
	blobbed := createItemAt(t, s, "removed-blobbed", StateRemoved, old, nil)
	if err := s.UpsertStrmBlobAt(ctx, blobbed.ID, 0, "tv/x/S01E01.strm", "http://dh:8381/stream/removed-blobbed/1?tok=t"); err != nil {
		t.Fatalf("UpsertStrmBlobAt: %v", err)
	}

	// Materialization receipt via strm_path only (pre-DB-authority items).
	sp := "/data/strm/tv/x/S01E02.strm"
	createItemAt(t, s, "removed-strmpath", StateRemoved, old, &sp)

	// Never materialized: the only legitimately prunable shape.
	createItemAt(t, s, "removed-bare", StateRemoved, old, nil)

	// Fresh removed item -- outside the cutoff regardless of shape.
	createItemAt(t, s, "removed-fresh", StateRemoved, time.Now().UTC(), nil)

	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	items, err := s.ListPrunableRemovedOlderThan(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("ListPrunableRemovedOlderThan: %v", err)
	}
	if len(items) != 1 || items[0].ID != "removed-bare" {
		got := make([]string, 0, len(items))
		for _, it := range items {
			got = append(got, it.ID)
		}
		t.Fatalf("prunable = %v, want exactly [removed-bare]", got)
	}

	// Full prune path: delete the bare item, then confirm the materialized
	// rows (the library playback identity) still resolve.
	if _, err := s.DeleteRemovedItemsByIDs(ctx, []string{"removed-bare"}); err != nil {
		t.Fatalf("DeleteRemovedItemsByIDs: %v", err)
	}
	for _, id := range []string{"removed-blobbed", "removed-strmpath"} {
		it, gerr := s.GetItemByID(ctx, id)
		if gerr != nil || it == nil {
			t.Fatalf("materialized removed item %s must survive prune (err=%v item=%v)", id, gerr, it)
		}
	}
	blobs, err := s.GetStrmBlobs(ctx, "removed-blobbed")
	if err != nil || len(blobs) != 1 {
		t.Fatalf("strm_blobs for removed-blobbed must survive prune (err=%v n=%d)", err, len(blobs))
	}
}

// COR-14: items stranded in StateAccepted by a crash between CreateItem and
// the Accepted->Resolving write must be requeued; fresh, claimed, and
// non-accepted items must be left alone.
func TestRequeueStaleAccepted(t *testing.T) {
	s := newTestStoreCOR(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-10 * time.Minute)

	createItemAt(t, s, "acc-stale", StateAccepted, old, nil)
	createItemAt(t, s, "acc-fresh", StateAccepted, time.Now().UTC(), nil)
	createItemAt(t, s, "res-stale", StateResolving, old, nil)

	claimed := createItemAt(t, s, "acc-claimed", StateAccepted, old, nil)
	if ok, err := s.ClaimItem(ctx, claimed.ID, "test-worker"); err != nil || !ok {
		t.Fatalf("ClaimItem: ok=%v err=%v", ok, err)
	}

	cutoff := time.Now().UTC().Add(-2 * time.Minute)
	ids, err := s.RequeueStaleAccepted(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("RequeueStaleAccepted: %v", err)
	}
	if len(ids) != 1 || ids[0] != "acc-stale" {
		t.Fatalf("requeued = %v, want exactly [acc-stale]", ids)
	}

	got, err := s.GetItemByID(ctx, "acc-stale")
	if err != nil || got == nil {
		t.Fatalf("GetItemByID(acc-stale): %v", err)
	}
	if got.State != StateResolving {
		t.Fatalf("acc-stale state = %s, want resolving", got.State)
	}
	if got.NextRunAt == nil {
		t.Fatalf("acc-stale next_run_at must be set so workers treat it as due")
	}

	for id, want := range map[string]ItemState{
		"acc-fresh":   StateAccepted,
		"res-stale":   StateResolving,
		"acc-claimed": StateAccepted,
	} {
		it, gerr := s.GetItemByID(ctx, id)
		if gerr != nil || it == nil {
			t.Fatalf("GetItemByID(%s): %v", id, gerr)
		}
		if it.State != want {
			t.Fatalf("%s state = %s, want %s (must be untouched)", id, it.State, want)
		}
	}

	// Idempotent: a second sweep at the same cutoff finds nothing.
	ids, err = s.RequeueStaleAccepted(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("RequeueStaleAccepted (second): %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("second sweep requeued %v, want none", ids)
	}
}
