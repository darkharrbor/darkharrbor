package nntp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func schedulerPool(name string) *Pool {
	return &Pool{providerName: name}
}

func schedulerNames(pools []*Pool) []string {
	names := make([]string, 0, len(pools))
	for _, pool := range pools {
		if pool != nil {
			names = append(names, pool.providerName)
		}
	}
	return names
}

func requireSchedulerNames(t *testing.T, got []*Pool, want ...string) {
	t.Helper()
	names := schedulerNames(got)
	if len(names) != len(want) {
		t.Fatalf("provider count = %d, want %d (%v)", len(names), len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("provider order = %v, want %v", names, want)
		}
	}
}

func TestNS84SchedulerDefaultPreservesPreference(t *testing.T) {
	candidates := []*Pool{schedulerPool("first"), schedulerPool("second")}
	got, policy := NewProviderHealth().orderCandidates(candidates, 0)
	requireSchedulerNames(t, got, "first", "second")
	if policy != "preference" {
		t.Fatalf("policy = %q, want preference", policy)
	}
}

func TestNS84SchedulerStripesHealthyProviders(t *testing.T) {
	candidates := []*Pool{
		schedulerPool("first"),
		schedulerPool("second"),
		schedulerPool("third"),
	}
	health := NewProviderHealthWithRouting(true, nil)
	for i, want := range []string{"first", "second", "third", "first"} {
		got, policy := health.orderCandidates(candidates, 0)
		if policy != "stripe" {
			t.Fatalf("call %d policy = %q, want stripe", i, policy)
		}
		if got[0].providerName != want {
			t.Fatalf("call %d first provider = %q, want %q", i, got[0].providerName, want)
		}
	}
}

func TestNS84SchedulerRetentionPrefersDeepestKnownHorizon(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(true, map[string]int{
		"short": 30,
		"deep":  1000,
		"mid":   500,
	})
	health.SetClock(func() time.Time { return now })
	candidates := []*Pool{
		schedulerPool("short"),
		schedulerPool("unknown"),
		schedulerPool("deep"),
		schedulerPool("mid"),
	}

	got, policy := health.orderCandidates(candidates, now.Add(-100*24*time.Hour).Unix())
	requireSchedulerNames(t, got, "deep", "mid", "short", "unknown")
	if policy != "retention" {
		t.Fatalf("policy = %q, want retention", policy)
	}
}

func TestNS84SchedulerYoungUnknownAndFutureDatesDoNotGuess(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	candidates := []*Pool{schedulerPool("short"), schedulerPool("deep")}
	tests := []struct {
		name     string
		postedAt int64
	}{
		{name: "missing", postedAt: 0},
		{name: "malformed negative", postedAt: -1},
		{name: "future", postedAt: now.Add(time.Hour).Unix()},
		{name: "within every horizon", postedAt: now.Add(-10 * 24 * time.Hour).Unix()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := NewProviderHealthWithRouting(false, map[string]int{"short": 30, "deep": 1000})
			health.SetClock(func() time.Time { return now })
			got, policy := health.orderCandidates(candidates, tt.postedAt)
			requireSchedulerNames(t, got, "short", "deep")
			if policy != "preference" {
				t.Fatalf("policy = %q, want preference", policy)
			}
		})
	}
}

func TestNS84SchedulerMovesBackedOffProviderBehindHealthyProvider(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(true, nil)
	health.SetClock(func() time.Time { return now })
	health.RecordFailure("first")
	health.RecordFailure("first")

	got, policy := health.orderCandidates(
		[]*Pool{schedulerPool("first"), schedulerPool("second")},
		0,
	)
	requireSchedulerNames(t, got, "second", "first")
	if policy != "preference" {
		t.Fatalf("policy = %q, want preference with one healthy provider", policy)
	}
}

