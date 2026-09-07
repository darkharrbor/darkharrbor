package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/experience"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func episodeKey(t *testing.T, backend string, season, episode int, ids map[string]string) httpstream.ResolveKey {
	t.Helper()
	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: backend, Handler: "generic",
		Kind: "episode", IDs: ids, Season: season, Episode: episode,
	}
	canonical, err := key.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := httpstream.ParseResolveKey(canonical)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func episodeItem(t *testing.T, id string, key httpstream.ResolveKey, files []httpstream.PersistedFile) *store.Item {
	t.Helper()
	canonical, err := key.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	fileJSON, err := httpstream.MarshalFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	return &store.Item{ID: id, ResolveKey: &canonical, FileList: &fileJSON}
}

func TestNextEpisodeCandidateRequiresUniqueExactIdentity(t *testing.T) {
	current := episodeKey(t, "backend-a", 2, 3, map[string]string{"tmdb": "42"})
	file := httpstream.PersistedFile{
		FileID: "hf000000000001", Selector: "next", Name: "next.mkv",
		Size: 1024, RangeVerified: true,
	}
	exact := episodeItem(t, "exact", episodeKey(t, "backend-a", 2, 4, map[string]string{"tmdb": "42"}), []httpstream.PersistedFile{file})
	wrongID := episodeItem(t, "wrong-id", episodeKey(t, "backend-a", 2, 4, map[string]string{"tmdb": "99"}), []httpstream.PersistedFile{file})
	wrongSeason := episodeItem(t, "wrong-season", episodeKey(t, "backend-a", 3, 4, map[string]string{"tmdb": "42"}), []httpstream.PersistedFile{file})

	got, gotFile, _, ok := nextEpisodeCandidate(current, []*store.Item{wrongID, wrongSeason, exact})
	if !ok || got.ID != exact.ID || gotFile.FileID != file.FileID {
		t.Fatalf("exact candidate = %#v/%#v/%v", got, gotFile, ok)
	}
	duplicate := episodeItem(t, "duplicate", episodeKey(t, "backend-a", 2, 4, map[string]string{"tmdb": "42"}), []httpstream.PersistedFile{file})
	if _, _, _, ok := nextEpisodeCandidate(current, []*store.Item{exact, duplicate}); ok {
		t.Fatal("ambiguous next episode must fail closed")
	}
	hls := file
	hls.Name = "next.m3u8"
	if _, _, _, ok := nextEpisodeCandidate(current, []*store.Item{
		episodeItem(t, "hls", episodeKey(t, "backend-a", 2, 4, map[string]string{"tmdb": "42"}), []httpstream.PersistedFile{hls}),
	}); ok {
		t.Fatal("manifest candidate must not enter progressive prewarm")
	}
}

type genericPrewarmHandler struct {
	url   string
	size  int64
	calls atomic.Int32
}

func (h *genericPrewarmHandler) Name() string { return "generic" }
func (h *genericPrewarmHandler) Search(context.Context, httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	return nil, nil
}
func (h *genericPrewarmHandler) Resolve(context.Context, httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	h.calls.Add(1)
	return []httpstream.ResolvedFile{{
		Selector: "next", Name: "next.mkv", Size: h.size, URL: h.url, SupportsRange: true,
	}}, nil
}

func nextEpisodeTestServer(t *testing.T, origin *httptest.Server, handler *genericPrewarmHandler) (*Server, *store.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "next-episode.db"), 5*time.Second)
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
	gov := accountgov.New("next-episode-test")
	exp := experience.New(cache, gov, HTTPSourceGovOp, experience.Config{
		Enabled: true, HotHeadBytes: 64 * 1024, Timeout: 5 * time.Second, NextEpisodeThreshold: 0.85,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	registry := httpstream.NewRegistry()
	registry.Register("backend-a", 1, handler)
	cfg := &config.Config{}
	cfg.Prewarm.NextEpisodeThreshold = 0.85
	cfg.Prewarm.TimeoutSec = 5
	s := &Server{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st,
		shutdownCtx: ctx, shutdownCancel: cancel, streamCache: cache, httpGov: gov,
		httpExpSvc: exp, httpHandlers: registry, httpResolveCoord: newHTTPResolveCoordinator(),
		nextEpisodePrewarmed: make(map[string]struct{}),
	}
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	return s, st
}

func TestNextEpisodePrewarmThresholdDedupAndShutdown(t *testing.T) {
	body := make([]byte, 256*1024)
	started := make(chan struct{}, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		http.ServeContent(w, r, "next.mkv", time.Unix(0, 0), bytes.NewReader(body))
	}))
	defer origin.Close()
	handler := &genericPrewarmHandler{url: origin.URL + "/next.mkv", size: int64(len(body))}
	s, st := nextEpisodeTestServer(t, origin, handler)
	currentKey := episodeKey(t, "backend-a", 1, 1, map[string]string{"tmdb": "42"})
	nextKey := episodeKey(t, "backend-a", 1, 2, map[string]string{"tmdb": "42"})
	file := httpstream.PersistedFile{
		FileID: "hf000000000002", Selector: "next", Name: "next.mkv",
		Size: int64(len(body)), RangeVerified: true,
	}
	next := episodeItem(t, "next", nextKey, []httpstream.PersistedFile{file})
	now := time.Now().UTC()
	next.PublicID, next.SourceType, next.ClientKind = "next", store.SourceTypeHTTP, store.ClientKindQBit
	next.Category, next.State, next.SubmissionKey, next.DisplayName = "series", store.StateReady, "next", "Next"
	next.CreatedAt, next.UpdatedAt = now, now
	if err := st.CreateItem(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	current := &store.Item{ID: "current"}
	currentFile := httpstream.PersistedFile{Size: int64(len(body))}

	s.queueNextEpisodePrewarm(current, currentKey, currentFile, &httpstream.ByteRange{Start: 0, End: int64(len(body))/2 - 1})
	if handler.calls.Load() != 0 {
		t.Fatal("below-threshold playback scheduled prewarm")
	}
	s.queueNextEpisodePrewarm(current, currentKey, currentFile, nil)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("next-episode prewarm did not reach origin")
	}
	s.queueNextEpisodePrewarm(current, currentKey, currentFile, nil)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown leaked predictive goroutine: %v", err)
	}
	if got := handler.calls.Load(); got != 1 {
		t.Fatalf("dedup resolve calls = %d, want 1", got)
	}
}
