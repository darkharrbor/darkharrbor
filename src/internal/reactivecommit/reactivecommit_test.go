package reactivecommit

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type classifyRepo struct {
	proofs     []contentproof.Evidence
	continuity []contentproof.ContinuityEntry
}

func (r *classifyRepo) ListContentProofs(context.Context, []string, time.Time) ([]contentproof.Evidence, error) {
	return r.proofs, nil
}
func (r *classifyRepo) ListContinuity(context.Context, string) ([]contentproof.ContinuityEntry, error) {
	return r.continuity, nil
}
func (*classifyRepo) GetPlaybackProposal(context.Context, string) (playbackcoverage.Proposal, bool, error) {
	panic("unused")
}
func (*classifyRepo) GetItemByID(context.Context, string) (*store.Item, error) { panic("unused") }
func (*classifyRepo) BeginReactiveCommit(context.Context, string, string, string, string, time.Time) (store.ReactiveCommit, bool, error) {
	panic("unused")
}
func (*classifyRepo) AbortReactiveCommit(context.Context, string) error { panic("unused") }
func (*classifyRepo) ParkReactiveCommit(context.Context, string, string, time.Time) error {
	panic("unused")
}
func (*classifyRepo) CompleteReactiveCommit(context.Context, store.ReactiveCommit, time.Time) error {
	panic("unused")
}
func (*classifyRepo) GetReactiveCommit(context.Context, string) (store.ReactiveCommit, bool, error) {
	panic("unused")
}
func (*classifyRepo) BeginReactiveUndo(context.Context, string, time.Time) (store.ReactiveCommit, error) {
	panic("unused")
}
func (*classifyRepo) CompleteReactiveUndo(context.Context, string, time.Time) error { panic("unused") }

type unusedRegistrar struct{}

func (unusedRegistrar) Commit(context.Context, CommitRequest) (Registration, error) { panic("unused") }
func (unusedRegistrar) Undo(context.Context, Registration) error                    { panic("unused") }

type unusedOutput struct{}

func (unusedOutput) RemoveCommitted(context.Context, *store.Item) error { panic("unused") }

type abortRepo struct {
	classifyRepo
	proposal  playbackcoverage.Proposal
	item      *store.Item
	aborted   bool
	parked    string
	completed store.ReactiveCommit
}

func (r *abortRepo) GetPlaybackProposal(context.Context, string) (playbackcoverage.Proposal, bool, error) {
	return r.proposal, true, nil
}
func (r *abortRepo) GetItemByID(context.Context, string) (*store.Item, error) { return r.item, nil }
func (r *abortRepo) BeginReactiveCommit(context.Context, string, string, string, string, time.Time) (store.ReactiveCommit, bool, error) {
	return store.ReactiveCommit{RepresentationID: r.proposal.RepresentationID, State: "committing"}, true, nil
}
func (r *abortRepo) AbortReactiveCommit(context.Context, string) error {
	r.aborted = true
	return nil
}
func (r *abortRepo) ParkReactiveCommit(_ context.Context, _ string, reason string, _ time.Time) error {
	r.parked = reason
	return nil
}
func (r *abortRepo) CompleteReactiveCommit(_ context.Context, record store.ReactiveCommit, _ time.Time) error {
	r.completed = record
	return nil
}

type failingRegistrar struct{}

func (failingRegistrar) Commit(context.Context, CommitRequest) (Registration, error) {
	return Registration{}, errors.New("registration failed")
}
func (failingRegistrar) Undo(context.Context, Registration) error { panic("unused") }

type recordingRegistrar struct{ commits int }

func (r *recordingRegistrar) Commit(context.Context, CommitRequest) (Registration, error) {
	r.commits++
	return Registration{ArrName: "classic", Kind: "movie", ItemID: 1}, nil
}
func (*recordingRegistrar) Undo(context.Context, Registration) error { panic("unused") }

