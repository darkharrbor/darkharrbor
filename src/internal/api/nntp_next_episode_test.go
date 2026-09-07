package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/experience"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func rarItem(t *testing.T, id, displayName string, manifest []nntp.RARPart, totalSize int64, sourceURI string) *store.Item {
	t.Helper()
	manifestJSON, err := nntp.MarshalRARManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return &store.Item{
		ID: id, PublicID: id, SourceType: store.SourceTypeNZB, ClientKind: store.ClientKindSAB,
		Category: "tv", State: store.StateReady, SubmissionKey: id,
		DisplayName: displayName, SourceURI: &sourceURI,
		RarManifest: &manifestJSON, TotalSize: totalSize,
		CreatedAt: now, UpdatedAt: now,
	}
}

func TestNextEpisodeNNTPCandidateRequiresUniqueExactIdentity(t *testing.T) {
	manifest := []nntp.RARPart{{PartNum: 1, DataBytes: 1024}}
	current, _ := parseNNTPEpisodeIdentity("Show.Name.S02E03.1080p.WEB.x264-GROUP")

	exact := rarItem(t, "exact", "Show.Name.S02E04.1080p.WEB.x264-GROUP", manifest, 1024, "nzb-exact")
	wrongSeries := rarItem(t, "wrong-series", "Other.Show.S02E04.1080p.WEB.x264-GROUP", manifest, 1024, "nzb-wrong-series")
	wrongSeason := rarItem(t, "wrong-season", "Show.Name.S03E04.1080p.WEB.x264-GROUP", manifest, 1024, "nzb-wrong-season")
	wrongEpisode := rarItem(t, "wrong-episode", "Show.Name.S02E09.1080p.WEB.x264-GROUP", manifest, 1024, "nzb-wrong-episode")
	notRAR := rarItem(t, "not-rar", "Show.Name.S02E04.1080p.WEB.x264-GROUP", manifest, 1024, "nzb-not-rar")
	notRAR.RarManifest = nil

	got, ok := nextEpisodeNNTPCandidate(current, []*store.Item{wrongSeries, wrongSeason, wrongEpisode, notRAR, exact})
	if !ok || got.ID != "exact" {
		t.Fatalf("exact candidate = %#v/%v", got, ok)
	}

	duplicate := rarItem(t, "duplicate", "Show.Name.S02E04.720p.WEB.x264-GROUP2", manifest, 1024, "nzb-duplicate")
	if _, ok := nextEpisodeNNTPCandidate(current, []*store.Item{exact, duplicate}); ok {
		t.Fatal("ambiguous next episode must fail closed")
	}
}

func TestParseNNTPEpisodeIdentity(t *testing.T) {
	cases := []struct {
		name    string
		title   string
		wantOK  bool
		season  int
		episode int
		series  string
	}{
		{"standard", "Show.Name.S03E05.1080p.WEB.x264-GROUP", true, 3, 5, "show name"},
		{"padded", "Show.Name.S003E005.WEB", true, 3, 5, "show name"},
		{"spaces", "Show Name S01E02 720p", true, 1, 2, "show name"},
		{"no episode token", "Show.Name.Season.Pack.1080p", false, 0, 0, ""},
		{"obfuscated", "abc123def456.mkv", false, 0, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseNNTPEpisodeIdentity(c.title)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if got.season != c.season || got.episode != c.episode || got.series != c.series {
				t.Fatalf("got %+v, want series=%q season=%d episode=%d", got, c.series, c.season, c.episode)
			}
		})
	}
}

// fakeChunkSource is a minimal deterministic rangecache.ChunkSource used only
// to prove NextEpisodePrewarm was actually invoked with real chunk plumbing;
// it never touches a network.
type fakeChunkSource struct {
	key  string
	data []byte
}

func (f *fakeChunkSource) Key() string { return f.key }
func (f *fakeChunkSource) Chunks() []rangecache.ChunkRef {
	return []rangecache.ChunkRef{{Index: 0, Key: "0", Size: int64(len(f.data))}}
}
func (f *fakeChunkSource) Fetch(_ context.Context, _ rangecache.ChunkRef, _ rangecache.FetchClass) ([]byte, error) {
	return f.data, nil
}
func (f *fakeChunkSource) ExactSizes() bool { return true }

// fakeNNTPPrewarmProvider satisfies provider.UsenetProvider (stubs, unused by
// this test) plus nntpPrewarmSourceProvider (the seam actually exercised).
type fakeNNTPPrewarmProvider struct {
	calls  atomic.Int32
	src    rangecache.ChunkSource
	called chan struct{}
}

