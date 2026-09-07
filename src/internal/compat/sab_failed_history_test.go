package compat

import (
	"reflect"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestFailedSABHistoryProjectionIsStableDuringRetention(t *testing.T) {
	failure := "nntp_dead_all_providers"
	updated := time.Unix(1_700_000_000, 0).UTC()
	item := &store.Item{
		ID:           "failed-item",
		PublicID:     "stable-public-id",
		SourceType:   store.SourceTypeNZB,
		ClientKind:   store.ClientKindSAB,
		Category:     "tv",
		State:        store.StateFailed,
		DisplayName:  "Dead.Release.S11",
		ErrorMessage: &failure,
		UpdatedAt:    updated,
	}

	first := ProjectSABHistory("4.5.1", []*store.Item{item}, "/data", "/mnt/darkharrbor")
	second := ProjectSABHistory("4.5.1", []*store.Item{item}, "/data", "/mnt/darkharrbor")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated failed-history projections differ:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if first.History.NoOfSlots != "1" || len(first.History.Slots) != 1 {
		t.Fatalf("history slots = %q/%d, want 1", first.History.NoOfSlots, len(first.History.Slots))
	}

	slot := first.History.Slots[0]
	if slot.NzoID != SABNZOID(item.PublicID) {
		t.Fatalf("nzo_id = %q, want %q", slot.NzoID, SABNZOID(item.PublicID))
	}
	if slot.Status != "Failed" || slot.FailMessage != failure {
		t.Fatalf("failed projection status/message = %q/%q", slot.Status, slot.FailMessage)
	}
	if slot.Name != item.DisplayName || slot.Category != item.Category || slot.Completed != updated.Unix() {
		t.Fatalf("failed projection identity changed: %+v", slot)
	}

	queue := ProjectSABQueue("4.5.1", []*store.Item{item})
	if queue.Queue.NoOfSlots != "0" || len(queue.Queue.Slots) != 0 {
		t.Fatalf("failed item appeared in SAB queue: %+v", queue.Queue)
	}
}
