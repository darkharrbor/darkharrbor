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

// A failed torrent's TorBox transfer must be released before its row is
// pruned, unless a live item shares the transfer.
func TestPruneFailedItemsReleasesUnsharedRemote(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "prune.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := store.New(db)
	old := time.Now().UTC().Add(-time.Hour)
	mk := func(id string, state store.ItemState, remote string) {
		t.Helper()
		r := remote
		it := &store.Item{ID: id, PublicID: "p" + id, SourceType: store.SourceTypeTorrent, ClientKind: "qbit",
			Category: "tv", State: store.StateAccepted, SubmissionKey: "k" + id, DisplayName: id,
			CreatedAt: old, UpdatedAt: old, RemoteID: &r}
		if err := st.CreateItem(ctx, it); err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		if state != store.StateAccepted {
			if err := st.UpdateItemState(ctx, it, state, ""); err != nil {
				t.Fatalf("UpdateItemState: %v", err)
			}
		}
	}
	mk("orphan", store.StateFailed, "r-orphan")
	mk("sharedfail", store.StateFailed, "r-shared")
	mk("live", store.StateResolving, "r-shared")

	var released []string
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	time.Sleep(5 * time.Millisecond)
	n := pruneFailedItems(ctx, log, st, nil, time.Millisecond, func(id string) { released = append(released, id) })
	if n != 2 {
		t.Fatalf("pruned %d, want 2", n)
	}
	if len(released) != 1 || released[0] != "orphan" {
		t.Fatalf("released %v, want [orphan]", released)
	}
}
