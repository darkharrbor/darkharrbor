package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0029UpDown(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/provider-identities-migration.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM grab_provider_identities LIMIT 1`); err != nil {
		t.Fatalf("grab_provider_identities missing after up: %v", err)
	}
	if err := goose.DownTo(db, "migrations", 28); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM grab_provider_identities LIMIT 1`); err == nil {
		t.Fatal("grab_provider_identities still exists after down")
	}
	if err := goose.UpTo(db, "migrations", 29); err != nil {
		t.Fatal(err)
	}
}
