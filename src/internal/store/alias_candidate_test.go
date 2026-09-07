package store

import (
	"context"
	"testing"
	"time"
)

// mkAliasTestItem creates and persists an item for FindAliasCandidateByInfoHash
// coverage. state/updatedAt/infoHash/realInfoHash/provider/remoteID/queuedID
// are all directly controllable so each test case can build an exact,
// otherwise-minimal item shape.
func mkAliasTestItem(t *testing.T, s *Store, id string, state ItemState, updatedAt time.Time, infoHash, realInfoHash, provider string, remoteID, queuedID *string) *Item {
	t.Helper()
	ctx := context.Background()
	now := updatedAt
	it := &Item{
		ID: id, PublicID: "p" + id, SourceType: SourceTypeTorrent,
		ClientKind: ClientKindQBit, Category: "tv", State: state,
		SubmissionKey: "key-" + id, DisplayName: id,
		RemoteID: remoteID, QueuedID: queuedID,
		CreatedAt: now, UpdatedAt: now,
	}
	if infoHash != "" {
		it.InfoHash = &infoHash
	}
	if provider != "" {
		it.Provider = &provider
	}
	if realInfoHash != "" {
		it.Metadata.RealInfoHash = realInfoHash
	}
	if err := s.CreateItem(ctx, it); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return it
}

func TestFindAliasCandidateByInfoHashNoCandidates(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()

	// Empty hash never matches anything, even against a real row.
	mkAliasTestItem(t, s, "a1", StateReady, time.Now().UTC(), "aaaa", "", "torbox", strp("r1"), nil)
	got, err := s.FindAliasCandidateByInfoHash(ctx, "")
	if err != nil || got != nil {
		t.Fatalf("empty hash: got %+v, err %v, want nil, nil", got, err)
	}

	// A hash with no rows at all.
	got, err = s.FindAliasCandidateByInfoHash(ctx, "no-such-hash")
	if err != nil || got != nil {
		t.Fatalf("no rows: got %+v, err %v, want nil, nil", got, err)
	}
}

func TestFindAliasCandidateByInfoHashMatchesTopLevelInfoHash(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mkAliasTestItem(t, s, "primary", StateResolving, now, "cafef00d", "", "torbox", strp("remote-1"), nil)

	got, err := s.FindAliasCandidateByInfoHash(ctx, "cafef00d")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil || got.ID != "primary" {
		t.Fatalf("got %+v, want primary", got)
	}
}

func TestFindAliasCandidateByInfoHashMatchesSyntheticRealInfoHash(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// A synthetic per-season grab: item.InfoHash is the synthetic
	// client-facing hash, item.Metadata.RealInfoHash is the underlying
	// season torrent's real hash. A second episode of the same season
	// grabbed separately must alias by real hash, not by the (per-episode
	// unique) synthetic top-level info_hash.
	mkAliasTestItem(t, s, "ep01", StateReady, now, "synth-e01", "real-season-hash", "torbox", strp("remote-season"), nil)

	got, err := s.FindAliasCandidateByInfoHash(ctx, "real-season-hash")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil || got.ID != "ep01" {
		t.Fatalf("got %+v, want ep01", got)
	}

	// The synthetic hash itself must still match its own top-level
	// info_hash (no accidental cross-match on the wrong column).
	got2, err := s.FindAliasCandidateByInfoHash(ctx, "synth-e01")
	if err != nil {
		t.Fatalf("find synthetic: %v", err)
	}
	if got2 == nil || got2.ID != "ep01" {
		t.Fatalf("synthetic hash should still match its own top-level info_hash: got %+v", got2)
	}
}

func TestFindAliasCandidateByInfoHashExcludesNoProviderBinding(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Resolving but never actually got a provider binding persisted yet
	// (e.g. crashed mid-submit, or the submit goroutine hasn't run). Must
	// never be aliased onto -- there is nothing to copy.
	mkAliasTestItem(t, s, "noprov", StateResolving, now, "deadbeef", "", "", nil, nil)
	got, err := s.FindAliasCandidateByInfoHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil (no provider/remote/queued id)", got)
	}

	// Provider set but neither remote_id nor queued_id -- still no binding.
	mkAliasTestItem(t, s, "provonly", StateResolving, now, "deadbeef2", "", "torbox", nil, nil)
	got, err = s.FindAliasCandidateByInfoHash(ctx, "deadbeef2")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil (provider set but no remote/queued id)", got)
	}

	// queued_id alone (uncached, still queued at the provider) is a valid
	// binding worth aliasing onto.
	mkAliasTestItem(t, s, "queuedonly", StateResolving, now, "deadbeef3", "", "torbox", nil, strp("q1"))
	got, err = s.FindAliasCandidateByInfoHash(ctx, "deadbeef3")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil || got.ID != "queuedonly" {
		t.Fatalf("got %+v, want queuedonly (queued_id alone is a valid binding)", got)
	}
}

func TestFindAliasCandidateByInfoHashExcludesTerminalAndAcceptedStates(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const hash = "terminalhash"
	mkAliasTestItem(t, s, "failed", StateFailed, now, hash, "", "torbox", strp("r1"), nil)
	mkAliasTestItem(t, s, "removed", StateRemoved, now, hash, "", "torbox", strp("r2"), nil)
	mkAliasTestItem(t, s, "accepted", StateAccepted, now, hash, "", "torbox", strp("r3"), nil)

	got, err := s.FindAliasCandidateByInfoHash(ctx, hash)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil (only failed/removed/accepted rows exist)", got)
	}
}

func TestFindAliasCandidateByInfoHashPrefersReadyOverResolving(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()
	base := time.Now().UTC()

	const hash = "preferready"
	// The resolving row is more recently updated than the ready row, but
	// ready must still win -- it is the more stable, fully-established
	// provider binding to alias onto.
	mkAliasTestItem(t, s, "ready-older", StateReady, base, hash, "", "torbox", strp("r-ready"), nil)
	mkAliasTestItem(t, s, "resolving-newer", StateResolving, base.Add(time.Hour), hash, "", "torbox", strp("r-resolving"), nil)

	got, err := s.FindAliasCandidateByInfoHash(ctx, hash)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil || got.ID != "ready-older" {
		t.Fatalf("got %+v, want ready-older (ready beats more-recently-updated resolving)", got)
	}
}

func TestFindAliasCandidateByInfoHashTieBreaksOnMostRecentlyUpdated(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()
	base := time.Now().UTC()

	const hash = "tiebreak"
	mkAliasTestItem(t, s, "older", StateResolving, base, hash, "", "torbox", strp("r-old"), nil)
	mkAliasTestItem(t, s, "newer", StateResolving, base.Add(time.Minute), hash, "", "torbox", strp("r-new"), nil)

	got, err := s.FindAliasCandidateByInfoHash(ctx, hash)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil || got.ID != "newer" {
		t.Fatalf("got %+v, want newer (most recently updated among same-state ties)", got)
	}
}
