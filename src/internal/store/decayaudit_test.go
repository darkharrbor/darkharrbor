package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestListReadyTorrentItemsAfterFiltersOrdersAndBounds covers TS-4.3's
// store query: only Ready torrent items qualify (wrong state/lane
// excluded), only ids strictly greater than afterID are returned, results
// are in ascending id order, and the explicit limit bounds the result even
// when more items qualify.
func TestListReadyTorrentItemsAfterFiltersOrdersAndBounds(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "decayaudit-list.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := New(db)
	now := time.Now().UTC()

	cases := []struct {
		id     string
		source SourceType
		state  ItemState
	}{
		{"ta-item-1", SourceTypeTorrent, StateReady},
		{"ta-item-2", SourceTypeTorrent, StateReady},
		{"ta-item-3", SourceTypeTorrent, StateReady},
		{"tb-not-ready", SourceTypeTorrent, StateFailed},
		{"tc-wrong-lane", SourceTypeHTTP, StateReady},
	}
	for _, tc := range cases {
		item := &Item{
			ID: tc.id, PublicID: tc.id, SourceType: tc.source, ClientKind: ClientKindQBit,
			Category: "movies", State: tc.state, SubmissionKey: tc.id, DisplayName: tc.id,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := st.CreateItem(ctx, item); err != nil {
			t.Fatalf("CreateItem(%s): %v", tc.id, err)
		}
	}

	// From the beginning, unbounded: only the three eligible torrent items,
	// ascending id order.
	got, err := st.ListReadyTorrentItemsAfter(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 eligible items, got %d: %+v", len(got), got)
	}
	wantOrder := []string{"ta-item-1", "ta-item-2", "ta-item-3"}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Errorf("order[%d] = %s, want %s", i, got[i].ID, w)
		}
	}

	// Cursor rotation: after the first item, only the remaining two.
	got, err = st.ListReadyTorrentItemsAfter(ctx, "ta-item-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "ta-item-2" || got[1].ID != "ta-item-3" {
		t.Errorf("after ta-item-1: got %+v", got)
	}

	// Bounded: limit 1 returns only the first eligible item.
	got, err = st.ListReadyTorrentItemsAfter(ctx, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "ta-item-1" {
		t.Errorf("bounded(1): got %+v", got)
	}

	// Past the last id: empty result signals "wrap the cursor".
	got, err = st.ListReadyTorrentItemsAfter(ctx, "ta-item-3", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("past last id: expected empty, got %+v", got)
	}
}

// TestListReadyTorrentItemsAfterZeroLimitReturnsNil mirrors
// ListKeepWarmCandidates/ListReadyHTTPItems/ListReadyNNTPItems's own
// limit<=0 abstain convention.
func TestListReadyTorrentItemsAfterZeroLimitReturnsNil(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "decayaudit-zerolimit.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := New(db)

	got, err := st.ListReadyTorrentItemsAfter(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for limit<=0, got %+v", got)
	}
}
