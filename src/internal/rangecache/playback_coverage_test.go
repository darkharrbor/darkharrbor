package rangecache_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type partialResponseWriter struct {
	header http.Header
	limit  int
}

func (w *partialResponseWriter) Header() http.Header { return w.header }
func (*partialResponseWriter) WriteHeader(int)       {}
func (w *partialResponseWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		return w.limit, nil
	}
	return len(p), nil
}

func TestStreamCountsOnlySuccessfulClientWrites(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/stream.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	tracker, _ := playbackcoverage.New(store.New(db), playbackcoverage.Options{})
	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)), tracker)
	t.Cleanup(cache.Close)
	source, err := rangecache.NewByteSourceBlocks(bytesource.NewMemSource("source", []byte("0123456789")), 10)
	if err != nil {
		t.Fatal(err)
	}
	ctx = playbackcoverage.WithRepresentation(ctx, "http:partial")
	writer := &partialResponseWriter{header: make(http.Header), limit: 3}
	if err := cache.Stream(ctx, source, rangecache.ModeNone, 10, writer, "bytes=2-7", 1); err == nil {
		t.Fatal("short client write returned nil error")
	}
	snapshot, found, err := tracker.Snapshot(context.Background(), "http:partial")
	if err != nil || !found || snapshot.DeliveredBytes != 3 || len(snapshot.Intervals) != 1 || snapshot.Intervals[0] != (playbackcoverage.Interval{Start: 2, End: 5}) {
		t.Fatalf("coverage = %+v found=%v err=%v", snapshot, found, err)
	}
}

func TestCacheFetchWithoutPlaybackContextDoesNotCount(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"prefetch.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	tracker, _ := playbackcoverage.New(store.New(db), playbackcoverage.Options{})
	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)), tracker)
	t.Cleanup(cache.Close)
	source, _ := rangecache.NewByteSourceBlocks(bytesource.NewMemSource("prefetch", []byte("0123456789")), 10)
	if _, err := cache.GetChunk(ctx, source, rangecache.ModeDisk, source.Chunks()[0]); err != nil {
		t.Fatal(err)
	}
	if _, found, err := tracker.Snapshot(ctx, "http:prefetch"); err != nil || found {
		t.Fatalf("prefetch created coverage: found=%v err=%v", found, err)
	}
}
