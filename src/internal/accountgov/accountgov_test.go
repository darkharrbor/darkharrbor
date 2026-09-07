package accountgov

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAcquire_Unbounded_NeverBlocks(t *testing.T) {
	g := New("test")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		l, err := g.Acquire(ctx, "query", PriorityGrab, "s1")
		if err != nil {
			t.Fatalf("Acquire #%d: %v", i, err)
		}
		l.Release()
	}
}

func TestAcquire_CapacityGating_BlocksBeyondCapacity(t *testing.T) {
	g := New("test")
	g.SetCapacity("create", 1)
	ctx := context.Background()

	l1, err := g.Acquire(ctx, "create", PriorityGrab, "s1")
	if err != nil {
		t.Fatalf("Acquire #1: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		l2, err := g.Acquire(ctx, "create", PriorityGrab, "s2")
		if err != nil {
			t.Errorf("Acquire #2: %v", err)
			return
		}
		l2.Release()
		close(acquired)
	}()

	// Give the goroutine a moment to actually block on the capacity.
	time.Sleep(20 * time.Millisecond)
	select {
	case <-acquired:
		t.Fatal("second Acquire granted before first Release — capacity=1 not enforced")
	default:
	}

	l1.Release()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire never granted after Release")
	}
}

func TestAcquire_PriorityOrdering_HigherPriorityServedFirst(t *testing.T) {
	g := New("test")
	g.SetCapacity("op", 1)
	ctx := context.Background()

	// Hold the single lease so both waiters queue.
	held, err := g.Acquire(ctx, "op", PriorityGrab, "holder")
	if err != nil {
		t.Fatalf("Acquire holder: %v", err)
	}

	var order []string
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Enqueue the low-priority (prewarm) waiter first...
	wg.Add(1)
	go func() {
		defer wg.Done()
		l, err := g.Acquire(ctx, "op", PriorityPrewarm, "bg")
		if err != nil {
			t.Errorf("Acquire prewarm: %v", err)
			return
		}
		mu.Lock()
		order = append(order, "prewarm")
		mu.Unlock()
		l.Release()
	}()
	waitUntil(t, func() bool { return g.Waiting("op") == 1 })

	// ...then the high-priority (playback) waiter second. Despite arriving
	// later, it must be admitted first (LC-06 fixed priority).
	wg.Add(1)
	go func() {
		defer wg.Done()
		l, err := g.Acquire(ctx, "op", PriorityPlayback, "fg")
		if err != nil {
			t.Errorf("Acquire playback: %v", err)
			return
		}
		mu.Lock()
		order = append(order, "playback")
		mu.Unlock()
		l.Release()
	}()
	waitUntil(t, func() bool { return g.Waiting("op") == 2 })

	held.Release()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "playback" || order[1] != "prewarm" {
		t.Fatalf("expected playback admitted before prewarm, got %v", order)
	}
}

func TestAcquire_SessionFairShare_RoundRobinsAtSamePriority(t *testing.T) {
	g := New("test")
	g.SetCapacity("op", 1)
	ctx := context.Background()

	held, err := g.Acquire(ctx, "op", PriorityGrab, "sessionA")
	if err != nil {
		t.Fatalf("Acquire holder: %v", err)
	}

	var order []string
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Two waiters from session B queue first, then one from session A.
	// Session A already has one grant (the initial holder), so fair share
	// must prefer session B's waiters over a second grant to session A.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := g.Acquire(ctx, "op", PriorityGrab, "sessionB")
			if err != nil {
				t.Errorf("Acquire sessionB: %v", err)
				return
			}
			mu.Lock()
			order = append(order, "B")
			mu.Unlock()
			l.Release()
		}()
	}
	waitUntil(t, func() bool { return g.Waiting("op") == 2 })

	wg.Add(1)
	go func() {
		defer wg.Done()
		l, err := g.Acquire(ctx, "op", PriorityGrab, "sessionA")
		if err != nil {
			t.Errorf("Acquire sessionA: %v", err)
			return
		}
		mu.Lock()
		order = append(order, "A")
		mu.Unlock()
		l.Release()
	}()
	waitUntil(t, func() bool { return g.Waiting("op") == 3 })

	held.Release()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != "B" {
		t.Fatalf("expected session B (least served) admitted before session A, got %v", order)
	}
}

