package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStoreNS13(t *testing.T) *Store {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "ns13.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db)
}

func createNS13Item(t *testing.T, s *Store, id string) {
	t.Helper()
	now := time.Now().UTC()
	item := &Item{
		ID:            id,
		PublicID:      id,
		SourceType:    SourceTypeNZB,
		ClientKind:    ClientKindSAB,
		Category:      "tv",
		State:         StateReady,
		SubmissionKey: id,
		DisplayName:   id,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
}

func TestIncrementNNTPZeroFill_PersistsCountAndStateMessage(t *testing.T) {
	s := newTestStoreNS13(t)
	createNS13Item(t, s, "ns13-item")
	ctx := context.Background()

	for want := int64(1); want <= 2; want++ {
		got, err := s.IncrementNNTPZeroFill(ctx, "ns13-item")
		if err != nil {
			t.Fatalf("IncrementNNTPZeroFill(%d): %v", want, err)
		}
		if got != want {
			t.Fatalf("count = %d, want %d", got, want)
		}
	}

	item, err := s.GetItemByID(ctx, "ns13-item")
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if item == nil {
		t.Fatal("item missing")
	}
	if item.State != StateReady {
		t.Fatalf("state = %q, want Ready unchanged", item.State)
	}
	if item.Metadata.NNTPZeroFillCount != 2 {
		t.Fatalf("metadata count = %d, want 2", item.Metadata.NNTPZeroFillCount)
	}
	wantMessage := nntpZeroFillMessagePrefix + "2"
	if item.ErrorMessage == nil || *item.ErrorMessage != wantMessage {
		t.Fatalf("error_message = %v, want %q", item.ErrorMessage, wantMessage)
	}
}

func TestIncrementNNTPZeroFill_ConcurrentIncrementsAreAtomic(t *testing.T) {
	s := newTestStoreNS13(t)
	createNS13Item(t, s, "ns13-concurrent")
	ctx := context.Background()

	const workers = 32
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.IncrementNNTPZeroFill(ctx, "ns13-concurrent"); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent increment: %v", err)
	}

	item, err := s.GetItemByID(ctx, "ns13-concurrent")
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if item.Metadata.NNTPZeroFillCount != workers {
		t.Fatalf("metadata count = %d, want %d", item.Metadata.NNTPZeroFillCount, workers)
	}
	wantMessage := fmt.Sprintf("%s%d", nntpZeroFillMessagePrefix, workers)
	if item.ErrorMessage == nil || *item.ErrorMessage != wantMessage {
		t.Fatalf("error_message = %v, want %q", item.ErrorMessage, wantMessage)
	}
}

func TestIncrementNNTPZeroFill_RejectsUnknownOrEmptyItem(t *testing.T) {
	s := newTestStoreNS13(t)
	if _, err := s.IncrementNNTPZeroFill(context.Background(), ""); err == nil {
		t.Fatal("empty item id must fail")
	}
	if _, err := s.IncrementNNTPZeroFill(context.Background(), "missing-item"); err == nil {
		t.Fatal("unknown item id must fail")
	}
}
