// Package reactivedispatch owns RX-5.2's bounded runtime proposal consumption.
package reactivedispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/reactivecommit"
	"github.com/darkharrbor/darkharrbor/internal/reactivequeue"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const BatchSize = 25

type Repository interface {
	SyncReactivePending(context.Context, time.Time) error
	ListReactivePending(context.Context, string, int) ([]store.ReactivePending, error)
	GetPlaybackProposal(context.Context, string) (playbackcoverage.Proposal, bool, error)
	GetItemByID(context.Context, string) (*store.Item, error)
	EnqueueReactivePending(context.Context, store.ReactivePending, time.Time) error
	ResolveReactivePending(context.Context, string, string, string, time.Time) error
}

type Router interface {
	Resolve(context.Context, *store.ProviderIdentity) (reactivequeue.RouteResult, error)
}

type Committer interface {
	Commit(context.Context, reactivecommit.Request) (reactivecommit.Result, error)
}

type Dispatcher struct {
	mode      string
	repo      Repository
	router    Router
	committer Committer
	log       *slog.Logger
	now       func() time.Time
	cursorMu  sync.Mutex
	after     string
}

type Result struct {
	Examined  int
	Committed int
	Parked    int
	Errors    int
}

// SetLogger installs the daemon logger used for per-entry automatic-dispatch
// failures. Production wires this so every counted error identifies both the
// failing representation and the concrete failure rather than only a cycle
// total.
func (d *Dispatcher) SetLogger(log *slog.Logger) {
	d.log = log
}

func (d *Dispatcher) recordError(result *Result, representationID, stage string, err error) {
	result.Errors++
	if err == nil {
		err = errors.New("reactivedispatch: unspecified entry failure")
	}
	if d.log != nil {
		d.log.Error("reactive automatic dispatch entry failed",
			"representation_id", representationID,
			"stage", stage,
			"error", err)
	}
}

func New(mode string, repo Repository, router Router, committer Committer, now func() time.Time) (*Dispatcher, error) {
	if mode != "off" && mode != "supervised" && mode != "auto" {
		return nil, errors.New("reactivedispatch: invalid mode")
	}
	if mode != "off" && repo == nil {
		return nil, errors.New("reactivedispatch: missing repository")
	}
	if mode == "auto" && (router == nil || committer == nil) {
		return nil, errors.New("reactivedispatch: missing automatic dependency")
	}
	if now == nil {
		now = time.Now
	}
	return &Dispatcher{mode: mode, repo: repo, router: router, committer: committer, now: now}, nil
}

