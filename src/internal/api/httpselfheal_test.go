package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// --- httpSelfHealState: bounded two-pass-reconfirm/cooldown bookkeeping ---

func TestHTTPSelfHealFirstFailureOnlyArmsSuspicion(t *testing.T) {
	hs := newHTTPSelfHealState(func() time.Time { return time.Unix(1000, 0) })
	if hs.recordFailure("i1", 10*time.Minute) {
		t.Fatal("first failure must never escalate on its own")
	}
}

func TestHTTPSelfHealSecondConsecutiveFailureEscalates(t *testing.T) {
	hs := newHTTPSelfHealState(func() time.Time { return time.Unix(1000, 0) })
	hs.recordFailure("i1", 10*time.Minute)
	if !hs.recordFailure("i1", 10*time.Minute) {
		t.Fatal("second consecutive failure (no intervening success) must escalate")
	}
}

func TestHTTPSelfHealSuccessBetweenFailuresResetsSuspicion(t *testing.T) {
	hs := newHTTPSelfHealState(func() time.Time { return time.Unix(1000, 0) })
	hs.recordFailure("i1", 10*time.Minute)
	hs.recordSuccess("i1")
	if hs.recordFailure("i1", 10*time.Minute) {
		t.Fatal("a genuine success between two failures must require a fresh two-observation sequence, not escalate immediately")
	}
	// The failure just above re-arms suspicion; a further failure now escalates.
	if !hs.recordFailure("i1", 10*time.Minute) {
		t.Fatal("third failure (second since the reset) must escalate")
	}
}

func TestHTTPSelfHealCooldownSuppressesButLeavesSuspicionArmed(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := &now
	hs := newHTTPSelfHealState(func() time.Time { return *clock })
	hs.recordFailure("i1", 10*time.Minute)
	if !hs.recordFailure("i1", 10*time.Minute) {
		t.Fatal("second failure should escalate and open a cooldown window")
	}
	// Within cooldown: further reconfirmed failures must not re-escalate.
	*clock = now.Add(1 * time.Minute)
	hs.recordFailure("i1", 10*time.Minute)
	if hs.recordFailure("i1", 10*time.Minute) {
		t.Fatal("escalation within cooldown window must be suppressed")
	}
	// After cooldown elapses, a fresh failure may escalate again immediately
	// (suspicion was left armed by the cooldown-suppressed failures above).
	*clock = now.Add(11 * time.Minute)
	if !hs.recordFailure("i1", 10*time.Minute) {
		t.Fatal("escalation after cooldown elapses must succeed without waiting for a fresh two-observation sequence")
	}
}

func TestHTTPSelfHealIndependentItemsDoNotInteract(t *testing.T) {
	hs := newHTTPSelfHealState(func() time.Time { return time.Unix(1000, 0) })
	hs.recordFailure("i1", 10*time.Minute)
	if hs.recordFailure("i2", 10*time.Minute) {
		t.Fatal("a different item's first failure must never escalate")
	}
}

// TestHTTPSelfHealZeroCooldownStillRequiresTwoObservationsPerEscalation
// proves cooldown=0 only removes time-based suppression between
// escalations -- it does NOT collapse the two-pass reconfirm requirement
// itself. Each escalation still needs its own fresh pair of consecutive
// failures (arm, then confirm), repeatable back-to-back with no waiting
// once cooldown is disabled.
func TestHTTPSelfHealZeroCooldownStillRequiresTwoObservationsPerEscalation(t *testing.T) {
	hs := newHTTPSelfHealState(func() time.Time { return time.Unix(1000, 0) })
	if hs.recordFailure("i1", 0) {
		t.Fatal("first failure of the pair must only arm suspicion")
	}
	if !hs.recordFailure("i1", 0) {
		t.Fatal("second failure of the pair must escalate")
	}
	if hs.recordFailure("i1", 0) {
		t.Fatal("immediately after an escalation, a lone next failure must only re-arm, not re-escalate")
	}
	if !hs.recordFailure("i1", 0) {
		t.Fatal("a second fresh pair must escalate again immediately, since cooldown is disabled")
	}
}

func TestHTTPSelfHealAttemptCounter(t *testing.T) {
	hs := newHTTPSelfHealState(func() time.Time { return time.Unix(1000, 0) })
	if hs.AttemptCount("i1") != 0 {
		t.Fatal("unattempted item must report 0")
	}
	hs.recordFailure("i1", 0)
	hs.recordFailure("i1", 0)
	if got := hs.AttemptCount("i1"); got != 1 {
		t.Fatalf("AttemptCount = %d, want 1", got)
	}
}

