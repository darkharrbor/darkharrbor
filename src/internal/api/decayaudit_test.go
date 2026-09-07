package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/cachegate"
	"github.com/darkharrbor/darkharrbor/internal/governor"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// newDecayAuditRepairLane mirrors newRepairLane (repair_test.go) but wires
// the governor to a real store instead of nil: unlike every existing TS-4.1
// repair test, this row's own decay-audit-triggers-repair test genuinely
// exercises cachegate.Check's NOT-cached branch (a real decayed item is, by
// definition, no longer cached), which calls through to governor.Allow ->
// the store-backed rolling-window budget check -- a nil store there would
// panic. Production always constructs its governor against the real store,
// so this is a test-harness correction, not new production behavior.
func newDecayAuditRepairLane(t *testing.T, st *store.Store, name string, prov provider.Provider) ProviderLane {
	t.Helper()
	gov := governor.New(st, 1000, 30)
	return ProviderLane{Prov: prov, Gate: cachegate.New(prov, gov, false)}
}

func mustCreateDecayAuditItem(t *testing.T, st *store.Store, id, hash, providerName string) *store.Item {
	t.Helper()
	now := time.Now().UTC()
	item := &store.Item{
		ID:            id,
		PublicID:      id,
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      "movies",
		State:         store.StateReady,
		SubmissionKey: id,
		DisplayName:   id,
		InfoHash:      &hash,
		Provider:      &providerName,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return item
}

// newDecayAuditTestServer builds on newRepairTestServer (repair_test.go),
// additionally wiring allProviders (pickDebridProvider's lookup table) and
// a fresh decayAuditState, neither of which newRepairTestServer sets since
// TS-4.1's own tests never needed them.
func newDecayAuditTestServer(t *testing.T, st *store.Store, lanes []ProviderLane, providerOrder []string, allProviders map[string]provider.Provider) *Server {
	t.Helper()
	s := newRepairTestServer(t, st, lanes, providerOrder, nil)
	s.allProviders = allProviders
	s.decayAuditState = newDecayAuditState()
	return s
}

// recordingCheckCachedProvider wraps altFakeProvider to additionally record
// which item IDs CheckCached was actually called for, so rotation-across-
// ticks tests can assert real per-item coverage rather than just a call
// count.
type recordingCheckCachedProvider struct {
	*altFakeProvider
	seen []string
}

func (r *recordingCheckCachedProvider) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	r.seen = append(r.seen, item.ID)
	return r.altFakeProvider.CheckCached(ctx, item)
}

func TestRunDecayAuditCycleTriggersRepairOnDecay(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	hash := "d0d0000000000000000000000000000000000d"
	item := mustCreateDecayAuditItem(t, st, "da-decayed", hash, "torbox")

	fake := &altFakeProvider{
		name:              "torbox",
		caps:              provider.Capabilities{CacheOracle: true},
		checkCachedResult: &provider.CheckCachedResult{Cached: false},
		submitResp:        &provider.CreateTaskResponse{RemoteID: "r-repair"},
	}
	lanes := []ProviderLane{newDecayAuditRepairLane(t, st, "torbox", fake)}
	s := newDecayAuditTestServer(t, st, lanes, []string{"torbox"}, map[string]provider.Provider{"torbox": fake})

	result := s.RunDecayAuditCycle(ctx)
	s.wg.Wait()

	// 2 calls: one from the decay-audit sample itself, one from TS-4.1's
	// own repair chain re-running cachegate.Check before its re-add --
	// this row deliberately reuses that existing primitive rather than a
	// second presence-check mechanism, so a real re-confirm here is
	// correct, not double-counting a bug.
	if fake.checkCachedCalls != 2 {
		t.Fatalf("checkCachedCalls = %d, want 2 (audit sample + repair's own re-check)", fake.checkCachedCalls)
	}
	if result.Sampled != 1 || len(result.Decayed) != 1 || result.Decayed[0] != item.ID {
		t.Fatalf("unexpected result: %+v", result)
	}
	if fake.submitCalls != 1 {
		t.Fatalf("submitCalls = %d, want 1 (repair triggered via TS-4.1's existing chain)", fake.submitCalls)
	}
	fresh, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if fresh.State != store.StateResolving {
		t.Fatalf("state = %q, want StateResolving after repair heal", fresh.State)
	}
}

