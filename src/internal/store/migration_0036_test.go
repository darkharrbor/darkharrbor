package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/pressly/goose/v3"
)

func TestMigration0036UpDownReUp(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/coverage.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM playback_coverage LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, "migrations", 35); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM playback_coverage LIMIT 1`); err == nil {
		t.Fatal("playback_coverage remains after down")
	}
	if err := goose.UpTo(db, "migrations", 36); err != nil {
		t.Fatal(err)
	}
}

func TestPlaybackCoverageUnionConcurrencyAndRestart(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/restart.db"
	db, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	tracker, _ := playbackcoverage.New(New(db), playbackcoverage.Options{Now: func() time.Time { return now }})
	spans := [][2]int64{{0, 10}, {5, 10}, {30, 5}, {15, 15}, {8, 4}}
	var wg sync.WaitGroup
	errs := make(chan error, len(spans))
	for _, span := range spans {
		span := span
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, observeErr := tracker.Observe(ctx, "http:coverage", span[0], span[1])
			errs <- observeErr
		}()
	}
	wg.Wait()
	close(errs)
	for observeErr := range errs {
		if observeErr != nil {
			t.Fatal(observeErr)
		}
	}
	snapshot, found, err := tracker.Snapshot(ctx, "http:coverage")
	if err != nil || !found || snapshot.DeliveredBytes != 35 || len(snapshot.Intervals) != 1 || snapshot.Intervals[0] != (playbackcoverage.Interval{Start: 0, End: 35}) {
		t.Fatalf("union = %+v, found=%v err=%v", snapshot, found, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tracker, _ = playbackcoverage.New(New(db), playbackcoverage.Options{})
	snapshot, found, err = tracker.Snapshot(ctx, "http:coverage")
	if err != nil || !found || snapshot.DeliveredBytes != 35 {
		t.Fatalf("restart union = %+v, found=%v err=%v", snapshot, found, err)
	}
}

func TestPlaybackCoverageIntervalLimitFailsClosed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/limit.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	tracker, _ := playbackcoverage.New(New(db), playbackcoverage.Options{MaxIntervals: 2})
	for _, offset := range []int64{0, 10} {
		if _, err := tracker.Observe(ctx, "torrent:limit", offset, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tracker.Observe(ctx, "torrent:limit", 20, 1); err == nil {
		t.Fatal("disjoint interval beyond limit accepted")
	}
	snapshot, _, _ := tracker.Snapshot(ctx, "torrent:limit")
	if snapshot.DeliveredBytes != 2 || len(snapshot.Intervals) != 2 {
		t.Fatalf("failed update changed state: %+v", snapshot)
	}
}
