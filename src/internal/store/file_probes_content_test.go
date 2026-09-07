package store

import (
	"context"
	"testing"
)

// A re-grab (new item id, same info_hash) must be served the prior item's
// stored probe for the same file + args.
func TestGetFileProbeByContentCrossItem(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()
	hash := "b6ac821ca59aece4841112cbd7aa64ca4504a3ec"

	oldItem := &Item{ID: "old1", PublicID: "pold1", SourceType: SourceTypeTorrent,
		ClientKind: ClientKindQBit, Category: "tv", State: StateRemoved,
		SubmissionKey: "k1", DisplayName: "pack", InfoHash: &hash}
	newItem := &Item{ID: "new1", PublicID: "pnew1", SourceType: SourceTypeTorrent,
		ClientKind: ClientKindQBit, Category: "tv", State: StateResolving,
		SubmissionKey: "k2", DisplayName: "pack", InfoHash: &hash}
	for _, it := range []*Item{oldItem, newItem} {
		if err := s.CreateItem(ctx, it); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := s.SetFileProbe(ctx, "old1", "5", "argsv1", `{"streams":[]}`); err != nil {
		t.Fatalf("set probe: %v", err)
	}

	got, found, err := s.GetFileProbeByContent(ctx, hash, "5", "argsv1")
	if err != nil || !found || got != `{"streams":[]}` {
		t.Fatalf("content lookup: got=%q found=%v err=%v", got, found, err)
	}
	// different args or file must miss
	if _, found, _ := s.GetFileProbeByContent(ctx, hash, "5", "argsv2"); found {
		t.Fatal("args mismatch must miss")
	}
	if _, found, _ := s.GetFileProbeByContent(ctx, hash, "6", "argsv1"); found {
		t.Fatal("file mismatch must miss")
	}
}
