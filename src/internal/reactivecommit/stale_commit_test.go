package reactivecommit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// inProgressRepo holds an existing 'committing' row updated at heldSince.
type inProgressRepo struct {
	abortRepo
	heldSince time.Time
	aborts    int
	begins    int
}

func (r *inProgressRepo) BeginReactiveCommit(context.Context, string, string, string, string, time.Time) (store.ReactiveCommit, bool, error) {
	r.begins++
	if r.aborts > 0 {
		return store.ReactiveCommit{RepresentationID: r.proposal.RepresentationID, State: "committing"}, true, nil
	}
	return store.ReactiveCommit{RepresentationID: r.proposal.RepresentationID, State: "committing", UpdatedAt: r.heldSince}, false, nil
}
func (r *inProgressRepo) AbortReactiveCommit(context.Context, string) error {
	r.aborts++
	return nil
}

func TestCommitInProgressIsTypedWhileFresh(t *testing.T) {
	now := time.Date(2026, 10, 17, 19, 0, 0, 0, time.UTC)
	repo := &inProgressRepo{heldSince: now.Add(-time.Minute)}
	repo.proposal = playbackcoverage.Proposal{RepresentationID: "nntp:rep", ItemID: "item", FileID: "file"}
	svc, err := New(repo, unusedRegistrar{}, unusedOutput{}, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Commit(context.Background(), Request{RepresentationID: "nntp:rep", Mode: "auto"})
	if !errors.Is(err, ErrCommitInProgress) {
		t.Fatalf("err=%v, want ErrCommitInProgress", err)
	}
	if repo.aborts != 0 {
		t.Fatalf("fresh in-flight commit was reclaimed (aborts=%d)", repo.aborts)
	}
}

func TestCommitReclaimsOrphanedRow(t *testing.T) {
	now := time.Date(2026, 10, 17, 19, 0, 0, 0, time.UTC)
	repo := &inProgressRepo{heldSince: now.Add(-StaleCommitAfter - time.Minute)}
	repo.proposal = playbackcoverage.Proposal{RepresentationID: "nntp:rep", ItemID: "item", FileID: "file"}
	repo.item = &store.Item{DisplayName: "x"} // incomplete: commit fails after reclaim, which is fine here
	svc, err := New(repo, unusedRegistrar{}, unusedOutput{}, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Commit(context.Background(), Request{RepresentationID: "nntp:rep", Mode: "auto"})
	if errors.Is(err, ErrCommitInProgress) {
		t.Fatal("orphaned committing row was not reclaimed")
	}
	if repo.begins != 2 || repo.aborts < 1 {
		t.Fatalf("begins=%d aborts=%d, want reclaim then re-begin", repo.begins, repo.aborts)
	}
}