func TestHTTPSelfHealEmptyItemIDNeverEscalates(t *testing.T) {
	hs := newHTTPSelfHealState(func() time.Time { return time.Unix(1000, 0) })
	hs.recordFailure("", 0)
	if hs.recordFailure("", 0) {
		t.Fatal("empty item ID must never escalate (never guessed)")
	}
}

// TestHTTPSelfHealConcurrentFailuresRace proves the bookkeeping holds under
// real goroutine concurrency (run with -race): of N concurrent recordFailure
// calls starting from a clean state, exactly one may observe the
// second-consecutive-failure transition and escalate.
func TestHTTPSelfHealConcurrentFailuresRace(t *testing.T) {
	hs := newHTTPSelfHealState(func() time.Time { return time.Unix(1000, 0) })
	hs.recordFailure("i1", 10*time.Minute) // arm suspicion once, deterministically
	var escalations int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if hs.recordFailure("i1", 10*time.Minute) {
				atomic.AddInt32(&escalations, 1)
			}
		}()
	}
	wg.Wait()
	if escalations != 1 {
		t.Fatalf("escalations = %d, want exactly 1", escalations)
	}
}

// --- Server.observeHTTPStreamFailure / observeHTTPStreamSuccess ---

// fakeDecayEscalator records EscalateDecay calls.
type fakeDecayEscalator struct {
	mu    sync.Mutex
	items []string
}

func (f *fakeDecayEscalator) EscalateDecay(item *store.Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, item.ID)
}

func (f *fakeDecayEscalator) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

func newSelfHealTestServer(t *testing.T, enabled bool, cooldownMin int, esc HTTPDecayEscalator) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.HTTPStream.SelfHealEnabled = enabled
	cfg.HTTPStream.SelfHealCooldownMin = cooldownMin
	s := &Server{
		cfg:          cfg,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpSelfHeal: newHTTPSelfHealState(nil),
	}
	s.httpDecayEscalator = esc
	return s
}

func TestObserveHTTPStreamFailureEscalatesOnSecondFailure(t *testing.T) {
	esc := &fakeDecayEscalator{}
	s := newSelfHealTestServer(t, true, 60, esc)
	item := &store.Item{ID: "item-1"}

	s.observeHTTPStreamFailure(item)
	s.wg.Wait()
	if esc.calls() != 0 {
		t.Fatalf("escalator calls = %d after first failure, want 0", esc.calls())
	}

	s.observeHTTPStreamFailure(item)
	s.wg.Wait()
	if esc.calls() != 1 {
		t.Fatalf("escalator calls = %d after second failure, want 1", esc.calls())
	}
}

func TestObserveHTTPStreamSuccessResetsSuspicion(t *testing.T) {
	esc := &fakeDecayEscalator{}
	s := newSelfHealTestServer(t, true, 60, esc)
	item := &store.Item{ID: "item-1"}

	s.observeHTTPStreamFailure(item)
	s.observeHTTPStreamSuccess(item)
	s.observeHTTPStreamFailure(item)
	s.wg.Wait()
	if esc.calls() != 0 {
		t.Fatalf("escalator calls = %d, want 0 (success between failures must reset the two-pass requirement)", esc.calls())
	}
}

func TestObserveHTTPStreamFailureDisabledNeverEscalates(t *testing.T) {
	esc := &fakeDecayEscalator{}
	s := newSelfHealTestServer(t, false, 60, esc)
	item := &store.Item{ID: "item-1"}

	s.observeHTTPStreamFailure(item)
	s.observeHTTPStreamFailure(item)
	s.wg.Wait()
	if esc.calls() != 0 {
		t.Fatalf("escalator calls = %d, want 0 (row disabled)", esc.calls())
	}
}

func TestObserveHTTPStreamFailureNilEscalatorNeverPanics(t *testing.T) {
	s := newSelfHealTestServer(t, true, 60, nil)
	item := &store.Item{ID: "item-1"}
	s.observeHTTPStreamFailure(item)
	s.observeHTTPStreamFailure(item)
	s.wg.Wait() // must not panic with a nil escalator
}

