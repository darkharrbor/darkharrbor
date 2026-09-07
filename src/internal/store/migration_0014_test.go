package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigration0014RetiresStubBlobsWithoutBreakingRuntimePaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := Open(ctx, filepath.Join(root, "dc9.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	s := New(db)

	for _, name := range []string{"stub_blobs", "idx_segment_offsets_item_file"} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, name).Scan(&count); err != nil {
			t.Fatalf("query sqlite_master for %s: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("schema object %s still exists after migration 0014", name)
		}
	}

	createdAt := formatTime(time.Now().UTC())
	if _, err := db.ExecContext(ctx, `
		INSERT INTO strm_blobs (item_id, file_index, rel_path, sha256, url, created_at)
		VALUES ('orphan', 0, 'orphan.strm', '', 'http://example/orphan', ?)`, createdAt); err != nil {
		t.Fatalf("insert orphan strm blob: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO segment_offsets (item_id, file_index, seg_index, decoded_bytes)
		VALUES ('orphan', 0, 0, 123) `); err != nil {
		t.Fatalf("insert orphan segment offset: %v", err)
	}
	if n, err := s.PruneOrphanSatelliteRows(ctx); err != nil {
		t.Fatalf("PruneOrphanSatelliteRows after migration 0014: %v", err)
	} else if n != 2 {
		t.Fatalf("PruneOrphanSatelliteRows removed %d rows, want 2", n)
	}

	mustCreateDC9Item(t, s, "ready", StateReady)
	const url = "http://darkharrbor:8381/dav/ready/0"
	if err := s.UpsertStrmBlobAt(ctx, "ready", 0, "ready/video.strm", url); err != nil {
		t.Fatalf("UpsertStrmBlobAt: %v", err)
	}
	dataRoot := filepath.Join(root, "data")
	res, err := s.ReconcileAll(ctx, dataRoot)
	if err != nil {
		t.Fatalf("ReconcileAll after migration 0014: %v", err)
	}
	if res.Checked != 1 || res.Restored != 1 || res.Errors != 0 {
		t.Fatalf("ReconcileAll result = %+v, want checked=1 restored=1 errors=0", res)
	}
	got, err := os.ReadFile(filepath.Join(dataRoot, "ready", "video.strm"))
	if err != nil {
		t.Fatalf("read reconciled .strm: %v", err)
	}
	if string(got) != url {
		t.Fatalf("reconciled .strm = %q, want %q", got, url)
	}

	mustCreateDC9Item(t, s, "removed", StateRemoved)
	if err := s.UpsertStrmBlobAt(ctx, "removed", 0, "removed/video.strm", url); err != nil {
		t.Fatalf("UpsertStrmBlobAt removed item: %v", err)
	}
	if _, err := s.execWrite(ctx, `INSERT INTO segment_offsets
		(item_id, file_index, seg_index, decoded_bytes) VALUES (?, ?, ?, ?)`,
		"removed", 0, 0, 456); err != nil {
		t.Fatalf("record segment for removed item: %v", err)
	}
	if n, err := s.DeleteRemovedItemsByIDs(ctx, []string{"removed"}); err != nil {
		t.Fatalf("DeleteRemovedItemsByIDs after migration 0014: %v", err)
	} else if n != 1 {
		t.Fatalf("DeleteRemovedItemsByIDs removed %d items, want 1", n)
	}
	assertDC9RowCount(t, s, `SELECT COUNT(*) FROM strm_blobs WHERE item_id=?`, "removed", 0)
	assertDC9RowCount(t, s, `SELECT COUNT(*) FROM segment_offsets WHERE item_id=?`, "removed", 0)
}

func mustCreateDC9Item(t *testing.T, s *Store, id string, state ItemState) {
	t.Helper()
	now := time.Now().UTC()
	item := &Item{
		ID:            id,
		PublicID:      id,
		SourceType:    SourceTypeNZB,
		ClientKind:    ClientKindSAB,
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

func assertDC9RowCount(t *testing.T, s *Store, query, itemID string, want int) {
	t.Helper()
	var got int
	if err := s.db.QueryRowContext(context.Background(), query, itemID).Scan(&got); err != nil {
		t.Fatalf("row count for %s: %v", itemID, err)
	}
	if got != want {
		t.Fatalf("row count for %s = %d, want %d", itemID, got, want)
	}
}