func TestAcquire_ContextCancellation_RemovesWaiterAndDoesNotLeakCapacity(t *testing.T) {
	g := New("test")
	g.SetCapacity("op", 1)

	held, err := g.Acquire(context.Background(), "op", PriorityGrab, "holder")
	if err != nil {
		t.Fatalf("Acquire holder: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := g.Acquire(cancelCtx, "op", PriorityGrab, "waiter")
		errCh <- err
	}()
	waitUntil(t, func() bool { return g.Waiting("op") == 1 })

	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected context error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Acquire never returned")
	}
	if g.Waiting("op") != 0 {
		t.Fatalf("cancelled waiter not removed from queue: waiting=%d", g.Waiting("op"))
	}

	held.Release()

	// Capacity must still be exactly usable once more (not leaked or
	// double-freed by the cancellation).
	l, err := g.Acquire(context.Background(), "op", PriorityGrab, "next")
	if err != nil {
		t.Fatalf("Acquire after cancellation: %v", err)
	}
	l.Release()
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// --- ConnCeiling (AIMD) ---

func TestConnCeiling_StartsAtConfigured(t *testing.T) {
	c := NewConnCeiling(16)
	if got := c.Current(); got != 16 {
		t.Fatalf("Current() = %d, want 16", got)
	}
	if got := c.Configured(); got != 16 {
		t.Fatalf("Configured() = %d, want 16", got)
	}
}

func TestConnCeiling_OnRejected_HalvesAndFloors(t *testing.T) {
	c := NewConnCeiling(16)
	c.OnRejected()
	if got := c.Current(); got != 8 {
		t.Fatalf("after 1 rejection: Current() = %d, want 8", got)
	}
	c.OnRejected()
	if got := c.Current(); got != 4 {
		t.Fatalf("after 2 rejections: Current() = %d, want 4", got)
	}
	c.OnRejected()
	c.OnRejected()
	if got := c.Current(); got != 1 {
		t.Fatalf("after repeated rejections: Current() = %d, want floor 1", got)
	}
	c.OnRejected()
	if got := c.Current(); got != 1 {
		t.Fatalf("floor breached: Current() = %d, want 1", got)
	}
}

func TestConnCeiling_GrowsBackTowardConfiguredAfterCleanWindow(t *testing.T) {
	c := NewConnCeiling(8)
	now := time.Unix(0, 0)
	c.SetClock(func() time.Time { return now })
	c.SetGrowInterval(10 * time.Second)

	c.OnRejected() // 8 -> 4, lastChange = now
	if got := c.Current(); got != 4 {
		t.Fatalf("after rejection: Current() = %d, want 4", got)
	}

	// Not enough time has passed: no growth yet.
	now = now.Add(5 * time.Second)
	if got := c.Current(); got != 4 {
		t.Fatalf("premature growth: Current() = %d, want 4", got)
	}

	// A full grow interval has now elapsed: probe up by one.
	now = now.Add(6 * time.Second)
	if got := c.Current(); got != 5 {
		t.Fatalf("after grow window: Current() = %d, want 5", got)
	}

	// Repeated elapsed windows keep probing up, capped at configured.
	for i := 0; i < 10; i++ {
		now = now.Add(11 * time.Second)
		c.Current()
	}
	if got := c.Current(); got != 8 {
		t.Fatalf("after repeated growth: Current() = %d, want configured 8", got)
	}
}

func TestConnCeiling_RejectionDuringGrowth_ResetsWindow(t *testing.T) {
	c := NewConnCeiling(8)
	now := time.Unix(0, 0)
	c.SetClock(func() time.Time { return now })
	c.SetGrowInterval(10 * time.Second)

	c.OnRejected() // 8 -> 4
	now = now.Add(11 * time.Second)
	if got := c.Current(); got != 5 {
		t.Fatalf("Current() = %d, want 5", got)
	}

	// A second rejection right after growing must halve from the current
	// (grown) value, not the original configured value, and reset the
	// grow window.
	c.OnRejected() // 5 -> 2
	if got := c.Current(); got != 2 {
		t.Fatalf("after second rejection: Current() = %d, want 2", got)
	}
	now = now.Add(5 * time.Second)
	if got := c.Current(); got != 2 {
		t.Fatalf("premature growth after reset: Current() = %d, want 2", got)
	}
}
