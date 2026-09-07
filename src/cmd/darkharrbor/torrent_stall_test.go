package main

import (
	"context"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestObserveTorrentStallMeasuresContinuousStallNotItemAge(t *testing.T) {
	now := time.Date(2026, 7, 27, 21, 0, 0, 0, time.UTC)
	var metadata store.SubmissionMetadata

	age, expired := observeTorrentStall(&metadata, true, now, 30*time.Minute)
	if age != 0 || expired || metadata.TorrentStalledAt == nil || !metadata.TorrentStalledAt.Equal(now) {
		t.Fatalf("first stall = (%v, %t, %v), want fresh non-expired observation", age, expired, metadata.TorrentStalledAt)
	}

	age, expired = observeTorrentStall(&metadata, true, now.Add(29*time.Minute), 30*time.Minute)
	if age != 29*time.Minute || expired {
		t.Fatalf("29-minute stall = (%v, %t), want non-expired", age, expired)
	}

	age, expired = observeTorrentStall(&metadata, true, now.Add(30*time.Minute), 30*time.Minute)
	if age != 30*time.Minute || !expired {
		t.Fatalf("30-minute stall = (%v, %t), want expired", age, expired)
	}
}

func TestObserveTorrentStallRecoveryClearsClock(t *testing.T) {
	now := time.Date(2026, 7, 27, 21, 0, 0, 0, time.UTC)
	metadata := store.SubmissionMetadata{TorrentStalledAt: &now}

	age, expired := observeTorrentStall(&metadata, false, now.Add(10*time.Minute), 30*time.Minute)
	if age != 0 || expired || metadata.TorrentStalledAt != nil {
		t.Fatalf("recovery = (%v, %t, %v), want cleared clock", age, expired, metadata.TorrentStalledAt)
	}

	age, expired = observeTorrentStall(&metadata, true, now.Add(2*time.Hour), 30*time.Minute)
	if age != 0 || expired {
		t.Fatalf("first later stall = (%v, %t), want a fresh stall window despite item age", age, expired)
	}
}

func TestTorrentStalledAtPersistsAcrossStoreInstances(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreForBackfill(t)
	now := time.Date(2026, 7, 27, 21, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID:            "persisted-stall-clock",
		PublicID:      "public",
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      "movies",
		State:         store.StateResolving,
		SubmissionKey: "persisted-stall-clock",
		DisplayName:   "release",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	observeTorrentStall(&item.Metadata, true, now, 30*time.Minute)
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	got, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if got.Metadata.TorrentStalledAt == nil || !got.Metadata.TorrentStalledAt.Equal(now) {
		t.Fatalf("persisted stalled_at = %v, want %v", got.Metadata.TorrentStalledAt, now)
	}
}
