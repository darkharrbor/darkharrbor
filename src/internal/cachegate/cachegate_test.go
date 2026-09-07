package cachegate

import (
	"context"
	"testing"

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
