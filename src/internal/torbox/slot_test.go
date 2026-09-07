package torbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestCountActiveSlotsCountsOnlyUncachedTorrents(t *testing.T) {
	var requests atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/torrents/mylist" {
			t.Errorf("unexpected slot endpoint %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []map[string]any{
				{"download_state": "downloading"},
				{"download_state": "checking"},
				{"download_state": "stalled (no seeds)"},
				{"download_state": "metaDL"},
				{"download_state": "downloading", "download_present": true},
				{"download_state": "seeding"},
				{"download_state": "uploading"},
				{"download_state": "cached"},
				{"download_state": "queued"},
			},
		})
	})

	got, err := c.CountActiveSlots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Fatalf("CountActiveSlots() = %d, want 4", got)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("slot endpoint requests = %d, want 1 torrent-only request", got)
	}
}

func TestCountActiveSlotsMalformedListFailsClosed(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":"not-a-list"}`))
	})
	if _, err := c.CountActiveSlots(context.Background()); err == nil {
		t.Fatal("malformed active-torrent list was accepted")
	}
}

func TestCountActiveSlotsCancellation(t *testing.T) {
	var requests atomic.Int32
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CountActiveSlots(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CountActiveSlots() error = %v, want context.Canceled", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("canceled request reached provider %d times", got)
	}
}
