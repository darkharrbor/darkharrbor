package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0031UpDown(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/torrent-meta-piece-layers-migration.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	if _, err := db.Exec(`SELECT piece_layers_json FROM torrent_meta LIMIT 1`); err != nil {
		t.Fatalf("piece_layers_json missing after up: %v", err)
	}
	if err := goose.DownTo(db, "migrations", 30); err != nil {
		t.Fatalf("DownTo 30: %v", err)
	}
	if _, err := db.Exec(`SELECT piece_layers_json FROM torrent_meta LIMIT 1`); err == nil {
		t.Fatal("piece_layers_json still present after down migration")
	}
	// torrent_meta itself (from 0028) must survive the 0031 down migration
	// untouched -- only the new column is dropped.
	if _, err := db.Exec(`SELECT info_hash FROM torrent_meta LIMIT 1`); err != nil {
		t.Fatalf("torrent_meta table itself damaged by down migration: %v", err)
	}
	if err := goose.UpTo(db, "migrations", 31); err != nil {
		t.Fatalf("UpTo 31: %v", err)
	}
	if _, err := db.Exec(`SELECT piece_layers_json FROM torrent_meta LIMIT 1`); err != nil {
		t.Fatalf("piece_layers_json missing after re-up: %v", err)
	}
}
