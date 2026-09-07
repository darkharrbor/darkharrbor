package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/cachegate"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/governor"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// --- repairState: bounded in-flight/cooldown/counter bookkeeping ---

func TestRepairStateTryClaimSingleFlight(t *testing.T) {
	rs := newRepairState()
	now := time.Unix(1000, 0)
	if !rs.tryClaim("h1", now) {
		t.Fatal("first claim should succeed")
	}
	if rs.tryClaim("h1", now) {
		t.Fatal("second concurrent claim for the same hash must be refused (in-flight)")
	}
	if !rs.tryClaim("h2", now) {
		t.Fatal("a different identity must claim independently")
	}
}

func TestRepairStateCooldownAfterRelease(t *testing.T) {
	rs := newRepairState()
	now := time.Unix(1000, 0)
	if !rs.tryClaim("h1", now) {
		t.Fatal("first claim should succeed")
	}
	rs.release("h1", now, 10*time.Minute)
	if rs.tryClaim("h1", now.Add(1*time.Minute)) {
		t.Fatal("claim within cooldown window must be refused")
	}
	if !rs.tryClaim("h1", now.Add(11*time.Minute)) {
		t.Fatal("claim after cooldown elapses must succeed")
	}
}

func TestRepairStateZeroCooldownReclaimableImmediately(t *testing.T) {
	rs := newRepairState()
	now := time.Unix(1000, 0)
	rs.tryClaim("h1", now)
	rs.release("h1", now, 0)
	if !rs.tryClaim("h1", now) {
		t.Fatal("zero cooldown must allow immediate re-claim")
	}
}

func TestRepairStateAttemptCounter(t *testing.T) {
	rs := newRepairState()
	now := time.Unix(1000, 0)
	if rs.AttemptCount("h1") != 0 {
		t.Fatal("unattempted identity must report 0")
	}
	rs.tryClaim("h1", now)
	rs.release("h1", now, 0)
	rs.tryClaim("h1", now)
	rs.release("h1", now, 0)
	if got := rs.AttemptCount("h1"); got != 2 {
		t.Fatalf("AttemptCount = %d, want 2", got)
	}
}

// TestRepairStateConcurrentClaimsRace proves single-flight holds under real
// goroutine concurrency (run with -race): of N concurrent tryClaim calls for
// the same identity, exactly one may succeed at a time.
func TestRepairStateConcurrentClaimsRace(t *testing.T) {
	rs := newRepairState()
	now := time.Unix(1000, 0)
	var successes int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rs.tryClaim("h1", now) {
				atomic.AddInt32(&successes, 1)
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1 (single-flight)", successes)
	}
}

// --- torrentRepairIdentity ---

func TestTorrentRepairIdentityPrefersRealInfoHash(t *testing.T) {
	ih := "ABCDEF0123456789ABCDEF0123456789ABCDEF01"
	item := &store.Item{InfoHash: &ih}
	item.Metadata.RealInfoHash = "1111111111111111111111111111111111111111"
	if got := torrentRepairIdentity(item); got != "1111111111111111111111111111111111111111" {
		t.Fatalf("got %q, want RealInfoHash lowercased", got)
	}
}

func TestTorrentRepairIdentityFallsBackToInfoHash(t *testing.T) {
	ih := "ABCDEF0123456789ABCDEF0123456789ABCDEF01"
	item := &store.Item{InfoHash: &ih}
	if got := torrentRepairIdentity(item); got != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("got %q, want lowercased InfoHash", got)
	}
}

func TestTorrentRepairIdentityEmptyWhenNeitherKnown(t *testing.T) {
	item := &store.Item{}
	if got := torrentRepairIdentity(item); got != "" {
		t.Fatalf("got %q, want empty (never guessed)", got)
	}
	if got := torrentRepairIdentity(nil); got != "" {
		t.Fatalf("nil item: got %q, want empty", got)
	}
}

// --- runRepairChain: the (a) -> (b) -> (c) walk ---

func newRepairTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "repair.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return store.New(db)
}

// fakeArrNotifier records Notify() calls.
type fakeArrNotifier struct {
	calls int32
}

func (f *fakeArrNotifier) Notify() { atomic.AddInt32(&f.calls, 1) }

func newRepairTestServer(t *testing.T, st *store.Store, lanes []ProviderLane, providerOrder []string, notify *fakeArrNotifier) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Governor.RepairEnabled = true
	cfg.Governor.RepairCooldownMin = 30
	s := &Server{
		cfg:           cfg,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:         st,
		lanes:         lanes,
		providerOrder: providerOrder,
		shutdownCtx:   context.Background(),
		repairState:   newRepairState(),
	}
	if notify != nil {
		s.arrRefresh = notify
	}
	return s
}

func newRepairLane(t *testing.T, name string, prov provider.Provider) ProviderLane {
	t.Helper()
	gov := governor.New(nil, 0, 0)
	return ProviderLane{Prov: prov, Gate: cachegate.New(prov, gov, false)}
}

