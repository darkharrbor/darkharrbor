package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0024UpDown(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/proof-migration.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO content_proofs
		(representation_id, scope, byte_offset, byte_length, kind, algorithm, digest, provenance, origin_id, observed_at)
		VALUES ('rep', 'whole', 0, 1, 'authoritative', 'sha256', zeroblob(32), 'http_digest', '', '2026-07-25T12:00:00Z')`); err != nil {
		t.Fatalf("insert migrated row: %v", err)
	}

	if err := goose.DownTo(db, "migrations", 23); err != nil {
		t.Fatalf("DownTo 23: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM content_proofs LIMIT 1`); err == nil {
		t.Fatal("content_proofs still exists after down migration")
	}
	if err := goose.UpTo(db, "migrations", 24); err != nil {
		t.Fatalf("UpTo 24: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM content_proofs LIMIT 1`); err != nil {
		t.Fatalf("content_proofs missing after re-up migration: %v", err)
	}
}
