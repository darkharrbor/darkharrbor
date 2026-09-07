package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/pressly/goose/v3"
)

func TestMigration0042UpDownReUp(t *testing.T) {
	db, err := Open(t.Context(), t.TempDir()+"/pending.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM reactive_pending LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, "migrations", 41); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM reactive_pending LIMIT 1`); err == nil {
		t.Fatal("reactive_pending remains after down")
	}
	if err := goose.UpTo(db, "migrations", 42); err != nil {
		t.Fatal(err)
	}
}

func TestReactivePendingLifecyclePersistenceAndBounds(t *testing.T) {
	repo, closeDB := newReactivePendingStore(t)
	defer closeDB()
	ctx := t.Context()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	createPendingProposal(t, repo, "http:pending", now)

	entry, found, err := repo.GetReactivePending(ctx, "http:pending")
	if err != nil || !found || entry.Reason != "supervised_review" || entry.State != "pending" {
		t.Fatalf("initial pending=%+v found=%v err=%v", entry, found, err)
	}
	if _, err := repo.ListReactivePending(ctx, "", MaxReactivePendingPage+1); err == nil {
		t.Fatal("oversized page accepted")
	}
	if err := repo.ResolveReactivePending(ctx, entry.RepresentationID, "acknowledge", "", now); err == nil {
		t.Fatal("incompatible exit accepted")
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- repo.ResolveReactivePending(ctx, entry.RepresentationID, "jellyfin_only", "", now.Add(time.Second))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entry, found, err = repo.GetReactivePending(ctx, entry.RepresentationID)
	if err != nil || !found || entry.State != "resolved" || entry.ExitAction != "jellyfin_only" {
		t.Fatalf("resolved pending=%+v found=%v err=%v", entry, found, err)
	}
	if err := repo.ResolveReactivePending(ctx, entry.RepresentationID, "remove", "", now); err == nil {
		t.Fatal("conflicting repeat accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repo.ListReactivePending(canceled, "", 1); err == nil {
		t.Fatal("canceled list succeeded")
	}
}

func TestReactivePendingTransitionTriggersAndAssignments(t *testing.T) {
	repo, closeDB := newReactivePendingStore(t)
	defer closeDB()
	ctx := t.Context()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		id   string
		weak bool
	}{
		{id: "http:park"},
		{id: "http:weak", weak: true},
	} {
		createPendingProposal(t, repo, tc.id, now)
		r, created, err := repo.BeginReactiveCommit(ctx, tc.id, "item", "file", "supervised", now)
		if err != nil || !created {
			t.Fatalf("begin %s: created=%v err=%v", tc.id, created, err)
		}
		if tc.weak {
			r.ArrName, r.ArrKind, r.ArrItemID = "radarr", "movie", 1
			r.ArrFileIDs, r.ReviewRequired = []int{1}, true
			err = repo.CompleteReactiveCommit(ctx, r, now.Add(time.Second))
		} else {
			err = repo.ParkReactiveCommit(ctx, tc.id, "identity_runtime_mismatch", now.Add(time.Second))
		}
		if err != nil {
			t.Fatal(err)
		}
		entry, found, err := repo.GetReactivePending(ctx, tc.id)
		want := "positive_mismatch"
		if tc.weak {
			want = "weak_evidence"
		}
		if err != nil || !found || entry.Reason != want || entry.State != "pending" {
			t.Fatalf("transition %s=%+v found=%v err=%v", tc.id, entry, found, err)
		}
	}

	want := ReactiveAssignment{Kind: "movie", Provider: "tmdb", Value: "42", ArrName: "radarr"}
	if err := repo.PutReactiveAssignment(ctx, want, now); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.GetReactiveAssignment(ctx, "movie", "tmdb", "42")
	if err != nil || !found || got != want {
		t.Fatalf("assignment=%+v found=%v err=%v", got, found, err)
	}
}

func newReactivePendingStore(t *testing.T) (*Store, func()) {
	t.Helper()
	db, err := Open(t.Context(), t.TempDir()+"/queue.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return New(db), func() { _ = db.Close() }
}

func createPendingProposal(t *testing.T, repo *Store, id string, now time.Time) {
	t.Helper()
	created, err := repo.CreatePlaybackProposal(t.Context(), playbackcoverage.Proposal{
		RepresentationID: id, ItemID: "item", FileID: "file",
		ExtentKind: playbackcoverage.ExtentDeclaredBytes, Delivered: 50, Total: 100,
		Threshold: 0.5, CreatedAt: now,
	})
	if err != nil || !created {
		t.Fatalf("create %s: created=%v err=%v", id, created, err)
	}
}
