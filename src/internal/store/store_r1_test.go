package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStoreForCleanup(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "never_played_test.db")
	db, err := Open(context.Background(), dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db)
}

func mustCreateItem(t *testing.T, s *Store, id string, state ItemState, remoteID *string, lastPlayedAt *time.Time) *Item {
	t.Helper()
	now := time.Now().UTC()
	item := &Item{
		ID:            id,
		PublicID:      id,
		SourceType:    SourceTypeTorrent,
		ClientKind:    ClientKindQBit,
		Category:      "darkharrbor",
		State:         state,
		SubmissionKey: id,
		DisplayName:   id,
		RemoteID:      remoteID,
		LastPlayedAt:  lastPlayedAt,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem(%s): %v", id, err)
	}
	return item
}

func strp(s string) *string { return &s }

// TestListNeverPlayedUncachedBefore covers the R1 sweep query: only
// StateReady torrent items with a logged uncached_add older than cutoff,
// still materialized (remote_id set), and never played are returned.
func TestListNeverPlayedUncachedBefore(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()

	// Candidate: uncached, ready, materialized, never played, old grab. Should match.
	mustCreateItem(t, s, "old-uncached-unplayed", StateReady, strp("rid-1"), nil)
	s.SetClock(func() time.Time { return time.Now().UTC().Add(-48 * time.Hour) })
	if err := s.RecordUncachedAdd(ctx, "old-uncached-unplayed"); err != nil {
		t.Fatalf("RecordUncachedAdd: %v", err)
	}
	s.SetClock(time.Now)

	// Recent uncached grab — inside the TTL window, should NOT match a
	// 24h cutoff.
	mustCreateItem(t, s, "recent-uncached-unplayed", StateReady, strp("rid-2"), nil)
	if err := s.RecordUncachedAdd(ctx, "recent-uncached-unplayed"); err != nil {
		t.Fatalf("RecordUncachedAdd: %v", err)
	}

	// Old uncached grab but already played — must NOT match.
	played := time.Now().UTC()
	mustCreateItem(t, s, "old-uncached-played", StateReady, strp("rid-3"), &played)
	s.SetClock(func() time.Time { return time.Now().UTC().Add(-48 * time.Hour) })
	if err := s.RecordUncachedAdd(ctx, "old-uncached-played"); err != nil {
		t.Fatalf("RecordUncachedAdd: %v", err)
	}
	s.SetClock(time.Now)

	// Old uncached grab but already cleaned up (remote_id cleared) — must NOT match.
	mustCreateItem(t, s, "old-uncached-cleaned", StateReady, nil, nil)
	s.SetClock(func() time.Time { return time.Now().UTC().Add(-48 * time.Hour) })
	if err := s.RecordUncachedAdd(ctx, "old-uncached-cleaned"); err != nil {
		t.Fatalf("RecordUncachedAdd: %v", err)
	}
	s.SetClock(time.Now)

	// Old uncached grab but still resolving (not yet ready) — must NOT match.
	mustCreateItem(t, s, "old-uncached-resolving", StateResolving, strp("rid-5"), nil)
	s.SetClock(func() time.Time { return time.Now().UTC().Add(-48 * time.Hour) })
	if err := s.RecordUncachedAdd(ctx, "old-uncached-resolving"); err != nil {
		t.Fatalf("RecordUncachedAdd: %v", err)
	}
	s.SetClock(time.Now)

	// Old grab, ready, unplayed, but never went through the uncached lane
	// (no governor_log row at all — a cached submission) — must NOT match.
	mustCreateItem(t, s, "old-cached-unplayed", StateReady, strp("rid-6"), nil)

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	got, err := s.ListNeverPlayedUncachedBefore(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("ListNeverPlayedUncachedBefore: %v", err)
	}
	if len(got) != 1 {
		ids := make([]string, 0, len(got))
		for _, it := range got {
			ids = append(ids, it.ID)
		}
		t.Fatalf("got %d items %v, want 1 [old-uncached-unplayed]", len(got), ids)
	}
	if got[0].ID != "old-uncached-unplayed" {
		t.Errorf("got item %q, want old-uncached-unplayed", got[0].ID)
	}
}
