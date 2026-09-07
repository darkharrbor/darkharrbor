package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0041UpDownReUp(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/reactive-owner.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT owner_created FROM reactive_commits LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, "migrations", 40); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT owner_created FROM reactive_commits LIMIT 1`); err == nil {
		t.Fatal("owner_created remains after down")
	}
	if err := goose.UpTo(db, "migrations", 41); err != nil {
		t.Fatal(err)
	}
}

func TestReactiveCommitPersistsCreatedOwner(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/created-owner.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	repo := New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	record, created, err := repo.BeginReactiveCommit(ctx, "rep", "item", "file", "supervised", now)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	record.ArrName, record.ArrKind, record.ArrItemID = "arr", "movie", 4
	record.ArrFileIDs, record.OwnerCreated = []int{9}, true
	if err := repo.CompleteReactiveCommit(ctx, record, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repo.GetReactiveCommit(ctx, "rep")
	if err != nil || !found || !stored.OwnerCreated {
		t.Fatalf("found=%v ownerCreated=%v err=%v", found, stored.OwnerCreated, err)
	}
	undoing, err := repo.BeginReactiveUndo(ctx, "rep", now.Add(2*time.Second))
	if err != nil || !undoing.OwnerCreated {
		t.Fatalf("ownerCreated=%v err=%v", undoing.OwnerCreated, err)
	}
}
