package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0025UpDown(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/continuity-migration.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO representation_continuity
		(representation_id, byte_offset, byte_length, digest, relation, proof_provenance, mutation, observed_at)
		VALUES ('rep', 0, 1, zeroblob(32), 'tofu', '', 0, '2026-07-25T12:00:00Z')`); err != nil {
		t.Fatalf("insert migrated row: %v", err)
	}

	if err := goose.DownTo(db, "migrations", 24); err != nil {
		t.Fatalf("DownTo 24: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM representation_continuity LIMIT 1`); err == nil {
		t.Fatal("representation_continuity still exists after down migration")
	}
	if err := goose.UpTo(db, "migrations", 25); err != nil {
		t.Fatalf("UpTo 25: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM representation_continuity LIMIT 1`); err != nil {
		t.Fatalf("representation_continuity missing after re-up migration: %v", err)
	}
}
