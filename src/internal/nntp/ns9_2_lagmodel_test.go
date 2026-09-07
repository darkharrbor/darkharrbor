package nntp

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNS92LagModelDisabledByDefaultPreservesPreference(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, nil) // SetLagModel never called
	health.SetClock(func() time.Time { return now })
	// Prime "fast"/"slow" as if the model were enabled, to prove a
	// disabled model ignores existing samples entirely.
	health.lagYoungAgeSec = 0 // explicit: default state
	candidates := []*Pool{schedulerPool("first"), schedulerPool("second")}
	got, policy := health.orderCandidates(candidates, now.Add(-time.Hour).Unix())
	requireSchedulerNames(t, got, "first", "second")
	if policy != "preference" {
		t.Fatalf("policy = %q, want preference", policy)
	}
}

func TestNS92LagModelOrdersByObservedAverageAscending(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, nil)
	health.SetClock(func() time.Time { return now })
	health.SetLagModel(int64((24 * time.Hour).Seconds()), 2)

	// "slow" observed at ~2h age-at-first-success; "fast" at ~10m.
	simulateLagSamples(health, "slow", now, 2*time.Hour, 3)
	simulateLagSamples(health, "fast", now, 10*time.Minute, 3)

	candidates := []*Pool{schedulerPool("slow"), schedulerPool("fast")}
	got, policy := health.orderCandidates(candidates, now.Add(-5*time.Minute).Unix())
	requireSchedulerNames(t, got, "fast", "slow")
	if policy != "lag" {
		t.Fatalf("policy = %q, want lag", policy)
	}
}

func TestNS92LagModelRequiresMinSamplesBeforeTrusting(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, nil)
	health.SetClock(func() time.Time { return now })
	health.SetLagModel(int64((24 * time.Hour).Seconds()), 3)

	// Only 2 samples each -- below the configured minimum of 3, so
	// neither average is trusted and ordering must fall back to
	// preference rather than guessing from thin evidence.
	simulateLagSamples(health, "slow", now, 2*time.Hour, 2)
	simulateLagSamples(health, "fast", now, 10*time.Minute, 2)

	candidates := []*Pool{schedulerPool("slow"), schedulerPool("fast")}
	got, policy := health.orderCandidates(candidates, now.Add(-5*time.Minute).Unix())
	requireSchedulerNames(t, got, "slow", "fast")
	if policy != "preference" {
		t.Fatalf("policy = %q, want preference (insufficient samples)", policy)
	}
}

func TestNS92LagModelIgnoresPostsAtOrAboveYoungThreshold(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, nil)
	health.SetClock(func() time.Time { return now })
	health.SetLagModel(int64((24 * time.Hour).Seconds()), 2)
	simulateLagSamples(health, "slow", now, 2*time.Hour, 3)
	simulateLagSamples(health, "fast", now, 10*time.Minute, 3)

	candidates := []*Pool{schedulerPool("slow"), schedulerPool("fast")}
	// Posted 48h ago: at/above the 24h young threshold, so the lag model
	// must not apply even though both providers have trusted samples.
	got, policy := health.orderCandidates(candidates, now.Add(-48*time.Hour).Unix())
	requireSchedulerNames(t, got, "slow", "fast")
	if policy != "preference" {
		t.Fatalf("policy = %q, want preference (post not young)", policy)
	}
}

func TestNS92LagModelMissingUnknownFutureAndMalformedPostedAtAbstain(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	candidates := []*Pool{schedulerPool("slow"), schedulerPool("fast")}
	tests := []struct {
		name     string
		postedAt int64
	}{
		{"missing", 0},
		{"malformed negative", -1},
		{"future", now.Add(time.Hour).Unix()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := NewProviderHealthWithRouting(false, nil)
			health.SetClock(func() time.Time { return now })
			health.SetLagModel(int64((24 * time.Hour).Seconds()), 2)
			simulateLagSamples(health, "slow", now, 2*time.Hour, 3)
			simulateLagSamples(health, "fast", now, 10*time.Minute, 3)

			got, policy := health.orderCandidates(candidates, tt.postedAt)
			requireSchedulerNames(t, got, "slow", "fast")
			if policy != "preference" {
				t.Fatalf("policy = %q, want preference", policy)
			}
		})
	}
}

func TestNS92RetentionTakesPriorityOverLagForOldPosts(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, map[string]int{"short": 30, "deep": 1000})
	health.SetClock(func() time.Time { return now })
	health.SetLagModel(int64((24 * time.Hour).Seconds()), 2)
	// Even with trusted lag samples favoring "short", an old post (which
	// exceeds "short"'s horizon) must still route by retention, not lag
	// -- retention is checked first and the two policies are mutually
	// exclusive by construction (lag requires a young post).
	simulateLagSamples(health, "short", now, 10*time.Minute, 3)
	simulateLagSamples(health, "deep", now, 2*time.Hour, 3)

	candidates := []*Pool{schedulerPool("short"), schedulerPool("deep")}
	got, policy := health.orderCandidates(candidates, now.Add(-100*24*time.Hour).Unix())
	requireSchedulerNames(t, got, "deep", "short")
	if policy != "retention" {
		t.Fatalf("policy = %q, want retention", policy)
	}
}

func TestNS92RecordSuccessAtIgnoresNonPositiveAndFuturePostedAt(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, nil)
	health.SetClock(func() time.Time { return now })
	health.SetLagModel(int64((24 * time.Hour).Seconds()), 1)

	health.RecordSuccessAt("p", 0)
	health.RecordSuccessAt("p", -1)
	health.RecordSuccessAt("p", now.Add(time.Hour).Unix())
	if _, ok := health.lagAverage("p"); ok {
		t.Fatal("lag average trusted after only non-positive/future postedAt observations, want no trusted samples")
	}

	// A single genuine, past, in-bounds observation must be recorded.
	health.RecordSuccessAt("p", now.Add(-time.Minute).Unix())
	if _, ok := health.lagAverage("p"); !ok {
		t.Fatal("lag average not trusted after one valid observation with minSamples=1")
	}
}

