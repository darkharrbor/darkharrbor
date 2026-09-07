package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// TestMigration0032UpDownReUp is TS-5.1's DG-02 gate: migration up/down/
// re-up, mirroring the established migration_00NN_test.go convention
// (e.g. migration_0027_test.go).
func TestMigration0032UpDownReUp(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/hash-availability-migration.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM hash_availability LIMIT 1`); err != nil {
		t.Fatalf("hash_availability missing after up: %v", err)
	}
	if err := goose.DownTo(db, "migrations", 31); err != nil {
		t.Fatalf("DownTo 31: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM hash_availability LIMIT 1`); err == nil {
		t.Fatal("hash_availability still exists after down migration")
	}
	if err := goose.UpTo(db, "migrations", 32); err != nil {
		t.Fatalf("UpTo 32: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM hash_availability LIMIT 1`); err != nil {
		t.Fatalf("hash_availability missing after re-up: %v", err)
	}
}

// TestHashAvailability_RestartPersistence is TS-5.1's DG-02 restart-
// persistence proof: a recorded observation survives closing and reopening
// the same on-disk database (simulating a process restart), matching the
// established restart-persistence convention used elsewhere in this
// package (e.g. TS-0.1's torrent_meta restart tests).
func TestHashAvailability_RestartPersistence(t *testing.T) {
	path := t.TempDir() + "/hash-availability-restart.db"
	ctx := context.Background()

	db1, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := RunMigrationsFS(db1, EmbeddedMigrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s1 := New(db1)
	if err := s1.RecordHashAvailability(ctx, "torbox", "deadbeef", true, "submit", time.Hour); err != nil {
		t.Fatalf("record: %v", err)
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
	ha, found, err := s2.GetHashAvailability(ctx, "torbox", "deadbeef")
	if err != nil || !found {
		t.Fatalf("get after restart: found=%v err=%v", found, err)
	}
	if !ha.Cached || ha.Source != "submit" {
		t.Fatalf("unexpected row after restart: %+v", ha)
	}
}
