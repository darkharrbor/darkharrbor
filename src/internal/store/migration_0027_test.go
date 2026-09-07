package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0027UpDown(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/sidecar-migration.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM sidecar_resources LIMIT 1`); err != nil {
		t.Fatalf("sidecar_resources missing after up: %v", err)
	}
	if err := goose.DownTo(db, "migrations", 26); err != nil {
		t.Fatalf("DownTo 26: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM sidecar_resources LIMIT 1`); err == nil {
		t.Fatal("sidecar_resources still exists after down migration")
	}
	if err := goose.UpTo(db, "migrations", 27); err != nil {
		t.Fatalf("UpTo 27: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM sidecar_resources LIMIT 1`); err != nil {
		t.Fatalf("sidecar_resources missing after re-up: %v", err)
	}
}
