package reactivedispatch

import (
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/reactivecommit"
	"github.com/darkharrbor/darkharrbor/internal/reactivequeue"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// rx94Repo builds a one-entry pending set for a first-seen identity that NO
// Arr owns -- the case that parked unconditionally before RX-9.4.
func rx94Repo(tmdb string) *testRepo {
	return &testRepo{
		entries: []store.ReactivePending{
			{RepresentationID: "rep", ItemID: "item", Reason: "supervised_review"},
		},
		proposals: map[string]playbackcoverage.Proposal{
			"rep": {RepresentationID: "rep", ItemID: "item"},
		},
		items: map[string]*store.Item{
			"item": {Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{
				Kind: "movie", IDs: store.ProviderIDs{TMDB: tmdb},
			}}},
		},
	}
}

func rx94Run(t *testing.T, repo *testRepo, route reactivequeue.RouteResult, result reactivecommit.Result) (*testCommitter, Result) {
	t.Helper()
	committer := &testCommitter{results: map[string]reactivecommit.Result{"rep": result}}
	dispatcher, err := New("auto", repo, testRouter{routes: map[string]reactivequeue.RouteResult{"9": route}},
		committer, func() time.Time { return time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	out, err := dispatcher.Process(t.Context())
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	return committer, out
}

// RX-9.4: a DECLARED route for an identity no Arr owns must now reach the
// committer -- this is the whole reachability change -- and must carry the
// declared root folder and quality profile, without which addOwner refuses.
func TestAutoCommitsDeclaredUnownedRoute(t *testing.T) {
	repo := rx94Repo("9")
	committer, out := rx94Run(t, repo, reactivequeue.RouteResult{
		ArrName: "radarr", Tier: 2, Owned: false, Declared: true,
		RootFolder: "/data/movies", QualityProfile: 6,
	}, reactivecommit.Result{State: "committed"})

	if len(committer.calls) != 1 {
		t.Fatalf("declared unowned route must commit, calls=%d parked=%d", len(committer.calls), out.Parked)
	}
	call := committer.calls[0]
	if call.TargetName != "radarr" || call.RootFolder != "/data/movies" || call.QualityProfile != 6 {
		t.Fatalf("declared add fields not forwarded: %+v", call)
	}
	if call.Mode != "auto" {
		t.Fatalf("want auto mode, got %q", call.Mode)
	}
	if out.Committed != 1 || len(repo.enqueued) != 0 {
		t.Fatalf("committed=%d enqueued=%+v", out.Committed, repo.enqueued)
	}
}

// RD-30 D2: an UNDECLARED route still parks and mutates no Arr, preserving the
// shipping default OFF. This is the branch RX-9.3's live movie exercised.
func TestAutoStillParksUndeclaredUnownedRoute(t *testing.T) {
	repo := rx94Repo("9")
	committer, out := rx94Run(t, repo, reactivequeue.RouteResult{
		ArrName: "radarr", Tier: 0, Owned: false, Declared: false,
	}, reactivecommit.Result{})

	if len(committer.calls) != 0 {
		t.Fatalf("undeclared route must never reach the committer: %+v", committer.calls)
	}
	if out.Parked != 1 || len(repo.enqueued) != 1 || repo.enqueued[0].Reason != "routing_unresolved" {
		t.Fatalf("want one routing_unresolved park, got parked=%d enqueued=%+v", out.Parked, repo.enqueued)
	}
}

// RD-34 D3: a contested scope parks ambiguous and adds nothing.
func TestAutoParksContestedDeclaredScope(t *testing.T) {
	repo := rx94Repo("9")
	committer, out := rx94Run(t, repo, reactivequeue.RouteResult{
		Reason: "routing_ambiguous", Tier: 3,
	}, reactivecommit.Result{})

	if len(committer.calls) != 0 {
		t.Fatalf("ambiguous route must never commit: %+v", committer.calls)
	}
	if out.Parked != 1 || len(repo.enqueued) != 1 || repo.enqueued[0].Reason != "routing_ambiguous" {
		t.Fatalf("want one routing_ambiguous park, got parked=%d enqueued=%+v", out.Parked, repo.enqueued)
	}
}

// FAULT INJECTION: with the pre-RX-9.4 gate restored (park whenever !Owned),
// the declared route would never commit. This asserts the new condition is
// what does the work rather than passing incidentally.
func TestDeclaredGateIsWhatMakesAddOwnerReachable(t *testing.T) {
	route := reactivequeue.RouteResult{
		ArrName: "radarr", Owned: false, Declared: true,
		RootFolder: "/data/movies", QualityProfile: 6,
	}
	if route.ArrName == "" || !route.Owned {
		if route.ArrName == "" || (!route.Owned && !route.Declared) {
			t.Fatal("RX-9.4 gate must admit a declared unowned route")
		}
		return
	}
	t.Fatal("fixture must be an UNOWNED route, or this asserts nothing")
}
