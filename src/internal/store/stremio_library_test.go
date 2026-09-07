package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newStremioLibraryTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "stremio.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

func addStremioLibraryItem(t *testing.T, st *Store, id, state, kind, imdb string, season, episode int, streamURL string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	identity := &ProviderIdentity{Kind: kind, Title: "Library title", IDs: ProviderIDs{IMDB: imdb}}
	if kind == "series" {
		identity.Episodes = []EpisodeProviderIDs{{Season: season, Episode: episode}}
	}
	item := &Item{
		ID: id, PublicID: id, SourceType: SourceTypeHTTP, ClientKind: ClientKindQBit,
		Category: "movies", State: StateReady, SubmissionKey: id, DisplayName: id,
		Metadata: SubmissionMetadata{ProviderIdentity: identity}, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if streamURL != "" {
		if err := st.UpsertStrmBlobAt(ctx, id, 0, id+"/video.strm", streamURL); err != nil {
			t.Fatal(err)
		}
	}
	if state == "ready" {
		return
	}
	record, _, err := st.BeginReactiveCommit(ctx, "rep:"+id, id, "0", "supervised", now)
	if err != nil {
		t.Fatal(err)
	}
	if state == "parked" {
		if err := st.ParkReactiveCommit(ctx, record.RepresentationID, "identity_runtime_mismatch", now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		return
	}
	record.ArrName, record.ArrKind, record.ArrItemID, record.ArrFileIDs = "arr", kind, 1, []int{1}
	if err := st.CompleteReactiveCommit(ctx, record, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if state == "undone" {
		if _, err := st.BeginReactiveUndo(ctx, record.RepresentationID, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := st.CompleteReactiveUndo(ctx, record.RepresentationID, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
	} else if state == "undoing" {
		if _, err := st.BeginReactiveUndo(ctx, record.RepresentationID, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListStremioLibraryStreamsExactAndLifecycleBounded(t *testing.T) {
	st := newStremioLibraryTestStore(t)
	addStremioLibraryItem(t, st, "movie", "committed", "movie", "tt0111161", 0, 0, "http://dh.invalid/stream/movie/0")
	addStremioLibraryItem(t, st, "ready", "ready", "movie", "tt0111161", 0, 0, "http://dh.invalid/stream/ready/0")
	addStremioLibraryItem(t, st, "undoing", "undoing", "movie", "tt0111161", 0, 0, "http://dh.invalid/stream/undoing/0")
	addStremioLibraryItem(t, st, "undone", "undone", "movie", "tt0111161", 0, 0, "http://dh.invalid/stream/undone/0")
	addStremioLibraryItem(t, st, "episode", "committed", "series", "tt0903747", 5, 14, "http://dh.invalid/stream/episode/0")
	addStremioLibraryItem(t, st, "parked", "parked", "series", "tt0903747", 5, 14, "http://dh.invalid/stream/parked/0")

	movies, err := st.ListStremioLibraryStreams(context.Background(), "movie", "tt0111161", 0, 0, MaxStremioLibraryStreams)
	if err != nil || len(movies) != 2 {
		t.Fatalf("movies=%v err=%v", movies, err)
	}
	seen := make(map[string]bool, len(movies))
	for _, movie := range movies {
		seen[movie.ItemID] = true
		if movie.Title != "Library title" || movie.RelPath == "" || movie.BlobCount != 1 || movie.EpisodeCount != 0 {
			t.Fatalf("movie metadata=%+v", movie)
		}
	}
	if !seen["movie"] || !seen["ready"] || seen["undoing"] || seen["undone"] {
		t.Fatalf("movie lifecycle set=%v", seen)
	}
	episodes, err := st.ListStremioLibraryStreams(context.Background(), "series", "tt0903747", 5, 14, MaxStremioLibraryStreams)
	if err != nil || len(episodes) != 1 {
		t.Fatalf("episodes=%v err=%v", episodes, err)
	}
	if episodes[0].ItemID != "episode" || episodes[0].EpisodeCount != 1 || episodes[0].BlobCount != 1 {
		t.Fatalf("episode metadata=%+v", episodes[0])
	}
	none, err := st.ListStremioLibraryStreams(context.Background(), "series", "tt0903747", 5, 15, MaxStremioLibraryStreams)
	if err != nil || len(none) != 0 {
		t.Fatalf("wrong episode returned streams=%v err=%v", none, err)
	}
}

func TestListStremioLibraryStreamsCancellationAndBounds(t *testing.T) {
	st := newStremioLibraryTestStore(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.ListStremioLibraryStreams(canceled, "movie", "tt0111161", 0, 0, 1); err == nil {
		t.Fatal("canceled query succeeded")
	}
	if _, err := st.ListStremioLibraryStreams(context.Background(), "movie", "tt0111161", 0, 0, MaxStremioLibraryStreams+1); err == nil {
		t.Fatal("oversized query succeeded")
	}
}
