package main

import (
	"context"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestDeadSentinelPollDoesNotConsumeResolveRetryBudget(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreForBackfill(t)
	start := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID:            "dead-sentinel-grace",
		PublicID:      "public",
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      "movies",
		State:         store.StateResolving,
		SubmissionKey: "dead-sentinel-grace",
		DisplayName:   "release",
		CreatedAt:     start,
		UpdatedAt:     start,
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	for i := 0; i < maxResolveAttempts+2; i++ {
		now := start.Add(time.Duration(i) * 15 * time.Second)
		scheduleResolvePoll(ctx, testLogger(), st, item, func() time.Time { return now }, 15*time.Second)
	}

	got, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if got.State != store.StateResolving {
		t.Fatalf("state = %q, want %q", got.State, store.StateResolving)
	}
	if got.RetryCount != 0 {
		t.Fatalf("retry count = %d, want 0", got.RetryCount)
	}
	if got.ErrorMessage != nil {
		t.Fatalf("error message set during normal grace polling")
	}
	wantNext := start.Add(time.Duration(maxResolveAttempts+2) * 15 * time.Second)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(wantNext) {
		t.Fatalf("next poll = %v, want %v", got.NextRunAt, wantNext)
	}
}

func TestResolveErrorsStillExhaustRetryBudget(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreForBackfill(t)
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID:            "real-resolve-error",
		PublicID:      "public",
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      "movies",
		State:         store.StateResolving,
		SubmissionKey: "real-resolve-error",
		DisplayName:   "release",
		RetryCount:    maxResolveAttempts - 1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	scheduleResolveRetry(ctx, testLogger(), st, item, time.Second, "provider error")

	got, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if got.State != store.StateFailed {
		t.Fatalf("state = %q, want %q", got.State, store.StateFailed)
	}
	if got.RetryCount != maxResolveAttempts {
		t.Fatalf("retry count = %d, want %d", got.RetryCount, maxResolveAttempts)
	}
}
