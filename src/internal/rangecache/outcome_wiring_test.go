package rangecache

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

// TestCacheHasSharedLimiter proves SF-01 wiring: every Cache built via New
// gets a non-nil shared outcome.Limiter, so readaheadGoroutine's
// c.limiter.Warn/FlushItem calls never hit a nil-pointer dereference in
// production wiring (only explicit zero-value Cache{} literals, which this
// package's own code never constructs — see rangecache.go's sole
// construction site, New — would lack one).
func TestCacheHasSharedLimiter(t *testing.T) {
	c := New(Config{DiskCachePath: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer c.Close()
	if c.limiter == nil {
		t.Fatal("New() did not initialize the shared outcome.Limiter")
	}
}

// TestReadaheadFailureLoggingIsRateLimited exercises the exact integration
// point readaheadGoroutine uses (c.limiter.Warn keyed by src.Key(), class
// ClassTransientHere, key "prefetch_backoff") without needing to simulate
// the goroutine's real network timing: repeated identical failures for the
// same source fold into one WARN plus a later summary, matching SF-01's
// rate-limit/periodic-summary requirement.
func TestReadaheadFailureLoggingIsRateLimited(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c := New(Config{DiskCachePath: t.TempDir()}, log)
	defer c.Close()

	srcKey := "nntp|test-item|0|10"
	for i := 0; i < 5; i++ {
		c.limiter.Warn(log, srcKey, outcome.ClassTransientHere, "prefetch_backoff",
			"rangecache: prefetch repeatedly failing, backing off",
			"chunk", 3, "consecutive_fails", i+1, "backoff", "50ms",
			"class", string(outcome.ClassTransientHere), "err", "boom")
	}
	if got := strings.Count(buf.String(), "prefetch repeatedly failing"); got != 1 {
		t.Fatalf("expected exactly 1 logged occurrence within the dedup window, got %d: %q", got, buf.String())
	}

	c.limiter.FlushItem(log, srcKey)
	if !strings.Contains(buf.String(), "suppressed_count=4") {
		t.Fatalf("expected FlushItem to emit the pending summary (suppressed_count=4), got: %q", buf.String())
	}
}
