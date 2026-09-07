package nntp

import (
	"testing"
	"time"
)

func TestProviderHealth_NilSafe(t *testing.T) {
	var h *ProviderHealth
	// Every method must be a no-op on a nil receiver, matching the
	// package's existing nil-safe-optional-collaborator convention.
	h.RecordSuccess("newshosting")
	h.RecordFailure("newshosting")
	if got := h.Backoff("newshosting"); got != 0 {
		t.Fatalf("nil receiver Backoff = %v, want 0", got)
	}
}

func TestProviderHealth_BelowThreshold_NoBackoff(t *testing.T) {
	h := NewProviderHealth()
	h.RecordFailure("p1")
	if got := h.Backoff("p1"); got != 0 {
		t.Fatalf("first failure alone must not trigger backoff, got %v", got)
	}
}

func TestProviderHealth_ThresholdReached_AppliesBackoff(t *testing.T) {
	h := NewProviderHealth()
	h.RecordFailure("p1")
	h.RecordFailure("p1")
	got := h.Backoff("p1")
	if got <= 0 {
		t.Fatalf("second consecutive failure must trigger backoff, got %v", got)
	}
	if got > providerHealthBackoffCap {
		t.Fatalf("backoff %v exceeds cap %v", got, providerHealthBackoffCap)
	}
}

func TestProviderHealth_GrowsThenCaps(t *testing.T) {
	h := NewProviderHealth()
	// Growth phase: while comfortably under the cap, each additional
	// consecutive failure must not produce a SMALLER backoff than the
	// previous one (allow generous slack since it's real wall-clock
	// timing, not a pure function of failure count).
	var prev time.Duration
	for i := 0; i < 4; i++ {
		h.RecordFailure("p1")
		got := h.Backoff("p1")
		if got <= 0 {
			continue
		}
		if got+50*time.Millisecond < prev {
			t.Fatalf("backoff shrank across consecutive failures: iter %d got %v, prev %v", i, got, prev)
		}
		prev = got
	}
	// Saturation phase: many more failures must never exceed the cap by
	// more than a small real-clock slop, and must not collapse back to
	// zero/negative.
	for i := 0; i < 20; i++ {
		h.RecordFailure("p1")
	}
	got := h.Backoff("p1")
	if got <= 0 {
		t.Fatal("backoff must still be active after many failures")
	}
	if got > providerHealthBackoffCap+50*time.Millisecond {
		t.Fatalf("backoff exceeded cap after many failures: %v > %v", got, providerHealthBackoffCap)
	}
}

func TestProviderHealth_SuccessClearsBackoff(t *testing.T) {
	h := NewProviderHealth()
	h.RecordFailure("p1")
	h.RecordFailure("p1")
	h.RecordFailure("p1")
	if h.Backoff("p1") <= 0 {
		t.Fatal("expected backoff to be active before success")
	}
	h.RecordSuccess("p1")
	if got := h.Backoff("p1"); got != 0 {
		t.Fatalf("success must clear backoff, got %v", got)
	}
}

func TestProviderHealth_IndependentPerName(t *testing.T) {
	h := NewProviderHealth()
	h.RecordFailure("p1")
	h.RecordFailure("p1")
	if h.Backoff("p1") <= 0 {
		t.Fatal("p1 should be backed off")
	}
	if h.Backoff("p2") != 0 {
		t.Fatal("p2 must be unaffected by p1's failures")
	}
}
