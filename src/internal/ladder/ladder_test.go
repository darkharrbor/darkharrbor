package ladder

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

var errBoom = errors.New("boom")

// TestRun_SuccessAtFirstRung proves a successful first rung stops the walk
// immediately without touching later rungs.
func TestRun_SuccessAtFirstRung(t *testing.T) {
	second := 0
	l := New(nil,
		Rung{Name: "a", Attempt: func(context.Context) error { return nil }},
		Rung{Name: "b", Attempt: func(context.Context) error { second++; return nil }},
	)
	res, err := l.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Succeeded || res.StoppedAt != "a" {
		t.Fatalf("expected success at rung a, got %+v", res)
	}
	if second != 0 {
		t.Fatalf("rung b should never have been attempted, got %d calls", second)
	}
	if len(res.Rungs) != 1 || res.Rungs[0].Attempts != 1 {
		t.Fatalf("expected exactly one recorded rung with 1 attempt, got %+v", res.Rungs)
	}
}

// TestRun_PermanentAdvancesWithoutRetry proves a ClassPermanentHere outcome
// advances to the next rung on the very first attempt -- no bounded retry
// of a rung that cannot succeed.
func TestRun_PermanentAdvancesWithoutRetry(t *testing.T) {
	calls := 0
	l := New(nil,
		Rung{Name: "a", MaxAttempts: 5, Attempt: func(context.Context) error {
			calls++
			return outcome.Permanent(errBoom)
		}},
		Rung{Name: "b", Attempt: func(context.Context) error { return nil }},
	)
	res, err := l.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("permanent rung should be attempted exactly once despite MaxAttempts=5, got %d", calls)
	}
	if !res.Succeeded || res.StoppedAt != "b" {
		t.Fatalf("expected fallthrough success at rung b, got %+v", res)
	}
	if res.Rungs[0].Class != outcome.ClassPermanentHere {
		t.Fatalf("expected rung a recorded as permanent_here, got %+v", res.Rungs[0])
	}
}

// TestRun_UnknownAdvancesWithoutRetry proves an unclassified (ClassUnknown)
// error is treated like permanent -- advance, never loop -- per SF-01's
// "fix the taxonomy gap, don't silently retry it" rule.
func TestRun_UnknownAdvancesWithoutRetry(t *testing.T) {
	calls := 0
	l := New(nil,
		Rung{Name: "a", MaxAttempts: 5, Attempt: func(context.Context) error {
			calls++
			return errBoom // deliberately unwrapped: classifies as ClassUnknown
		}},
		Rung{Name: "b", Attempt: func(context.Context) error { return nil }},
	)
	res, err := l.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("unknown-classified rung should be attempted exactly once, got %d", calls)
	}
	if res.Rungs[0].Class != outcome.ClassUnknown {
		t.Fatalf("expected ClassUnknown, got %q", res.Rungs[0].Class)
	}
}

// TestRun_TransientRetriesThenAdvances proves a ClassTransientHere outcome
// retries the SAME rung up to MaxAttempts, then advances.
func TestRun_TransientRetriesThenAdvances(t *testing.T) {
	calls := 0
	l := New(nil,
		Rung{Name: "a", MaxAttempts: 3, Attempt: func(context.Context) error {
			calls++
			return outcome.Transient(errBoom)
		}},
		Rung{Name: "b", Attempt: func(context.Context) error { return nil }},
	)
	res, err := l.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected exactly MaxAttempts=3 attempts of the transient rung, got %d", calls)
	}
	if res.Rungs[0].Attempts != 3 || res.Rungs[0].Class != outcome.ClassTransientHere {
		t.Fatalf("unexpected rung a accounting: %+v", res.Rungs[0])
	}
	if !res.Succeeded || res.StoppedAt != "b" {
		t.Fatalf("expected fallthrough success at rung b, got %+v", res)
	}
}

// TestRun_TransientSucceedsWithinBudget proves a transient rung that
// succeeds on a later bounded attempt stops there rather than continuing to
// burn its remaining budget or falling through to the next rung.
func TestRun_TransientSucceedsWithinBudget(t *testing.T) {
	calls := 0
	l := New(nil,
		Rung{Name: "a", MaxAttempts: 5, Attempt: func(context.Context) error {
			calls++
			if calls < 3 {
				return outcome.Transient(errBoom)
			}
			return nil
		}},
		Rung{Name: "b", Attempt: func(context.Context) error {
			t.Fatal("rung b should not be attempted")
			return nil
		}},
	)
	res, err := l.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected exactly 3 attempts (2 transient + 1 success), got %d", calls)
	}
	if !res.Succeeded || res.StoppedAt != "a" || res.Rungs[0].Attempts != 3 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestRun_AccountLevelStopsEntireWalk proves an account-level outcome halts
