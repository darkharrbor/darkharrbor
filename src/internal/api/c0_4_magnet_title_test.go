package api

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

func newC04TestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "c0_4.db")
	db, err := store.Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return store.New(db)
}

// TestMagnetCache_DurableFallback_SurvivesMemoryWipe is the direct C0.4
// regression test: it reproduces the exact live failure (2026-07-18) where a
// DarkHarrbor restart between a Sonarr search (which records a release's
// title via magnetCacheSet) and a later grab (which only carries the hash)
// wiped the in-memory magnetCache, causing the item to be created with its
// DisplayName equal to the bare info hash. A Server whose in-memory map is
// reset to empty (simulating the post-restart state) must still recover the
// title from the durable store on a cache miss.
func TestMagnetCache_DurableFallback_SurvivesMemoryWipe(t *testing.T) {
	st := newC04TestStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	s := &Server{log: log, store: st, magnetCache: make(map[string]magnetEntry)}

	const hash = "adca1824433af9d3076994b5a9fe1d7e65f3c22b"
	const magnet = "magnet:?xt=urn:btih:adca1824433af9d3076994b5a9fe1d7e65f3c22b"
	const title = "Frasier s11 720p"

	// Step 1: a torznab render (Sonarr search) records the title, exactly as
	// writeTorznabFeed's magnetCacheSet call does today.
	s.magnetCacheSet(ctx, hash, magnet, title)
	if got := s.magnetCacheTitle(ctx, hash); got != title {
		t.Fatalf("title immediately after set = %q, want %q", got, title)
	}

	// Step 2: simulate a DarkHarrbor restart by discarding the in-memory
	// cache (a fresh process starts with an empty map) while keeping the
	// same underlying store — this is exactly what happens across a real
	// container restart, since *store.Store wraps a persistent SQLite file.
	s.magnetCache = make(map[string]magnetEntry)
	s.magnetCacheOrder = nil

	// Step 3: the later grab (qbit add / /download redirect) must still
	// recover the real title, not fall back to the bare hash.
	if got := s.magnetCacheTitle(ctx, hash); got != title {
		t.Fatalf("title after simulated restart = %q, want %q (durable fallback failed)", got, title)
	}
	gotMagnet, ok := s.magnetCacheGet(ctx, hash)
	if !ok || gotMagnet != magnet {
		t.Fatalf("magnetCacheGet after simulated restart = (%q, %v), want (%q, true)", gotMagnet, ok, magnet)
	}

	// The durable read-through must also have repopulated the in-memory
	// cache so subsequent lookups do not keep hitting the store.
	s.magnetCacheMu.RLock()
	_, memHit := s.magnetCache[hash]
	s.magnetCacheMu.RUnlock()
	if !memHit {
		t.Fatal("durable hit did not repopulate the in-memory cache")
	}
}

// TestMagnetCache_NoStoreWired_FailsClosedWithoutPanic covers the pre-C0.4
// call shape (a Server constructed without a store, as several existing unit
// tests in this package still do) to guarantee the new durable-fallback code
// path degrades to the old in-memory-only behavior rather than panicking on
// a nil store.
func TestMagnetCache_NoStoreWired_FailsClosedWithoutPanic(t *testing.T) {
	s := &Server{magnetCache: make(map[string]magnetEntry)}
	ctx := context.Background()
	if got := s.magnetCacheTitle(ctx, "never-set"); got != "" {
		t.Fatalf("magnetCacheTitle with no store = %q, want empty", got)
	}
	if _, ok := s.magnetCacheGet(ctx, "never-set"); ok {
		t.Fatal("magnetCacheGet with no store returned ok=true for an unset hash")
	}
	// Must not panic even though s.store and s.log are both nil.
	s.magnetCacheSet(ctx, "", "magnet:x", "title")
}