func mustCreateTorrentItem(t *testing.T, st *store.Store, id, hash, providerName string) *store.Item {
	t.Helper()
	item := &store.Item{
		ID:          id,
		PublicID:    id,
		SourceType:  store.SourceTypeTorrent,
		State:       store.StateReady,
		DisplayName: "Test.Torrent.Item",
		InfoHash:    &hash,
		Provider:    &providerName,
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return item
}

func TestRunRepairChainStepASucceedsSameProvider(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	hash := "1111111111111111111111111111111111111111"
	item := mustCreateTorrentItem(t, st, "item-a", hash, "torbox")

	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitResp: &provider.CreateTaskResponse{RemoteID: "r2"}}
	alt := &altFakeProvider{name: "rd"}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary), newRepairLane(t, "rd", alt)}
	notify := &fakeArrNotifier{}
	s := newRepairTestServer(t, st, lanes, []string{"torbox", "rd"}, notify)

	s.runRepairChain(ctx, item.ID, hash)

	if primary.submitCalls != 1 {
		t.Fatalf("primary submitCalls = %d, want 1", primary.submitCalls)
	}
	if alt.submitCalls != 0 {
		t.Fatalf("alt submitCalls = %d, want 0 (step (a) already healed it)", alt.submitCalls)
	}
	fresh, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if fresh.State != store.StateResolving {
		t.Fatalf("state = %q, want StateResolving", fresh.State)
	}
	if fresh.Provider == nil || *fresh.Provider != "torbox" {
		t.Fatalf("provider = %v, want torbox (unchanged)", fresh.Provider)
	}
	if notify.calls != 0 {
		t.Fatalf("arr notify calls = %d, want 0 (no blacklist path taken)", notify.calls)
	}
}

func TestRunRepairChainStepBSucceedsAltProvider(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	hash := "2222222222222222222222222222222222222222"
	item := mustCreateTorrentItem(t, st, "item-b", hash, "torbox")

	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitErr: errors.New("gone")}
	alt := &altFakeProvider{name: "rd", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitResp: &provider.CreateTaskResponse{RemoteID: "r3"}}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary), newRepairLane(t, "rd", alt)}
	notify := &fakeArrNotifier{}
	s := newRepairTestServer(t, st, lanes, []string{"torbox", "rd"}, notify)

	s.runRepairChain(ctx, item.ID, hash)

	if primary.submitCalls != 1 || alt.submitCalls != 1 {
		t.Fatalf("submitCalls primary=%d alt=%d, want 1/1", primary.submitCalls, alt.submitCalls)
	}
	fresh, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if fresh.State != store.StateResolving {
		t.Fatalf("state = %q, want StateResolving", fresh.State)
	}
	if fresh.Provider == nil || *fresh.Provider != "rd" {
		t.Fatalf("provider = %v, want rd (rebound to alternate)", fresh.Provider)
	}
	if notify.calls != 0 {
		t.Fatalf("arr notify calls = %d, want 0", notify.calls)
	}
}

func TestRunRepairChainStepCBlacklistsAndFailsWhenAllLanesExhausted(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	hash := "3333333333333333333333333333333333333333"
	item := mustCreateTorrentItem(t, st, "item-c", hash, "torbox")

	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitErr: errors.New("gone")}
	alt := &altFakeProvider{name: "rd", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitErr: errors.New("also gone")}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary), newRepairLane(t, "rd", alt)}
	notify := &fakeArrNotifier{}
	s := newRepairTestServer(t, st, lanes, []string{"torbox", "rd"}, notify)

	s.runRepairChain(ctx, item.ID, hash)

	fresh, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if fresh.State != store.StateFailed {
		t.Fatalf("state = %q, want StateFailed", fresh.State)
	}
	if fresh.Provider == nil || *fresh.Provider != "torbox" {
		t.Fatalf("provider = %v, want unchanged (torbox) — repair never succeeded", fresh.Provider)
	}
	blacklisted, blerr := st.IsBlacklisted(ctx, hash)
	if blerr != nil {
		t.Fatalf("IsBlacklisted: %v", blerr)
	}
	if !blacklisted {
		t.Fatal("identity should be blacklisted after repair exhaustion")
	}
	if notify.calls != 1 {
		t.Fatalf("arr notify calls = %d, want 1", notify.calls)
	}
}

func TestRunRepairChainAbstainsWhenItemNotReady(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	hash := "4444444444444444444444444444444444444444"
	item := mustCreateTorrentItem(t, st, "item-d", hash, "torbox")
	// Move it on before repair runs (simulates a race with another path).
	if err := st.UpdateItemState(ctx, item, store.StateFailed, "unrelated failure"); err != nil {
		t.Fatalf("UpdateItemState: %v", err)
	}

	primary := &altFakeProvider{name: "torbox"}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary)}
	notify := &fakeArrNotifier{}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, notify)

	s.runRepairChain(ctx, item.ID, hash)

	if primary.submitCalls != 0 {
		t.Fatalf("submitCalls = %d, want 0 (must abstain on non-Ready item)", primary.submitCalls)
	}
	if notify.calls != 0 {
		t.Fatalf("arr notify calls = %d, want 0", notify.calls)
	}
}