func (f *fakeNNTPPrewarmProvider) Name() string { return "fake-nntp" }
func (f *fakeNNTPPrewarmProvider) Probe(context.Context, []byte, int64) (string, error) {
	return "", nil
}
func (f *fakeNNTPPrewarmProvider) Stream(context.Context, string, []byte, int, http.ResponseWriter, string) error {
	return nil
}
func (f *fakeNNTPPrewarmProvider) CorrectedTotal(context.Context, string, []byte, int) int64 {
	return 0
}
func (f *fakeNNTPPrewarmProvider) StreamRARManifest(context.Context, string, int64, http.ResponseWriter, string, string, string) error {
	return nil
}
func (f *fakeNNTPPrewarmProvider) StreamZIPEntry(context.Context, string, int, http.ResponseWriter, string, string, string) error {
	return nil
}
func (f *fakeNNTPPrewarmProvider) GrabTimeHealthCheck(context.Context, []byte, int) (bool, int, error) {
	return false, 0, nil
}
func (f *fakeNNTPPrewarmProvider) PrewarmRARSource(_ context.Context, _, _ string, _ []nntp.RARPart) rangecache.ChunkSource {
	f.calls.Add(1)
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
	return f.src
}

func nntpNextEpisodeTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "nntp-next-episode.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = db.Close()
	})
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), PinnedBudgetMB: 8}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(cache.Close)
	exp := experience.New(cache, nil, "", experience.Config{
		Enabled: true, HotHeadBytes: 64 * 1024, Timeout: 5 * time.Second, NextEpisodeThreshold: 0.85,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	cfg := &config.Config{}
	cfg.Prewarm.NextEpisodeThreshold = 0.85
	cfg.Prewarm.TimeoutSec = 5
	s := &Server{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st,
		shutdownCtx: ctx, shutdownCancel: cancel, streamCache: cache, nntpExpSvc: exp,
		nextEpisodePrewarmed: make(map[string]struct{}),
	}
	return s, st
}

func TestNextEpisodeNNTPPrewarmThresholdDedupAndShutdown(t *testing.T) {
	s, st := nntpNextEpisodeTestServer(t)

	manifest := []nntp.RARPart{{PartNum: 1, DataBytes: 1024}}
	nextItem := rarItem(t, "next", "Show.Name.S01E02.1080p.WEB.x264-GROUP", manifest, 1024, "nzb-data-next")
	if err := st.CreateItem(context.Background(), nextItem); err != nil {
		t.Fatal(err)
	}

	current := rarItem(t, "current", "Show.Name.S01E01.1080p.WEB.x264-GROUP", manifest, 1024, "nzb-data-current")
	fakeSrc := &fakeChunkSource{key: "nntp|next|rar", data: make([]byte, 1024)}
	prov := &fakeNNTPPrewarmProvider{src: fakeSrc, called: make(chan struct{}, 1)}

	// Below threshold: first half of the file.
	s.queueNextEpisodeNNTPPrewarm(current, prov, "bytes=0-511")
	if prov.calls.Load() != 0 {
		t.Fatal("below-threshold playback scheduled prewarm")
	}

	// Crosses threshold: no Range header means "whole file", fraction 1.0.
	s.queueNextEpisodeNNTPPrewarm(current, prov, "")

	// Wait for the async goroutine to actually reach PrewarmRARSource before
	// issuing the duplicate call and Shutdown -- otherwise Shutdown's
	// shutdownCancel() can race the goroutine's own ListReadyNNTPItems call
	// on s.shutdownCtx and cancel it before it ever queries the store.
	select {
	case <-prov.called:
	case <-time.After(5 * time.Second):
		t.Fatal("next-episode prewarm did not reach PrewarmRARSource")
	}

	// Duplicate call for the same current item must be deduped.
	s.queueNextEpisodeNNTPPrewarm(current, prov, "")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown leaked predictive goroutine: %v", err)
	}
	if got := prov.calls.Load(); got != 1 {
		t.Fatalf("dedup PrewarmRARSource calls = %d, want 1", got)
	}
}

func TestNextEpisodeNNTPPrewarmSkipsNonRARSplit(t *testing.T) {
	s, _ := nntpNextEpisodeTestServer(t)
	direct := &store.Item{ID: "direct", DisplayName: "Show.Name.S01E01.1080p.WEB.x264-GROUP", TotalSize: 1024}
	// RarManifest is nil -> IsRARSplit() is false -> must no-op, not panic.
	prov := &fakeNNTPPrewarmProvider{}
	s.queueNextEpisodeNNTPPrewarm(direct, prov, "")
	if prov.calls.Load() != 0 {
		t.Fatal("non-RAR-split item must not trigger prewarm")
	}
}
