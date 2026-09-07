package governor

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// fakeSlotSource is a minimal provider.SlotSource stub for exercising the
// R2 slot-based gating without a live TorBox account.
type fakeSlotSource struct {
	status *provider.SlotStatus
	err    error
}

func (f *fakeSlotSource) SlotStatus(ctx context.Context) (*provider.SlotStatus, error) {
	return f.status, f.err
}

// fakeProvider satisfies provider.Provider just enough to be passed to
// Governor.SetProvider; only the SlotSource type assertion matters here.
type fakeProvider struct {
	fakeSlotSource
}

func (f *fakeProvider) Name() string                        { return "fake" }
func (f *fakeProvider) Governor() provider.Policy           { return nil }
func (f *fakeProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (f *fakeProvider) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	return nil, nil
}
func (f *fakeProvider) Submit(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	return nil, nil
}
func (f *fakeProvider) Poll(ctx context.Context, item *store.Item) (*provider.TaskStatus, error) {
	return nil, nil
}
func (f *fakeProvider) RequestDownloadURL(ctx context.Context, item *store.Item, fileID string) (string, error) {
	return "", nil
}
func (f *fakeProvider) Remove(ctx context.Context, item *store.Item) error { return nil }

var _ provider.Provider = (*fakeProvider)(nil)
var _ provider.SlotSource = (*fakeProvider)(nil)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "governor_test.db")
	db, err := store.Open(context.Background(), dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return store.New(db)
}

// createTestItem inserts a minimal item row so RecordUncachedAdd's
// governor_log FK constraint on items(id) is satisfied.
func createTestItem(t *testing.T, st *store.Store, id string) {
	t.Helper()
	now := time.Now().UTC()
	item := &store.Item{
		ID:            id,
		PublicID:      id,
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      "darkharrbor",
		State:         store.StateResolving,
		SubmissionKey: id,
		DisplayName:   id,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("createTestItem(%s): %v", id, err)
	}
}

func TestAllow_NoProvider_LegacyRollingWindow(t *testing.T) {
	st := newTestStore(t)
	g := New(st, 2, 15) // budget=2 over 15 days, no provider wired

	ctx := context.Background()
	if err := g.Allow(ctx); err != nil {
		t.Fatalf("Allow #1: unexpected error: %v", err)
	}
	createTestItem(t, st, "item-1")
	if err := g.Record(ctx, "item-1"); err != nil {
		t.Fatalf("Record #1: %v", err)
	}
	if err := g.Allow(ctx); err != nil {
		t.Fatalf("Allow #2: unexpected error: %v", err)
	}
	createTestItem(t, st, "item-2")
	if err := g.Record(ctx, "item-2"); err != nil {
		t.Fatalf("Record #2: %v", err)
	}
	if err := g.Allow(ctx); err == nil {
		t.Fatal("Allow #3: expected budget-exhausted error, got nil")
	}
}

func TestAllow_SlotHeadroom_Available(t *testing.T) {
	st := newTestStore(t)
	g := New(st, 20, 15)
	g.SetProvider(&fakeProvider{fakeSlotSource{status: &provider.SlotStatus{
		AllowedActiveSlots: 10,
		ActiveCount:        3,
	}}})

	if err := g.Allow(context.Background()); err != nil {
		t.Fatalf("Allow: expected headroom to permit, got error: %v", err)
	}
}

func TestAllow_SlotHeadroom_Exhausted(t *testing.T) {
	st := newTestStore(t)
	g := New(st, 20, 15)
	g.SetProvider(&fakeProvider{fakeSlotSource{status: &provider.SlotStatus{
		AllowedActiveSlots: 10,
		ActiveCount:        10,
	}}})

	if err := g.Allow(context.Background()); err == nil {
		t.Fatal("Allow: expected no-headroom error, got nil")
	}
}

func TestAllow_FreeTierCooldownActive_Blocks(t *testing.T) {
	st := newTestStore(t)
	g := New(st, 20, 15)
	g.SetProvider(&fakeProvider{fakeSlotSource{status: &provider.SlotStatus{
		AllowedActiveSlots: 10,
		ActiveCount:        0,
		FreeTier:           true,
		CooldownUntil:      time.Now().Add(1 * time.Hour),
	}}})

	if err := g.Allow(context.Background()); err == nil {
		t.Fatal("Allow: expected cooldown error, got nil")
	}
}

func TestAllow_PaidTierCooldownActive_DoesNotBlock(t *testing.T) {
	st := newTestStore(t)
	g := New(st, 20, 15)
	g.SetProvider(&fakeProvider{fakeSlotSource{status: &provider.SlotStatus{
		AllowedActiveSlots: 10,
		ActiveCount:        0,
		FreeTier:           false,
		CooldownUntil:      time.Now().Add(1 * time.Hour),
	}}})

	if err := g.Allow(context.Background()); err != nil {
		t.Fatalf("Allow: paid tier should ignore advisory cooldown, got error: %v", err)
	}
}

func TestAllow_CooldownExpired_DoesNotBlock(t *testing.T) {
	st := newTestStore(t)
	g := New(st, 20, 15)
	g.SetProvider(&fakeProvider{fakeSlotSource{status: &provider.SlotStatus{
		AllowedActiveSlots: 10,
		ActiveCount:        0,
		CooldownUntil:      time.Now().Add(-1 * time.Hour), // already cleared
	}}})

	if err := g.Allow(context.Background()); err != nil {
		t.Fatalf("Allow: expected pass (cooldown expired), got error: %v", err)
	}
}

func TestAllow_FreeTier_EnforcesDailyBudgetOnTopOfSlots(t *testing.T) {
	st := newTestStore(t)
	g := New(st, 10, 15) // monthly budget 10 (unused by daily check)
	g.SetProvider(&fakeProvider{fakeSlotSource{status: &provider.SlotStatus{
		AllowedActiveSlots: 1, // Free plan slot ceiling
		ActiveCount:        0,
		FreeTier:           true,
	}}})

	ctx := context.Background()
	if err := g.Allow(ctx); err != nil {
		t.Fatalf("Allow #1: expected pass, got: %v", err)
	}
	createTestItem(t, st, "item-1")
	if err := g.Record(ctx, "item-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Free-tier daily budget is 1/24h — the second uncached add today must
	// be rejected even though slot headroom (1 - 0 active) still looks free,
	// because ActiveCount doesn't reflect grabs not yet submitted to TorBox.
	if err := g.Allow(ctx); err == nil {
		t.Fatal("Allow #2: expected daily-budget error for Free tier, got nil")
	}
}

func TestAllow_PaidTier_IgnoresTimeWindowedBudget(t *testing.T) {
	st := newTestStore(t)
	g := New(st, 1, 15) // monthly budget of 1 — would block a Free-tier account fast
	g.SetProvider(&fakeProvider{fakeSlotSource{status: &provider.SlotStatus{
		AllowedActiveSlots: 10,
		ActiveCount:        0,
		FreeTier:           false,
	}}})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := g.Allow(ctx); err != nil {
			t.Fatalf("Allow #%d: paid tier should only gate on slots, got: %v", i, err)
		}
		id := fmt.Sprintf("item-paid-%d", i)
		createTestItem(t, st, id)
		if err := g.Record(ctx, id); err != nil {
			t.Fatalf("Record #%d: %v", i, err)
		}
	}
}