func TestNS84SchedulerConcurrentStripeIsRaceSafeAndBalanced(t *testing.T) {
	const calls = 300
	candidates := []*Pool{
		schedulerPool("first"),
		schedulerPool("second"),
		schedulerPool("third"),
	}
	health := NewProviderHealthWithRouting(true, nil)
	counts := make(map[string]int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(calls)
	for range calls {
		go func() {
			defer wg.Done()
			got, policy := health.orderCandidates(candidates, 0)
			if policy != "stripe" || len(got) != len(candidates) {
				t.Errorf("policy/order = %q/%d, want stripe/%d", policy, len(got), len(candidates))
				return
			}
			mu.Lock()
			counts[got[0].providerName]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, name := range []string{"first", "second", "third"} {
		if counts[name] != calls/3 {
			t.Fatalf("first-choice counts = %v, want exactly %d each", counts, calls/3)
		}
	}
}

func TestNS84SuccessfulRouteEvidenceIsBoundedAndRaceSafe(t *testing.T) {
	health := NewProviderHealthWithRouting(true, nil)
	const calls = 300
	var firstCount atomic.Int32
	var wg sync.WaitGroup
	wg.Add(calls)
	for range calls {
		go func() {
			defer wg.Done()
			if health.firstSuccessfulRoute("first", "stripe") {
				firstCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := firstCount.Load(); got != 1 {
		t.Fatalf("first-route evidence count = %d, want 1", got)
	}
	if !health.firstSuccessfulRoute("second", "stripe") {
		t.Fatal("distinct provider did not receive its own bounded evidence event")
	}
	if !health.firstSuccessfulRoute("first", "retention") {
		t.Fatal("distinct policy did not receive its own bounded evidence event")
	}
	if got := len(health.observedRoute); got != 3 {
		t.Fatalf("observed route state = %d, want 3 bounded policy/provider pairs", got)
	}
}

func TestNS84FetchCancellationMakesNoProviderAttempt(t *testing.T) {
	const articleID = "cancelled@example.invalid"
	firstServer := startFakeArticleServer(t, map[string]string{articleID: "222 body follows"})
	secondServer := startFakeArticleServer(t, map[string]string{articleID: "222 body follows"})
	first := newNamedTestPool(t, firstServer, 1, "first")
	second := newNamedTestPool(t, secondServer, 1, "second")
	first.SetFailover([]*Pool{second}, NewProviderHealthWithRouting(true, nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := fetchArticleWithRetryAt(ctx, first, testLogger(), "stream", 1, articleID, 0, nil)
	if err != context.Canceled {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := firstServer.bodyCount() + secondServer.bodyCount(); got != 0 {
		t.Fatalf("BODY count after cancellation = %d, want 0", got)
	}
}

func TestNS84FetchStripesRealPoolAttempts(t *testing.T) {
	const (
		firstID  = "stripe-one@example.invalid"
		secondID = "stripe-two@example.invalid"
	)
	firstServer := startFakeArticleServer(t, map[string]string{
		firstID:  "222 body follows",
		secondID: "222 body follows",
	})
	secondServer := startFakeArticleServer(t, map[string]string{
		firstID:  "222 body follows",
		secondID: "222 body follows",
	})
	first := newNamedTestPool(t, firstServer, 1, "first")
	second := newNamedTestPool(t, secondServer, 1, "second")
	health := NewProviderHealthWithRouting(true, nil)
	first.SetFailover([]*Pool{second}, health)

	if err := fetchArticleWithRetryAt(context.Background(), first, testLogger(), "stream", 1, firstID, 0, nil); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if err := fetchArticleWithRetryAt(context.Background(), first, testLogger(), "stream", 2, secondID, 0, nil); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if got := firstServer.bodyCount(); got != 1 {
		t.Fatalf("first provider BODY count = %d, want 1", got)
	}
	if got := secondServer.bodyCount(); got != 1 {
		t.Fatalf("second provider BODY count = %d, want 1", got)
	}
}

func TestNS84FetchRoutesOldArticleToDeepRetentionProvider(t *testing.T) {
	const articleID = "retention@example.invalid"
	firstServer := startFakeArticleServer(t, map[string]string{articleID: "222 body follows"})
	deepServer := startFakeArticleServer(t, map[string]string{articleID: "222 body follows"})
	first := newNamedTestPool(t, firstServer, 1, "short")
	deep := newNamedTestPool(t, deepServer, 1, "deep")
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, map[string]int{"short": 30, "deep": 1000})
	health.SetClock(func() time.Time { return now })
	first.SetFailover([]*Pool{deep}, health)

	err := fetchArticleWithRetryAt(
		context.Background(),
		first,
		testLogger(),
		"stream",
		1,
		articleID,
		now.Add(-100*24*time.Hour).Unix(),
		nil,
	)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := firstServer.bodyCount(); got != 0 {
		t.Fatalf("short-retention provider BODY count = %d, want 0", got)
	}
	if got := deepServer.bodyCount(); got != 1 {
		t.Fatalf("deep-retention provider BODY count = %d, want 1", got)
	}
}
