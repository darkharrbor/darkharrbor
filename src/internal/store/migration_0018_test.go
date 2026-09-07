package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCOR3DownloadConsumedReconciliation verifies the COR-3 fix:
//   - A Ready SAB/NZB item whose download-side .strm has been consumed by the
//     Arr (SABHistoryHidden / download_consumed) is NOT recreated by reconcile.
//   - An unconsumed Ready item whose .strm is missing IS restored.
//   - Failed/removed items remain excluded from reconcile.
//   - The library .strm (pointed at DH by URL) is unaffected.
func TestCOR3DownloadConsumedReconciliation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := Open(ctx, filepath.Join(root, "cor3.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	s := New(db)
	dataRoot := filepath.Join(root, "data")
	const url = "http://darkharrbor:8381/dav/test/0"

	// Case 1: unconsumed Ready NZB — must be restored by reconcile.
	mustCreateCOR3Item(t, s, "unconsumed", StateReady, ClientKindSAB)
	if err := s.UpsertStrmBlobAt(ctx, "unconsumed", 0, "unconsumed/video.strm", url); err != nil {
		t.Fatalf("UpsertStrmBlobAt unconsumed: %v", err)
	}

	// Case 2: consumed Ready NZB (SAB history hidden) — must NOT be restored.
	mustCreateCOR3Item(t, s, "consumed", StateReady, ClientKindSAB)
	if err := s.UpsertStrmBlobAt(ctx, "consumed", 0, "consumed/video.strm", url); err != nil {
		t.Fatalf("UpsertStrmBlobAt consumed: %v", err)
	}
	if err := s.MarkDownloadConsumed(ctx, "consumed"); err != nil {
		t.Fatalf("MarkDownloadConsumed: %v", err)
	}

	// Case 3: failed item — already excluded by terminal-state filter.
	mustCreateCOR3Item(t, s, "failed-item", StateFailed, ClientKindSAB)
	if err := s.UpsertStrmBlobAt(ctx, "failed-item", 0, "failed/video.strm", url); err != nil {
		t.Fatalf("UpsertStrmBlobAt failed: %v", err)
	}

	// Case 4: removed item — already excluded.
	mustCreateCOR3Item(t, s, "removed-item", StateRemoved, ClientKindQBit)
	if err := s.UpsertStrmBlobAt(ctx, "removed-item", 0, "removed/video.strm", url); err != nil {
		t.Fatalf("UpsertStrmBlobAt removed: %v", err)
	}

	// Case 5: consumed Ready qBit item — must NOT be restored.
	mustCreateCOR3Item(t, s, "consumed-qbit", StateReady, ClientKindQBit)
	if err := s.UpsertStrmBlobAt(ctx, "consumed-qbit", 0, "consumed-qbit/video.strm", url); err != nil {
		t.Fatalf("UpsertStrmBlobAt consumed-qbit: %v", err)
	}
	if err := s.MarkDownloadConsumed(ctx, "consumed-qbit"); err != nil {
		t.Fatalf("MarkDownloadConsumed consumed-qbit: %v", err)
	}

	// Run reconciliation.
	res, err := s.ReconcileAll(ctx, dataRoot)
	if err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	// Only the unconsumed item should have been checked and restored.
	if res.Checked != 1 {
		t.Errorf("ReconcileAll checked = %d, want 1 (only unconsumed)", res.Checked)
	}
	if res.Restored != 1 {
		t.Errorf("ReconcileAll restored = %d, want 1 (only unconsumed)", res.Restored)
	}
	if res.Errors != 0 {
		t.Errorf("ReconcileAll errors = %d, want 0", res.Errors)
	}

	// Verify the unconsumed .strm was materialized on disk.
	got, err := os.ReadFile(filepath.Join(dataRoot, "unconsumed", "video.strm"))
	if err != nil {
		t.Fatalf("read unconsumed .strm: %v", err)
	}
	if string(got) != url {
		t.Fatalf("unconsumed .strm = %q, want %q", got, url)
	}

	// Verify the consumed .strm was NOT materialized.
	if _, err := os.Stat(filepath.Join(dataRoot, "consumed", "video.strm")); !os.IsNotExist(err) {
		t.Fatalf("consumed .strm should not exist on disk, got err=%v", err)
	}

	// Run reconciliation again — the unconsumed one should now be intact.
	res2, err := s.ReconcileAll(ctx, dataRoot)
	if err != nil {
		t.Fatalf("ReconcileAll second pass: %v", err)
	}
	if res2.Checked != 1 {
		t.Errorf("ReconcileAll second pass checked = %d, want 1", res2.Checked)
	}
	if res2.Restored != 0 {
		t.Errorf("ReconcileAll second pass restored = %d, want 0 (already intact)", res2.Restored)
	}
}

// TestHideFromSABHistorySetsDownloadConsumed verifies the SAB history hide
// atomically sets download_consumed, preventing future reconcile recreations.
func TestHideFromSABHistorySetsDownloadConsumed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := Open(ctx, filepath.Join(root, "sab.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	s := New(db)

	mustCreateCOR3Item(t, s, "sab-item", StateReady, ClientKindSAB)
	if err := s.UpsertStrmBlobAt(ctx, "sab-item", 0, "sab/video.strm", "http://dh/dav/sab-item/0"); err != nil {
		t.Fatalf("UpsertStrmBlobAt: %v", err)
	}

	// Before hide: reconcile should include this item.
	ids, err := s.strmBlobItemIDs(ctx)
	if err != nil {
		t.Fatalf("strmBlobItemIDs before hide: %v", err)
	}
	if len(ids) != 1 || ids[0] != "sab-item" {
		t.Fatalf("strmBlobItemIDs before hide = %v, want [sab-item]", ids)
	}

	// Hide from SAB history (simulates Arr import completion).
	hidden, err := s.HideFromSABHistory(ctx, "sab-item")
	if err != nil {
		t.Fatalf("HideFromSABHistory: %v", err)
	}
	if !hidden {
		t.Fatalf("HideFromSABHistory returned false")
	}

	// After hide: reconcile should exclude this item.
	ids2, err := s.strmBlobItemIDs(ctx)
	if err != nil {
		t.Fatalf("strmBlobItemIDs after hide: %v", err)
	}
	if len(ids2) != 0 {
		t.Fatalf("strmBlobItemIDs after hide = %v, want []", ids2)
	}
}

// TestDownloadConsumedPersistence verifies that MarkDownloadConsumed persists
// correctly and is read back by GetItem.
func TestDownloadConsumedPersistence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := Open(ctx, filepath.Join(root, "persist.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	s := New(db)

	mustCreateCOR3Item(t, s, "persist-test", StateReady, ClientKindSAB)

	// Before marking consumed.
	got, err := s.GetItemByID(ctx, "persist-test")
	if err != nil {
		t.Fatalf("GetItem before: %v", err)
	}
	if got.DownloadConsumed {
		t.Fatalf("DownloadConsumed = true before mark, want false")
	}

	// Mark consumed.
	if err := s.MarkDownloadConsumed(ctx, "persist-test"); err != nil {
		t.Fatalf("MarkDownloadConsumed: %v", err)
	}

	// After marking consumed.
	got2, err := s.GetItemByID(ctx, "persist-test")
	if err != nil {
		t.Fatalf("GetItem after: %v", err)
	}
	if !got2.DownloadConsumed {
		t.Fatalf("DownloadConsumed = false after mark, want true")
	}
}

func mustCreateCOR3Item(t *testing.T, s *Store, id string, state ItemState, kind ClientKind) {
	t.Helper()
	now := time.Now().UTC()
	item := &Item{
		ID:            id,
		PublicID:      id,
		SourceType:    SourceTypeNZB,
		ClientKind:    kind,
		Category:      "darkharrbor",
		State:         state,
		SubmissionKey: id,
		DisplayName:   id,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem(%s): %v", id, err)
	}
}
