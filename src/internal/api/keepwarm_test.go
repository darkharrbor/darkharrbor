package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// --- ProviderExpiryHorizon ---

func TestProviderExpiryHorizonKnownProvider(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	name := "torbox"
	item := &store.Item{Provider: &name, CreatedAt: created}

	horizon, provName, days, ok := ProviderExpiryHorizon(item)
	if !ok {
		t.Fatal("expected ok=true for known provider")
	}
	if provName != "torbox" || days != 30 {
		t.Fatalf("provName=%q days=%d, want torbox/30", provName, days)
	}
	want := created.AddDate(0, 0, 30)
	if !horizon.Equal(want) {
		t.Fatalf("horizon = %v, want %v", horizon, want)
	}
}

func TestProviderExpiryHorizonAbstainsUnknownProvider(t *testing.T) {
	name := "realdebrid"
	item := &store.Item{Provider: &name, CreatedAt: time.Now()}
	_, provName, _, ok := ProviderExpiryHorizon(item)
	if ok {
		t.Fatal("expected ok=false for an unlisted provider (never guessed)")
	}
	if provName != "realdebrid" {
		t.Fatalf("provName = %q, want realdebrid even when abstaining", provName)
	}
}

func TestProviderExpiryHorizonAbstainsNilOrEmptyProvider(t *testing.T) {
	if _, _, _, ok := ProviderExpiryHorizon(nil); ok {
		t.Fatal("nil item must abstain")
	}
	if _, _, _, ok := ProviderExpiryHorizon(&store.Item{}); ok {
		t.Fatal("item with nil Provider must abstain")
	}
	empty := ""
	if _, _, _, ok := ProviderExpiryHorizon(&store.Item{Provider: &empty}); ok {
		t.Fatal("item with empty Provider must abstain")
	}
}

func TestProviderExpiryHorizonAbstainsZeroCreatedAt(t *testing.T) {
	name := "torbox"
	item := &store.Item{Provider: &name} // CreatedAt zero value
	if _, _, _, ok := ProviderExpiryHorizon(item); ok {
		t.Fatal("zero CreatedAt must abstain, never guessed")
	}
}

// --- RunKeepWarmCycle / maybeKeepWarmReAdd ---

func mustCreateKeepWarmItem(t *testing.T, st *store.Store, id, providerName string, createdAt, lastPlayedAt time.Time) *store.Item {
	t.Helper()
	hash := "aaaa0000000000000000000000000000000000"
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
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
		LastPlayedAt:  &lastPlayedAt,
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return item
}

func TestRunKeepWarmCycleReAddsNearExpiryItem(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	now := time.Now().UTC()
	// Grabbed 26 days ago on a 30-day assumed horizon => 4 days out,
	// inside the 5-day keepWarmLeadDays buffer => due.
	created := now.AddDate(0, 0, -26)
	played := now.Add(-1 * time.Hour)
	item := mustCreateKeepWarmItem(t, st, "kw-due", "torbox", created, played)

	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitResp: &provider.CreateTaskResponse{RemoteID: "r-kw"}}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary)}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, nil)
	s.cfg.Governor.TorrentKeepWarmDays = 20

	s.RunKeepWarmCycle(ctx, s.cfg.Governor.TorrentKeepWarmDays)

	if primary.submitCalls != 1 {
		t.Fatalf("submitCalls = %d, want 1 (item is due)", primary.submitCalls)
	}
	fresh, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if fresh.State != store.StateResolving {
		t.Fatalf("state = %q, want StateResolving after re-add", fresh.State)
	}
}

func TestRunKeepWarmCycleSkipsNotYetNearExpiry(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	now := time.Now().UTC()
	// Grabbed just 1 day ago: horizon ~29 days out, well outside the
	// 5-day lead buffer -- must not re-add yet.
	created := now.AddDate(0, 0, -1)
	played := now.Add(-1 * time.Hour)
	mustCreateKeepWarmItem(t, st, "kw-not-due", "torbox", created, played)

	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitResp: &provider.CreateTaskResponse{RemoteID: "r"}}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary)}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, nil)

	s.RunKeepWarmCycle(ctx, 20)

	if primary.submitCalls != 0 {
		t.Fatalf("submitCalls = %d, want 0 (not yet near expiry)", primary.submitCalls)
	}
}

