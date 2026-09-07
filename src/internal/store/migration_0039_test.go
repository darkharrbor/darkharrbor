package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0039UpDownReUp(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/reactive.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM reactive_commits LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, "migrations", 38); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM reactive_commits LIMIT 1`); err == nil {
		t.Fatal("reactive_commits remains after down")
	}
	if err := goose.UpTo(db, "migrations", 39); err != nil {
		t.Fatal(err)
	}
}

func TestReactiveCommitBeginIsConcurrentIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/concurrent.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	repo := New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	const workers = 16
	var wg sync.WaitGroup
	created := make(chan bool, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, wasCreated, beginErr := repo.BeginReactiveCommit(ctx, "rep", "item", "file", "supervised", now)
			created <- wasCreated
			errs <- beginErr
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	count := 0
	for value := range created {
		if value {
			count++
		}
	}
	for beginErr := range errs {
		if beginErr != nil {
			t.Fatal(beginErr)
		}
	}
	if count != 1 {
		t.Fatalf("created=%d, want 1", count)
	}
}

func TestReactiveCommitAbortAllowsRetry(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/abort.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	repo := New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	if _, created, err := repo.BeginReactiveCommit(ctx, "rep", "item", "file", "supervised", now); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if err := repo.AbortReactiveCommit(ctx, "rep"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.GetReactiveCommit(ctx, "rep"); err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if _, created, err := repo.BeginReactiveCommit(ctx, "rep", "item", "file", "supervised", now.Add(time.Second)); err != nil || !created {
		t.Fatalf("retry created=%v err=%v", created, err)
	}
}

func TestReactiveCommitTerminalRecordIsIdempotentAcrossDispatchMode(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/terminal-mode.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	repo := New(db)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	record, created, err := repo.BeginReactiveCommit(ctx, "rep", "item", "file", "supervised", now)
	if err != nil || !created {
		t.Fatalf("begin created=%v err=%v", created, err)
	}
	record.ArrName, record.ArrKind, record.ArrItemID, record.ArrFileIDs = "radarr", "movie", 118, []int{9}
	if err := repo.CompleteReactiveCommit(ctx, record, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	got, created, err := repo.BeginReactiveCommit(ctx, "rep", "item", "file", "auto", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("terminal auto replay: %v", err)
	}
	if created || got.State != "committed" || got.Mode != "supervised" || got.ArrName != "radarr" {
		t.Fatalf("terminal replay created=%v record=%+v", created, got)
	}
	if _, _, err := repo.BeginReactiveCommit(ctx, "rep", "different-item", "file", "auto", now.Add(3*time.Second)); err == nil {
		t.Fatal("different item must still conflict")
	}
}

func TestReactiveCommitPersistsUndoNoRecommit(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/state.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	repo := New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	r, created, err := repo.BeginReactiveCommit(ctx, "rep", "item", "file", "supervised", now)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	prior := false
	r.ArrName, r.ArrKind, r.ArrItemID, r.ArrFileIDs, r.OwnerCreated, r.PriorMonitored = "arr", "movie", 4, []int{9}, true, &prior
	if err := repo.CompleteReactiveCommit(ctx, r, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	undoing, err := repo.BeginReactiveUndo(ctx, "rep", now.Add(2*time.Second))
	if err != nil || !undoing.OwnerCreated {
		t.Fatal(err)
	}
	if err := repo.CompleteReactiveUndo(ctx, "rep", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.BeginReactiveCommit(ctx, "rep", "item", "file", "supervised", now.Add(4*time.Second)); !errors.Is(err, ErrReactiveNoRecommit) {
		t.Fatalf("recommit err=%v", err)
	}
}
