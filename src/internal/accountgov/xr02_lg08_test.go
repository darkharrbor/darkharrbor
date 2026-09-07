package accountgov

import (
	"context"
	"sync"
	"testing"
)

func TestXR02LG08PriorityAndAsymmetricPlaybackFairness(t *testing.T) {
	t.Run("all operation priorities", func(t *testing.T) {
		g := New("xr02-priority")
		g.SetCapacity("shared", 1)
		held, err := g.Acquire(context.Background(), "shared", PriorityPlayback, "holder")
		if err != nil {
			t.Fatal(err)
		}

		priorities := []Priority{
			PriorityPrewarm,
			PriorityAudit,
			PriorityRepair,
			PriorityGrab,
			PriorityRecovery,
			PriorityPlayback,
		}
		var (
			mu    sync.Mutex
			order []Priority
			wg    sync.WaitGroup
		)
		for i, priority := range priorities {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lease, acquireErr := g.Acquire(context.Background(), "shared", priority, priority.String())
				if acquireErr != nil {
					t.Errorf("acquire %s: %v", priority, acquireErr)
					return
				}
				mu.Lock()
				order = append(order, priority)
				mu.Unlock()
				lease.Release()
			}()
			waitUntil(t, func() bool { return g.Waiting("shared") == i+1 })
		}

		held.Release()
		wg.Wait()
		want := []Priority{
			PriorityPlayback,
			PriorityRecovery,
			PriorityGrab,
			PriorityRepair,
			PriorityAudit,
			PriorityPrewarm,
		}
		if len(order) != len(want) {
			t.Fatalf("grant count = %d, want %d", len(order), len(want))
		}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("grant order = %v, want %v", order, want)
			}
		}
		if g.InUse("shared") != 0 || g.Waiting("shared") != 0 {
			t.Fatalf("governor leaked: in_use=%d waiting=%d", g.InUse("shared"), g.Waiting("shared"))
		}
	})

	t.Run("asymmetric bitrate playback sessions", func(t *testing.T) {
		g := New("xr02-fairness")
		g.SetCapacity("stream", 2)

		highA, err := g.Acquire(context.Background(), "stream", PriorityPlayback, "high-bitrate")
		if err != nil {
			t.Fatal(err)
		}
		highB, err := g.Acquire(context.Background(), "stream", PriorityPlayback, "high-bitrate")
		if err != nil {
			t.Fatal(err)
		}

		grants := make(chan string, 2)
		var wg sync.WaitGroup
		for _, session := range []string{"high-bitrate", "low-bitrate"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lease, acquireErr := g.Acquire(context.Background(), "stream", PriorityPlayback, session)
				if acquireErr != nil {
					t.Errorf("acquire %s: %v", session, acquireErr)
					return
				}
				grants <- session
				lease.Release()
			}()
		}
		waitUntil(t, func() bool { return g.Waiting("stream") == 2 })

		highA.Release()
		if first := <-grants; first != "low-bitrate" {
			t.Fatalf("first fair-share grant = %q, want low-bitrate", first)
		}
		highB.Release()
		wg.Wait()
		close(grants)
		for range grants {
		}
		if g.InUse("stream") != 0 || g.Waiting("stream") != 0 {
			t.Fatalf("governor leaked: in_use=%d waiting=%d", g.InUse("stream"), g.Waiting("stream"))
		}
	})
}
