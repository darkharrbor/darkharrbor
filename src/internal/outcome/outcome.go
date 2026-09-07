// Package outcome implements SF-01: one shared, secret-safe attempt/failure/
// outcome vocabulary for DarkHarrbor's NNTP, torrent, and HTTP lanes.
//
// This package does not replace any lane's own detailed classification —
// httpstream.ErrClass, provider.TorrentOutcome/TorrentFailureCode, and nntp's
// sentinel errors (e.g. ErrDeadPost) remain the lane-owned, detailed reason
// used for control flow and retry decisions. Class is a coarser bucket used
// only for shared observability: rate-limited logging, summary counters, and
// the "zero unknown outcomes over a soak window" closure gate. Existing
// persisted event history (store.ItemEvent messages) is untouched by this
// package; it is additive.
package outcome

import (
	"errors"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

// Class is the shared SF-01 vocabulary bucket for an attempt/failure outcome.
type Class string

const (
	// ClassPermanentHere means retrying this exact source/path will not help
	// (e.g. a dead/removed post, a terminally dead torrent source, a
	// structurally invalid resolve key). The caller should not retry the same
	// rung; it should fail the item, blacklist, or move to the next rung.
	ClassPermanentHere Class = "permanent_here"
	// ClassTransientHere means a bounded retry of this same source/path may
	// succeed (e.g. a single dropped connection, a momentary backend 5xx).
	ClassTransientHere Class = "transient_here"
	// ClassAccountLevel means the failure is scoped to the provider/account
	// (429, quota exhaustion, cooldown, no active slots) rather than to this
	// specific source. Retrying the same source immediately will not help;
	// the account-level limit must clear first.
	ClassAccountLevel Class = "account_level"
	// ClassUnknown marks an error that did not map into any of the three
	// buckets above. SF-01 closure requires zero ClassUnknown outcomes over
	// its required soak window — every occurrence is a taxonomy gap to fix,
	// not an accepted steady-state value.
	ClassUnknown Class = "unknown"
)

// Classifier is implemented by an error type that already knows its own
// shared-vocabulary bucket. A lane adds this method to its own error type
// (see httpstream.Error.OutcomeClass) rather than teaching this package
// about the lane's internals, so outcome never imports a lane package that
// itself needs to import outcome (avoiding an import cycle).
type Classifier interface {
	OutcomeClass() Class
}

// Classify maps any error into the shared vocabulary. It walks the error
// chain for a Classifier first; failing that, it recognizes
// provider.RetryableError (the existing torrent-lane bounded-retry marker) as
// ClassTransientHere. Anything else — including nil-adjacent sentinel errors
// a lane has not (yet) wrapped with Permanent/Transient/AccountLevel below —
// is ClassUnknown. Classify(nil) returns "" (not a failure).
func Classify(err error) Class {
	if err == nil {
		return ""
	}
	var c Classifier
	if errors.As(err, &c) {
		return c.OutcomeClass()
	}
	if provider.IsRetryable(err) {
		return ClassTransientHere
	}
	return ClassUnknown
}

// classified wraps an error with an explicit shared-vocabulary bucket without
// requiring the wrapped error's own type to implement Classifier. Unwrap
// returns the original error unchanged, so existing errors.Is/errors.As
// checks against sentinel values (e.g. errors.Is(err, nntp.ErrDeadPost))
// keep working on a classified error exactly as they did before wrapping.
type classified struct {
	err   error
	class Class
}

func (c *classified) Error() string       { return c.err.Error() }
func (c *classified) Unwrap() error       { return c.err }
func (c *classified) OutcomeClass() Class { return c.class }

// Permanent wraps err as ClassPermanentHere. err's own errors.Is/As identity
// is preserved via Unwrap.
func Permanent(err error) error { return wrap(err, ClassPermanentHere) }

// Transient wraps err as ClassTransientHere. err's own errors.Is/As identity
// is preserved via Unwrap.
func Transient(err error) error { return wrap(err, ClassTransientHere) }

// AccountLevel wraps err as ClassAccountLevel. err's own errors.Is/As
// identity is preserved via Unwrap.
func AccountLevel(err error) error { return wrap(err, ClassAccountLevel) }

func wrap(err error, class Class) error {
	if err == nil {
		return nil
	}
	return &classified{err: err, class: class}
}

// ClassifyTorrentOutcome maps the existing provider-neutral
// provider.TorrentOutcome (torbox, realdebrid, and future debrid adapters)
// into the shared vocabulary. provider.TorrentOutcomeReady is not a failure
// and returns "". Consuming lanes call this to log/observe a TaskStatus
// without duplicating the mapping; it does not change TaskStatus, Outcome,
// or any provider-lane behavior.
func ClassifyTorrentOutcome(o provider.TorrentOutcome) Class {
	switch o {
	case "", provider.TorrentOutcomeReady:
		return ""
	case provider.TorrentOutcomeTerminalDeadSource,
		provider.TorrentOutcomeRemoteRemoved,
		provider.TorrentOutcomeCancelled:
		return ClassPermanentHere
	case provider.TorrentOutcomeTransientError,
		provider.TorrentOutcomeStalled,
		provider.TorrentOutcomeDeadSentinel:
		// TorrentOutcomeDeadSentinel (b442c86, 2026-07-18) is TorBox's own
		// ambiguous-but-not-yet-definitive signal, deliberately given its own
		// short grace period rather than treated as instantly dead -- exactly
		// the transient_here contract (a bounded retry/wait may still
		// succeed), not permanent_here.
		return ClassTransientHere
	case provider.TorrentOutcomeCapacityLimited:
		return ClassAccountLevel
	default:
		return ClassUnknown
	}
}
