package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0040UpDownReUp(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/reactive-observability.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM reactive_events LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, "migrations", 39); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM reactive_events LIMIT 1`); err == nil {
		t.Fatal("reactive_events remains after down")
	}
	if err := goose.UpTo(db, "migrations", 40); err != nil {
		t.Fatal(err)
	}
}

func TestReactiveMetricsDeterministicLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/reactive-metrics.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	repo := New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	complete := func(id, mode string, at time.Time, weak bool) {
		t.Helper()
		if weak {
			createPendingProposal(t, repo, id, at)
		}
		r, created, beginErr := repo.BeginReactiveCommit(ctx, id, "item-"+id, "file-"+id, mode, at)
		if beginErr != nil || !created {
			t.Fatalf("begin %s: created=%v err=%v", id, created, beginErr)
		}
		r.ArrName, r.ArrKind, r.ArrItemID, r.ReviewRequired, r.ArrFileIDs = "arr", "movie", 1, weak, []int{1}
		if completeErr := repo.CompleteReactiveCommit(ctx, r, at); completeErr != nil {
			t.Fatalf("complete %s: %v", id, completeErr)
		}
	}
	complete("http:day-auto", "auto", now.Add(-time.Hour), false)
	complete("http:week-supervised", "supervised", now.Add(-3*24*time.Hour), true)
	complete("http:month-auto", "auto", now.Add(-20*24*time.Hour), false)
	if _, err := repo.BeginReactiveUndo(ctx, "http:day-auto", now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteReactiveUndo(ctx, "http:day-auto", now.Add(-29*time.Minute)); err != nil {
		t.Fatal(err)
	}
	createPendingProposal(t, repo, "http:parked", now.Add(-2*time.Hour))
	if _, _, err := repo.BeginReactiveCommit(ctx, "http:parked", "item-parked", "file-parked", "supervised", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := repo.ParkReactiveCommit(ctx, "http:parked", "identity_runtime_mismatch", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	createPendingProposal(t, repo, "http:progress", now.Add(-30*time.Minute))
	if _, _, err := repo.BeginReactiveCommit(ctx, "http:progress", "item-progress", "file-progress", "auto", now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordReactivePromotion(ctx, 4096, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetReactivePromotionHeadroom(ctx, 8192, now); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ReactiveMetrics(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.CommitsByPeriod["24h"]["auto"] != 1 || got.CommitsByPeriod["7d"]["supervised"] != 1 || got.CommitsByPeriod["30d"]["auto"] != 2 {
		t.Fatalf("commit windows = %+v", got.CommitsByPeriod)
	}
	if got.PendingByReason["positive_mismatch"] != 1 || got.OldestByReason["positive_mismatch"] != 7200 ||
		got.PendingByReason["weak_evidence"] != 1 || got.OldestByReason["weak_evidence"] != 259200 ||
		got.PendingByReason["supervised_review"] != 1 || got.OldestByReason["supervised_review"] != 1800 {
		t.Fatalf("pending = %+v ages=%+v", got.PendingByReason, got.OldestByReason)
	}
	if got.Identity["strong_evidence"] != 2 || got.Identity["weak_evidence"] != 1 || got.Identity["positive_mismatch"] != 1 {
		t.Fatalf("identity = %+v", got.Identity)
	}
	if got.UndoTotal != 1 || got.PromotionBytes != 4096 || !got.HeadroomKnown || got.HeadroomBytes != 8192 {
		t.Fatalf("lifecycle metrics = %+v", got)
	}
}

func TestReactiveMetricsCancellationConcurrencyAndRetention(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/reactive-bounds.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	repo := New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	if err := repo.RecordReactivePromotion(ctx, 7, now.Add(-367*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- repo.RecordReactivePromotion(ctx, 1, now)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.ReactiveMetrics(ctx, now)
	if err != nil || got.PromotionBytes != workers {
		t.Fatalf("bounded promotion bytes=%d err=%v", got.PromotionBytes, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repo.ReactiveMetrics(canceled, now); err == nil {
		t.Fatal("canceled metrics query succeeded")
	}
	if err := repo.RecordReactivePromotion(canceled, 1, now); err == nil {
		t.Fatal("canceled promotion write succeeded")
	}
	if err := repo.RecordReactivePromotion(ctx, 0, now); err == nil {
		t.Fatal("zero-byte promotion accepted")
	}
	if err := repo.SetReactivePromotionHeadroom(ctx, -1, now); err == nil {
		t.Fatal("negative headroom accepted")
	}
}
