package reactivedispatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/reactivecommit"
	"github.com/darkharrbor/darkharrbor/internal/reactivequeue"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type testRepo struct {
	syncs      int
	entries    []store.ReactivePending
	proposals  map[string]playbackcoverage.Proposal
	items      map[string]*store.Item
	enqueued   []store.ReactivePending
	resolved   []string
	listLimits []int
}

func (r *testRepo) SyncReactivePending(ctx context.Context, _ time.Time) error {
	r.syncs++
	return ctx.Err()
}
func (r *testRepo) ListReactivePending(_ context.Context, after string, limit int) ([]store.ReactivePending, error) {
	r.listLimits = append(r.listLimits, limit)
	start := 0
	for start < len(r.entries) && r.entries[start].RepresentationID <= after {
		start++
	}
	end := min(start+limit, len(r.entries))
	return r.entries[start:end], nil
}

func TestAutoPagesPastUnchangedReviewEntries(t *testing.T) {
	repo := &testRepo{proposals: make(map[string]playbackcoverage.Proposal), items: make(map[string]*store.Item)}
	routes := make(map[string]reactivequeue.RouteResult)
	for i := 0; i < BatchSize+1; i++ {
		id := string(rune('a' + i))
		repo.entries = append(repo.entries, store.ReactivePending{RepresentationID: id, ItemID: id, Reason: "supervised_review"})
		repo.proposals[id] = playbackcoverage.Proposal{RepresentationID: id, ItemID: id}
		repo.items[id] = &store.Item{Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: id}}}}
		routes[id] = reactivequeue.RouteResult{Reason: "routing_unresolved", Tier: 2}
	}
	dispatcher, err := New("auto", repo, testRouter{routes: routes}, &testCommitter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := dispatcher.Process(t.Context())
	if err != nil || first.Examined != BatchSize {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := dispatcher.Process(t.Context())
	if err != nil || second.Examined != 1 || len(repo.listLimits) != 2 || repo.listLimits[0] != BatchSize || repo.listLimits[1] != BatchSize {
		t.Fatalf("second=%+v limits=%v err=%v", second, repo.listLimits, err)
	}
}
func (r *testRepo) GetPlaybackProposal(_ context.Context, id string) (playbackcoverage.Proposal, bool, error) {
	p, ok := r.proposals[id]
	return p, ok, nil
}
func (r *testRepo) GetItemByID(_ context.Context, id string) (*store.Item, error) {
	return r.items[id], nil
}
func (r *testRepo) EnqueueReactivePending(_ context.Context, entry store.ReactivePending, _ time.Time) error {
	r.enqueued = append(r.enqueued, entry)
	return nil
}
func (r *testRepo) ResolveReactivePending(_ context.Context, id, action, arr string, _ time.Time) error {
	r.resolved = append(r.resolved, id+":"+action+":"+arr)
	return nil
}

type testRouter struct {
	routes map[string]reactivequeue.RouteResult
	err    error
}

func (r testRouter) Resolve(_ context.Context, identity *store.ProviderIdentity) (reactivequeue.RouteResult, error) {
	if r.err != nil {
		return reactivequeue.RouteResult{}, r.err
	}
	return r.routes[identity.IDs.TMDB], nil
}

type testCommitter struct {
	results map[string]reactivecommit.Result
	errs    map[string]error
	calls   []reactivecommit.Request
}

func (c *testCommitter) Commit(_ context.Context, req reactivecommit.Request) (reactivecommit.Result, error) {
	c.calls = append(c.calls, req)
	if err := c.errs[req.RepresentationID]; err != nil {
		return reactivecommit.Result{}, err
	}
	return c.results[req.RepresentationID], nil
}

func TestOffAndSupervisedDoNotCommit(t *testing.T) {
	repo := &testRepo{}
	off, err := New("off", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := off.Process(t.Context()); err != nil || result != (Result{}) {
		t.Fatalf("off result=%+v err=%v", result, err)
	}
	supervised, err := New("supervised", repo, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervised.Process(t.Context()); err != nil || repo.syncs != 1 {
		t.Fatalf("supervised syncs=%d err=%v", repo.syncs, err)
	}
}

func TestAutoCommitsOwnedAndParksUnresolved(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	repo := &testRepo{
		entries: []store.ReactivePending{
			{RepresentationID: "owned", ItemID: "one", Reason: "supervised_review"},
			{RepresentationID: "weak", ItemID: "three", Reason: "supervised_review"},
			{RepresentationID: "absent", ItemID: "two", Reason: "supervised_review"},
		},
		proposals: map[string]playbackcoverage.Proposal{
			"owned":  {RepresentationID: "owned", ItemID: "one"},
			"weak":   {RepresentationID: "weak", ItemID: "three"},
			"absent": {RepresentationID: "absent", ItemID: "two"},
		},
		items: map[string]*store.Item{
			"one":   {Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "1"}}}},
			"two":   {Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "2"}}}},
			"three": {Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "3"}}}},
		},
	}
	committer := &testCommitter{results: map[string]reactivecommit.Result{
		"owned": {State: "committed"},
		"weak":  {State: "committed", ReviewRequired: true, Reason: "weak_evidence"},
	}}
	dispatcher, err := New("auto", repo, testRouter{routes: map[string]reactivequeue.RouteResult{
		"1": {ArrName: "radarr", Tier: 1, Owned: true},
		"2": {ArrName: "radarr", Tier: 0},
		"3": {ArrName: "radarr", Tier: 1, Owned: true},
	}}, committer, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Process(t.Context())
	if err != nil || result.Examined != 3 || result.Committed != 1 || result.Parked != 2 || result.Errors != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(committer.calls) != 2 || committer.calls[0].Mode != "auto" || committer.calls[1].Mode != "auto" || len(repo.resolved) != 1 || len(repo.enqueued) != 1 || repo.enqueued[0].Reason != "routing_unresolved" {
		t.Fatalf("calls=%+v resolved=%v enqueued=%+v", committer.calls, repo.resolved, repo.enqueued)
	}
}

