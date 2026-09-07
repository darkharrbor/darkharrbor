package store

import (
	"context"
	"testing"
	"time"
)

func TestHashAvailability_RoundTrip(t *testing.T) {
	s := newTestStoreSF05(t)
	ctx := context.Background()
	if err := s.RecordHashAvailability(ctx, "torbox", "deadbeef", true, "submit", time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}
	ha, found, err := s.GetHashAvailability(ctx, "torbox", "deadbeef")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if !ha.Cached || ha.Source != "submit" || ha.Provider != "torbox" || ha.InfoHash != "deadbeef" {
		t.Fatalf("unexpected row: %+v", ha)
	}
	if ha.Hits != 0 {
		t.Fatalf("expected hits=0 on first observation, got %d", ha.Hits)
	}
}

func TestHashAvailability_UpsertRefreshesAndIncrementsHits(t *testing.T) {
	s := newTestStoreSF05(t)
	ctx := context.Background()
	if err := s.RecordHashAvailability(ctx, "torbox", "deadbeef", true, "submit", time.Hour); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	// A later stream-time observation flips the verdict to uncached (e.g.
	// the provider genuinely removed the content since submit).
	if err := s.RecordHashAvailability(ctx, "torbox", "deadbeef", false, "stream", time.Hour); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	ha, found, err := s.GetHashAvailability(ctx, "torbox", "deadbeef")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if ha.Cached || ha.Source != "stream" {
		t.Fatalf("expected latest observation (cached=false, source=stream) to win, got %+v", ha)
	}
	if ha.Hits != 1 {
		t.Fatalf("expected hits=1 after second observation, got %d", ha.Hits)
	}
}

func TestHashAvailability_DifferentProviderIsIndependent(t *testing.T) {
	s := newTestStoreSF05(t)
	ctx := context.Background()
	if err := s.RecordHashAvailability(ctx, "torbox", "deadbeef", true, "submit", time.Hour); err != nil {
		t.Fatalf("record torbox: %v", err)
	}
	if _, found, _ := s.GetHashAvailability(ctx, "realdebrid", "deadbeef"); found {
		t.Fatal("expected a different provider's observation for the same hash to be independent (miss)")
	}
}

func TestHashAvailability_ExpiryReportsNotFound(t *testing.T) {
	s := newTestStoreSF05(t)
	ctx := context.Background()
	if err := s.RecordHashAvailability(ctx, "torbox", "deadbeef", true, "submit", time.Millisecond); err != nil {
		t.Fatalf("record: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	_, found, err := s.GetHashAvailability(ctx, "torbox", "deadbeef")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if found {
		t.Fatal("expected expired observation to report not-found")
	}
	// Unlike SF-05's negative probe cache, the expired row is left in place
	// (not delete-on-read) -- confirm it still physically exists.
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM hash_availability").Scan(&n); err != nil || n != 1 {
		t.Fatalf("expected expired row to remain persisted: n=%d err=%v", n, err)
	}
}

func TestHashAvailability_MissingProviderOrHashRefused(t *testing.T) {
	s := newTestStoreSF05(t)
	ctx := context.Background()
	if err := s.RecordHashAvailability(ctx, "", "deadbeef", true, "submit", time.Hour); err == nil {
		t.Fatal("expected empty provider to be refused")
	}
	if err := s.RecordHashAvailability(ctx, "torbox", "", true, "submit", time.Hour); err == nil {
		t.Fatal("expected empty info_hash to be refused")
	}
}

func TestHashAvailability_UnknownIsNotFoundWithoutError(t *testing.T) {
	s := newTestStoreSF05(t)
	ctx := context.Background()
	_, found, err := s.GetHashAvailability(ctx, "torbox", "neverobserved")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if found {
		t.Fatal("expected an unobserved (provider, hash) pair to report not-found, not an error")
	}
}
