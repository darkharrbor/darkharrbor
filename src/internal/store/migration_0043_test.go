package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0043UpDownReUp(t *testing.T) {
	db, err := Open(t.Context(), t.TempDir()+"/promotion.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM reactive_promotions LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, "migrations", 42); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM reactive_promotions LIMIT 1`); err == nil {
		t.Fatal("reactive_promotions remains after down")
	}
	if err := goose.UpTo(db, "migrations", 43); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverReactivePromotionsIsExplicitAndCancellable(t *testing.T) {
	db, err := Open(t.Context(), t.TempDir()+"/recover.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := New(db).RecoverReactivePromotions(canceled, time.Now()); err == nil {
		t.Fatal("canceled recovery succeeded")
	}
}
