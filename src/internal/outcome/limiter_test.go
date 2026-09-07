package outcome

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestLogger(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func countOccurrences(s, substr string) int {
	return strings.Count(s, substr)
}

func TestLimiterFirstOccurrenceAlwaysLogs(t *testing.T) {
	var buf strings.Builder
	log := newTestLogger(&buf)
	l := NewLimiter()

	l.Warn(log, "item-1", ClassTransientHere, "seg_fetch", "nntp: stream: fetch failed", "seg", 1)

	if !strings.Contains(buf.String(), "fetch failed") {
		t.Fatalf("first occurrence did not log: %q", buf.String())
	}
}

func TestLimiterSuppressesRepeatsWithinWindow(t *testing.T) {
	var buf strings.Builder
	log := newTestLogger(&buf)
	l := NewLimiter()
	now := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return now })
	l.SetWindow(30 * time.Second)

	for i := 0; i < 10; i++ {
		l.Warn(log, "item-1", ClassTransientHere, "seg_fetch", "nntp: stream: fetch failed", "seg", i)
	}

	got := countOccurrences(buf.String(), "fetch failed")
	if got != 1 {
		t.Fatalf("expected exactly 1 logged occurrence within window, got %d: %q", got, buf.String())
	}
}

func TestLimiterEmitsSummaryAfterWindowElapses(t *testing.T) {
	var buf strings.Builder
	log := newTestLogger(&buf)
	l := NewLimiter()
	now := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return now })
	l.SetWindow(30 * time.Second)

	for i := 0; i < 5; i++ {
		l.Warn(log, "item-1", ClassTransientHere, "seg_fetch", "nntp: stream: fetch failed", "seg", i)
	}
	// Advance past the window and log one more occurrence — this should
	// surface the prior window's summary (4 suppressed) before logging the
	// new window's first occurrence.
	now = now.Add(31 * time.Second)
	l.Warn(log, "item-1", ClassTransientHere, "seg_fetch", "nntp: stream: fetch failed", "seg", 99)

	out := buf.String()
	if !strings.Contains(out, "repeated warning suppressed") {
		t.Fatalf("expected a summary line, got: %q", out)
	}
	if !strings.Contains(out, "suppressed_count=4") {
		t.Fatalf("expected suppressed_count=4, got: %q", out)
	}
}

func TestLimiterDistinguishesByClassAndKey(t *testing.T) {
	var buf strings.Builder
	log := newTestLogger(&buf)
	l := NewLimiter()

	l.Warn(log, "item-1", ClassTransientHere, "seg_fetch", "fetch failed")
	l.Warn(log, "item-1", ClassPermanentHere, "seg_fetch", "fetch failed")
	l.Warn(log, "item-1", ClassTransientHere, "seg_decode", "decode failed")

	if got := countOccurrences(buf.String(), "fetch failed"); got != 2 {
		t.Fatalf("expected 2 distinct-class fetch-failed lines, got %d: %q", got, buf.String())
	}
	if got := countOccurrences(buf.String(), "decode failed"); got != 1 {
		t.Fatalf("expected 1 decode-failed line, got %d: %q", got, buf.String())
	}
}

func TestLimiterDistinguishesByItem(t *testing.T) {
	var buf strings.Builder
	log := newTestLogger(&buf)
	l := NewLimiter()

	l.Warn(log, "item-1", ClassTransientHere, "seg_fetch", "fetch failed")
	l.Warn(log, "item-2", ClassTransientHere, "seg_fetch", "fetch failed")

	if got := countOccurrences(buf.String(), "fetch failed"); got != 2 {
		t.Fatalf("expected per-item independent windows, got %d occurrences: %q", got, buf.String())
	}
}

func TestFlushEmitsPendingSummaryImmediately(t *testing.T) {
	var buf strings.Builder
	log := newTestLogger(&buf)
	l := NewLimiter()
	now := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return now })
	l.SetWindow(time.Hour) // window won't naturally elapse in this test

	for i := 0; i < 3; i++ {
		l.Warn(log, "item-1", ClassTransientHere, "seg_fetch", "fetch failed")
	}
	if strings.Contains(buf.String(), "suppressed") {
		t.Fatalf("summary should not appear before window elapses or Flush: %q", buf.String())
	}

	l.Flush(log, "item-1", ClassTransientHere, "seg_fetch")

	out := buf.String()
	if !strings.Contains(out, "suppressed_count=2") {
		t.Fatalf("expected Flush to emit suppressed_count=2, got: %q", out)
	}

	// Flushing again with nothing pending must not re-emit.
	buf.Reset()
	l.Flush(log, "item-1", ClassTransientHere, "seg_fetch")
	if buf.Len() != 0 {
		t.Fatalf("expected no re-emitted summary after entry cleared, got: %q", buf.String())
	}
}

func TestFlushItemDrainsAllClassesAndKeys(t *testing.T) {
	var buf strings.Builder
	log := newTestLogger(&buf)
	l := NewLimiter()
	l.SetWindow(time.Hour)

	for i := 0; i < 2; i++ {
		l.Warn(log, "item-1", ClassTransientHere, "seg_fetch", "fetch failed")
	}
	for i := 0; i < 3; i++ {
		l.Warn(log, "item-1", ClassPermanentHere, "seg_decode", "decode failed")
	}
	l.Warn(log, "item-2", ClassTransientHere, "seg_fetch", "fetch failed") // different item, untouched

	l.FlushItem(log, "item-1")

	out := buf.String()
	if !strings.Contains(out, "suppressed_count=1") {
		t.Fatalf("expected seg_fetch suppressed_count=1 in summary, got: %q", out)
	}
	if !strings.Contains(out, "suppressed_count=2") {
		t.Fatalf("expected seg_decode suppressed_count=2 in summary, got: %q", out)
	}

	// item-2's entry must remain untouched by item-1's FlushItem.
	buf.Reset()
	l.Flush(log, "item-2", ClassTransientHere, "seg_fetch")
	if buf.Len() != 0 {
		t.Fatalf("item-2 had no suppressed occurrences yet and must not emit: %q", buf.String())
	}
}

func TestLimiterConcurrentSafe(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := NewLimiter()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.Warn(log, "item-1", ClassTransientHere, "seg_fetch", "fetch failed", "seg", n)
		}(i)
	}
	wg.Wait()
	l.FlushItem(log, "item-1")
}

func TestWarnNilLoggerNoop(t *testing.T) {
	l := NewLimiter()
	// Must not panic.
	l.Warn(nil, "item-1", ClassTransientHere, "seg_fetch", "fetch failed")
	l.Flush(nil, "item-1", ClassTransientHere, "seg_fetch")
	l.FlushItem(nil, "item-1")
}
