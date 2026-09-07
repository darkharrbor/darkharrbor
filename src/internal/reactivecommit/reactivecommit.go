// Package reactivecommit owns RX-4.1's explicit proposal commit and undo.
package reactivecommit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/identitycheck"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const MaxEpisodes = 256

type Repository interface {
	GetPlaybackProposal(context.Context, string) (playbackcoverage.Proposal, bool, error)
	GetItemByID(context.Context, string) (*store.Item, error)
	ListContentProofs(context.Context, []string, time.Time) ([]contentproof.Evidence, error)
	ListContinuity(context.Context, string) ([]contentproof.ContinuityEntry, error)
	BeginReactiveCommit(context.Context, string, string, string, string, time.Time) (store.ReactiveCommit, bool, error)
	AbortReactiveCommit(context.Context, string) error
	ParkReactiveCommit(context.Context, string, string, time.Time) error
	CompleteReactiveCommit(context.Context, store.ReactiveCommit, time.Time) error
	GetReactiveCommit(context.Context, string) (store.ReactiveCommit, bool, error)
	BeginReactiveUndo(context.Context, string, time.Time) (store.ReactiveCommit, error)
	CompleteReactiveUndo(context.Context, string, time.Time) error
}

type Output interface {
	RemoveCommitted(context.Context, *store.Item) error
}

type Registrar interface {
	Commit(context.Context, CommitRequest) (Registration, error)
	Undo(context.Context, Registration) error
}

type CommitRequest struct {
	Identity        *store.ProviderIdentity
	SourcePath      string
	TargetName      string
	RootFolder      string
	QualityProfile  int
	MonitorEpisodes bool
	MonitorMovie    bool
}

type Registration struct {
	ArrName        string
	Kind           string
	ItemID         int
	FileIDs        []int
	OwnerCreated   bool
	PriorMonitored *bool
	Episodes       []store.ReactiveCommitEpisode
}

type Options struct {
	Now             func() time.Time
	MonitorEpisodes bool
	MonitorMovie    bool
}

type Service struct {
	repo      Repository
	registrar Registrar
	output    Output
	now       func() time.Time
	monitorEp bool
	monitorMv bool
}

