package store

import (
	"context"
	"testing"
	"time"
)

func TestMagnetTitle_UpsertAndGet_RoundTrips(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	if err := s.UpsertMagnetTitle(ctx, "abc123", "magnet:?xt=urn:btih:abc123", "Show.S05.DVDRip.x264-GRP"); err != nil {
		t.Fatalf("UpsertMagnetTitle: %v", err)
	}
	magnet, title, ok, err := s.GetMagnetTitle(ctx, "abc123")
	if err != nil {
		t.Fatalf("GetMagnetTitle: %v", err)
	}
	if !ok || magnet != "magnet:?xt=urn:btih:abc123" || title != "Show.S05.DVDRip.x264-GRP" {
		t.Fatalf("GetMagnetTitle = (%q, %q, %v), want the upserted row", magnet, title, ok)
	}
}

func TestMagnetTitle_GetMissing_ReturnsNotOK(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	_, _, ok, err := s.GetMagnetTitle(ctx, "never-seen")
	if err != nil {
		t.Fatalf("GetMagnetTitle: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a hash never recorded")
	}
	// Empty infoHash is a no-op, not an error, on both sides.
	if err := s.UpsertMagnetTitle(ctx, "", "magnet:x", "title"); err != nil {
		t.Fatalf("UpsertMagnetTitle(empty hash): %v", err)
	}
	if _, _, ok, err := s.GetMagnetTitle(ctx, ""); err != nil || ok {
		t.Fatalf("GetMagnetTitle(empty hash) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestMagnetTitle_Upsert_OverwritesExistingRow(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	if err := s.UpsertMagnetTitle(ctx, "abc123", "magnet:old", "Old.Title"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := s.UpsertMagnetTitle(ctx, "abc123", "magnet:new", "New.Title"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	magnet, title, ok, err := s.GetMagnetTitle(ctx, "abc123")
	if err != nil || !ok || magnet != "magnet:new" || title != "New.Title" {
		t.Fatalf("GetMagnetTitle after overwrite = (%q, %q, %v, %v), want the newer row", magnet, title, ok, err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM magnet_titles WHERE info_hash='abc123'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("row count = %d, want exactly 1 (upsert, not insert)", rows)
	}
}

// TestMagnetTitle_SurvivesAcrossStoreInstances is the direct regression test
// for C0.4: the in-memory magnetCache is wiped by any process restart, but a
// value durably recorded through this store method must still be readable
// by a freshly opened *Store against the same database file, simulating a
// DarkHarrbor restart between a Sonarr search and a later grab.
func TestMagnetTitle_SurvivesAcrossStoreInstances(t *testing.T) {
	dbPath := t.TempDir() + "/restart.db"
	ctx := context.Background()

	db1, err := Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if err := RunMigrationsFS(db1, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS (first): %v", err)
	}
	s1 := New(db1)
	if err := s1.UpsertMagnetTitle(ctx, "adca1824433af9d3076994b5a9fe1d7e65f3c22b", "magnet:?xt=urn:btih:adca1824433af9d3076994b5a9fe1d7e65f3c22b", "Frasier s11 720p"); err != nil {
		t.Fatalf("UpsertMagnetTitle: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close first db: %v", err)
	}

	// Simulate the restart: a brand new Store/DB handle against the same file.
	db2, err := Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	defer func() { _ = db2.Close() }()
	s2 := New(db2)
	magnet, title, ok, err := s2.GetMagnetTitle(ctx, "adca1824433af9d3076994b5a9fe1d7e65f3c22b")
	if err != nil {
		t.Fatalf("GetMagnetTitle (post-restart): %v", err)
	}
	if !ok || title != "Frasier s11 720p" {
		t.Fatalf("post-restart lookup = (%q, %q, %v), want the title recorded before the simulated restart", magnet, title, ok)
	}
}

func TestMagnetTitle_TTLPrunesOldRows(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixedNow }
	if err := s.UpsertMagnetTitle(ctx, "old-hash", "magnet:old", "Old"); err != nil {
		t.Fatalf("upsert old: %v", err)
	}

	// Advance the injectable clock well past the retention window and upsert
	// a second, unrelated hash — its own write opportunistically prunes the
	// now-stale first row.
	s.now = func() time.Time { return fixedNow.Add(magnetTitleTTL + 24*time.Hour) }
	if err := s.UpsertMagnetTitle(ctx, "new-hash", "magnet:new", "New"); err != nil {
		t.Fatalf("upsert new: %v", err)
	}

	if _, _, ok, err := s.GetMagnetTitle(ctx, "old-hash"); err != nil {
		t.Fatalf("GetMagnetTitle(old-hash): %v", err)
	} else if ok {
		t.Fatal("stale row past the TTL window was not pruned")
	}
	if _, _, ok, err := s.GetMagnetTitle(ctx, "new-hash"); err != nil || !ok {
		t.Fatalf("GetMagnetTitle(new-hash) = ok=%v err=%v, want ok=true", ok, err)
	}
}
