package httpstream

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestOriginScorerSnapshotIsBoundedAndSecretSafe(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	scorer := NewOriginScorer(func() time.Time { return now })
	scorer.Observe("origin:opaque", OriginObservation{
		FirstByte: 100 * time.Millisecond, RangeSuccess: true,
		Expired: true, RetryStage: "reresolve",
	})
	scorer.ObserveTokenLifetime("origin:opaque", 30*time.Minute)

	got := scorer.Snapshot()
	if len(got) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(got))
	}
	if got[0].Source == "origin:opaque" || strings.Contains(got[0].Source, ":") {
		t.Fatalf("source was not reduced to a digest: %q", got[0].Source)
	}
	if got[0].FirstByteMillis != 100 || got[0].TokenLifetimeSecs != 1800 {
		t.Fatalf("latency/lifetime = %d/%d", got[0].FirstByteMillis, got[0].TokenLifetimeSecs)
	}
	if got[0].ExpiryObservations != 1 || got[0].RetryStage != "reresolve" {
		t.Fatalf("expiry/stage = %d/%q", got[0].ExpiryObservations, got[0].RetryStage)
	}

	scorer.Observe("origin:opaque", OriginObservation{Err: context.Canceled, RetryStage: "alternate"})
	if after := scorer.Snapshot()[0]; after.Observations != got[0].Observations || after.RetryStage != got[0].RetryStage {
		t.Fatalf("cancellation changed telemetry: %#v", after)
	}
}

func TestOriginScorerUnknownRetryStageFailsClosed(t *testing.T) {
	scorer := NewOriginScorer(time.Now)
	scorer.Observe("safe", OriginObservation{RetryStage: "unsafe?query"})
	if got := scorer.Snapshot()[0].RetryStage; got != "unknown" {
		t.Fatalf("retry stage = %q, want unknown", got)
	}
}
