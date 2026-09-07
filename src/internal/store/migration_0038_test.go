package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/pressly/goose/v3"
)

func TestMigration0038UpDownReUp(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/proposals.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM playback_proposals LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, "migrations", 37); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1 FROM playback_proposals LIMIT 1`); err == nil {
		t.Fatal("playback_proposals remains after down")
	}
	if err := goose.UpTo(db, "migrations", 38); err != nil {
		t.Fatal(err)
	}
}

func TestPlaybackProposalIdempotencyConcurrencyAndRestart(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/restart.db"
	db, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	repo := New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	proposal := playbackcoverage.Proposal{
		RepresentationID: "http:representation", ItemID: "item", FileID: "file",
		ExtentKind: playbackcoverage.ExtentDeclaredBytes, Delivered: 50, Total: 100,
		Threshold: 0.5, CreatedAt: now,
	}
	const workers = 16
	var wg sync.WaitGroup
	results := make(chan bool, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, createErr := repo.CreatePlaybackProposal(ctx, proposal)
			results <- created
			errs <- createErr
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	createdCount := 0
	for created := range results {
		if created {
			createdCount++
		}
	}
	for createErr := range errs {
		if createErr != nil {
			t.Fatal(createErr)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created=%d, want 1", createdCount)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	got, found, err := New(db).GetPlaybackProposal(ctx, proposal.RepresentationID)
	if err != nil || !found {
		t.Fatalf("restart proposal found=%v err=%v", found, err)
	}
	if got != proposal {
		t.Fatalf("restart proposal=%+v, want %+v", got, proposal)
	}
}

func TestPlaybackProposalRejectsMalformedEvidence(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/invalid.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	base := playbackcoverage.Proposal{
		RepresentationID: "http:representation", ItemID: "item", FileID: "file",
		ExtentKind: playbackcoverage.ExtentDeclaredBytes, Delivered: 50, Total: 100,
		Threshold: 0.5, CreatedAt: time.Now().UTC(),
	}
	cases := []playbackcoverage.Proposal{base, base, base}
	cases[0].ExtentKind = "guessed"
	cases[1].ItemID = "private?value"
	cases[2].Delivered = 0
	for _, proposal := range cases {
		if _, err := New(db).CreatePlaybackProposal(ctx, proposal); err == nil {
			t.Fatalf("accepted malformed proposal: %+v", proposal)
		}
	}
}
