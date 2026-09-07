package rangecache

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type hr74Source struct {
	fetches atomic.Int64
}

func (s *hr74Source) Key() string { return "hr74-source" }
func (s *hr74Source) Chunks() []ChunkRef {
	return []ChunkRef{{Index: 0, Key: "chunk", Size: 4}}
}
func (s *hr74Source) Fetch(context.Context, ChunkRef, FetchClass) ([]byte, error) {
	s.fetches.Add(1)
	return []byte("data"), nil
}
func (s *hr74Source) ExactSizes() bool { return true }

func TestPlaybackFirstByteColdThenWarm(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	cache := New(Config{DiskCachePath: t.TempDir(), ReadaheadEnabled: false}, slog.Default())
	defer cache.Close()
	cache.now = func() time.Time { return now }
	src := &hr74Source{}

	cold := WithPlaybackTelemetry(context.Background(), "torrent", now.Add(-20*time.Millisecond))
	if err := cache.Stream(cold, src, ModeDisk, 4, httptest.NewRecorder(), "", 1); err != nil {
		t.Fatal(err)
	}
	warm := WithPlaybackTelemetry(context.Background(), "torrent", now.Add(-5*time.Millisecond))
	if err := cache.Stream(warm, src, ModeDisk, 4, httptest.NewRecorder(), "", 1); err != nil {
		t.Fatal(err)
	}
	if got := src.fetches.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
	snapshot := cache.SnapshotMap()
	if snapshot["first_byte_torrent_cold_count"] != int64(1) ||
		snapshot["first_byte_torrent_warm_count"] != int64(1) ||
		snapshot["first_byte_torrent_cold_total_ms"] != int64(20) ||
		snapshot["first_byte_torrent_warm_total_ms"] != int64(5) {
		t.Fatalf("unexpected playback snapshot: %#v", snapshot)
	}
}

func TestPlaybackTelemetryInvalidLaneAbstains(t *testing.T) {
	ctx := context.Background()
	if got := WithPlaybackTelemetry(ctx, "unsafe?query", time.Now()); got != ctx {
		t.Fatal("invalid lane did not abstain")
	}
}