func TestNS92RecordSuccessAtNilReceiverSafe(t *testing.T) {
	var h *ProviderHealth
	h.RecordSuccessAt("p", time.Now().Unix()) // must not panic
}

func TestNS92LagModelConcurrentRecordAndOrderIsRaceSafe(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, nil)
	health.SetClock(func() time.Time { return now })
	health.SetLagModel(int64((24 * time.Hour).Seconds()), 2)
	candidates := []*Pool{schedulerPool("first"), schedulerPool("second")}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			name := "first"
			if i%2 == 0 {
				name = "second"
			}
			health.RecordSuccessAt(name, now.Add(-time.Duration(i+1)*time.Minute).Unix())
		}(i)
		go func() {
			defer wg.Done()
			_, _ = health.orderCandidates(candidates, now.Add(-5*time.Minute).Unix())
		}()
	}
	wg.Wait() // race detector (go test -race) proves clean concurrent access
}

func TestNS92FetchRoutesYoungArticleToObservedFasterProvider(t *testing.T) {
	const articleID = "lag@example.invalid"
	slowServer := startFakeArticleServer(t, map[string]string{articleID: "222 body follows"})
	fastServer := startFakeArticleServer(t, map[string]string{articleID: "222 body follows"})
	slow := newNamedTestPool(t, slowServer, 1, "slow")
	fast := newNamedTestPool(t, fastServer, 1, "fast")

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, nil)
	health.SetClock(func() time.Time { return now })
	health.SetLagModel(int64((24 * time.Hour).Seconds()), 2)
	simulateLagSamples(health, "slow", now, 2*time.Hour, 3)
	simulateLagSamples(health, "fast", now, 10*time.Minute, 3)
	// "slow" is first in preference/failover order, so this proves the
	// lag model -- not preference -- picked the winner.
	slow.SetFailover([]*Pool{fast}, health)

	err := fetchArticleWithRetryAt(
		context.Background(),
		slow,
		testLogger(),
		"stream",
		1,
		articleID,
		now.Add(-5*time.Minute).Unix(),
		nil,
	)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := slowServer.bodyCount(); got != 0 {
		t.Fatalf("slow (by observed lag) provider BODY count = %d, want 0", got)
	}
	if got := fastServer.bodyCount(); got != 1 {
		t.Fatalf("fast (by observed lag) provider BODY count = %d, want 1", got)
	}
}

func TestNS92FetchSuccessFeedsLagModelForFutureOrdering(t *testing.T) {
	const (
		firstID  = "lag-learn-1@example.invalid"
		secondID = "lag-learn-2@example.invalid"
	)
	server := startFakeArticleServer(t, map[string]string{
		firstID:  "222 body follows",
		secondID: "222 body follows",
	})
	pool := newNamedTestPool(t, server, 1, "solo")
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, nil)
	health.SetClock(func() time.Time { return now })
	health.SetLagModel(int64((24 * time.Hour).Seconds()), 1)
	pool.health = health // single-pool wiring, no failover needed to observe RecordSuccessAt

	if _, ok := health.lagAverage("solo"); ok {
		t.Fatal("lag average trusted before any real fetch succeeded")
	}
	postedAt := now.Add(-10 * time.Minute).Unix()
	if err := fetchArticleWithRetryAt(context.Background(), pool, testLogger(), "stream", 1, firstID, postedAt, nil); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	meanSec, ok := health.lagAverage("solo")
	if !ok {
		t.Fatal("lag average not trusted after one genuine successful fetch with minSamples=1")
	}
	if meanSec < 590 || meanSec > 610 { // ~10 minutes, allowing clock/test slack
		t.Fatalf("observed mean age-at-first-success = %.0fs, want ~600s", meanSec)
	}
}

func TestNS92LagModelStillDefersBackedOffProviderRegardlessOfLagRank(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	health := NewProviderHealthWithRouting(false, nil)
	health.SetClock(func() time.Time { return now })
	health.SetLagModel(int64((24 * time.Hour).Seconds()), 2)
	// "fast" has the better (lower) observed lag average, but it is
	// currently backed off (simulating a real dial/auth/transport
	// disconnect); a young post must still prefer the healthy "slow"
	// provider over a disconnected faster one.
	simulateLagSamples(health, "slow", now, 2*time.Hour, 3)
	simulateLagSamples(health, "fast", now, 10*time.Minute, 3)
	health.RecordFailure("fast")
	health.RecordFailure("fast")

	candidates := []*Pool{schedulerPool("slow"), schedulerPool("fast")}
	got, policy := health.orderCandidates(candidates, now.Add(-5*time.Minute).Unix())
	requireSchedulerNames(t, got, "slow", "fast")
	if policy != "preference" {
		t.Fatalf("policy = %q, want preference (only one healthy candidate)", policy)
	}
}

// simulateLagSamples feeds n synthetic age-at-first-success observations
// for name, each corresponding to a real success postedAt ago at the
// fixed test clock now. Used to deterministically prime the lag model
// above its configured minimum sample count without needing n real
// network round trips per test.
func simulateLagSamples(h *ProviderHealth, name string, now time.Time, age time.Duration, n int) {
	for i := 0; i < n; i++ {
		h.RecordSuccessAt(name, now.Add(-age).Unix())
	}
}
