// Package reactivequeue owns RX-4.2's single durable operator-decision queue.
package reactivequeue

import (
	"context"
	"errors"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/reactivecommit"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type Repository interface {
	SyncReactivePending(context.Context, time.Time) error
	ListReactivePending(context.Context, string, int) ([]store.ReactivePending, error)
	GetReactivePending(context.Context, string) (store.ReactivePending, bool, error)
	EnqueueReactivePending(context.Context, store.ReactivePending, time.Time) error
	ResolveReactivePending(context.Context, string, string, string, time.Time) error
	GetPlaybackProposal(context.Context, string) (playbackcoverage.Proposal, bool, error)
	GetItemByID(context.Context, string) (*store.Item, error)
}

type Committer interface {
	Commit(context.Context, reactivecommit.Request) (reactivecommit.Result, error)
	Undo(context.Context, string) error
}

type Output interface {
	RemoveCommitted(context.Context, *store.Item) error
}

type Manager struct {
	repo      Repository
	committer Committer
	output    Output
	now       func() time.Time
}

type Options struct{ Now func() time.Time }

type Decision struct {
	RepresentationID string
	Action           string
	ArrName          string
	RootFolder       string
	QualityProfile   int
}

func New(repo Repository, committer Committer, output Output, opts Options) (*Manager, error) {
	if repo == nil || committer == nil || output == nil {
		return nil, errors.New("reactivequeue: missing dependency")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Manager{repo: repo, committer: committer, output: output, now: opts.Now}, nil
}

func (m *Manager) List(ctx context.Context, after string, limit int) ([]store.ReactivePending, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.repo.SyncReactivePending(ctx, m.now().UTC()); err != nil {
		return nil, err
	}
	return m.repo.ListReactivePending(ctx, after, limit)
}

func (m *Manager) Decide(ctx context.Context, decision Decision) (reactivecommit.Result, error) {
	if err := ctx.Err(); err != nil {
		return reactivecommit.Result{}, err
	}
	if err := m.repo.SyncReactivePending(ctx, m.now().UTC()); err != nil {
		return reactivecommit.Result{}, err
	}
	entry, found, err := m.repo.GetReactivePending(ctx, decision.RepresentationID)
	if err != nil || !found || entry.State != "pending" {
		if err == nil {
			err = errors.New("reactivequeue: pending entry unavailable")
		}
		return reactivecommit.Result{}, err
	}
	switch decision.Action {
	case "approve", "assign":
		if decision.Action == "assign" && decision.ArrName == "" {
			return reactivecommit.Result{}, errors.New("reactivequeue: assignment requires Arr target")
		}
		result, err := m.committer.Commit(ctx, reactivecommit.Request{
			RepresentationID: decision.RepresentationID, Mode: "supervised", TargetName: decision.ArrName,
			RootFolder: decision.RootFolder, QualityProfile: decision.QualityProfile,
		})
		if err != nil {
			return reactivecommit.Result{}, err
		}
		if result.State == "parked" {
			err = m.repo.EnqueueReactivePending(ctx, store.ReactivePending{RepresentationID: entry.RepresentationID, ItemID: entry.ItemID, Reason: "positive_mismatch"}, m.now().UTC())
			return result, err
		}
		if result.ReviewRequired {
			err = m.repo.EnqueueReactivePending(ctx, store.ReactivePending{RepresentationID: entry.RepresentationID, ItemID: entry.ItemID, Reason: "weak_evidence"}, m.now().UTC())
			return result, err
		}
		return result, m.repo.ResolveReactivePending(ctx, entry.RepresentationID, decision.Action, decision.ArrName, m.now().UTC())
	case "jellyfin_only", "acknowledge":
		return reactivecommit.Result{State: "resolved"}, m.repo.ResolveReactivePending(ctx, entry.RepresentationID, decision.Action, decision.ArrName, m.now().UTC())
	case "remove":
		proposal, found, err := m.repo.GetPlaybackProposal(ctx, entry.RepresentationID)
		if err != nil || !found {
			if err == nil {
				err = errors.New("reactivequeue: proposal unavailable")
			}
			return reactivecommit.Result{}, err
		}
		item, err := m.repo.GetItemByID(ctx, proposal.ItemID)
		if err != nil || item == nil {
			if err == nil {
				err = errors.New("reactivequeue: item unavailable")
			}
			return reactivecommit.Result{}, err
		}
		if err := m.output.RemoveCommitted(ctx, item); err != nil {
			return reactivecommit.Result{}, err
		}
		return reactivecommit.Result{State: "resolved"}, m.repo.ResolveReactivePending(ctx, entry.RepresentationID, decision.Action, "", m.now().UTC())
	case "undo":
		if err := m.committer.Undo(ctx, entry.RepresentationID); err != nil {
			return reactivecommit.Result{}, err
		}
		return reactivecommit.Result{State: "undone"}, m.repo.ResolveReactivePending(ctx, entry.RepresentationID, decision.Action, "", m.now().UTC())
	default:
		return reactivecommit.Result{}, errors.New("reactivequeue: invalid decision")
	}
}

func Actions(reason string) []string {
	switch reason {
	case "supervised_review":
		return []string{"approve", "assign", "jellyfin_only", "remove"}
	case "weak_evidence":
		return []string{"acknowledge", "undo"}
	case "positive_mismatch":
		return []string{"jellyfin_only", "remove"}
	case "routing_unresolved", "routing_ambiguous":
		return []string{"assign", "jellyfin_only", "remove"}
	}
	return nil
}