// the whole ladder immediately -- later rungs, even ones that would
// otherwise succeed, are never tried.
func TestRun_AccountLevelStopsEntireWalk(t *testing.T) {
	laterCalled := false
	l := New(nil,
		Rung{Name: "a", Attempt: func(context.Context) error {
			return outcome.AccountLevel(errBoom)
		}},
		Rung{Name: "b", Attempt: func(context.Context) error {
			laterCalled = true
			return nil
		}},
	)
	res, err := l.Run(context.Background(), "")
	if !errors.Is(err, ErrAccountLevel) {
		t.Fatalf("expected ErrAccountLevel, got %v", err)
	}
	if laterCalled {
		t.Fatal("rung b must never be attempted after an account-level stop")
	}
	if res.Succeeded || res.StoppedAt != "a" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestRun_ExhaustedAllRungsFail proves that when every rung fails without
// an account-level stop, Run reports ErrExhausted and records every rung.
func TestRun_ExhaustedAllRungsFail(t *testing.T) {
	l := New(nil,
		Rung{Name: "a", Attempt: func(context.Context) error { return outcome.Permanent(errBoom) }},
		Rung{Name: "b", Attempt: func(context.Context) error { return outcome.Permanent(errBoom) }},
	)
	res, err := l.Run(context.Background(), "")
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("expected ErrExhausted, got %v", err)
	}
	if res.Succeeded || res.StoppedAt != "" || len(res.Rungs) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestRun_ContextCancellationAbortsWalk proves a cancelled context aborts
// mid-rung and never proceeds to a later rung.
func TestRun_ContextCancellationAbortsWalk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	laterCalled := false
	l := New(nil,
		Rung{Name: "a", MaxAttempts: 3, Attempt: func(context.Context) error {
			return outcome.Transient(errBoom)
		}},
		Rung{Name: "b", Attempt: func(context.Context) error {
			laterCalled = true
			return nil
		}},
	)
	_, err := l.Run(ctx, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if laterCalled {
		t.Fatal("rung b must never be attempted once ctx is cancelled")
	}
}

// TestRun_EmptyLadder proves a Ladder with zero rungs deterministically
// reports exhaustion rather than a false success.
func TestRun_EmptyLadder(t *testing.T) {
	l := New(nil)
	res, err := l.Run(context.Background(), "")
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("expected ErrExhausted for an empty ladder, got %v", err)
	}
	if res.Succeeded || len(res.Rungs) != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestRun_LeasePriorityOrdering proves the ladder's lease acquisition
// genuinely honors accountgov's LC-06 priority ordering under real
// contention: with capacity=1, a higher-priority Run queued behind a
// lower-priority one that is already holding the lease is admitted before a
// same-priority-tier waiter that arrived earlier, exactly matching
// accountgov's own documented fair-share/priority contract, proving the
// ladder is not silently bypassing it (e.g. by leasing outside the
// Governor, or leasing per-attempt in a way that loses ordering).
func TestRun_LeasePriorityOrdering(t *testing.T) {
	gov := accountgov.New("cg05-det-proof")
	gov.SetCapacity("op", 1)

	holdRelease := make(chan struct{})
	holding := make(chan struct{})
	var order []string
	var mu sync.Mutex

	// Rung whose Attempt blocks until told to release, so we can force
	// real queueing behind it.
	blocker := New(gov, Rung{Name: "hold", Op: "op", Priority: accountgov.PriorityPlayback,
		Attempt: func(context.Context) error {
			close(holding)
			<-holdRelease
			return nil
		}})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		blocker.Run(context.Background(), "holder")
	}()
	<-holding // the blocker now holds the only lease for "op"

	// Two more Run calls queue behind it: a low-priority prewarm request
	// and a high-priority playback-recovery request, low-priority queued
	// first in wall-clock order. Priority must still win.
	lowDone := make(chan struct{})
	highDone := make(chan struct{})

	low := New(gov, Rung{Name: "low", Op: "op", Priority: accountgov.PriorityPrewarm,
		Attempt: func(context.Context) error {
			mu.Lock()
			order = append(order, "low")
			mu.Unlock()
			close(lowDone)
			return nil
		}})
	high := New(gov, Rung{Name: "high", Op: "op", Priority: accountgov.PriorityRecovery,
		Attempt: func(context.Context) error {
			mu.Lock()
			order = append(order, "high")
			mu.Unlock()
			close(highDone)
			return nil
		}})

	wg.Add(2)
	go func() { defer wg.Done(); low.Run(context.Background(), "low-session") }()
	// Ensure low's Acquire call has actually enqueued before high's, so this
	// is a genuine same-capacity priority-inversion test, not a race.
	time.Sleep(50 * time.Millisecond)
	go func() { defer wg.Done(); high.Run(context.Background(), "high-session") }()
	time.Sleep(50 * time.Millisecond)

	close(holdRelease) // release the lease; the queue now admits by priority

	select {
	case <-highDone:
	case <-time.After(2 * time.Second):
		t.Fatal("high-priority rung never completed")
	}
	select {
	case <-lowDone:
	case <-time.After(2 * time.Second):
		t.Fatal("low-priority rung never completed")
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "high" || order[1] != "low" {
		t.Fatalf("expected high-priority rung admitted before low-priority despite arriving later, got order=%v", order)
	}
}
