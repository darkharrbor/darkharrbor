package contentproof_test

import (
	"context"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestConvergenceRejectsUnprovenAndMismatchedCandidates(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir()+"/reject.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	converger, _ := contentproof.NewConverger(store.New(db), contentproof.Options{})
	left, _ := contentproof.NewLaneCandidate(contentproof.LaneTorrent, "left", "0", "torrent:left", "Release.One", 8)
	right, _ := contentproof.NewLaneCandidate(contentproof.LaneHTTP, "right", "0", "http:right", "Release.Two", 8)
	span := contentproof.VerifiedSpan{Offset: 0, Length: 8}
	if _, err := converger.ObserveVerified(context.Background(), left, right, span,
		contentproof.Decision{Relation: contentproof.RelationNoProof, Offset: 0, Length: 8}); err == nil {
		t.Fatal("unproven convergence accepted")
	}
	if _, err := converger.ObserveVerified(context.Background(), left, right, span,
		contentproof.Decision{Relation: contentproof.RelationProven, Offset: 0, Length: 8}); err == nil {
		t.Fatal("mismatched release convergence accepted")
	}
	if routes, err := converger.Routes(context.Background(), left.RepresentationID); err != nil || len(routes) != 0 {
		t.Fatalf("rejected convergence persisted routes: %d, %v", len(routes), err)
	}
}

func TestConvergenceCancellationLeavesNoState(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir()+"/cancel.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	converger, _ := contentproof.NewConverger(store.New(db), contentproof.Options{})
	left, _ := contentproof.NewLaneCandidate(contentproof.LaneTorrent, "left", "0", "torrent:left", "Release", 8)
	right, _ := contentproof.NewLaneCandidate(contentproof.LaneHTTP, "right", "0", "http:right", "Release", 8)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := converger.ObserveVerified(ctx, left, right, contentproof.VerifiedSpan{Offset: 0, Length: 8},
		contentproof.Decision{Relation: contentproof.RelationProven, Offset: 0, Length: 8}); err == nil {
		t.Fatal("cancelled convergence returned nil error")
	}
	if routes, _ := converger.Routes(context.Background(), left.RepresentationID); len(routes) != 0 {
		t.Fatal("cancelled convergence persisted routes")
	}
}
