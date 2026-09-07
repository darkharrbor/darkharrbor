package accountgov

import (
	"context"
	"testing"
	"time"
)

func TestGovernorDenialAttributionAndNoLeaseLeak(t *testing.T) {
	g := New("test")
	g.SetCapacity("stream", 1)
	held, err := g.Acquire(context.Background(), "stream", PriorityPlayback, "held")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if lease, err := g.Acquire(ctx, "stream", PriorityRecovery, "waiter"); err != context.DeadlineExceeded || lease != nil {
		t.Fatalf("Acquire = (%v, %v), want nil/deadline", lease, err)
	}
	if got := g.Waiting("stream"); got != 0 {
		t.Fatalf("waiting = %d, want 0", got)
	}
	got := g.Denials("stream")
	if len(got) != 1 || got[0].Priority != "recovery" || got[0].Reason != "deadline" || got[0].Count != 1 {
		t.Fatalf("denials = %#v", got)
	}
}
