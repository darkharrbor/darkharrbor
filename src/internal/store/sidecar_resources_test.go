package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/sidecar"
)

func TestSidecarResourcesPersistBoundAndCascade(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sidecars.db")
	db, err := Open(ctx, dbPath, time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	st := New(db)
	item := mustCreateItem(t, st, "sidecar-item", StateReady, nil, nil)
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	resource := sidecar.Resource{
		ID: "sc000000000000000000000001", ItemID: item.ID, FileID: "file-1",
		Kind: sidecar.KindSubtitle, Filename: "episode.en.srt",
		MediaType: "application/x-subrip", Language: "en", Bytes: []byte("subtitle"),
		CreatedAt: now,
	}
	if err := st.InsertSidecarResource(ctx, resource, 1, int64(len(resource.Bytes))); err != nil {
		t.Fatalf("InsertSidecarResource: %v", err)
	}
	second := resource
	second.ID = "sc000000000000000000000002"
	if err := st.InsertSidecarResource(ctx, second, 1, 1024); !errors.Is(err, sidecar.ErrCapacity) {
		t.Fatalf("capacity error = %v, want ErrCapacity", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first db: %v", err)
	}

	db2, err := Open(ctx, dbPath, time.Second)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	st2 := New(db2)
	got, err := st2.GetSidecarResource(ctx, resource.ID)
	if err != nil || got == nil || string(got.Bytes) != "subtitle" || !got.CreatedAt.Equal(now) {
		t.Fatalf("persisted resource = %#v, err=%v", got, err)
	}
	listed, err := st2.ListSidecarResources(ctx, item.ID, "file-1")
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListSidecarResources len=%d err=%v", len(listed), err)
	}
	persistedItem, err := st2.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if err := st2.UpdateItemState(ctx, persistedItem, StateRemoved, "test cleanup"); err != nil {
		t.Fatalf("UpdateItemState removed: %v", err)
	}
	if _, err := st2.DeleteRemovedItemsByIDs(ctx, []string{item.ID}); err != nil {
		t.Fatalf("DeleteRemovedItemsByIDs: %v", err)
	}
	got, err = st2.GetSidecarResource(ctx, resource.ID)
	if err != nil || got != nil {
		t.Fatalf("resource survived item cascade: %#v err=%v", got, err)
	}
}

func TestSidecarResourceConcurrentCapacity(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "sidecar-capacity.db"), time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	st := New(db)
	item := mustCreateItem(t, st, "sidecar-capacity-item", StateReady, nil, nil)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs <- st.InsertSidecarResource(ctx, sidecar.Resource{
				ID:        []string{"sc000000000000000000000010", "sc000000000000000000000011"}[i],
				ItemID:    item.ID,
				FileID:    "file-1",
				Kind:      sidecar.KindSubtitle,
				Filename:  "episode.srt",
				MediaType: "application/x-subrip",
				Bytes:     []byte("subtitle"),
				CreatedAt: time.Now().UTC(),
			}, 1, 1024)
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	var succeeded, capped int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, sidecar.ErrCapacity):
			capped++
		default:
			t.Fatalf("unexpected insert error: %v", err)
		}
	}
	if succeeded != 1 || capped != 1 {
		t.Fatalf("succeeded=%d capped=%d, want 1/1", succeeded, capped)
	}
}
