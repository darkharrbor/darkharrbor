package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/cachegate"
	"github.com/darkharrbor/darkharrbor/internal/governor"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// fullSlotsProvider reports every torrent uncached and every slot in use.
type fullSlotsProvider struct {
	submitErr error
	submits   int
}

func (*fullSlotsProvider) Name() string                        { return "torbox" }
func (*fullSlotsProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (*fullSlotsProvider) CheckCached(context.Context, *store.Item) (*provider.CheckCachedResult, error) {
	return &provider.CheckCachedResult{Cached: false}, nil
}
func (f *fullSlotsProvider) Submit(context.Context, *store.Item, provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	f.submits++
	return nil, f.submitErr
}
func (*fullSlotsProvider) Poll(context.Context, *store.Item) (*provider.TaskStatus, error) {
	return nil, errors.New("unused")
}
func (*fullSlotsProvider) RequestDownloadURL(context.Context, *store.Item, string) (string, error) {
	return "", errors.New("unused")
}
func (*fullSlotsProvider) Remove(context.Context, *store.Item) error { return nil }
func (*fullSlotsProvider) SlotStatus(context.Context) (*provider.SlotStatus, error) {
	return &provider.SlotStatus{AllowedActiveSlots: 10, ActiveCount: 10}, nil
}

func newSubmitTestItem(t *testing.T) (*store.Store, *store.Item) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "submit.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := store.New(db)
	now := time.Now().UTC()
	h := "00000000000000000000000000000000deadbeef"
	it := &store.Item{ID: "i1", PublicID: "p1", SourceType: store.SourceTypeTorrent, ClientKind: "qbit", Category: "tv",
		State: store.StateAccepted, SubmissionKey: "k1", DisplayName: "x", CreatedAt: now, UpdatedAt: now, InfoHash: &h}
	if err := st.CreateItem(ctx, it); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if err := st.UpdateItemState(ctx, it, store.StateResolving, ""); err != nil {
		t.Fatalf("UpdateItemState: %v", err)
	}
	return st, it
}

func TestSubmitItemDefersWhenSlotsFull(t *testing.T) {
	st, item := newSubmitTestItem(t)
	prov := &fullSlotsProvider{}
	gov := governor.New(st, 20, 15)
	gov.SetProvider(prov)
	lanes := []providerLane{{prov: prov, gate: cachegate.New(prov, gov, false), gov: gov}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for i := 1; i <= maxSubmitDeferrals; i++ {
		submitItem(context.Background(), log, lanes, st, item)
		if item.State != store.StateResolving || item.RetryCount != i {
			t.Fatalf("attempt %d: state=%s retry=%d, want resolving/%d", i, item.State, item.RetryCount, i)
		}
		if item.ErrorMessage == nil || !strings.HasPrefix(*item.ErrorMessage, "waiting: ") {
			t.Fatalf("attempt %d: message %v, want waiting:", i, item.ErrorMessage)
		}
	}
	submitItem(context.Background(), log, lanes, st, item)
	if item.State != store.StateFailed || !strings.Contains(*item.ErrorMessage, "no active slots") {
		t.Fatalf("after bound: state=%s msg=%v, want failed with reason", item.State, *item.ErrorMessage)
	}
	if prov.submits != 0 {
		t.Fatalf("createtask called %d times while slots were full", prov.submits)
	}
}

func TestSubmitItemKeepsRetryableCreateTaskScheduled(t *testing.T) {
	st, item := newSubmitTestItem(t)
	prov := &fullSlotsProvider{submitErr: provider.MarkRetryable(errors.New("torbox 503"))}
	// Cached check: the gate passes and createtask returns a retryable error.
	cachedProv := &cachedFullProvider{fullSlotsProvider: prov}
	lanes := []providerLane{{prov: cachedProv, gate: cachegate.New(cachedProv, nil, false)}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	submitItem(context.Background(), log, lanes, st, item)
	if item.State != store.StateResolving || item.RetryCount != 1 || item.NextRunAt == nil {
		t.Fatalf("state=%s retry=%d next=%v, want resolving/1/scheduled", item.State, item.RetryCount, item.NextRunAt)
	}
}

type cachedFullProvider struct{ *fullSlotsProvider }

func (*cachedFullProvider) CheckCached(context.Context, *store.Item) (*provider.CheckCachedResult, error) {
	return &provider.CheckCachedResult{Cached: true}, nil
}
