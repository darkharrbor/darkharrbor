package cachegate

import (
	"context"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/governor"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type uncachedProvider struct {
	provider.Provider
}

func (uncachedProvider) CheckCached(context.Context, *store.Item) (*provider.CheckCachedResult, error) {
	return &provider.CheckCachedResult{Cached: false}, nil
}

func (uncachedProvider) SlotStatus(context.Context) (*provider.SlotStatus, error) {
	return &provider.SlotStatus{AllowedActiveSlots: 10, ActiveCount: 10}, nil
}

func fullGovernor(p provider.Provider) *governor.Governor {
	g := governor.New(nil, 20, 15)
	g.SetProvider(p)
	return g
}

func TestCheckDoesNotApplyTorrentGovernorToNZB(t *testing.T) {
	prov := uncachedProvider{}
	gate := New(prov, fullGovernor(prov), false)
	result, err := gate.Check(context.Background(), &store.Item{SourceType: store.SourceTypeNZB})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AllowUncached {
		t.Fatal("uncached NZB was not admitted independently of the torrent governor")
	}
}

func TestCheckAppliesProviderGovernorToUncachedTorrent(t *testing.T) {
	prov := uncachedProvider{}
	gate := New(prov, fullGovernor(prov), false)
	if _, err := gate.Check(context.Background(), &store.Item{SourceType: store.SourceTypeTorrent}); err == nil {
		t.Fatal("uncached torrent bypassed the full provider slot governor")
	}
}

// heldProvider blocks like a client waiting out a 429 Retry-After hold.
type heldProvider struct{ provider.Provider }

func (heldProvider) CheckCached(ctx context.Context, _ *store.Item) (*provider.CheckCachedResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCheckBoundsHeldProviderAndMarksRetryable(t *testing.T) {
	old := CheckTimeout
	CheckTimeout = 50 * time.Millisecond
	defer func() { CheckTimeout = old }()
	gate := New(heldProvider{}, nil, false)
	start := time.Now()
	_, err := gate.Check(context.Background(), &store.Item{SourceType: store.SourceTypeTorrent})
	if err == nil || !provider.IsRetryable(err) {
		t.Fatalf("err=%v, want retryable timeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("check took %v, want bounded", time.Since(start))
	}
}
