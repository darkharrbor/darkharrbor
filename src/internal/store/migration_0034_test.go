package store

import (
	"context"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/searchbudget"
	"github.com/pressly/goose/v3"
)

func TestMigration0034UpDownReUp(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/search-budget.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM search_budgets LIMIT 1`); err != nil {
		t.Fatalf("search_budgets missing after up: %v", err)
	}
	if err := goose.DownTo(db, "migrations", 33); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM search_budgets LIMIT 1`); err == nil {
		t.Fatal("search_budgets remains after down")
	}
	if err := goose.UpTo(db, "migrations", 34); err != nil {
		t.Fatal(err)
	}
}

func TestSearchBudgetPersistenceAndAtomicCap(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/restart.db"
	db, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		if _, err := st.RecordSearchNoResult(ctx, "movie:tmdb:1", 5, 7*24*time.Hour, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	state, found, err := New(db).GetSearchBudget(ctx, "movie:tmdb:1")
	if err != nil || !found {
		t.Fatalf("persisted state missing: found=%v err=%v", found, err)
	}
	if state.ConsecutiveNoResults != 5 || !state.SuppressedUntil.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("unexpected state: %+v", state)
	}
	var _ searchbudget.Repository = New(db)
}
