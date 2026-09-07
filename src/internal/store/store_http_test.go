package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// HS-1.1/HS-1.2: resolve_key persists through create/read/update, and an
// HTTP item crashed inside the accepted window recovers into StateResolving
// with its resolve key intact (COR-14 semantics for the HTTP lane).
func TestHTTPResolveKeyRoundTripAndCrashRecovery(t *testing.T) {
	s := newTestStoreCOR(t)
	ctx := context.Background()

	key := `{"v":1,"backend_id":"b1","handler":"ia","kind":"movie","ids":{"ia":"SomeMovie1949"}}`
	stale := time.Now().UTC().Add(-10 * time.Minute)
	item := &Item{
		ID:            "http-crash-1",
		PublicID:      "http-crash-1",
		SourceType:    SourceTypeHTTP,
		ClientKind:    ClientKindQBit,
		Category:      "movies",
		State:         StateAccepted,
		SubmissionKey: "http-crash-1",
		DisplayName:   "Some.Movie.1949.DH-HTTP",
		ResolveKey:    &key,
		CreatedAt:     stale,
		UpdatedAt:     stale,
	}
	if err := s.CreateItem(ctx, item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	got, err := s.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if got.SourceType != SourceTypeHTTP {
		t.Fatalf("source type: %s", got.SourceType)
	}
	if got.ResolveKey == nil || *got.ResolveKey != key {
		t.Fatalf("resolve key did not round-trip: %v", got.ResolveKey)
	}

	// Crash window: process died after CreateItem, before StateResolving.
	ids, err := s.RequeueStaleAccepted(ctx, time.Now().UTC().Add(-5*time.Minute), 100)
	if err != nil {
		t.Fatalf("RequeueStaleAccepted: %v", err)
	}
	if len(ids) != 1 || ids[0] != item.ID {
		t.Fatalf("stale http item not requeued: %v", ids)
	}
	got, err = s.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID after requeue: %v", err)
	}
	if got.State != StateResolving {
		t.Fatalf("state after requeue: %s", got.State)
	}
	if got.ResolveKey == nil || *got.ResolveKey != key {
		t.Fatal("resolve key lost across crash recovery")
	}

	// Full-column update paths carry the key too.
	fl := `[{"file_id":"hf000000000001","selector":"x","name":"a.mp4","size":9}]`
	got.FileList = &fl
	if err := s.UpdateItemState(ctx, got, StateReady, "http: resolved"); err != nil {
		t.Fatalf("UpdateItemState: %v", err)
	}
	got, _ = s.GetItemByID(ctx, item.ID)
	if got.State != StateReady || got.ResolveKey == nil || *got.ResolveKey != key {
		t.Fatal("resolve key lost across UpdateItemState")
	}
}

func TestListReadyHTTPItemsIsTypeStateAndLimitBounded(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "http-list.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := New(db)
	now := time.Now().UTC()
	for i, tc := range []struct {
		source SourceType
		state  ItemState
	}{
		{SourceTypeHTTP, StateReady},
		{SourceTypeHTTP, StateReady},
		{SourceTypeHTTP, StateFailed},
		{SourceTypeTorrent, StateReady},
	} {
		id := fmt.Sprintf("item-%d", i)
		if err := st.CreateItem(ctx, &Item{
			ID: id, PublicID: id, SourceType: tc.source, ClientKind: ClientKindQBit,
			Category: "series", State: tc.state, SubmissionKey: id, DisplayName: id,
			CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := st.ListReadyHTTPItems(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "item-1" {
		t.Fatalf("bounded ready HTTP items = %#v", items)
	}
}
