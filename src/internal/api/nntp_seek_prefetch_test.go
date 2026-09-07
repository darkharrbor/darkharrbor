package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/experience"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type seekPrefetchSource struct {
	fetched chan int
	block   bool
}

func (*seekPrefetchSource) Key() string      { return "nntp|item|0|2" }
func (*seekPrefetchSource) ExactSizes() bool { return false }
func (*seekPrefetchSource) Chunks() []rangecache.ChunkRef {
	return []rangecache.ChunkRef{
		{Index: 0, Key: "chunk-a", Size: 100},
		{Index: 1, Key: "chunk-b", Size: 100},
	}
}
func (s *seekPrefetchSource) Fetch(ctx context.Context, ref rangecache.ChunkRef, _ rangecache.FetchClass) ([]byte, error) {
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s.fetched <- ref.Index
	return make([]byte, ref.Size), nil
}

type seekPrefetchProvider struct{ source rangecache.ChunkSource }

func (*seekPrefetchProvider) Name() string { return "test" }
func (*seekPrefetchProvider) Probe(context.Context, []byte, int64) (string, error) {
	return "", nil
}
func (*seekPrefetchProvider) Stream(context.Context, string, []byte, int, http.ResponseWriter, string) error {
	return nil
}
func (*seekPrefetchProvider) CorrectedTotal(context.Context, string, []byte, int) int64 {
	return 0
}
func (*seekPrefetchProvider) StreamRARManifest(context.Context, string, int64, http.ResponseWriter, string, string, string) error {
	return nil
}
func (*seekPrefetchProvider) StreamZIPEntry(context.Context, string, int, http.ResponseWriter, string, string, string) error {
	return nil
}
func (p *seekPrefetchProvider) SeekFileSource(string, []byte, int) rangecache.ChunkSource {
	return p.source
}
func (p *seekPrefetchProvider) SeekRARSource(context.Context, string, string, string) rangecache.ChunkSource {
	return p.source
}
func (p *seekPrefetchProvider) SeekZIPSource(context.Context, string, string, string, int) rangecache.ChunkSource {
	return p.source
}
func (*seekPrefetchProvider) GrabTimeHealthCheck(context.Context, []byte, int) (bool, int, error) {
	return false, 0, nil
}

var _ provider.UsenetProvider = (*seekPrefetchProvider)(nil)

func TestSeekTargetForRange(t *testing.T) {
	index := &mediatruth.Index{Entries: []mediatruth.Entry{
		{TimeMS: 0, Offset: 10},
		{TimeMS: 5000, Offset: 100},
		{TimeMS: 10000, Offset: 180},
	}}
	for _, tc := range []struct {
		header string
		want   int64
		ok     bool
	}{
		{"bytes=150-", 5000, true},
		{"bytes=180-199", 10000, true},
		{"bytes=0-", 0, false},
		{"bytes=-50", 0, false},
		{"bytes=150-160,180-190", 0, false},
		{"invalid", 0, false},
	} {
		got, ok := seekTargetForRange(tc.header, index)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("seekTargetForRange(%q) = %d,%v want %d,%v", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPrefetchNNTPSeekPinsIndexedChunkAndShutsDown(t *testing.T) {
	cache := rangecache.New(rangecache.Config{
		DiskCachePath:   t.TempDir(),
		DiskCacheSizeMB: 8,
		PinnedBudgetMB:  8,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer cache.Close()
	exp := experience.New(cache, nil, "", experience.Config{
		Enabled: true,
		Timeout: time.Second,
	}, nil, nil)
	shutdownCtx, cancel := context.WithCancel(context.Background())
	s := &Server{nntpExpSvc: exp, shutdownCtx: shutdownCtx, shutdownCancel: cancel}
	source := &seekPrefetchSource{fetched: make(chan int, 1)}
	prov := &seekPrefetchProvider{source: source}
	raw := "nzb"
	item := &store.Item{
		ID:        "item",
		SourceURI: &raw,
		Metadata: store.SubmissionMetadata{
			NNTPSeekSourceKey: source.Key(),
			NNTPSeekIndex: &mediatruth.Index{Entries: []mediatruth.Entry{
				{TimeMS: 0, Offset: 0},
				{TimeMS: 5000, Offset: 100},
			}},
		},
	}
	s.prefetchNNTPSeek(context.Background(), item, prov, 0, "content", "bytes=150-")
	select {
	case got := <-source.fetched:
		if got != 1 {
			t.Fatalf("fetched chunk %d, want indexed chunk 1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("seek prefetch did not complete")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPrefetchNNTPSeekCancellationDoesNotLeak(t *testing.T) {
	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 8},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer cache.Close()
	exp := experience.New(cache, nil, "", experience.Config{Enabled: true, Timeout: time.Second}, nil, nil)
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	s := &Server{nntpExpSvc: exp, shutdownCtx: shutdownCtx, shutdownCancel: shutdownCancel}
	source := &seekPrefetchSource{block: true}
	prov := &seekPrefetchProvider{source: source}
	raw := "nzb"
	item := &store.Item{ID: "item", SourceURI: &raw, Metadata: store.SubmissionMetadata{
		NNTPSeekSourceKey: source.Key(),
		NNTPSeekIndex: &mediatruth.Index{Entries: []mediatruth.Entry{
			{TimeMS: 0, Offset: 0},
			{TimeMS: 5000, Offset: 100},
		}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	s.prefetchNNTPSeek(ctx, item, prov, 0, "content", "bytes=150-")
	cancel()
	done, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := s.Shutdown(done); err != nil {
		t.Fatalf("shutdown waiting for cancelled prefetch: %v", err)
	}
}