func TestRunKeepWarmCycleSkipsUnplayedItem(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	now := time.Now().UTC()
	created := now.AddDate(0, 0, -26)
	// Played 25 days ago -- outside the 20-day "recent play" window, so
	// ListKeepWarmCandidates itself must exclude it.
	played := now.AddDate(0, 0, -25)
	mustCreateKeepWarmItem(t, st, "kw-stale-play", "torbox", created, played)

	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitResp: &provider.CreateTaskResponse{RemoteID: "r"}}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary)}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, nil)

	s.RunKeepWarmCycle(ctx, 20)

	if primary.submitCalls != 0 {
		t.Fatalf("submitCalls = %d, want 0 (played outside recency window)", primary.submitCalls)
	}
}

func TestRunKeepWarmCycleDisabledWhenDaysZero(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	now := time.Now().UTC()
	created := now.AddDate(0, 0, -26)
	played := now.Add(-1 * time.Hour)
	mustCreateKeepWarmItem(t, st, "kw-disabled", "torbox", created, played)

	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitResp: &provider.CreateTaskResponse{RemoteID: "r"}}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary)}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, nil)

	s.RunKeepWarmCycle(ctx, 0)

	if primary.submitCalls != 0 {
		t.Fatalf("submitCalls = %d, want 0 (days<=0 disables the cycle)", primary.submitCalls)
	}
}

func TestRunKeepWarmCycleAbstainsUnknownProviderLane(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	now := time.Now().UTC()
	created := now.AddDate(0, 0, -26)
	played := now.Add(-1 * time.Hour)
	mustCreateKeepWarmItem(t, st, "kw-no-lane", "torbox", created, played)

	// No lanes configured for "torbox" at all.
	s := newRepairTestServer(t, st, nil, nil, nil)

	// Must not panic and must not touch the item's state.
	s.RunKeepWarmCycle(ctx, 20)

	fresh, err := st.GetItemByID(ctx, "kw-no-lane")
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if fresh.State != store.StateReady {
		t.Fatalf("state = %q, want unchanged StateReady", fresh.State)
	}
}

func TestRunKeepWarmCycleFailedReAddLeavesItemReadyForRetry(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	now := time.Now().UTC()
	created := now.AddDate(0, 0, -26)
	played := now.Add(-1 * time.Hour)
	item := mustCreateKeepWarmItem(t, st, "kw-fail", "torbox", created, played)

	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitErr: errors.New("submit failed")}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary)}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, nil)

	s.RunKeepWarmCycle(ctx, 20)

	if primary.submitCalls != 1 {
		t.Fatalf("submitCalls = %d, want 1 (attempted)", primary.submitCalls)
	}
	fresh, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	// A failed keep-warm re-add must NOT blacklist or fail the item --
	// only TS-4.1's own full repair chain (triggered separately, on a
	// genuine break) may do that.
	if fresh.State != store.StateReady {
		t.Fatalf("state = %q, want unchanged StateReady so a future tick can retry", fresh.State)
	}
	blacklisted, err := st.IsBlacklisted(ctx, *item.InfoHash)
	if err != nil {
		t.Fatalf("IsBlacklisted: %v", err)
	}
	if blacklisted {
		t.Fatal("a failed keep-warm attempt must never blacklist the item")
	}
}

func TestRunKeepWarmCycleBoundedPerTick(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	now := time.Now().UTC()
	played := now.Add(-1 * time.Hour)
	for i := 0; i < 5; i++ {
		created := now.AddDate(0, 0, -26).Add(time.Duration(i) * time.Minute)
		id := "kw-bulk-" + string(rune('a'+i))
		item := &store.Item{
			ID: id, PublicID: id, SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit,
			Category: "movies", State: store.StateReady, SubmissionKey: id, DisplayName: id,
			InfoHash:  strPtr("bbbb" + string(rune('0'+i)) + "000000000000000000000000000000000"),
			Provider:  strPtr("torbox"),
			CreatedAt: created, UpdatedAt: created,
			LastPlayedAt: &played,
		}
		if err := st.CreateItem(ctx, item); err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
	}

	primary := &altFakeProvider{name: "torbox", checkCachedResult: &provider.CheckCachedResult{Cached: true}, submitResp: &provider.CreateTaskResponse{RemoteID: "r"}}
	lanes := []ProviderLane{newRepairLane(t, "torbox", primary)}
	s := newRepairTestServer(t, st, lanes, []string{"torbox"}, nil)

	s.RunKeepWarmCycle(ctx, 20)

	if primary.submitCalls != keepWarmCandidatesPerTick {
		t.Fatalf("submitCalls = %d, want exactly %d (bounded per tick)", primary.submitCalls, keepWarmCandidatesPerTick)
	}
}

func strPtr(s string) *string { return &s }