// Process runs at most one bounded page. Per-item faults abstain and remain in
// the existing pending queue; cancellation stops the page immediately.
func (d *Dispatcher) Process(ctx context.Context) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if d.mode == "off" {
		return Result{}, nil
	}
	if err := d.repo.SyncReactivePending(ctx, d.now().UTC()); err != nil {
		return Result{}, err
	}
	if d.mode == "supervised" {
		return Result{}, nil
	}
	d.cursorMu.Lock()
	after := d.after
	d.cursorMu.Unlock()
	entries, err := d.repo.ListReactivePending(ctx, after, BatchSize)
	if err != nil {
		return Result{}, err
	}
	if len(entries) == 0 && after != "" {
		after = ""
		entries, err = d.repo.ListReactivePending(ctx, after, BatchSize)
		if err != nil {
			return Result{}, err
		}
	}
	if len(entries) == BatchSize {
		after = entries[len(entries)-1].RepresentationID
	} else {
		after = ""
	}
	d.cursorMu.Lock()
	d.after = after
	d.cursorMu.Unlock()
	var result Result
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if entry.Reason != "supervised_review" {
			continue
		}
		result.Examined++
		proposal, found, err := d.repo.GetPlaybackProposal(ctx, entry.RepresentationID)
		if err != nil {
			d.recordError(&result, entry.RepresentationID, "get_playback_proposal", err)
			continue
		}
		if !found {
			d.recordError(&result, entry.RepresentationID, "get_playback_proposal",
				errors.New("reactivedispatch: playback proposal not found"))
			continue
		}
		item, err := d.repo.GetItemByID(ctx, proposal.ItemID)
		if err != nil {
			d.recordError(&result, entry.RepresentationID, "get_item", err)
			continue
		}
		if item == nil {
			d.recordError(&result, entry.RepresentationID, "get_item",
				errors.New("reactivedispatch: item not found"))
			continue
		}
		// RD-29: an item carrying no captured provider identity can never be
		// routed, because that identity is the router's only input. Counting
		// it as a transient error left the entry in supervised_review to be
		// re-examined every cycle forever with no operator-visible signal --
		// a legacy pre-ID-04 item silently jammed the auto path. Park it
		// under the reason the router itself already returns for an
		// unresolvable route, reusing this function's existing park branch
		// verbatim: no new schema, no new pending vocabulary, and the entry
		// stays available for operator assignment through RX-4.2's queue.
		if item.Metadata.ProviderIdentity == nil {
			if err := d.repo.EnqueueReactivePending(ctx, store.ReactivePending{
				RepresentationID: entry.RepresentationID,
				ItemID:           entry.ItemID,
				Reason:           "routing_unresolved",
			}, d.now().UTC()); err != nil {
				d.recordError(&result, entry.RepresentationID, "park_identity_absent", err)
			} else {
				result.Parked++
			}
			continue
		}
		route, err := d.router.Resolve(ctx, item.Metadata.ProviderIdentity)
		if err != nil {
			d.recordError(&result, entry.RepresentationID, "resolve_route", err)
			continue
		}
		// RX-9.4 (RD-30 D1/D2/D3, RD-34): `!route.Owned` was the single
		// condition standing between automatic dispatch and `addOwner`, so a
		// first-seen identity could never commit. A route the router selected
		// from a target's DECLARED scope now proceeds and ADDS the owner; an
		// undeclared or contested one still parks under the reason the router
		// already returns, preserving the shipping default OFF.
		if route.ArrName == "" || (!route.Owned && !route.Declared) {
			reason := route.Reason
			if reason == "" {
				reason = "routing_unresolved"
			}
			if err := d.repo.EnqueueReactivePending(ctx, store.ReactivePending{
				RepresentationID: entry.RepresentationID,
				ItemID:           entry.ItemID,
				Reason:           reason,
			}, d.now().UTC()); err != nil {
				d.recordError(&result, entry.RepresentationID, "park_route", err)
			} else {
				result.Parked++
			}
			continue
		}
		commitResult, err := d.committer.Commit(ctx, reactivecommit.Request{
			RepresentationID: entry.RepresentationID,
			Mode:             "auto",
			TargetName:       route.ArrName,
			RootFolder:       route.RootFolder,
			QualityProfile:   route.QualityProfile,
		})
		if err != nil {
			// A resolved Arr owner that already has the exact file is a
			// permanent, non-mutating refusal rather than a transient dispatch
			// failure. Resolve the stale supervised-review entry to the existing
			// jellyfin-only action so it cannot retry every cycle forever. Every
			// other commit error remains pending for retry and is logged.
			if errors.Is(err, reactivecommit.ErrArrFileAlreadyPresent) {
				if err := d.repo.ResolveReactivePending(ctx, entry.RepresentationID, "jellyfin_only", "", d.now().UTC()); err != nil {
					d.recordError(&result, entry.RepresentationID, "resolve_already_present", err)
				} else {
					result.Parked++
				}
				continue
			}
			d.recordError(&result, entry.RepresentationID, "commit", err)
			continue
		}
		switch commitResult.State {
		case "committed":
			if commitResult.ReviewRequired {
				result.Parked++
				continue
			}
			if err := d.repo.ResolveReactivePending(ctx, entry.RepresentationID, "approve", route.ArrName, d.now().UTC()); err != nil {
				d.recordError(&result, entry.RepresentationID, "resolve_pending", err)
			} else {
				result.Committed++
			}
		case "parked", "pending":
			result.Parked++
		default:
			d.recordError(&result, entry.RepresentationID, "commit_result",
				fmt.Errorf("reactivedispatch: unexpected commit state %q", commitResult.State))
		}
	}
	return result, nil
}
