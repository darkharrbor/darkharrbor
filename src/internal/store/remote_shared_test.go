package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoteIDSharedWithLive(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "shared.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := New(db)
	now := time.Now().UTC()
	mk := func(id, key string, state ItemState, remote string) {
		t.Helper()
		r := remote
		it := &Item{ID: id, PublicID: "p" + id, SourceType: SourceTypeTorrent, ClientKind: "qbit", Category: "tv",
			State: StateAccepted, SubmissionKey: key, DisplayName: id, CreatedAt: now, UpdatedAt: now, RemoteID: &r}
		if err := st.CreateItem(ctx, it); err != nil {
			t.Fatalf("CreateItem %s: %v", id, err)
		}
		if state != StateAccepted {
			if err := st.UpdateItemState(ctx, it, state, ""); err != nil {
				t.Fatalf("UpdateItemState %s: %v", id, err)
			}
		}
	}
	mk("failed1", "k1", StateFailed, "r-1")
	mk("failed2", "k2", StateFailed, "r-2")
	mk("live2", "k3", StateResolving, "r-2")

	if shared, err := st.RemoteIDSharedWithLive(ctx, "r-1", "failed1"); err != nil || shared {
		t.Fatalf("r-1: shared=%v err=%v, want false", shared, err)
	}
	if shared, err := st.RemoteIDSharedWithLive(ctx, "r-2", "failed2"); err != nil || !shared {
		t.Fatalf("r-2: shared=%v err=%v, want true", shared, err)
	}
}
