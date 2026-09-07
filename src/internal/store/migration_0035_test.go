package store

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/pressly/goose/v3"
)

func TestMigration0035UpDownReUp(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/convergence.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM representation_routes LIMIT 1`); err != nil {
		t.Fatalf("representation_routes missing after up: %v", err)
	}
	if err := goose.DownTo(db, "migrations", 34); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM representation_routes LIMIT 1`); err == nil {
		t.Fatal("representation_routes remains after down")
	}
	if err := goose.UpTo(db, "migrations", 35); err != nil {
		t.Fatal(err)
	}
}

func TestRepresentationConvergenceRestartAndAliasContinuity(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/restart.db"
	db, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	converger, err := contentproof.NewConverger(st, contentproof.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	left := convergenceCandidate(t, contentproof.LaneTorrent, "torrent-item", "0", "torrent:left", 16)
	right := convergenceCandidate(t, contentproof.LaneHTTP, "http-item", "file", "http:right", 16)
	decision := contentproof.Decision{Relation: contentproof.RelationProven, Offset: 0, Length: 8}
	if canonical, err := converger.ObserveVerified(ctx, left, right, contentproof.VerifiedSpan{Offset: 0, Length: 8}, decision); err != nil || canonical != "" {
		t.Fatalf("partial convergence = %q, %v", canonical, err)
	}
	decision.Offset = 8
	if canonical, err := converger.ObserveVerified(ctx, left, right, contentproof.VerifiedSpan{Offset: 8, Length: 8}, decision); err != nil || canonical == "" {
		t.Fatalf("complete convergence = %q, %v", canonical, err)
	}
	leftCanonical, leftFound, err := converger.CanonicalRepresentationID(ctx, left.RepresentationID)
	if err != nil || !leftFound {
		t.Fatalf("left canonical identity = %q, %v, %v", leftCanonical, leftFound, err)
	}
	rightCanonical, rightFound, err := converger.CanonicalRepresentationID(ctx, right.RepresentationID)
	if err != nil || !rightFound || rightCanonical != leftCanonical {
		t.Fatalf("right canonical identity = %q, %v, %v; left=%q", rightCanonical, rightFound, err, leftCanonical)
	}
	ledger, _ := contentproof.NewLedger(st, contentproof.Options{Now: func() time.Time { return now }})
	digest := sha256.Sum256([]byte("same-span"))
	if _, err := ledger.Observe(ctx, contentproof.Observation{RepresentationID: left.RepresentationID, Offset: 0, Length: 9, Digest: digest[:]}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	entry, err := ledger.Observe(ctx, contentproof.Observation{RepresentationID: right.RepresentationID, Offset: 0, Length: 9, Digest: digest[:]})
	if err != nil || entry.Mutation {
		t.Fatalf("aliased observation = %+v, %v", entry, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	restarted := New(db)
	converger, _ = contentproof.NewConverger(restarted, contentproof.Options{})
	routes, err := converger.Routes(ctx, left.RepresentationID)
	if err != nil || len(routes) != 2 {
		t.Fatalf("restart routes = %d, %v", len(routes), err)
	}
	graph, _ := contentproof.New(restarted, contentproof.Options{})
	comparison, err := graph.Compare(ctx, left.RepresentationID, right.RepresentationID, 4, 8)
	if err != nil || comparison.Relation != contentproof.RelationProven {
		t.Fatalf("canonical comparison = %+v, %v", comparison, err)
	}
	ledger, _ = contentproof.NewLedger(restarted, contentproof.Options{})
	history, err := ledger.History(ctx, right.RepresentationID)
	if err != nil || len(history) != 2 {
		t.Fatalf("aliased history = %d, %v", len(history), err)
	}
}

func TestRepresentationConvergenceMergesTransitiveGroups(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/merge.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	converger, _ := contentproof.NewConverger(New(db), contentproof.Options{})
	candidates := []contentproof.LaneCandidate{
		convergenceCandidate(t, contentproof.LaneTorrent, "torrent-a", "0", "torrent:a", 8),
		convergenceCandidate(t, contentproof.LaneHTTP, "http-a", "0", "http:a", 8),
		convergenceCandidate(t, contentproof.LaneNNTP, "nntp-a", "0", "nntp:a", 8),
		convergenceCandidate(t, contentproof.LaneTorrent, "torrent-b", "0", "torrent:b", 8),
	}
	decision := contentproof.Decision{Relation: contentproof.RelationProven, Offset: 0, Length: 8}
	span := contentproof.VerifiedSpan{Offset: 0, Length: 8}
	for _, pair := range [][2]int{{0, 1}, {2, 3}, {1, 2}} {
		if _, err := converger.ObserveVerified(ctx, candidates[pair[0]], candidates[pair[1]], span, decision); err != nil {
			t.Fatal(err)
		}
	}
	for _, candidate := range candidates {
		routes, err := converger.Routes(ctx, candidate.RepresentationID)
		if err != nil || len(routes) != len(candidates) {
			t.Fatalf("routes for %s = %d, %v", candidate.RepresentationID, len(routes), err)
		}
	}
	var groups int
	if err := db.QueryRow(`SELECT COUNT(*) FROM canonical_representations`).Scan(&groups); err != nil || groups != 1 {
		t.Fatalf("canonical groups = %d, %v", groups, err)
	}
}

func TestRepresentationConvergenceConcurrentObservationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/concurrent.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	converger, _ := contentproof.NewConverger(New(db), contentproof.Options{})
	left := convergenceCandidate(t, contentproof.LaneTorrent, "torrent", "0", "torrent:concurrent", 8)
	right := convergenceCandidate(t, contentproof.LaneHTTP, "http", "0", "http:concurrent", 8)
	span := contentproof.VerifiedSpan{Offset: 0, Length: 8}
	decision := contentproof.Decision{Relation: contentproof.RelationProven, Offset: 0, Length: 8}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, observeErr := converger.ObserveVerified(ctx, left, right, span, decision)
			errs <- observeErr
		}()
	}
	wg.Wait()
	close(errs)
	for observeErr := range errs {
		if observeErr != nil {
			t.Fatal(observeErr)
		}
	}
	routes, err := converger.Routes(ctx, left.RepresentationID)
	if err != nil || len(routes) != 2 {
		t.Fatalf("routes = %d, %v", len(routes), err)
	}
}

func convergenceCandidate(t *testing.T, lane contentproof.Lane, itemID, fileID, representationID string, size int64) contentproof.LaneCandidate {
	t.Helper()
	candidate, err := contentproof.NewLaneCandidate(lane, itemID, fileID, representationID, "Example.Release", size)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}
