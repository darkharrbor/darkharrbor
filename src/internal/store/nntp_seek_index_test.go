package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
)

func TestSetNNTPSeekIndexPersistsWithoutChangingItemTime(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "seek.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	at := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	item := &Item{
		ID: "item", PublicID: "public", SourceType: SourceTypeNZB, ClientKind: ClientKindSAB,
		Category: "tv", State: StateReady, SubmissionKey: "submission", DisplayName: "episode",
		CreatedAt: at, UpdatedAt: at, Metadata: SubmissionMetadata{NNTPZeroFillCount: 2},
	}
	if err := s.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	index := mediatruth.Index{
		Source: mediatruth.IndexSourceMatroskaCues,
		Entries: []mediatruth.Entry{
			{TimeMS: 0, Offset: 1024},
			{TimeMS: 5000, Offset: 8192},
		},
		Complete:  true,
		Declared:  2,
		Estimated: true,
	}
	if err := s.SetNNTPSeekIndex(ctx, item.ID, "rar|item|12", index); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.NNTPSeekIndex == nil || got.Metadata.NNTPSeekIndex.Entries[1].Offset != 8192 {
		t.Fatalf("persisted index = %#v", got.Metadata.NNTPSeekIndex)
	}
	if got.Metadata.NNTPSeekSourceKey != "rar|item|12" || got.Metadata.NNTPZeroFillCount != 2 {
		t.Fatalf("metadata changed unexpectedly: %#v", got.Metadata)
	}
	if !got.UpdatedAt.Equal(at) {
		t.Fatalf("updated_at changed: got %s want %s", got.UpdatedAt, at)
	}

	restarted := New(db)
	again, err := restarted.GetItemByID(ctx, item.ID)
	if err != nil || again.Metadata.NNTPSeekIndex == nil || len(again.Metadata.NNTPSeekIndex.Entries) != 2 {
		t.Fatalf("restart read: item=%#v err=%v", again, err)
	}
}

func TestSetNNTPSeekIndexRejectsMalformedAndURLKey(t *testing.T) {
	s := &Store{}
	index := mediatruth.Index{Source: mediatruth.IndexSourceMatroskaCues, Entries: []mediatruth.Entry{{TimeMS: 5, Offset: 10}, {TimeMS: 4, Offset: 11}}}
	if err := s.SetNNTPSeekIndex(context.Background(), "item", "nntp|item", index); err == nil {
		t.Fatal("non-monotonic index accepted")
	}
	index.Entries = index.Entries[:1]
	if err := s.SetNNTPSeekIndex(context.Background(), "item", "https://source.invalid", index); err == nil {
		t.Fatal("URL-shaped source key accepted")
	}
	index.Source = "https://source.invalid"
	if err := s.SetNNTPSeekIndex(context.Background(), "item", "nntp|item", index); err == nil {
		t.Fatal("unknown index source accepted")
	}
}
