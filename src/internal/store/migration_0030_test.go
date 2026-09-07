package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0030UpDown(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/nntp-health-audit-migration.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM nntp_health_audit LIMIT 1`); err != nil {
		t.Fatalf("nntp_health_audit missing after up: %v", err)
	}
	if err := goose.DownTo(db, "migrations", 29); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM nntp_health_audit LIMIT 1`); err == nil {
		t.Fatal("nntp_health_audit still exists after down")
	}
	if err := goose.UpTo(db, "migrations", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM nntp_health_audit LIMIT 1`); err != nil {
		t.Fatalf("nntp_health_audit missing after re-up: %v", err)
	}
}
