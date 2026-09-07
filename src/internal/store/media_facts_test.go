package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
)

func TestSetMediaFactsPersistsWithoutChangingItemTimeOrUnrelatedMetadata(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "facts.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	at := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	item := &Item{
		ID: "item", PublicID: "public", SourceType: SourceTypeTorrent, ClientKind: ClientKindQBit,
		Category: "movies", State: StateReady, SubmissionKey: "submission", DisplayName: "movie",
		CreatedAt: at, UpdatedAt: at, Metadata: SubmissionMetadata{NNTPZeroFillCount: 3},
	}
	if err := s.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	facts := mediatruth.Facts{
		Container: mediatruth.ContainerMatroska, DurationMS: 5_400_000, BitRate: 8_000_000,
		VideoCodec: "hevc", Width: 3840, Height: 2160, AudioCodec: "eac3",
		AudioLangs: []string{"eng"}, SizeBytes: 12_000_000_000,
	}
	if err := s.SetMediaFacts(ctx, item.ID, "0", "torrent|item|0", facts); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.MediaFacts == nil || got.Metadata.MediaFacts.VideoCodec != "hevc" || got.Metadata.MediaFacts.Width != 3840 {
		t.Fatalf("persisted facts = %#v", got.Metadata.MediaFacts)
	}
	if got.Metadata.MediaFactsFileID != "0" || got.Metadata.MediaFactsSourceKey != "torrent|item|0" {
		t.Fatalf("file/source key not persisted: %#v", got.Metadata)
	}
	// Unrelated metadata field written before this call must survive
	// unchanged -- SetMediaFacts must be a targeted json_set, not a
	// clobbering full-row rewrite.
	if got.Metadata.NNTPZeroFillCount != 3 {
		t.Fatalf("unrelated metadata changed: %#v", got.Metadata)
	}
	if !got.UpdatedAt.Equal(at) {
		t.Fatalf("updated_at changed: got %s want %s", got.UpdatedAt, at)
	}

	restarted := New(db)
	again, err := restarted.GetItemByID(ctx, item.ID)
	if err != nil || again.Metadata.MediaFacts == nil || again.Metadata.MediaFacts.AudioCodec != "eac3" {
		t.Fatalf("restart read: item=%#v err=%v", again, err)
	}
}

func TestSetMediaFactsRejectsEmptyOrMalformed(t *testing.T) {
	s := &Store{}
	if err := s.SetMediaFacts(context.Background(), "", "0", "key", mediatruth.Facts{VideoCodec: "h264"}); err == nil {
		t.Fatal("empty item id accepted")
	}
	if err := s.SetMediaFacts(context.Background(), "item", "", "key", mediatruth.Facts{VideoCodec: "h264"}); err == nil {
		t.Fatal("empty file id accepted")
	}
	if err := s.SetMediaFacts(context.Background(), "item", "0", "https://source.invalid", mediatruth.Facts{VideoCodec: "h264"}); err == nil {
		t.Fatal("URL-shaped source key accepted")
	}
	if err := s.SetMediaFacts(context.Background(), "item", "0", "torrent://item/0", mediatruth.Facts{VideoCodec: "h264"}); err == nil {
		t.Fatal("scheme-shaped source key accepted even with an otherwise-plausible prefix")
	}
	if err := s.SetMediaFacts(context.Background(), "item", "0", "key", mediatruth.Facts{}); err == nil {
		t.Fatal("empty facts accepted")
	}
}

// TestSetMediaFactsAcceptsRealTorrentLaneSourceKeyShape proves the exact
// live-gate-caught defect stays fixed: the real torrent-lane key shape
// ("torrent|" + item.ID + "/" + fileID, api.ResolveKey) contains a bare "/"
// and must be accepted -- it is not a "://" URL.
func TestSetMediaFactsAcceptsRealTorrentLaneSourceKeyShape(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "facts3.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	at := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	item := &Item{
		ID: "ef6e9b6aef7827d3fb75886a59a39794", PublicID: "pub", SourceType: SourceTypeTorrent, ClientKind: ClientKindQBit,
		Category: "movies", State: StateReady, SubmissionKey: "sub", DisplayName: "movie",
		CreatedAt: at, UpdatedAt: at,
	}
	if err := s.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	sourceKey := "torrent|" + item.ID + "/2"
	if err := s.SetMediaFacts(ctx, item.ID, "2", sourceKey, mediatruth.Facts{VideoCodec: "hevc", Width: 1920, Height: 1080}); err != nil {
		t.Fatalf("real torrent-lane source key shape rejected: %v", err)
	}
	got, err := s.GetItemByID(ctx, item.ID)
	if err != nil || got.Metadata.MediaFactsSourceKey != sourceKey {
		t.Fatalf("source key not persisted: got=%#v err=%v", got, err)
	}
}

func TestSetMediaFactsMissingItemIsError(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "facts2.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	err = s.SetMediaFacts(ctx, "does-not-exist", "0", "key", mediatruth.Facts{VideoCodec: "h264"})
	if err == nil {
		t.Fatal("expected error persisting facts for a missing item")
	}
}
