package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestPlaybackRepresentationIDUsesProvenCanonicalAlias(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/canonical.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	converger, _ := contentproof.NewConverger(store.New(db), contentproof.Options{})
	leftItem := &store.Item{ID: "torrent-item", SourceType: store.SourceTypeTorrent}
	rightItem := &store.Item{ID: "http-item", SourceType: store.SourceTypeHTTP}
	leftID := torrentCrossLaneRepresentationID(leftItem.ID, "0")
	rightID := httpRepresentationID(rightItem.ID, "file")
	left, err := contentproof.NewLaneCandidate(contentproof.LaneTorrent, leftItem.ID, "0", leftID, "Example.Release", 8)
	if err != nil {
		t.Fatal(err)
	}
	right, err := contentproof.NewLaneCandidate(contentproof.LaneHTTP, rightItem.ID, "file", rightID, "Example.Release", 8)
	if err != nil {
		t.Fatal(err)
	}
	span := contentproof.VerifiedSpan{Offset: 0, Length: 8}
	decision := contentproof.Decision{Relation: contentproof.RelationProven, Offset: 0, Length: 8}
	canonical, err := converger.ObserveVerified(ctx, left, right, span, decision)
	if err != nil || canonical == "" {
		t.Fatalf("convergence = %q, %v", canonical, err)
	}
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil)), representationConvergence: converger}
	if got := s.playbackRepresentationID(ctx, leftItem, "0"); got != canonical {
		t.Fatalf("torrent identity = %q, want canonical", got)
	}
	if got := s.playbackRepresentationID(ctx, rightItem, "file"); got != canonical {
		t.Fatalf("http identity = %q, want canonical", got)
	}
}
