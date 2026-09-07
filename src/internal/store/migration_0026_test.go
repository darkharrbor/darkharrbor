package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0026UpDown(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/hls-session-migration.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO hls_sessions (id, item_id, file_id, created_at, expires_at)
		VALUES ('hs000000000000', 'item-1', 'file-1', '2026-07-25T12:00:00Z', '2026-07-25T18:00:00Z')`); err != nil {
		t.Fatalf("insert migrated session row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO hls_resources (id, session_id, kind, ref_json, created_at)
		VALUES ('hz000000000000', 'hs000000000000', 'master', '{}', '2026-07-25T12:00:00Z')`); err != nil {
		t.Fatalf("insert migrated resource row: %v", err)
	}

	// Cascade delete: removing the session must remove its resource too.
	if _, err := db.Exec(`DELETE FROM hls_sessions WHERE id='hs000000000000'`); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM hls_resources WHERE id='hz000000000000'`).Scan(&remaining); err != nil {
		t.Fatalf("count resources: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected cascade delete to remove the resource row, got %d remaining", remaining)
	}

	if err := goose.DownTo(db, "migrations", 25); err != nil {
		t.Fatalf("DownTo 25: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM hls_sessions LIMIT 1`); err == nil {
		t.Fatal("hls_sessions still exists after down migration")
	}
	if _, err := db.Exec(`SELECT 1 FROM hls_resources LIMIT 1`); err == nil {
		t.Fatal("hls_resources still exists after down migration")
	}

	if err := goose.UpTo(db, "migrations", 26); err != nil {
		t.Fatalf("UpTo 26: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM hls_sessions LIMIT 1`); err != nil {
		t.Fatalf("hls_sessions missing after re-up migration: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM hls_resources LIMIT 1`); err != nil {
		t.Fatalf("hls_resources missing after re-up migration: %v", err)
	}
}