func TestObserveHTTPStreamFailureNilItemAndNilStateAreNoOps(t *testing.T) {
	esc := &fakeDecayEscalator{}
	s := newSelfHealTestServer(t, true, 60, esc)
	s.observeHTTPStreamFailure(nil)
	s.observeHTTPStreamSuccess(nil)

	s2 := &Server{cfg: &config.Config{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s2.cfg.HTTPStream.SelfHealEnabled = true
	s2.observeHTTPStreamFailure(&store.Item{ID: "x"}) // httpSelfHeal is nil
	s2.observeHTTPStreamSuccess(&store.Item{ID: "x"})
	s.wg.Wait()
	if esc.calls() != 0 {
		t.Fatalf("escalator calls = %d, want 0", esc.calls())
	}
}

// --- httpStreamUnavailable: HR3.4's client-cancellation filter ---
//
// A real live-gate session caught this defect for real: a genuine client
// timeout produced a real context.Canceled error that routed through
// httpStreamUnavailable, which (before this fix) counted it toward the
// two-pass reconfirm exactly like a real backend failure. Ordinary
// Jellyfin seek/probe disconnects must never do that.

func TestHTTPStreamUnavailableSkipsSelfHealOnClientCancellation(t *testing.T) {
	esc := &fakeDecayEscalator{}
	s := newSelfHealTestServer(t, true, 60, esc)
	item := &store.Item{ID: "item-1"}
	w := httptest.NewRecorder()

	s.httpStreamUnavailable(w, item, context.Canceled)
	s.httpStreamUnavailable(w, item, context.Canceled)
	s.wg.Wait()
	if esc.calls() != 0 {
		t.Fatalf("escalator calls = %d, want 0 (client cancellation must never arm or reconfirm self-heal suspicion)", esc.calls())
	}

	// A wrapped context.Canceled (errors.Is chain) must be filtered too,
	// not just the bare sentinel.
	joined := errors.Join(context.Canceled, errors.New("client gone"))
	s.httpStreamUnavailable(w, item, joined)
	s.wg.Wait()
	if esc.calls() != 0 {
		t.Fatalf("escalator calls = %d, want 0 (a joined/wrapped context.Canceled must still be filtered via errors.Is)", esc.calls())
	}
}

func TestHTTPStreamUnavailableSkipsSelfHealOnTransientBackendFailure(t *testing.T) {
	esc := &fakeDecayEscalator{}
	s := newSelfHealTestServer(t, true, 60, esc)
	item := &store.Item{ID: "item-1"}
	w := httptest.NewRecorder()

	transient := httpstream.NewError(httpstream.ClassBackendUnavailable, "source fetch failed")
	s.httpStreamUnavailable(w, item, transient)
	s.httpStreamUnavailable(w, item, transient)
	s.wg.Wait()
	if esc.calls() != 0 {
		t.Fatalf("escalator calls = %d, want 0 (transient backend outages must never arm or reconfirm permanent decay)", esc.calls())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHTTPStreamUnavailableRecordsPermanentSourceFailure(t *testing.T) {
	esc := &fakeDecayEscalator{}
	s := newSelfHealTestServer(t, true, 60, esc)
	item := &store.Item{ID: "item-1"}
	w := httptest.NewRecorder()

	permanent := httpstream.NewError(httpstream.ClassRepresentationLost, "selected representation is gone")
	s.httpStreamUnavailable(w, item, permanent)
	s.httpStreamUnavailable(w, item, permanent)
	s.wg.Wait()
	if esc.calls() != 1 {
		t.Fatalf("escalator calls = %d, want 1 (two permanent source failures must still reconfirm and escalate)", esc.calls())
	}
}

// TestObserveHTTPStreamFailureConcurrentDoesNotDoubleEscalate proves that
// firing many concurrent stream failures for the same item never produces
// more than one escalation within the cooldown window (run with -race).
func TestObserveHTTPStreamFailureConcurrentDoesNotDoubleEscalate(t *testing.T) {
	esc := &fakeDecayEscalator{}
	s := newSelfHealTestServer(t, true, 60, esc)
	item := &store.Item{ID: "item-1"}
	s.observeHTTPStreamFailure(item) // deterministically arm suspicion first

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.observeHTTPStreamFailure(item)
		}()
	}
	wg.Wait()
	s.wg.Wait()
	if esc.calls() != 1 {
		t.Fatalf("escalator calls = %d, want exactly 1 across 30 concurrent reconfirmations", esc.calls())
	}
}
