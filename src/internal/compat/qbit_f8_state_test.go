package compat

import (
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

// F-8: resolving items with live TorBox progress must present as
// "downloading" so the arrs show real progress instead of a frozen "Queued"
// during long uncached materializations. All other mappings unchanged.
func TestProjectQBitState_F8ResolvingProgress(t *testing.T) {
	mk := func(st store.ItemState, progress float64) *store.Item {
		it := &store.Item{State: st}
		it.Metadata.DownloadProgress = progress
		return it
	}
	cases := []struct {
		name string
		item *store.Item
		want string
	}{
		{"accepted", mk(store.StateAccepted, 0), "queuedDL"},
		{"resolving no progress", mk(store.StateResolving, 0), "queuedDL"},
		{"resolving mid download", mk(store.StateResolving, 0.34), "downloading"},
		{"resolving progress complete (finalizing)", mk(store.StateResolving, 1.0), "downloading"},
		{"ready", mk(store.StateReady, 1.0), "pausedUP"},
		{"removed", mk(store.StateRemoved, 1.0), "pausedUP"},
		{"failed", mk(store.StateFailed, 0), "error"},
	}
	for _, c := range cases {
		if got := projectQBitState(c.item); got != c.want {
			t.Errorf("%s: projectQBitState = %q, want %q", c.name, got, c.want)
		}
	}
}
