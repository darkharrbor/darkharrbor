package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0028UpDown(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/torrent-meta-migration.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM torrent_meta LIMIT 1`); err != nil {
		t.Fatalf("torrent_meta missing after up: %v", err)
	}
	if err := goose.DownTo(db, "migrations", 27); err != nil {
		t.Fatalf("DownTo 27: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM torrent_meta LIMIT 1`); err == nil {
		t.Fatal("torrent_meta still exists after down migration")
	}
	if err := goose.UpTo(db, "migrations", 28); err != nil {
		t.Fatalf("UpTo 28: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM torrent_meta LIMIT 1`); err != nil {
		t.Fatalf("torrent_meta missing after re-up: %v", err)
	}
}
