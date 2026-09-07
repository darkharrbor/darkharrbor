package reactivequeue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/reactivecommit"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type queueRepo struct {
	entry      store.ReactivePending
	proposal   playbackcoverage.Proposal
	item       *store.Item
	enqueued   string
	resolved   string
	resolvedTo string
}

func (r *queueRepo) SyncReactivePending(ctx context.Context, _ time.Time) error { return ctx.Err() }
func (r *queueRepo) ListReactivePending(context.Context, string, int) ([]store.ReactivePending, error) {
	return []store.ReactivePending{r.entry}, nil
}
func (r *queueRepo) GetReactivePending(context.Context, string) (store.ReactivePending, bool, error) {
	return r.entry, r.entry.RepresentationID != "", nil
}
func (r *queueRepo) EnqueueReactivePending(_ context.Context, entry store.ReactivePending, _ time.Time) error {
	r.enqueued = entry.Reason
	return nil
}
func (r *queueRepo) ResolveReactivePending(_ context.Context, _ string, action, arrName string, _ time.Time) error {
	r.resolved, r.resolvedTo = action, arrName
	return nil
}
func (r *queueRepo) GetPlaybackProposal(context.Context, string) (playbackcoverage.Proposal, bool, error) {
	return r.proposal, r.proposal.RepresentationID != "", nil
}
func (r *queueRepo) GetItemByID(context.Context, string) (*store.Item, error) { return r.item, nil }

type queueCommitter struct {
	result reactivecommit.Result
	err    error
	undo   bool
}

func (c *queueCommitter) Commit(context.Context, reactivecommit.Request) (reactivecommit.Result, error) {
	return c.result, c.err
}
func (c *queueCommitter) Undo(context.Context, string) error { c.undo = true; return c.err }

type queueOutput struct{ removed bool }

func (o *queueOutput) RemoveCommitted(context.Context, *store.Item) error {
	o.removed = true
	return nil
}

func TestManagerDecisionTransitions(t *testing.T) {
	tests := []struct {
		name         string
		action       string
		result       reactivecommit.Result
		wantEnqueue  string
		wantResolved string
		wantRemoved  bool
		wantUndo     bool
	}{
		{name: "strong approve", action: "approve", result: reactivecommit.Result{State: "committed"}, wantResolved: "approve"},
		{name: "weak approve", action: "approve", result: reactivecommit.Result{State: "committed", ReviewRequired: true}, wantEnqueue: "weak_evidence"},
		{name: "mismatch park", action: "approve", result: reactivecommit.Result{State: "parked"}, wantEnqueue: "positive_mismatch"},
		{name: "jellyfin only", action: "jellyfin_only", wantResolved: "jellyfin_only"},
		{name: "remove", action: "remove", wantResolved: "remove", wantRemoved: true},
		{name: "undo", action: "undo", wantResolved: "undo", wantUndo: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := &queueRepo{
				entry:    store.ReactivePending{RepresentationID: "http:pending", ItemID: "item", Reason: "supervised_review", State: "pending"},
				proposal: playbackcoverage.Proposal{RepresentationID: "http:pending", ItemID: "item"},
				item:     &store.Item{ID: "item"},
			}
			committer := &queueCommitter{result: tc.result}
			out := &queueOutput{}
			manager, err := New(repo, committer, out, Options{Now: func() time.Time { return time.Unix(1, 0) }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Decide(t.Context(), Decision{RepresentationID: "http:pending", Action: tc.action, ArrName: "radarr"}); err != nil {
				t.Fatal(err)
			}
			if repo.enqueued != tc.wantEnqueue || repo.resolved != tc.wantResolved || out.removed != tc.wantRemoved || committer.undo != tc.wantUndo {
				t.Fatalf("enqueue=%q resolved=%q removed=%v undo=%v", repo.enqueued, repo.resolved, out.removed, committer.undo)
			}
		})
	}
}

func TestManagerFailsClosedOnCancellationAndCommitFailure(t *testing.T) {
	repo := &queueRepo{entry: store.ReactivePending{RepresentationID: "http:pending", State: "pending"}}
	committer := &queueCommitter{err: errors.New("bounded failure")}
	manager, err := New(repo, committer, &queueOutput{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Decide(t.Context(), Decision{RepresentationID: "http:pending", Action: "approve"}); err == nil || repo.resolved != "" {
		t.Fatalf("commit failure err=%v resolved=%q", err, repo.resolved)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := manager.List(canceled, "", 1); err == nil {
		t.Fatal("canceled list succeeded")
	}
	if got := Actions("routing_ambiguous"); len(got) != 3 || got[0] != "assign" {
		t.Fatalf("actions=%v", got)
	}
}
