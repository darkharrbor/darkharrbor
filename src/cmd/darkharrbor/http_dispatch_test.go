package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

func newDispatchStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "d.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.New(db)
}

func httpItem(t *testing.T, st *store.Store, id string, state store.ItemState) *store.Item {
	t.Helper()
	key := `{"v":1,"backend_id":"b1","handler":"ia","kind":"movie","ids":{"ia":"X1949"}}`
	item := &store.Item{
		ID: id, PublicID: id,
		SourceType: store.SourceTypeHTTP, ClientKind: store.ClientKindQBit,
		Category: "movies", State: state, SubmissionKey: id, DisplayName: id,
		ResolveKey: &key,
		CreatedAt:  time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return item
}

// HS-1.2: the recovery resolver dispatches HTTP items to the injected HTTP
// resolver BEFORE any remote-ID/provider logic — with a nil provider, no
// panic, no provider call, no state mutation by resolveItem itself.
func TestResolveItemDispatchesHTTP(t *testing.T) {
	st := newDispatchStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	item := httpItem(t, st, "http-dispatch-1", store.StateResolving)

	called := false
	spy := func(ctx context.Context, it *store.Item) {
		called = true
		if it.ID != item.ID {
			t.Fatalf("wrong item dispatched: %s", it.ID)
		}
	}
	// nil provider/writer/prober: HTTP must return before touching any of them.
	resolveItem(context.Background(), log, nil, st, nil, nil, 0, "http://dh:8381",
		nil, nil, item, "secret", "/tmp", "", nil, nil, spy, nil, nil, nil, nil, nil)
	if !called {
		t.Fatal("http resolver was not dispatched")
	}
}

// Without a wired resolver, HTTP items fail closed (never silently spin).
func TestResolveItemHTTPWithoutResolverFailsClosed(t *testing.T) {
	st := newDispatchStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	item := httpItem(t, st, "http-dispatch-2", store.StateResolving)

	resolveItem(context.Background(), log, nil, st, nil, nil, 0, "http://dh:8381",
		nil, nil, item, "secret", "/tmp", "", nil, nil, nil, nil, nil, nil, nil, nil)
	got, err := st.GetItemByID(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
}

// HS-1.2: the submit recovery loop never routes HTTP items into debrid
// lanes — the item is left untouched for the resolve cycle.
func TestSubmitItemSkipsHTTP(t *testing.T) {
	st := newDispatchStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	item := httpItem(t, st, "http-submit-1", store.StateResolving)

	// Zero lanes: for a torrent this path would fail the item; for HTTP it
	// must return first and leave state untouched.
	submitItem(context.Background(), log, nil, st, item)
	got, err := st.GetItemByID(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateResolving {
		t.Fatalf("state = %s, want resolving (untouched)", got.State)
	}
}
