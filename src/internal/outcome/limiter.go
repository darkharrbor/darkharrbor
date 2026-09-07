package outcome

import (
	"log/slog"
	"strings"
	"sync"
	"time"
)

// DefaultWindow is the dedup/summary window a Limiter uses unless overridden.
const DefaultWindow = 30 * time.Second

// limiterEntry tracks one (itemID, class, key) dedup window.
type limiterEntry struct {
	windowStart time.Time
	suppressed  int
}

// Limiter is the single shared per-(item, class, key) WARN rate limiter and
// summary-counter mechanism required by SF-01. Lane code must use this
// instead of adding its own ad hoc per-attempt dedup, so every lane's
// repeated-failure noise reduces the same way and shares one soak-observable
// vocabulary.
//
// "Identical" is keyed by (itemID, class, key), where key is a short, static,
// secret-free operation label supplied by the caller (e.g. "stream_segment_
// fetch") — never raw error text, a URL, or a message ID. Only the key needs
// to stay log-safe by construction; msg/args passed to Warn must already be
// sanitized by the caller exactly as they were before this package existed —
// Limiter never inspects, scrubs, or persists them beyond the current log
// call.
//
// Time-dependent by design (SF-01/standing convention): the clock is
// injectable via SetClock so window-expiry and summary-flush behavior is
// deterministically testable.
type Limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	window  time.Duration
	entries map[string]*limiterEntry
}

// NewLimiter builds a Limiter using DefaultWindow and the real clock.
func NewLimiter() *Limiter {
	return &Limiter{
		now:     time.Now,
		window:  DefaultWindow,
		entries: make(map[string]*limiterEntry),
	}
}

// SetClock overrides the clock used by the limiter. For testing only.
func (l *Limiter) SetClock(fn func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = fn
}

// SetWindow overrides the dedup/summary window. For testing only.
func (l *Limiter) SetWindow(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.window = d
}

const keySep = "\x00"

func dedupKey(itemID string, class Class, key string) string {
	return itemID + keySep + string(class) + keySep + key
}

// Warn logs one WARN occurrence for (itemID, class, key), or, if an identical
// occurrence already logged within the current window, silently counts it
// instead. The first occurrence in a fresh window always logs immediately
// with the full msg/args (never delayed, so the first real signal of a new
// failure is never held back). Later occurrences in the same window are
// folded into one summary WARN line emitted once the window elapses (seen on
// the next Warn call past the window boundary) or via an explicit Flush/
// FlushItem call.
//
// log may be nil (no-op, matching existing lane conventions such as
// NNTPProvider.debug). class should be the caller's best current
// classification (see Classify) — closure for SF-01 requires this to never
// be ClassUnknown during a live soak; passing ClassUnknown is allowed here
// (the limiter itself does not reject it) precisely so a soak observer can
// grep logs for it as the taxonomy-gap signal it is meant to be.
func (l *Limiter) Warn(log *slog.Logger, itemID string, class Class, key, msg string, args ...any) {
	if log == nil {
		return
	}
	shouldLog, pending := l.record(itemID, class, key)
	if shouldLog {
		log.Warn(msg, args...)
	}
	if pending != nil {
		l.emitSummary(log, itemID, class, key, pending)
	}
}

// record advances the limiter state for one occurrence and returns whether
// this occurrence itself should log immediately, plus, if a prior window
// just elapsed with suppressed occurrences pending, that window's snapshot
// to summarize.
func (l *Limiter) record(itemID string, class Class, key string) (shouldLog bool, pendingSummary *limiterEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	k := dedupKey(itemID, class, key)
	e, ok := l.entries[k]
	if !ok || now.Sub(e.windowStart) >= l.window {
		var flushed *limiterEntry
		if ok && e.suppressed > 0 {
			flushed = &limiterEntry{windowStart: e.windowStart, suppressed: e.suppressed}
		}
		l.entries[k] = &limiterEntry{windowStart: now}
		return true, flushed
	}
	e.suppressed++
	return false, nil
}

func (l *Limiter) emitSummary(log *slog.Logger, itemID string, class Class, key string, e *limiterEntry) {
	if log == nil || e == nil || e.suppressed <= 0 {
		return
	}
	l.mu.Lock()
	window := l.window
	l.mu.Unlock()
	log.Warn("outcome: repeated warning suppressed",
		"item_id", itemID,
		"class", string(class),
		"key", key,
		"suppressed_count", e.suppressed,
		"window_seconds", int(window.Seconds()),
	)
}

// Flush emits any pending suppressed-count summary for (itemID, class, key)
// immediately, regardless of whether the window has elapsed, and clears that
// entry.
func (l *Limiter) Flush(log *slog.Logger, itemID string, class Class, key string) {
	l.mu.Lock()
	k := dedupKey(itemID, class, key)
	e, ok := l.entries[k]
	if ok {
		delete(l.entries, k)
	}
	l.mu.Unlock()
	if ok && e.suppressed > 0 {
		l.emitSummary(log, itemID, class, key, e)
	}
}

// FlushItem flushes every pending summary for itemID, across every class and
// key, and clears those entries. Call it once at the natural end of a
// bounded per-item operation (e.g. end of NNTPProvider.Stream) so a final
// suppressed burst within an unfinished window is never silently dropped.
func (l *Limiter) FlushItem(log *slog.Logger, itemID string) {
	type pendingEntry struct {
		class Class
		key   string
		e     *limiterEntry
	}
	var pending []pendingEntry

	l.mu.Lock()
	prefix := itemID + keySep
	for k, e := range l.entries {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if e.suppressed > 0 {
			parts := strings.SplitN(k, keySep, 3)
			if len(parts) == 3 {
				pending = append(pending, pendingEntry{class: Class(parts[1]), key: parts[2], e: e})
			}
		}
		delete(l.entries, k)
	}
	l.mu.Unlock()

	for _, p := range pending {
		l.emitSummary(log, itemID, p.class, p.key, p.e)
	}
}
