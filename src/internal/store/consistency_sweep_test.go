package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mustCreateSweepItem creates a minimal Ready item for TS-0.5 sweep tests,
// mirroring mustCreateDC9Item's shape (migration_0014_test.go) but always
// StateReady, since ConsistencySweep's legibility checks are Ready-scoped.
func mustCreateSweepItem(t *testing.T, s *Store, id string) {
	t.Helper()
	now := time.Now().UTC()
	item := &Item{
		ID:            id,
		PublicID:      id,
		SourceType:    SourceTypeNZB,
		ClientKind:    ClientKindSAB,
		Category:      "darkharrbor",
		State:         StateReady,
		SubmissionKey: id,
		DisplayName:   id,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem(%s): %v", id, err)
	}
}

func newSweepTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	db, err := Open(ctx, filepath.Join(root, "sweep.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db)
}

// TestConsistencySweepHealsExactlyLikeReconcileAll proves ConsistencySweep's
// heal half is not a second heal path: an unconsumed Ready item's missing
// on-disk .strm is restored with the identical Checked/Restored/Errors shape
// ReconcileAll already produces, and it produces zero findings (no drift).
func TestConsistencySweepHealsExactlyLikeReconcileAll(t *testing.T) {
	ctx := context.Background()
	s := newSweepTestStore(t)
	mustCreateSweepItem(t, s, "healable")
	const url = "http://darkharrbor:8381/dav/healable/0"
	if err := s.UpsertStrmBlobAt(ctx, "healable", 0, "healable/video.strm", url); err != nil {
		t.Fatalf("UpsertStrmBlobAt: %v", err)
	}

	dataRoot := t.TempDir()
	res, err := s.ConsistencySweep(ctx, dataRoot)
	if err != nil {
		t.Fatalf("ConsistencySweep: %v", err)
	}
	if res.Checked != 1 || res.Restored != 1 || res.Errors != 0 {
		t.Fatalf("ConsistencySweep heal = %+v, want checked=1 restored=1 errors=0", res.MaterializeResult)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("ConsistencySweep findings = %+v, want none for a healable unconsumed item", res.Findings)
	}
	got, err := os.ReadFile(filepath.Join(dataRoot, "healable", "video.strm"))
	if err != nil {
		t.Fatalf("read healed .strm: %v", err)
	}
	if string(got) != url {
		t.Fatalf("healed .strm = %q, want %q", got, url)
	}
}

// TestConsistencySweepReportsNoOutputDrift proves a Ready item with neither
// a strm_path receipt nor any strm_blobs row is reported, never fabricated.
func TestConsistencySweepReportsNoOutputDrift(t *testing.T) {
	ctx := context.Background()
	s := newSweepTestStore(t)
	mustCreateSweepItem(t, s, "empty-ready")

	res, err := s.ConsistencySweep(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("ConsistencySweep: %v", err)
	}
	if res.Checked != 0 || res.Restored != 0 {
		t.Fatalf("ConsistencySweep heal = %+v, want a no-op heal (item has no blobs to heal)", res.MaterializeResult)
	}
	found := false
	for _, f := range res.Findings {
		if f.ItemID == "empty-ready" && f.Kind == "no_output" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ConsistencySweep findings = %+v, want a no_output finding for empty-ready", res.Findings)
	}
}

// TestConsistencySweepReportsConsumedPresentWithoutTouchingDisk proves a
// download-consumed Ready item whose strm_blobs rows still exist is reported
// as informational only -- ConsistencySweep must not restore, delete, or
// otherwise mutate anything for it (COR-3: consumed items are Arr's, not
// DH's, to manage on disk).
func TestConsistencySweepReportsConsumedPresentWithoutTouchingDisk(t *testing.T) {
	ctx := context.Background()
	s := newSweepTestStore(t)
	mustCreateSweepItem(t, s, "consumed")
	const url = "http://darkharrbor:8381/dav/consumed/0"
	if err := s.UpsertStrmBlobAt(ctx, "consumed", 0, "consumed/video.strm", url); err != nil {
		t.Fatalf("UpsertStrmBlobAt: %v", err)
	}
	if err := s.MarkDownloadConsumed(ctx, "consumed"); err != nil {
		t.Fatalf("MarkDownloadConsumed: %v", err)
	}

	dataRoot := t.TempDir()
	res, err := s.ConsistencySweep(ctx, dataRoot)
	if err != nil {
		t.Fatalf("ConsistencySweep: %v", err)
	}
	// COR-3 boundary: strmBlobItemIDs excludes download_consumed=1, so the
	// heal pass must not have touched this item at all.
	if res.Checked != 0 || res.Restored != 0 {
		t.Fatalf("ConsistencySweep heal = %+v, want zero -- consumed items are outside the heal boundary", res.MaterializeResult)
	}
	if _, statErr := os.Stat(filepath.Join(dataRoot, "consumed", "video.strm")); !os.IsNotExist(statErr) {
		t.Fatalf("consumed item's download-side .strm must not be (re)created by the sweep")
	}
	found := false
	for _, f := range res.Findings {
		if f.ItemID == "consumed" && f.Kind == "consumed_present" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ConsistencySweep findings = %+v, want a consumed_present finding for consumed", res.Findings)
	}
}

// TestConsistencySweepReportsOrphanBlob proves a strm_blobs row with no
// corresponding items row is reported as a defensive DB-resolves-cleanly
// finding, never deleted.
func TestConsistencySweepReportsOrphanBlob(t *testing.T) {
	ctx := context.Background()
	s := newSweepTestStore(t)
	createdAt := formatTime(time.Now().UTC())
	if _, err := s.db.ExecContext(ctx, `
        INSERT INTO strm_blobs (item_id, file_index, rel_path, sha256, url, created_at)
        VALUES ('orphan-item', 0, 'orphan.strm', '', 'http://example/orphan', ?)`, createdAt); err != nil {
		t.Fatalf("insert orphan strm blob: %v", err)
	}

	res, err := s.ConsistencySweep(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("ConsistencySweep: %v", err)
	}
	found := false
	for _, f := range res.Findings {
		if f.ItemID == "orphan-item" && f.Kind == "orphan_blob" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ConsistencySweep findings = %+v, want an orphan_blob finding for orphan-item", res.Findings)
	}
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM strm_blobs WHERE item_id='orphan-item'`).Scan(&count); err != nil {
		t.Fatalf("count orphan blob rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("orphan blob row count = %d, want 1 -- ConsistencySweep must never delete it", count)
	}
}

// TestConsistencySweepDoesNotTouchNonReadyItems proves a non-terminal,
// non-Ready item (e.g. still downloading, mirroring the TS-0.4 in-flight
// item this session must not touch) that happens to have no strm_blobs rows
// yet produces no findings and no heal activity -- the sweep is entirely a
// no-op for it, matching an item that has not reached resolve/materialize.
func TestConsistencySweepDoesNotTouchNonReadyItems(t *testing.T) {
	ctx := context.Background()
	s := newSweepTestStore(t)
	now := time.Now().UTC()
	item := &Item{
		ID:            "downloading-item",
		PublicID:      "downloading-item",
		SourceType:    SourceTypeTorrent,
		ClientKind:    ClientKindQBit,
		Category:      "darkharrbor",
		State:         StateResolving,
		SubmissionKey: "downloading-item",
		DisplayName:   "downloading-item",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreateItem(ctx, item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	res, err := s.ConsistencySweep(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("ConsistencySweep: %v", err)
	}
	if res.Checked != 0 || res.Restored != 0 || res.Errors != 0 {
		t.Fatalf("ConsistencySweep heal = %+v, want a full no-op for a not-yet-materialized item", res.MaterializeResult)
	}
	for _, f := range res.Findings {
		if f.ItemID == "downloading-item" {
			t.Fatalf("ConsistencySweep must not report any finding for a non-Ready item, got %+v", f)
		}
	}
}