func TestRunRepairChainAbstainsWhenItemGone(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	primary := &altFakeProvider{name: "torbox"}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary)}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, nil)

	// No item was ever created with this ID.
	s.runRepairChain(ctx, "does-not-exist", "5555555555555555555555555555555555555555")

	if primary.submitCalls != 0 {
		t.Fatalf("submitCalls = %d, want 0", primary.submitCalls)
	}
}

// TestTriggerRepairAsyncGatesOnConfigAndSourceType proves the cheap
// synchronous gate: non-torrent items and a disabled row never spawn any
// background work (no store/provider calls at all).
func TestTriggerRepairAsyncGatesOnConfigAndSourceType(t *testing.T) {
	st := newRepairTestStore(t)
	primary := &altFakeProvider{name: "torbox"}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary)}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, nil)

	hash := "6666666666666666666666666666666666666666"
	httpItem := &store.Item{ID: "http-item", SourceType: store.SourceTypeHTTP, State: store.StateReady, InfoHash: &hash}
	s.TriggerRepairAsync(httpItem)

	s.cfg.Governor.RepairEnabled = false
	torrentItem := &store.Item{ID: "torrent-item", SourceType: store.SourceTypeTorrent, State: store.StateReady, InfoHash: &hash}
	s.TriggerRepairAsync(torrentItem)

	s.wg.Wait() // no goroutine should have been spawned in either case
	if primary.submitCalls != 0 {
		t.Fatalf("submitCalls = %d, want 0 (gated before any provider call)", primary.submitCalls)
	}
}

// TestTriggerRepairAsyncSingleFlightUnderConcurrentTriggers proves that
// firing TriggerRepairAsync many times concurrently for the same identity
// results in at most one actual repair execution (bounded goroutines,
// cooldown-gated) — run with -race.
func TestTriggerRepairAsyncSingleFlightUnderConcurrentTriggers(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	hash := "7777777777777777777777777777777777777777"
	item := mustCreateTorrentItem(t, st, "item-e", hash, "torbox")

	var submitStarted int32
	blockCh := make(chan struct{})
	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitResp: &provider.CreateTaskResponse{RemoteID: "r"}}
	// Wrap Submit via an adapter provider that blocks until released, so
	// concurrent triggers race against a genuinely in-flight attempt.
	blocking := &blockingSubmitProvider{altFakeProvider: primary, started: &submitStarted, block: blockCh}
	lanes := []ProviderLane{newRepairLane(t, "torbox", blocking)}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, nil)

	for i := 0; i < 20; i++ {
		s.TriggerRepairAsync(item)
	}
	// Let the single admitted goroutine reach the blocking Submit call.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&submitStarted) == 0 {
		select {
		case <-deadline:
			t.Fatal("no repair attempt ever started")
		case <-time.After(time.Millisecond):
		}
	}
	// Fire more concurrent triggers while the first is genuinely in-flight.
	for i := 0; i < 20; i++ {
		s.TriggerRepairAsync(item)
	}
	close(blockCh)
	s.wg.Wait()

	if got := blocking.altFakeProvider.submitCalls; got != 1 {
		t.Fatalf("submitCalls = %d, want exactly 1 across 40 concurrent triggers", got)
	}
	fresh, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if fresh.State != store.StateResolving {
		t.Fatalf("state = %q, want StateResolving", fresh.State)
	}
}

// blockingSubmitProvider wraps altFakeProvider so Submit blocks on a channel
// until released, letting a test observe genuine in-flight concurrency.
type blockingSubmitProvider struct {
	*altFakeProvider
	started *int32
	block   chan struct{}
}

func (b *blockingSubmitProvider) Submit(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	atomic.AddInt32(b.started, 1)
	select {
	case <-b.block:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.altFakeProvider.Submit(ctx, item, opts)
}

// TestRunRepairChainRespectsCancellation proves a cancelled context aborts
// mid-repair without panicking or falsely healing/blacklisting the item —
// DG-07's cancellation-isolation requirement.
func TestRunRepairChainRespectsCancellation(t *testing.T) {
	st := newRepairTestStore(t)
	hash := "8888888888888888888888888888888888888888"
	item := mustCreateTorrentItem(t, st, "item-f", hash, "torbox")

	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitResp: &provider.CreateTaskResponse{RemoteID: "r"}}
	blocking := &blockingSubmitProvider{altFakeProvider: primary, started: new(int32), block: make(chan struct{})}
	lanes := []ProviderLane{newRepairLane(t, "torbox", blocking)}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the attempt even starts

	s.runRepairChain(ctx, item.ID, hash)

	fresh, err := s.store.GetItemByID(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if fresh.State != store.StateReady {
		t.Fatalf("state = %q, want unchanged StateReady after a cancelled repair attempt", fresh.State)
	}
}
