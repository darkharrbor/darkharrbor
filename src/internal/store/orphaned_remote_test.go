package store

import (
	"context"
	"testing"
	"time"
)

func TestListOrphanedRemoteBefore(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()
	rid1, rid2, rid3 := "r1", "r2", "r3"

	mk := func(id string, state ItemState, rid *string) *Item {
		now := time.Now().UTC()
		it := &Item{ID: id, PublicID: "p" + id, SourceType: SourceTypeTorrent,
			ClientKind: ClientKindQBit, Category: "tv", State: state,
			SubmissionKey: "k" + id, DisplayName: id, RemoteID: rid,
			CreatedAt: now, UpdatedAt: now}
		if err := s.CreateItem(ctx, it); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		return it
	}
	mk("orph1", StateRemoved, &rid1)     // orphan → returned
	mk("shared", StateRemoved, &rid2)    // shares rid2 with live → excluded
	mk("live", StateReady, &rid2)        // live holder
	mk("failedOrph", StateFailed, &rid3) // failed orphan → returned
	mk("noRemote", StateRemoved, nil)    // no remote → excluded

	got, err := s.ListOrphanedRemoteBefore(ctx, time.Now().UTC().Add(time.Minute), 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := map[string]bool{}
	for _, it := range got {
		ids[it.ID] = true
	}
	if !ids["orph1"] || !ids["failedOrph"] || len(ids) != 2 {
		t.Fatalf("wrong orphan set: %v", ids)
	}
	// grace window: cutoff in the past excludes fresh rows
	got, err = s.ListOrphanedRemoteBefore(ctx, time.Now().UTC().Add(-time.Hour), 50)
	if err != nil || len(got) != 0 {
		t.Fatalf("grace window violated: %v %v", got, err)
	}
}