func New(repo Repository, registrar Registrar, output Output, opts Options) (*Service, error) {
	if repo == nil || registrar == nil || output == nil {
		return nil, errors.New("reactivecommit: missing dependency")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{repo: repo, registrar: registrar, output: output, now: opts.Now, monitorEp: opts.MonitorEpisodes, monitorMv: opts.MonitorMovie}, nil
}

type Request struct {
	RepresentationID string
	Mode             string
	TargetName       string
	RootFolder       string
	QualityProfile   int
}

type Result struct {
	State          string
	ReviewRequired bool
	Reason         string
}

func (s *Service) Commit(ctx context.Context, req Request) (result Result, retErr error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if req.RepresentationID == "" || (req.Mode != "auto" && req.Mode != "supervised") {
		return Result{}, errors.New("reactivecommit: invalid request")
	}
	proposal, found, err := s.repo.GetPlaybackProposal(ctx, req.RepresentationID)
	if err != nil || !found {
		if err == nil {
			err = errors.New("reactivecommit: proposal not found")
		}
		return Result{}, err
	}
	record, created, err := s.repo.BeginReactiveCommit(ctx, proposal.RepresentationID, proposal.ItemID, proposal.FileID, req.Mode, s.now().UTC())
	if err != nil {
		return Result{}, err
	}
	if record.State == "committed" || record.State == "parked" {
		return Result{State: record.State, ReviewRequired: record.ReviewRequired, Reason: record.Reason}, nil
	}
	if !created {
		return Result{}, errors.New("reactivecommit: commit already in progress")
	}
	defer func() {
		if retErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		retErr = errors.Join(retErr, s.repo.AbortReactiveCommit(cleanupCtx, proposal.RepresentationID))
	}()
	item, err := s.repo.GetItemByID(ctx, proposal.ItemID)
	if err != nil {
		return Result{}, err
	}
	if item == nil || item.StrmPath == nil || *item.StrmPath == "" || item.Metadata.ProviderIdentity == nil {
		return Result{}, errors.New("reactivecommit: incomplete proposal identity or output")
	}
	if len(item.Metadata.ProviderIdentity.Episodes) > MaxEpisodes {
		return Result{}, errors.New("reactivecommit: episode bound exceeded")
	}
	if _, err := os.Stat(*item.StrmPath); err != nil {
		return Result{}, fmt.Errorf("reactivecommit: committed source unavailable: %w", err)
	}
	reason, strong, err := s.classify(ctx, proposal, item)
	if err != nil {
		return Result{}, err
	}
	if reason != "" {
		if err := s.repo.ParkReactiveCommit(ctx, proposal.RepresentationID, reason, s.now().UTC()); err != nil {
			return Result{}, err
		}
		return Result{State: "parked", Reason: reason}, nil
	}
	registration, err := s.registrar.Commit(ctx, CommitRequest{
		Identity: item.Metadata.ProviderIdentity, SourcePath: *item.StrmPath,
		TargetName: req.TargetName, RootFolder: req.RootFolder, QualityProfile: req.QualityProfile,
		MonitorEpisodes: s.monitorEp, MonitorMovie: s.monitorMv,
	})
	if err != nil {
		return Result{}, err
	}
	record.ArrName, record.ArrKind, record.ArrItemID = registration.ArrName, registration.Kind, registration.ItemID
	record.ArrFileIDs, record.Episodes, record.OwnerCreated, record.PriorMonitored = registration.FileIDs, registration.Episodes, registration.OwnerCreated, registration.PriorMonitored
	record.ReviewRequired = !strong
	if record.ReviewRequired {
		record.Reason = "weak_evidence"
	}
	if err := s.repo.CompleteReactiveCommit(ctx, record, s.now().UTC()); err != nil {
		_ = s.registrar.Undo(ctx, registration)
		return Result{}, err
	}
	return Result{State: "committed", ReviewRequired: record.ReviewRequired, Reason: record.Reason}, nil
}

func (s *Service) classify(ctx context.Context, proposal playbackcoverage.Proposal, item *store.Item) (mismatch string, strong bool, err error) {
	id := item.Metadata.ProviderIdentity
	facts := item.Metadata.MediaFacts
	if item.Metadata.MediaFactsFileID != "" && item.Metadata.MediaFactsFileID != proposal.FileID {
		facts = nil
	}
	if id.Kind == "series" && len(id.Episodes) > 0 {
		for _, ep := range id.Episodes {
			if verdicts := identitycheck.Verify(id, item.DisplayName, ep.Season, ep.Episode, facts, ""); len(verdicts) > 0 {
				return "identity_" + string(verdicts[0].Reason), false, nil
			}
		}
	} else if verdicts := identitycheck.Verify(id, item.DisplayName, 0, 0, facts, ""); len(verdicts) > 0 {
		return "identity_" + string(verdicts[0].Reason), false, nil
	}
	continuity, err := s.repo.ListContinuity(ctx, proposal.RepresentationID)
	if err != nil {
		return "", false, err
	}
	// RX-9.2 (RD-30 D6): park only on a POSITIVE contradiction. A bare
	// `tofu` mutation is a byte change under a retained identity that no
	// authoritative proof disagrees with -- the unknown case, which this
	// system resolves by abstaining everywhere else. Once RX-9.1 keys
	// aggregator playback by identity, a self-healed link is exactly such a
	// mutation, and parking it would defeat the automation RD-30 requires.
	// ContinuityConflict -- disagreement with an authoritative SHA-256 proof
	// -- still parks.
	for _, entry := range continuity {
		if entry.Relation == contentproof.ContinuityConflict {
			return "continuity_conflict", false, nil
		}
	}
	proofs, err := s.repo.ListContentProofs(ctx, []string{proposal.RepresentationID}, s.now().UTC())
	if err != nil {
		return "", false, err
	}
	for _, proof := range proofs {
		if proof.Kind != contentproof.KindAuthoritative {
			continue
		}
		switch proof.Provenance {
		case contentproof.ProvenancePAR2IFSC, contentproof.ProvenanceHTTPDigest,
			contentproof.ProvenanceTorrentMerkle, contentproof.ProvenanceIADigest:
			return "", true, nil
		}
	}
	return "", false, nil
}

func (s *Service) Undo(ctx context.Context, representationID string) error {
	if representationID == "" {
		return errors.New("reactivecommit: empty representation")
	}
	record, err := s.repo.BeginReactiveUndo(ctx, representationID, s.now().UTC())
	if err != nil {
		return err
	}
	if record.State == "undone" {
		return nil
	}
	registration := Registration{ArrName: record.ArrName, Kind: record.ArrKind, ItemID: record.ArrItemID, FileIDs: record.ArrFileIDs, OwnerCreated: record.OwnerCreated, PriorMonitored: record.PriorMonitored, Episodes: record.Episodes}
	if err := s.registrar.Undo(ctx, registration); err != nil {
		return err
	}
	item, err := s.repo.GetItemByID(ctx, record.ItemID)
	if err != nil {
		return err
	}
	if err := s.output.RemoveCommitted(ctx, item); err != nil {
		return err
	}
	return s.repo.CompleteReactiveUndo(ctx, representationID, s.now().UTC())
}