func TestRunDecayAuditCycleAbstainsWhenCached(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	hash := "cacacacacacacacacacacacacacacacacacaca"
	item := mustCreateDecayAuditItem(t, st, "da-cached", hash, "torbox")

	fake := &altFakeProvider{
		name:              "torbox",
		caps:              provider.Capabilities{CacheOracle: true},
		checkCachedResult: &provider.CheckCachedResult{Cached: true},
	}
	lanes := []ProviderLane{newRepairLane(t, "torbox", fake)}
	s := newDecayAuditTestServer(t, st, lanes, []string{"torbox"}, map[string]provider.Provider{"torbox": fake})

	result := s.RunDecayAuditCycle(ctx)
	s.wg.Wait()

	if fake.checkCachedCalls != 1 {
		t.Fatalf("checkCachedCalls = %d, want 1", fake.checkCachedCalls)
	}
	if result.Cached != 1 || len(result.Decayed) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if fake.submitCalls != 0 {
		t.Fatalf("submitCalls = %d, want 0 (still cached, no repair)", fake.submitCalls)
	}
	fresh, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if fresh.State != store.StateReady {
		t.Fatalf("state = %q, want unchanged StateReady", fresh.State)
	}
}

func TestRunDecayAuditCycleAbstainsNoCacheOracle(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	hash := "eeee0000000000000000000000000000000000"
	mustCreateDecayAuditItem(t, st, "da-no-oracle", hash, "realdebrid")

	fake := &altFakeProvider{name: "realdebrid", caps: provider.Capabilities{CacheOracle: false}}
	lanes := []ProviderLane{newRepairLane(t, "realdebrid", fake)}
	s := newDecayAuditTestServer(t, st, lanes, []string{"realdebrid"}, map[string]provider.Provider{"realdebrid": fake})

	result := s.RunDecayAuditCycle(ctx)
	s.wg.Wait()

	if fake.checkCachedCalls != 0 {
		t.Fatalf("checkCachedCalls = %d, want 0 (no cache oracle => abstain before ever calling)", fake.checkCachedCalls)
	}
	if result.Abstained != 1 || result.Sampled != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunDecayAuditCycleAbstainsOnCheckCachedError(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	hash := "ffff0000000000000000000000000000000000"
	mustCreateDecayAuditItem(t, st, "da-err", hash, "torbox")

	fake := &altFakeProvider{
		name:           "torbox",
		caps:           provider.Capabilities{CacheOracle: true},
		checkCachedErr: errors.New("transient upstream error"),
	}
	lanes := []ProviderLane{newRepairLane(t, "torbox", fake)}
	s := newDecayAuditTestServer(t, st, lanes, []string{"torbox"}, map[string]provider.Provider{"torbox": fake})

	result := s.RunDecayAuditCycle(ctx)
	s.wg.Wait()

	if fake.checkCachedCalls != 1 {
		t.Fatalf("checkCachedCalls = %d, want 1", fake.checkCachedCalls)
	}
	if result.Abstained != 1 || result.Sampled != 0 || len(result.Decayed) != 0 {
		t.Fatalf("unexpected result: %+v (a transient error must never be treated as decay evidence)", result)
	}
	if fake.submitCalls != 0 {
		t.Fatalf("submitCalls = %d, want 0", fake.submitCalls)
	}
}

func TestRunDecayAuditCycleAbstainsNoInfohash(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	now := time.Now().UTC()
	item := &store.Item{
		ID: "da-no-hash", PublicID: "da-no-hash", SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateReady, SubmissionKey: "da-no-hash", DisplayName: "da-no-hash",
		Provider: strPtr("torbox"), CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	fake := &altFakeProvider{name: "torbox", caps: provider.Capabilities{CacheOracle: true}, checkCachedResult: &provider.CheckCachedResult{Cached: false}}
	lanes := []ProviderLane{newRepairLane(t, "torbox", fake)}
	s := newDecayAuditTestServer(t, st, lanes, []string{"torbox"}, map[string]provider.Provider{"torbox": fake})

	result := s.RunDecayAuditCycle(ctx)
	s.wg.Wait()

	if fake.checkCachedCalls != 0 {
		t.Fatalf("checkCachedCalls = %d, want 0 (no infohash known => abstain, never guessed)", fake.checkCachedCalls)
	}
	if result.Abstained != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunDecayAuditCycleBoundedPerTick(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	for i := 0; i < decayAuditCandidatesPerTick+3; i++ {
		hash := fmt.Sprintf("%040d", i+1)
		mustCreateDecayAuditItem(t, st, fmt.Sprintf("da-bulk-%02d", i), hash, "torbox")
	}

	fake := &altFakeProvider{name: "torbox", caps: provider.Capabilities{CacheOracle: true}, checkCachedResult: &provider.CheckCachedResult{Cached: true}}
	lanes := []ProviderLane{newRepairLane(t, "torbox", fake)}
	s := newDecayAuditTestServer(t, st, lanes, []string{"torbox"}, map[string]provider.Provider{"torbox": fake})

	result := s.RunDecayAuditCycle(ctx)

	if fake.checkCachedCalls != decayAuditCandidatesPerTick {
		t.Fatalf("checkCachedCalls = %d, want exactly %d (bounded per tick)", fake.checkCachedCalls, decayAuditCandidatesPerTick)
	}
	if result.Sampled != decayAuditCandidatesPerTick {
		t.Fatalf("Sampled = %d, want %d", result.Sampled, decayAuditCandidatesPerTick)
	}
}

func TestRunDecayAuditCycleRotatesAcrossTicksAndWraps(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	total := decayAuditCandidatesPerTick + 2
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("da-rot-%02d", i)
		hash := fmt.Sprintf("%040d", i+1)
		mustCreateDecayAuditItem(t, st, id, hash, "torbox")
		ids = append(ids, id)
	}

	base := &altFakeProvider{name: "torbox", caps: provider.Capabilities{CacheOracle: true}, checkCachedResult: &provider.CheckCachedResult{Cached: true}}
	fake := &recordingCheckCachedProvider{altFakeProvider: base}
	lanes := []ProviderLane{newRepairLane(t, "torbox", fake)}
	s := newDecayAuditTestServer(t, st, lanes, []string{"torbox"}, map[string]provider.Provider{"torbox": fake})

	// Tick 1: covers the first decayAuditCandidatesPerTick items.
	s.RunDecayAuditCycle(ctx)
	if len(fake.seen) != decayAuditCandidatesPerTick {
		t.Fatalf("tick1 seen = %d, want %d", len(fake.seen), decayAuditCandidatesPerTick)
	}

	// Tick 2: covers the remaining items (a short page), then wraps the
	// cursor for the next tick.
	s.RunDecayAuditCycle(ctx)
	if len(fake.seen) != total {
		t.Fatalf("after tick2, seen = %d, want %d (full rotation)", len(fake.seen), total)
	}
	seenSet := make(map[string]bool, len(fake.seen))
	for _, id := range fake.seen {
		seenSet[id] = true
	}
	for _, id := range ids {
		if !seenSet[id] {
			t.Errorf("item %s never audited across two ticks", id)
		}
	}

	// Tick 3: cursor has wrapped, so this tick must restart from the
	// beginning rather than staying empty forever.
	s.RunDecayAuditCycle(ctx)
	if len(fake.seen) != total+decayAuditCandidatesPerTick {
		t.Fatalf("after tick3, seen = %d, want %d (wrapped rotation restarted)", len(fake.seen), total+decayAuditCandidatesPerTick)
	}
	if fake.seen[total] != ids[0] {
		t.Fatalf("wrapped tick did not restart from the first item: got %s want %s", fake.seen[total], ids[0])
	}
}

func TestRunDecayAuditCycleNoopOnUninitializedServer(t *testing.T) {
	s := &Server{}
	// Must not panic when store/decayAuditState are both nil.
	result := s.RunDecayAuditCycle(context.Background())
	if result.Sampled != 0 || result.Cached != 0 || len(result.Decayed) != 0 || result.Abstained != 0 {
		t.Fatalf("expected zero-value result, got %+v", result)
	}
}
