// Package ladder implements SF-02: the minimal shared acquisition-ladder
// order/budget/lease/outcome contract.
//
// It does not implement any lane's actual rungs. The NNTP, torrent, and HTTP
// robustness plans each define their own concrete rung sequence and recovery
// semantics (NS-1.1, TS-2.1, HR3.1) -- what those rows share, and what this
// package supplies once so none of them reinvent it, is:
//
//   - a fixed, ordered walk over named rungs ("order");
//   - a bounded number of attempts per rung before advancing ("budget" --
//     this package does not implement backoff/scheduling, only the bound);
//   - lease acquisition through the existing HR1.4 accountgov.Governor
//     before each attempt, honoring its priority/fair-share admission
//     ("lease"); and
//   - an advance/retry/stop decision driven entirely by the existing SF-01
//     outcome vocabulary ("outcome") -- this package invents no new failure
//     taxonomy of its own.
//
// A rung's Attempt is responsible for performing its own real operation and
// returning an error already classified per SF-01 (via outcome.Permanent,
// outcome.Transient, outcome.AccountLevel, or by the error's own
// outcome.Classifier implementation) exactly as any other lane code does.
// Ladder does not know or care what kind of operation a rung performs.
package ladder

import (
	"context"
	"errors"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

// Attempt is one rung's unit of work. A nil return means success. Any
// non-nil error is classified via outcome.Classify to decide the ladder's
// next move.
type Attempt func(ctx context.Context) error

// Rung is one named, ordered step of a Ladder.
type Rung struct {
	// Name identifies the rung for accounting/logging (e.g. "cache",
	// "current", "reresolve", "repair"). Lane-owned meaning; ladder treats
	// it only as a label.
	Name string
	// Op is the accountgov operation-class name this rung's attempts lease
	// against. Rungs sharing the same Op contend through the same
	// Governor capacity and priority ordering; rungs with different Op
	// values are independent.
	Op string
	// Priority is the accountgov.Priority this rung's attempts request.
	Priority accountgov.Priority
	// MaxAttempts bounds retries of this exact rung when its outcome
	// classifies ClassTransientHere. A value <= 1 means exactly one
	// attempt (no bounded retry) -- the ladder's own advance-to-next-rung
	// behavior is the fallback in that case.
	MaxAttempts int
	// Attempt performs the rung's real operation.
	Attempt Attempt
}

// RungResult is one rung's accounting after a Run.
type RungResult struct {
	Name     string
	Attempts int
	// Class is the outcome.Class of the last attempt ("" on success).
	Class outcome.Class
	// Err is the last attempt's error (nil on success).
	Err error
}

// Result is the ladder's common accounting record for one Run.
type Result struct {
	Succeeded bool
	// StoppedAt is the rung name where the walk ended: the succeeding
	// rung, or the last rung tried before a permanent/account-level stop
	// or full exhaustion. Empty only if Run was called with zero rungs.
	StoppedAt string
	// Rungs records every rung actually attempted, in order.
	Rungs []RungResult
}

// ErrAccountLevel is returned by Run when a rung's outcome classifies
// ClassAccountLevel. The ladder stops immediately rather than trying
// further rungs: an account-level condition (429, quota exhaustion, no
// active slots, cooldown) is scoped to the account, not to one rung's
// source/path, so trying a different rung of the same account cannot help
// until the account-level limit clears.
var ErrAccountLevel = errors.New("ladder: account-level outcome, stopping walk")

// ErrExhausted is returned by Run when every configured rung was tried and
// none succeeded, with no permanent account-level stop along the way.
var ErrExhausted = errors.New("ladder: all rungs exhausted")

// Ladder is a fixed, ordered sequence of rungs, optionally sharing one
// accountgov.Governor for lease admission.
type Ladder struct {
	gov   *accountgov.Governor
	rungs []Rung
}

// New builds a Ladder over rungs, tried in the given order. gov may be nil,
// in which case Run performs no lease acquisition at all (an unbounded walk
// with no account concept -- e.g. a lane wired to a provider that has no
// accountgov.Governor of its own yet).
func New(gov *accountgov.Governor, rungs ...Rung) *Ladder {
	return &Ladder{gov: gov, rungs: rungs}
}

// Run walks the ladder's rungs in order for one session (used only for
// accountgov fair-share bookkeeping when a Governor is set; may be an item
// ID, or empty if the caller does not distinguish sessions).
//
// For each rung, Run repeats, up to the rung's MaxAttempts: acquire an
// accountgov lease for the rung's Op/Priority (skipped when the Ladder has
// no Governor), call Attempt, classify the result via outcome.Classify, and
// release the lease.
//
//   - success ("" class): Run returns immediately with Result.Succeeded
//     true.
//   - ClassPermanentHere: this exact rung is dead; retrying it cannot help.
//     Run advances to the next rung without retrying this one.
//   - ClassUnknown: SF-01 requires unknown outcomes to be fixed at their
//     classification site, not silently retried here, so Run treats an
//     unknown outcome the same as permanent -- advance, do not loop.
//   - ClassTransientHere: Run retries the SAME rung (re-acquiring a lease
//     each time) up to MaxAttempts before advancing to the next rung.
//   - ClassAccountLevel: Run stops the ENTIRE walk immediately and returns
//     ErrAccountLevel -- no later rung on this ladder is tried.
//
// If every rung is tried without success and no rung produced an
// account-level stop, Run returns ErrExhausted. A cancelled ctx aborts the
// current attempt and returns ctx.Err() immediately without trying further
// rungs.
func (l *Ladder) Run(ctx context.Context, session string) (Result, error) {
	res := Result{}
	for _, rung := range l.rungs {
		rr, class, err := l.runRung(ctx, rung, session)
		res.Rungs = append(res.Rungs, rr)

		// runRung only returns a non-nil error for a cancelled/expired ctx
		// or a failed lease acquisition (accountgov.Governor.Acquire itself
		// only errors on ctx cancellation) -- either way, abort the whole
		// walk immediately rather than trying further rungs.
		if err != nil {
			res.StoppedAt = rung.Name
			return res, err
		}
		if class == "" {
			res.Succeeded = true
			res.StoppedAt = rung.Name
			return res, nil
		}
		if class == outcome.ClassAccountLevel {
			res.StoppedAt = rung.Name
			return res, ErrAccountLevel
		}
		// ClassPermanentHere, ClassUnknown, or ClassTransientHere with
		// MaxAttempts exhausted: advance to the next rung.
	}
	return res, ErrExhausted
}

// runRung executes one rung's bounded-attempt loop.
func (l *Ladder) runRung(ctx context.Context, rung Rung, session string) (RungResult, outcome.Class, error) {
	max := rung.MaxAttempts
	if max < 1 {
		max = 1
	}
	rr := RungResult{Name: rung.Name}
	var lastClass outcome.Class

	for attempt := 1; attempt <= max; attempt++ {
		if err := ctx.Err(); err != nil {
			rr.Attempts = attempt - 1
			rr.Err = err
			return rr, "", err
		}

		var lease *accountgov.Lease
		if l.gov != nil {
			var lerr error
			lease, lerr = l.gov.Acquire(ctx, rung.Op, rung.Priority, session)
			if lerr != nil {
				rr.Attempts = attempt
				rr.Err = lerr
				return rr, "", lerr
			}
		}

		err := rung.Attempt(ctx)
		if lease != nil {
			lease.Release()
		}
		rr.Attempts = attempt

		if err == nil {
			rr.Class = ""
			rr.Err = nil
			return rr, "", nil
		}

		class := outcome.Classify(err)
		rr.Class = class
		rr.Err = err
		lastClass = class

		if class == outcome.ClassTransientHere && attempt < max {
			continue
		}
		break
	}
	return rr, lastClass, nil
}
