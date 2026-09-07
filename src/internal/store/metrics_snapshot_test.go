package store

import (
	"context"
	"testing"
	"time"
)

func TestItemStateCountsAndZeroFillTotal(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/metrics-snapshot.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	s := New(db)
	now := time.Now().UTC()
	for i, st := range []ItemState{StateReady, StateReady, StateFailed} {
		id := "item-" + string(rune('a'+i))
		if err := s.CreateItem(ctx, &Item{
			ID: id, PublicID: id, SourceType: SourceTypeNZB, ClientKind: ClientKindSAB,
			Category: "tv", State: st, SubmissionKey: id, DisplayName: id,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if _, err := s.IncrementNNTPZeroFill(ctx, "item-a"); err != nil {
		t.Fatalf("increment a: %v", err)
	}
	if _, err := s.IncrementNNTPZeroFill(ctx, "item-b"); err != nil {
		t.Fatalf("increment b: %v", err)
	}
	if _, err := s.IncrementNNTPZeroFill(ctx, "item-b"); err != nil {
		t.Fatalf("increment b again: %v", err)
	}

	counts, err := s.ItemStateCounts(ctx)
	if err != nil {
		t.Fatalf("ItemStateCounts: %v", err)
	}
	if counts[StateReady] != 2 {
		t.Errorf("expected 2 ready, got %d", counts[StateReady])
	}
	if counts[StateFailed] != 1 {
		t.Errorf("expected 1 failed, got %d", counts[StateFailed])
	}

	total, err := s.NNTPZeroFillTotal(ctx)
	if err != nil {
		t.Fatalf("NNTPZeroFillTotal: %v", err)
	}
	if total != 3 {
		t.Errorf("expected zero-fill total 3, got %d", total)
	}
}
