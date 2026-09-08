package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

func unreachableTestServer(t *testing.T, ttl time.Duration) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Cache.UnreachableOriginTTL = ttl
	return &Server{cfg: cfg, log: slog.New(slog.NewTextHandler(newDiscard(), nil))}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newDiscard() discardWriter { return discardWriter{} }

func TestUnreachableOriginCache(t *testing.T) {
	const url = "https://cdn.example/media?token=secret"

	t.Run("records and reports within ttl", func(t *testing.T) {
		s := unreachableTestServer(t, time.Minute)
		if s.originRecentlyUnreachable(url) {
			t.Fatal("nothing recorded yet")
		}
		s.noteOriginUnreachable(url)
		if !s.originRecentlyUnreachable(url) {
			t.Fatal("a just-recorded origin must be reported unreachable")
		}
	})

	t.Run("a different URL is unaffected", func(t *testing.T) {
		s := unreachableTestServer(t, time.Minute)
		s.noteOriginUnreachable(url)
		// This is what makes the cache safe against repair: a repaired or
		// re-resolved item arrives on a different URL and is never suppressed.
		if s.originRecentlyUnreachable("https://cdn2.example/media?token=fresh") {
			t.Fatal("a re-resolved URL must not inherit the failed URL's entry")
		}
	})

	t.Run("expires after ttl", func(t *testing.T) {
		s := unreachableTestServer(t, 20*time.Millisecond)
		s.noteOriginUnreachable(url)
		time.Sleep(40 * time.Millisecond)
		if s.originRecentlyUnreachable(url) {
			t.Fatal("entry must lapse once the TTL passes")
		}
	})

	t.Run("clear forgets a recovered origin", func(t *testing.T) {
		s := unreachableTestServer(t, time.Minute)
		s.noteOriginUnreachable(url)
		s.clearOriginUnreachable(url)
		if s.originRecentlyUnreachable(url) {
			t.Fatal("a cleared origin must be retried")
		}
	})

	t.Run("ttl of zero disables entirely", func(t *testing.T) {
		s := unreachableTestServer(t, 0)
		s.noteOriginUnreachable(url)
		if s.originRecentlyUnreachable(url) {
			t.Fatal("TTL 0 must disable the cache, not enable a permanent one")
		}
	})

	t.Run("key never contains the URL or its token", func(t *testing.T) {
		key := unreachableOriginKey(url)
		if strings.Contains(key, "secret") || strings.Contains(key, "cdn.example") ||
			strings.Contains(key, "http") {
			t.Fatalf("cache key leaked URL material: %s", key)
		}
		if key == unreachableOriginKey("https://cdn.example/media?token=other") {
			t.Fatal("distinct URLs must produce distinct keys")
		}
	})

	t.Run("SweepCaches drops expired entries", func(t *testing.T) {
		s := unreachableTestServer(t, 20*time.Millisecond)
		s.magnetCache = make(map[string]magnetEntry)
		s.nzbProxyCache = make(map[string]string)
		s.lastPlayedWrites = make(map[string]time.Time)
		s.noteOriginUnreachable(url)
		time.Sleep(40 * time.Millisecond)
		if removed := s.SweepCaches(); removed != 1 {
			t.Fatalf("SweepCaches removed %d, want 1", removed)
		}
		s.unreachableMu.Lock()
		n := len(s.unreachableOrigins)
		s.unreachableMu.Unlock()
		if n != 0 {
			t.Fatalf("expired entry still present: %d", n)
		}
	})
}

// End to end through streamViaCache: the first attempt on an unreachable
// origin pays the connect cost, the second is answered from the cache without
// touching the network at all.
func TestUnreachableOriginShortCircuitsSecondAttempt(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 1}, log)
	t.Cleanup(cache.Close)
	cfg := &config.Config{}
	cfg.Cache.UnreachableOriginTTL = time.Minute
	s := &Server{cfg: cfg, log: log, streamCache: cache}
	req := httptest.NewRequest(http.MethodGet, "/stream/item/0", nil)
	const dead = "http://127.0.0.1:1/media?token=nope"

	rec1 := httptest.NewRecorder()
	if !s.streamViaCache(rec1, req, nil, "item", "0", dead, nil, nil, 1, "", nil, nil, nil, nil) {
		t.Fatal("first attempt should answer 503 itself")
	}
	if rec1.Code != http.StatusServiceUnavailable {
		t.Fatalf("first attempt status = %d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	start := time.Now()
	if !s.streamViaCache(rec2, req, nil, "item", "0", dead, nil, nil, 1, "", nil, nil, nil, nil) {
		t.Fatal("second attempt should answer 503 from the cache")
	}
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("second attempt status = %d", rec2.Code)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("second attempt took %s -- it should not have dialled at all", elapsed)
	}
	if !strings.Contains(logs.String(), "failing fast") {
		t.Fatalf("expected the fast-fail path to be logged: %s", logs.String())
	}
}
