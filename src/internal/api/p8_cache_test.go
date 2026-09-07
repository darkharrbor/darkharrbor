package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

type panicCorrectedTotalProvider struct{}

func (panicCorrectedTotalProvider) Name() string { return "test" }
func (panicCorrectedTotalProvider) Probe(context.Context, []byte, int64) (string, error) {
	return "", nil
}
func (panicCorrectedTotalProvider) Stream(context.Context, string, []byte, int, http.ResponseWriter, string) error {
	return nil
}
func (panicCorrectedTotalProvider) CorrectedTotal(context.Context, string, []byte, int) int64 {
	panic("CorrectedTotal must not be used for RAR media")
}
func (panicCorrectedTotalProvider) StreamRARManifest(context.Context, string, int64, http.ResponseWriter, string, string, string) error {
	return nil
}
func (panicCorrectedTotalProvider) StreamZIPEntry(_ context.Context, _ string, _ int, _ http.ResponseWriter, _, _, _ string) error {
	return nil
}
func (panicCorrectedTotalProvider) GrabTimeHealthCheck(context.Context, []byte, int) (bool, int, error) {
	return false, 0, nil
}

func newP8APIStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "p8.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return store.New(db)
}

func createP8Item(t *testing.T, st *store.Store, id string) *store.Item {
	t.Helper()
	now := time.Now().UTC()
	item := &store.Item{
		ID:            id,
		PublicID:      id,
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      "tv",
		State:         store.StateReady,
		SubmissionKey: id,
		DisplayName:   id,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return item
}

func TestPersistLastPlayedAtThrottlesConcurrentWrites(t *testing.T) {
	st := newP8APIStore(t)
	item := createP8Item(t, st, "concurrent-play")
	if _, err := st.DB().Exec(`CREATE TABLE last_played_write_count (n INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	if _, err := st.DB().Exec(`
		CREATE TRIGGER count_last_played_writes
		AFTER UPDATE OF last_played_at ON items
		BEGIN INSERT INTO last_played_write_count(n) VALUES (1); END`); err != nil {
		t.Fatalf("create counter trigger: %v", err)
	}

	s := &Server{
		store:            st,
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		lastPlayedWrites: make(map[string]time.Time),
	}
	const requests = 32
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- s.persistLastPlayedAt(context.Background(), item)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("persistLastPlayedAt: %v", err)
		}
	}
	var writes int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM last_played_write_count`).Scan(&writes); err != nil {
		t.Fatalf("count writes: %v", err)
	}
	if writes != 1 {
		t.Fatalf("last_played_at writes = %d, want 1", writes)
	}
	// A fresh Server instance seeds its empty in-memory throttle from the
	// persisted timestamp, so a restart cannot cause an immediate extra write.
	persisted, err := st.GetItemByID(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	restarted := &Server{
		store:            st,
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		lastPlayedWrites: make(map[string]time.Time),
	}
	if err := restarted.persistLastPlayedAt(context.Background(), persisted); err != nil {
		t.Fatalf("post-restart persistLastPlayedAt: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM last_played_write_count`).Scan(&writes); err != nil {
		t.Fatalf("count post-restart writes: %v", err)
	}
	if writes != 1 {
		t.Fatalf("post-restart last_played_at writes = %d, want 1", writes)
	}
}

func TestNZBEffectiveSizeKeepsRARManifestTotal(t *testing.T) {
	raw := "nzb"
	manifest := "[]"
	item := &store.Item{ID: "rar", TotalSize: 30_050_037_848, SourceURI: &raw, RarManifest: &manifest}
	s := &Server{}
	if got := s.nzbEffectiveSize(context.Background(), item, panicCorrectedTotalProvider{}, 0); got != item.TotalSize {
		t.Fatalf("RAR effective size = %d, want manifest total %d", got, item.TotalSize)
	}
}

func TestWebDAVEffectiveSizeWriterNormalizesProviderHeaders(t *testing.T) {
	const exact = int64(2_110_636_542)
	rec := httptest.NewRecorder()
	w := &webdavEffectiveSizeWriter{ResponseWriter: rec, total: exact}
	w.Header().Set("Content-Range", "bytes 0-65535/2177891311")
	w.Header().Set("Content-Length", "65536")
	w.WriteHeader(http.StatusPartialContent)
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-65535/2110636542" {
		t.Fatalf("Content-Range = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "65536" {
		t.Fatalf("Content-Length = %q", got)
	}

	rec = httptest.NewRecorder()
	w = &webdavEffectiveSizeWriter{ResponseWriter: rec, total: exact}
	w.Header().Set("Content-Length", "2177891311")
	w.WriteHeader(http.StatusOK)
	if got := rec.Header().Get("Content-Length"); got != "2110636542" {
		t.Fatalf("200 Content-Length = %q", got)
	}
}

func TestPersistLastPlayedAtRollsBackFailedReservation(t *testing.T) {
	st := newP8APIStore(t)
	item := createP8Item(t, st, "retry-play")
	s := &Server{
		store:            st,
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		lastPlayedWrites: make(map[string]time.Time),
	}
	if _, err := st.DB().Exec(`
		CREATE TRIGGER fail_last_played_write
		BEFORE UPDATE OF last_played_at ON items
		WHEN NEW.id = 'retry-play'
		BEGIN SELECT RAISE(FAIL, 'write blocked'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if err := s.persistLastPlayedAt(context.Background(), item); err == nil {
		t.Fatal("failed last_played_at write returned nil")
	}
	if _, err := st.DB().Exec(`DROP TRIGGER fail_last_played_write`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if err := s.persistLastPlayedAt(context.Background(), item); err != nil {
		t.Fatalf("retry persistLastPlayedAt: %v", err)
	}
	got, err := st.GetItemByID(context.Background(), item.ID)
	if err != nil || got.LastPlayedAt == nil {
		t.Fatalf("last_played_at after retry: item=%#v err=%v", got, err)
	}
}

func TestEphemeralCachesCapByInsertionOrder(t *testing.T) {
	s := &Server{
		magnetCache:   make(map[string]magnetEntry),
		nzbProxyCache: make(map[string]string),
	}
	ctx := context.Background()
	for i := 0; i <= ephemeralCacheMaxEntries; i++ {
		key := fmt.Sprintf("key-%05d", i)
		s.magnetCacheSet(ctx, key, "magnet:"+key, key)
		s.nzbProxySet(key, "https://example.invalid/"+key)
	}
	if len(s.magnetCache) != ephemeralCacheMaxEntries || len(s.nzbProxyCache) != ephemeralCacheMaxEntries {
		t.Fatalf("cache sizes = magnet:%d nzb:%d, want %d", len(s.magnetCache), len(s.nzbProxyCache), ephemeralCacheMaxEntries)
	}
	if _, ok := s.magnetCacheGet(ctx, "key-00000"); ok {
		t.Fatal("oldest magnet entry was not evicted")
	}
	if _, ok := s.nzbProxyGet("key-00000"); ok {
		t.Fatal("oldest NZB entry was not evicted")
	}
	if _, ok := s.magnetCacheGet(ctx, "key-10000"); !ok {
		t.Fatal("newest magnet entry was evicted")
	}
	magnetOrderLen, nzbOrderLen := len(s.magnetCacheOrder), len(s.nzbProxyCacheOrder)
	s.magnetCacheSet(ctx, "key-10000", "magnet:updated", "updated")
	s.nzbProxySet("key-10000", "https://example.invalid/updated")
	if len(s.magnetCacheOrder) != magnetOrderLen || len(s.nzbProxyCacheOrder) != nzbOrderLen {
		t.Fatal("overwriting an entry grew an insertion-order queue")
	}
}

func TestSweepCachesExpiresResolveAndThrottleEntries(t *testing.T) {
	s := &Server{
		magnetCache:      make(map[string]magnetEntry),
		nzbProxyCache:    make(map[string]string),
		lastPlayedWrites: make(map[string]time.Time),
	}
	now := time.Now()
	s.resolveCache.Store("old", &resolveCacheEntry{resolvedAt: now.Add(-resolveTTL - time.Second)})
	s.resolveCache.Store("fresh", &resolveCacheEntry{resolvedAt: now})
	s.lastPlayedWrites["old"] = now.Add(-3 * lastPlayedWriteInterval)
	s.lastPlayedWrites["fresh"] = now

	if removed := s.SweepCaches(); removed != 2 {
		t.Fatalf("SweepCaches removed %d entries, want 2", removed)
	}
	if _, ok := s.resolveCache.Load("old"); ok {
		t.Fatal("expired resolve entry survived sweep")
	}
	if _, ok := s.resolveCache.Load("fresh"); !ok {
		t.Fatal("fresh resolve entry was swept")
	}
	if _, ok := s.lastPlayedWrites["old"]; ok {
		t.Fatal("old last-play throttle entry survived sweep")
	}
	if _, ok := s.lastPlayedWrites["fresh"]; !ok {
		t.Fatal("fresh last-play throttle entry was swept")
	}
}

func TestEphemeralCachesConcurrentSweep(t *testing.T) {
	s := &Server{
		magnetCache:   make(map[string]magnetEntry),
		nzbProxyCache: make(map[string]string),
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < 500; i++ {
				key := fmt.Sprintf("%d-%d", worker, i)
				s.magnetCacheSet(ctx, key, "magnet:"+key, key)
				_, _ = s.magnetCacheGet(ctx, key)
				s.nzbProxySet(key, key)
				_, _ = s.nzbProxyGet(key)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			s.SweepCaches()
		}
	}()
	wg.Wait()
	if len(s.magnetCache) > ephemeralCacheMaxEntries || len(s.nzbProxyCache) > ephemeralCacheMaxEntries {
		t.Fatal("concurrent cache setters exceeded cap")
	}
}
