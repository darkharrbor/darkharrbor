package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestListKeepWarmCandidatesFiltersAndBounds covers TS-4.2's store query:
// only Ready torrent items with a last_played_at at or after the cutoff
// qualify; wrong state, wrong lane, never-played, and too-long-ago-played
// items are all excluded; the explicit limit bounds the result even when
// more items qualify; ordering is oldest-created-first.
func TestListKeepWarmCandidatesFiltersAndBounds(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "keepwarm-list.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := New(db)
	now := time.Now().UTC()
	playedSince := now.Add(-20 * 24 * time.Hour)

	recentPlay := now.Add(-1 * time.Hour)
	stalePlay := now.Add(-30 * 24 * time.Hour) // before the cutoff

	cases := []struct {
		id           string
		source       SourceType
		state        ItemState
		lastPlayedAt *time.Time
		createdAt    time.Time
	}{
		{"tw-eligible-older", SourceTypeTorrent, StateReady, &recentPlay, now.Add(-2 * time.Hour)},
		{"tw-eligible-newer", SourceTypeTorrent, StateReady, &recentPlay, now.Add(-1 * time.Hour)},
		{"tw-never-played", SourceTypeTorrent, StateReady, nil, now},
		{"tw-stale-play", SourceTypeTorrent, StateReady, &stalePlay, now},
		{"tw-not-ready", SourceTypeTorrent, StateFailed, &recentPlay, now},
		{"tw-wrong-lane", SourceTypeHTTP, StateReady, &recentPlay, now},
	}
	for _, tc := range cases {
		item := &Item{
			ID: tc.id, PublicID: tc.id, SourceType: tc.source, ClientKind: ClientKindQBit,
			Category: "movies", State: tc.state, SubmissionKey: tc.id, DisplayName: tc.id,
			LastPlayedAt: tc.lastPlayedAt,
			CreatedAt:    tc.createdAt, UpdatedAt: now,
		}
		if err := st.CreateItem(ctx, item); err != nil {
			t.Fatalf("CreateItem(%s): %v", tc.id, err)
		}
	}

	// Unbounded (large limit): only the two truly-eligible items, oldest first.
	got, err := st.ListKeepWarmCandidates(ctx, playedSince, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %#v, want exactly 2", got)
	}
	if got[0].ID != "tw-eligible-older" || got[1].ID != "tw-eligible-newer" {
		t.Fatalf("ordering = [%s, %s], want oldest-created-first", got[0].ID, got[1].ID)
	}

	// Bounded: limit=1 returns only the oldest.
	bounded, err := st.ListKeepWarmCandidates(ctx, playedSince, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != 1 || bounded[0].ID != "tw-eligible-older" {
		t.Fatalf("bounded candidates = %#v, want [tw-eligible-older]", bounded)
	}
}

// TestListKeepWarmCandidatesZeroLimitReturnsNil mirrors
// ListReadyHTTPItems/ListReadyNNTPItems's own limit<=0 abstain convention.
func TestListKeepWarmCandidatesZeroLimitReturnsNil(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "keepwarm-zerolimit.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := New(db)
	got, err := st.ListKeepWarmCandidates(ctx, time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("got = %#v, want nil for limit<=0", got)
	}
}