func TestAutoNonStrongEvidenceCommitsFlagged(t *testing.T) {
	tests := []struct {
		name   string
		proofs []contentproof.Evidence
	}{
		{name: "absent"},
		{name: "same origin", proofs: []contentproof.Evidence{{
			RepresentationID: "http:representation", Kind: contentproof.KindSameOrigin,
			Provenance: contentproof.ProvenanceSameOriginETag,
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := t.TempDir() + "/candidate.strm"
			if err := os.WriteFile(path, []byte("opaque"), 0600); err != nil {
				t.Fatal(err)
			}
			repo := &abortRepo{
				classifyRepo: classifyRepo{proofs: tt.proofs},
				proposal:     playbackcoverage.Proposal{RepresentationID: "http:representation", ItemID: "item", FileID: "file"},
				item: &store.Item{DisplayName: "Movie.2024.WEB-DL", StrmPath: &path, Metadata: store.SubmissionMetadata{
					ProviderIdentity: &store.ProviderIdentity{Kind: "movie", Year: 2024, IDs: store.ProviderIDs{TMDB: "42"}},
				}},
			}
			registrar := &recordingRegistrar{}
			service, err := New(repo, registrar, unusedOutput{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.Commit(t.Context(), Request{RepresentationID: repo.proposal.RepresentationID, Mode: "auto", TargetName: "classic"})
			if err != nil || result.State != "committed" || !result.ReviewRequired || result.Reason != "weak_evidence" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if repo.aborted || registrar.commits != 1 || !repo.completed.ReviewRequired || repo.completed.Reason != "weak_evidence" {
				t.Fatalf("aborted=%v commits=%d completed=%+v", repo.aborted, registrar.commits, repo.completed)
			}
		})
	}
}

func TestAutoPositiveContradictionParksBeforeArrMutation(t *testing.T) {
	path := t.TempDir() + "/candidate.strm"
	if err := os.WriteFile(path, []byte("opaque"), 0600); err != nil {
		t.Fatal(err)
	}
	repo := &abortRepo{
		proposal: playbackcoverage.Proposal{RepresentationID: "http:representation", ItemID: "item", FileID: "file"},
		item: &store.Item{DisplayName: "Movie.2023.WEB-DL", StrmPath: &path, Metadata: store.SubmissionMetadata{
			ProviderIdentity: &store.ProviderIdentity{Kind: "movie", Year: 2024, IDs: store.ProviderIDs{TMDB: "42"}},
		}},
	}
	registrar := &recordingRegistrar{}
	service, err := New(repo, registrar, unusedOutput{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Commit(t.Context(), Request{RepresentationID: repo.proposal.RepresentationID, Mode: "auto", TargetName: "classic"})
	if err != nil || result.State != "parked" || result.Reason != "identity_year_mismatch" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if repo.parked != "identity_year_mismatch" || registrar.commits != 0 {
		t.Fatalf("parked=%q commits=%d", repo.parked, registrar.commits)
	}
}

func TestCommitAbortsDurableAttemptAfterRegistrationFailure(t *testing.T) {
	path := t.TempDir() + "/candidate.strm"
	if err := os.WriteFile(path, []byte("opaque"), 0600); err != nil {
		t.Fatal(err)
	}
	repo := &abortRepo{
		proposal: playbackcoverage.Proposal{RepresentationID: "http:representation", ItemID: "item", FileID: "file"},
		item: &store.Item{DisplayName: "Movie.2024.WEB-DL", StrmPath: &path, Metadata: store.SubmissionMetadata{
			ProviderIdentity: &store.ProviderIdentity{Kind: "movie", Year: 2024},
		}},
	}
	service, err := New(repo, failingRegistrar{}, unusedOutput{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Commit(context.Background(), Request{RepresentationID: repo.proposal.RepresentationID, Mode: "supervised"}); err == nil {
		t.Fatal("registration failure accepted")
	}
	if !repo.aborted {
		t.Fatal("durable committing attempt was not aborted")
	}
}

func TestClassifyStrongWeakAndConflict(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	proposal := playbackcoverage.Proposal{RepresentationID: "rep", FileID: "file"}
	item := &store.Item{DisplayName: "Movie.2024.WEB-DL", Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", Year: 2024}}}
	tests := []struct {
		name     string
		repo     *classifyRepo
		mismatch string
		strong   bool
	}{
		{"weak", &classifyRepo{}, "", false},
		{"strong", &classifyRepo{proofs: []contentproof.Evidence{{RepresentationID: "rep", Kind: contentproof.KindAuthoritative, Provenance: contentproof.ProvenanceHTTPDigest}}}, "", true},
		{"conflict", &classifyRepo{continuity: []contentproof.ContinuityEntry{{Relation: contentproof.ContinuityConflict}}}, "continuity_conflict", false},
		// RX-9.2 (RD-30 D6) INTENTIONALLY REVERSES this expectation. A bare
		// `tofu` mutation is a byte change under a retained identity that no
		// authoritative proof contradicts -- the unknown case. Once RX-9.1
		// keys aggregator playback by identity, a self-healed link is exactly
		// this, and parking it would defeat RD-30's required automation.
		{"tofu-mutation-heals-silently", &classifyRepo{continuity: []contentproof.ContinuityEntry{{Relation: contentproof.ContinuityTOFU, Mutation: true}}}, "", false},
		// A mutation that DISAGREES with an authoritative proof is a positive
		// contradiction and must still park.
		{"mutation-conflicting-with-proof", &classifyRepo{continuity: []contentproof.ContinuityEntry{{Relation: contentproof.ContinuityConflict, Mutation: true}}}, "continuity_conflict", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, err := New(tt.repo, unusedRegistrar{}, unusedOutput{}, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			mismatch, strong, err := svc.classify(context.Background(), proposal, item)
			if err != nil || mismatch != tt.mismatch || strong != tt.strong {
				t.Fatalf("mismatch=%q strong=%v err=%v", mismatch, strong, err)
			}
		})
	}
}

func TestClassifyPositiveIdentityMismatchParks(t *testing.T) {
	repo := &classifyRepo{}
	svc, _ := New(repo, unusedRegistrar{}, unusedOutput{}, Options{})
	item := &store.Item{DisplayName: "Wrong.2023.WEB-DL", Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", Year: 2024}}}
	mismatch, strong, err := svc.classify(context.Background(), playbackcoverage.Proposal{RepresentationID: "rep", FileID: "file"}, item)
	if err != nil || mismatch != "identity_year_mismatch" || strong {
		t.Fatalf("mismatch=%q strong=%v err=%v", mismatch, strong, err)
	}
}
