package compat

import (
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

func f9Item(st store.ItemState, progress float64, eta int64, size int64) *store.Item {
	it := &store.Item{State: st, TotalSize: size}
	it.Metadata.DownloadProgress = progress
	it.Metadata.DownloadETASeconds = eta
	return it
}

// F-9: both emulators must surface live percent / remaining / time-left while
// an uncached grab materializes, and never claim completion before StateReady.
func TestF9QueueTelemetry(t *testing.T) {
	// qBit eta: only while resolving with progress.
	if got := projectQBitEta(f9Item(store.StateResolving, 0.4, 900, 0)); got != 900 {
		t.Errorf("qbit eta resolving = %d, want 900", got)
	}
	if got := projectQBitEta(f9Item(store.StateResolving, 0, 900, 0)); got != 0 {
		t.Errorf("qbit eta no-progress = %d, want 0", got)
	}
	if got := projectQBitEta(f9Item(store.StateReady, 1.0, 900, 0)); got != 0 {
		t.Errorf("qbit eta ready = %d, want 0", got)
	}

	// qBit progress cap: provider 1.0 while resolving reports 0.99.
	if got := projectQBitProgress(f9Item(store.StateResolving, 1.0, 0, 0)); got != 0.99 {
		t.Errorf("qbit progress finalizing = %v, want 0.99", got)
	}

	// Honest downloaded bytes.
	if got := qbitDownloadedBytes(f9Item(store.StateResolving, 0.34, 0, 1000), 0.34); got != 340 {
		t.Errorf("downloaded bytes = %d, want 340", got)
	}
	if got := qbitDownloadedBytes(f9Item(store.StateReady, 1.0, 0, 1000), 1.0); got != 1000 {
		t.Errorf("downloaded bytes ready = %d, want 1000", got)
	}

	// SAB percent.
	for _, c := range []struct {
		name string
		item *store.Item
		want int
	}{
		{"ready", f9Item(store.StateReady, 1.0, 0, 0), 100},
		{"removed", f9Item(store.StateRemoved, 1.0, 0, 0), 100},
		{"resolving mid", f9Item(store.StateResolving, 0.34, 0, 0), 34},
		{"resolving finalizing", f9Item(store.StateResolving, 1.0, 0, 0), 99},
		{"resolving tiny", f9Item(store.StateResolving, 0.001, 0, 0), 1},
		{"resolving none", f9Item(store.StateResolving, 0, 0, 0), 0},
		{"accepted", f9Item(store.StateAccepted, 0, 0, 0), 0},
	} {
		if got := projectSABPercent(c.item); got != c.want {
			t.Errorf("sab percent %s = %d, want %d", c.name, got, c.want)
		}
	}

	// SAB remaining bytes.
	if got := sabRemainingBytes(f9Item(store.StateResolving, 0.25, 0, 1000)); got != 750 {
		t.Errorf("sab remaining mid = %d, want 750", got)
	}
	if got := sabRemainingBytes(f9Item(store.StateReady, 1.0, 0, 1000)); got != 0 {
		t.Errorf("sab remaining ready = %d, want 0", got)
	}
	if got := sabRemainingBytes(f9Item(store.StateAccepted, 0, 0, 1000)); got != 1000 {
		t.Errorf("sab remaining accepted = %d, want 1000", got)
	}

	// SAB timeleft formatting.
	if got := sabTimeLeft(f9Item(store.StateResolving, 0.5, 3725, 0)); got != "1:02:05" {
		t.Errorf("sab timeleft = %q, want 1:02:05", got)
	}
	if got := sabTimeLeft(f9Item(store.StateResolving, 0.5, 0, 0)); got != "0:00:00" {
		t.Errorf("sab timeleft no-eta = %q, want 0:00:00", got)
	}
	if got := sabTimeLeft(f9Item(store.StateReady, 1.0, 500, 0)); got != "0:00:00" {
		t.Errorf("sab timeleft ready = %q, want 0:00:00", got)
	}
}
