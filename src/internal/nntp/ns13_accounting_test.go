package nntp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/ladder"
	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

type finalRungRecorder struct {
	mu     sync.Mutex
	events []FinalRungEvent
}

func (r *finalRungRecorder) report(_ context.Context, event FinalRungEvent) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return int64(len(r.events)), nil
}

func (r *finalRungRecorder) snapshot() []FinalRungEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]FinalRungEvent(nil), r.events...)
}

func TestNS13FinalRungAccounting_WireThenNegativeCache_CountsExactlyOnceEach(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{})
	pool := newTestPool(t, s, 1)
	pool.SetNegativeCache(newNegativeCache(time.Hour, 100))

	var logBuf bytes.Buffer
	pool.log = slog.New(slog.NewJSONHandler(&logBuf, nil))
	recorder := &finalRungRecorder{}
	pool.setFinalRungAccounting("newshosting", recorder.report)
	ctx := withAccountingItemID(context.Background(), "item-ns13")

	first := fetchArticleWithRetry(ctx, pool, pool.log, "cache", 7, "missing-secret@example", nil)
	if !errors.Is(first, ErrArticleMissing) || !errors.Is(first, ladder.ErrExhausted) {
		t.Fatalf("first error chain = %v", first)
	}
	second := fetchArticleWithRetry(ctx, pool, pool.log, "cache", 7, "missing-secret@example", nil)
	if !errors.Is(second, ErrArticleMissing) || !errors.Is(second, ladder.ErrExhausted) {
		t.Fatalf("second error chain = %v", second)
	}

	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].KnownMissing {
		t.Fatal("wire verdict must not be marked known_missing")
	}
	if !events[1].KnownMissing {
		t.Fatal("negative-cache verdict must be marked known_missing")
	}
	for i, event := range events {
		if event.ItemID != "item-ns13" || event.Provider != "newshosting" || event.Operation != "cache" || event.SegmentNumber != 7 {
			t.Fatalf("event[%d] = %#v", i, event)
		}
		if event.OutcomeClass != outcome.ClassPermanentHere {
			t.Fatalf("event[%d] class = %q, want permanent_here", i, event.OutcomeClass)
		}
	}
	if got := s.bodyCount(); got != 1 {
		t.Fatalf("wire + negative-cache repeat must cost one BODY command, got %d", got)
	}

	logs := logBuf.String()
	if strings.Contains(logs, "missing-secret@example") {
		t.Fatal("structured accounting log leaked message id")
	}
	for _, want := range []string{"nntp_final_rung_exhausted", "item-ns13", "newshosting", "event_count"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("structured accounting log missing %q: %s", want, logs)
		}
	}
}

func TestNS13FinalRungAccounting_TransientExhaustionClassifiesAndCounts(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"boom@example": "400 service temporarily unavailable"})
	pool := newTestPool(t, s, 1)
	recorder := &finalRungRecorder{}
	pool.setFinalRungAccounting("newshosting", recorder.report)

	err := fetchArticleWithRetry(
		withAccountingItemID(context.Background(), "item-transient"),
		pool,
		testLogger(),
		"stream",
		3,
		"boom@example",
		nil,
	)
	if !errors.Is(err, ladder.ErrExhausted) {
		t.Fatalf("expected ladder exhaustion, got %v", err)
	}
	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].OutcomeClass != outcome.ClassTransientHere || events[0].KnownMissing {
		t.Fatalf("event = %#v", events[0])
	}
}

func TestNS13FinalRungAccounting_NoItemOrCancellationDoesNotCount(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{})
	pool := newTestPool(t, s, 1)
	recorder := &finalRungRecorder{}
	pool.setFinalRungAccounting("newshosting", recorder.report)

	_ = fetchArticleWithRetry(context.Background(), pool, testLogger(), "probe", 1, "missing-a@example", nil)

	ctx, cancel := context.WithCancel(withAccountingItemID(context.Background(), "item-cancelled"))
	cancel()
	err := fetchArticleWithRetry(ctx, pool, testLogger(), "stream", 2, "missing-b@example", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	if errors.Is(err, ladder.ErrExhausted) {
		t.Fatalf("cancellation must not be ladder exhaustion: %v", err)
	}
	if got := len(recorder.snapshot()); got != 0 {
		t.Fatalf("events = %d, want 0", got)
	}
}

func TestNS13FinalRungAccounting_ReporterFailurePreservesOriginalError(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{})
	pool := newTestPool(t, s, 1)
	pool.setFinalRungAccounting("newshosting", func(context.Context, FinalRungEvent) (int64, error) {
		return 0, errors.New("store unavailable")
	})
	err := fetchArticleWithRetry(
		withAccountingItemID(context.Background(), "item-store-down"),
		pool,
		testLogger(),
		"stream",
		1,
		"missing@example",
		nil,
	)
	if !errors.Is(err, ErrArticleMissing) || !errors.Is(err, ladder.ErrExhausted) {
		t.Fatalf("reporter failure masked original chain: %v", err)
	}
}
