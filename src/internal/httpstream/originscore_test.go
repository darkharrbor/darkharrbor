package httpstream

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestOriginScorerRanksSignalsAndDecaysToNeutral(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	scorer := NewOriginScorer(func() time.Time { return now })

	scorer.Observe("good", OriginObservation{
		FirstByte:    50 * time.Millisecond,
		Duration:     100 * time.Millisecond,
		Bytes:        1 << 20,
		RangeSuccess: true,
	})
	scorer.Observe("partial", OriginObservation{
		Duration: 2 * time.Second,
		Bytes:    128,
		Err:      NewError(ClassBackendUnavailable, "source body read failed"),
	})
	scorer.Observe("expired", OriginObservation{Expired: true})
	scorer.Observe("throttled", OriginObservation{Err: NewRateLimitError("30")})
	scorer.Observe("mutated", OriginObservation{Err: mutated("source entity tag changed")})

	ranked := scorer.Rank([]string{"throttled", "unknown", "good", "mutated", "expired", "partial"})
	if ranked[0] != "good" || ranked[len(ranked)-1] != "throttled" {
		t.Fatalf("unexpected ranking: %v", ranked)
	}
	if scorer.Score("unknown") != 0 {
		t.Fatal("unknown origin was not neutral")
	}
	before := scorer.Score("good")
	now = now.Add(100 * time.Minute)
	after := scorer.Score("good")
	if !(after > 0 && after < before/500) {
		t.Fatalf("score did not decay toward neutral: before=%f after=%f", before, after)
	}
}

func TestOriginScorerStableTiesCancellationTimeoutAndIdentityRejection(t *testing.T) {
	scorer := NewOriginScorer(nil)
	input := []string{"third", "first", "second"}
	if got := scorer.Rank(input); fmt.Sprint(got) != fmt.Sprint(input) {
		t.Fatalf("neutral tie changed caller order: %v", got)
	}
	scorer.Observe("first", OriginObservation{Err: context.Canceled})
	scorer.Observe("second", OriginObservation{Err: context.DeadlineExceeded})
	scorer.Observe("https://origin.invalid/file?tok=secret", OriginObservation{})
	if scorer.Len() != 1 || scorer.Score("first") != 0 || scorer.Score("second") >= 0 {
		t.Fatalf("cancellation/timeout classification wrong: len=%d first=%f second=%f",
			scorer.Len(), scorer.Score("first"), scorer.Score("second"))
	}
}

func TestOriginScorerBoundedConcurrentState(t *testing.T) {
	scorer := NewOriginScorer(nil)
	var wg sync.WaitGroup
	for i := 0; i < maxScoredOrigins*4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("origin-%04d", i)
			scorer.Observe(id, OriginObservation{RangeSuccess: true})
			_ = scorer.Score(id)
		}(i)
	}
	wg.Wait()
	if scorer.Len() != maxScoredOrigins {
		t.Fatalf("bounded state len=%d want=%d", scorer.Len(), maxScoredOrigins)
	}
}

// TestOriginScorerTokenLifetimeEWMAAndBounds covers HR3.2's lifetime
// tracking additions to the existing shared OriginScorer state.
func TestOriginScorerTokenLifetimeEWMAAndBounds(t *testing.T) {
	scorer := NewOriginScorer(nil)
	if got := scorer.EstimatedLifetime("unknown"); got != 0 {
		t.Fatalf("unknown origin lifetime = %v, want 0", got)
	}
	scorer.ObserveTokenLifetime("origin-a", 100*time.Second)
	if got := scorer.EstimatedLifetime("origin-a"); got != 100*time.Second {
		t.Fatalf("first observation = %v, want 100s", got)
	}
	scorer.ObserveTokenLifetime("origin-a", 200*time.Second)
	got := scorer.EstimatedLifetime("origin-a")
	if got <= 100*time.Second || got >= 200*time.Second {
		t.Fatalf("EWMA did not smooth toward the new sample: %v", got)
	}
	// Invalid IDs and non-positive lifetimes are ignored, matching Observe's
	// own validation, never panicking or corrupting existing state.
	scorer.ObserveTokenLifetime("https://origin.invalid/?tok=secret", 5*time.Second)
	scorer.ObserveTokenLifetime("origin-a", 0)
	scorer.ObserveTokenLifetime("origin-a", -5*time.Second)
	if scorer.EstimatedLifetime("origin-a") != got {
		t.Fatalf("invalid observations mutated existing state: %v", scorer.EstimatedLifetime("origin-a"))
	}
	if scorer.Len() != 1 {
		t.Fatalf("lifetime tracking used a second store: Len()=%d", scorer.Len())
	}
}

func TestByteSourcePassivelyScoresWithoutExtraRequests(t *testing.T) {
	data := fixtureBytes(4096)
	origin, client := startOrigin(t, &bsOrigin{data: data, etag: `"v1"`})
	resolver := &staticResolver{url: originURL("/media.mp4")}
	now := time.Now
	scorer := NewOriginScorer(now)
	source := mustSource(t, ByteSourceConfig{
		Key:           "http|passive-score",
		Size:          int64(len(data)),
		RangeVerified: true,
		Client:        client,
		Resolve:       resolver.Resolve,
		Now:           now,
		OriginScores:  scorer,
	})

	buf := make([]byte, 256)
	if n, err := source.ReadAt(context.Background(), buf, 32); err != nil || n != len(buf) {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if origin.count() != 1 {
		t.Fatalf("scoring changed origin request count: %d", origin.count())
	}
	id := opaqueOriginID(originURL("/media.mp4"))
	if score := scorer.Score(id); score <= 0 {
		t.Fatalf("successful range was not scored positively: %f", score)
	}
	for i := 0; i < 100; i++ {
		_ = scorer.Score(id)
		_ = scorer.Rank([]string{id})
	}
	if origin.count() != 1 {
		t.Fatalf("score reads generated probe traffic: %d", origin.count())
	}
}