// TestAutoParksIdentityAbsentInsteadOfErroring is RD-29's regression. A
// legacy item with no captured provider identity previously counted as a
// transient error and stayed in supervised_review, so the auto path
// re-examined it every cycle forever and never committed anything. It must
// now leave the review queue exactly once, under routing_unresolved, with
// no error and no commit attempt.
func TestAutoParksIdentityAbsentInsteadOfErroring(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	repo := &testRepo{
		entries: []store.ReactivePending{
			{RepresentationID: "noident", ItemID: "legacy", Reason: "supervised_review"},
		},
		proposals: map[string]playbackcoverage.Proposal{
			"noident": {RepresentationID: "noident", ItemID: "legacy"},
		},
		items: map[string]*store.Item{
			"legacy": {Metadata: store.SubmissionMetadata{}},
		},
	}
	committer := &testCommitter{results: map[string]reactivecommit.Result{}}
	dispatcher, err := New("auto", repo, testRouter{routes: map[string]reactivequeue.RouteResult{}}, committer, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Process(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Examined != 1 || result.Parked != 1 || result.Errors != 0 || result.Committed != 0 {
		t.Fatalf("identity-absent entry must park, not error: result=%+v", result)
	}
	if len(committer.calls) != 0 {
		t.Fatalf("must not attempt a commit without identity: calls=%+v", committer.calls)
	}
	if len(repo.enqueued) != 1 || repo.enqueued[0].Reason != "routing_unresolved" || repo.enqueued[0].RepresentationID != "noident" {
		t.Fatalf("enqueued=%+v", repo.enqueued)
	}
}

func TestAutoLogsRepresentationAndConcreteEntryError(t *testing.T) {
	repo := &testRepo{
		entries: []store.ReactivePending{
			{RepresentationID: "rep-router-failure", ItemID: "item", Reason: "supervised_review"},
		},
		proposals: map[string]playbackcoverage.Proposal{
			"rep-router-failure": {RepresentationID: "rep-router-failure", ItemID: "item"},
		},
		items: map[string]*store.Item{
			"item": {Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{
				Kind: "movie",
				IDs:  store.ProviderIDs{TMDB: "9"},
			}}},
		},
	}
	dispatcher, err := New("auto", repo, testRouter{err: errors.New("fixture route failure")}, &testCommitter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	dispatcher.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))

	result, err := dispatcher.Process(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Errors != 1 || result.Examined != 1 {
		t.Fatalf("result=%+v", result)
	}
	logged := buf.String()
	for _, want := range []string{"rep-router-failure", "resolve_route", "fixture route failure"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log %q missing %q", logged, want)
		}
	}
}

func TestAutoResolvesAlreadyPresentWithoutRetryError(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	repo := &testRepo{
		entries: []store.ReactivePending{{RepresentationID: "already", ItemID: "item", Reason: "supervised_review"}},
		proposals: map[string]playbackcoverage.Proposal{
			"already": {RepresentationID: "already", ItemID: "item"},
		},
		items: map[string]*store.Item{
			"item": {Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "9"}}}},
		},
	}
	committer := &testCommitter{errs: map[string]error{
		"already": errors.Join(errors.New("fixture wrapper"), reactivecommit.ErrArrFileAlreadyPresent),
	}}
	dispatcher, err := New("auto", repo, testRouter{routes: map[string]reactivequeue.RouteResult{
		"9": {ArrName: "radarr", Tier: 1, Owned: true},
	}}, committer, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Process(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Examined != 1 || result.Parked != 1 || result.Errors != 0 || result.Committed != 0 {
		t.Fatalf("result=%+v", result)
	}
	if len(repo.resolved) != 1 || repo.resolved[0] != "already:jellyfin_only:" {
		t.Fatalf("resolved=%v", repo.resolved)
	}
}

func TestCancellationStopsWithoutWork(t *testing.T) {
	dispatcher, err := New("supervised", &testRepo{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := dispatcher.Process(ctx); err == nil {
		t.Fatal("canceled process succeeded")
	}
}
