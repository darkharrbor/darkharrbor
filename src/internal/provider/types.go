package provider

import (
	"errors"
)

// Shared provider result types. Relocated verbatim from internal/torbox at
// depends on these neutral types; internal/torbox re-exports them as
// transitional type aliases (removed at 11.feature_implementation_audit) and
// imports this package — never the reverse.

// CreateTaskResponse is the provider's answer to a submission.
type CreateTaskResponse struct {
	RemoteID    string
	QueuedID    string
	QueueAuthID string
	RemoteHash  string
	DisplayName string
}

// RemoteFile is a file entry as returned by provider task status.
type RemoteFile struct {
	FileID       string
	Name         string
	ShortName    string
	RelativePath string
	Size         int64
}

// CachedFile is a file entry from a checkcached response.
// RequestDLURL is empty at checkcached time; populated by the resolver
// when it calls the requestdl endpoint.
type CachedFile struct {
	FileID       string
	Name         string
	RelativePath string
	Size         int64
	RequestDLURL string
}

// CheckCachedResult is the result of a checkcached call.
type CheckCachedResult struct {
	Cached bool
	Files  []CachedFile
}

// TorrentOutcome is a provider-neutral classification of a torrent task.
type TorrentOutcome string

const (
	TorrentOutcomeUnknown            TorrentOutcome = "unknown"
	TorrentOutcomeReady              TorrentOutcome = "ready"
	TorrentOutcomeTerminalDeadSource TorrentOutcome = "terminal_dead_source"
	// TorrentOutcomeDeadSentinel is TorBox's own "checking, zero seeders,
	// 100-day never-will-complete ETA sentinel" signal. Unlike an explicit
	// provider failure state (TorrentOutcomeTerminalDeadSource -- "cannot be
	// completed"/"repair failed"/"incomplete"), this sentinel has been
	// observed live to appear within seconds of an uncached add and later
	// resolve successfully (2026-07-18 soak finding) -- it reflects "no
	// metadata/swarm found *yet*", not a confirmed-dead source. It gets its
	// own short grace period (HARRBOR_GOVERNOR_DEAD_SENTINEL_GRACE_MIN)
	// before being treated as terminal, distinct from both the instant
	// explicit-failure path and the longer generic stall timeout.
	TorrentOutcomeDeadSentinel    TorrentOutcome = "dead_sentinel_suspected"
	TorrentOutcomeTransientError  TorrentOutcome = "transient_error"
	TorrentOutcomeStalled         TorrentOutcome = "stalled"
	TorrentOutcomeCapacityLimited TorrentOutcome = "capacity_limited"
	TorrentOutcomeCancelled       TorrentOutcome = "cancelled"
	TorrentOutcomeRemoteRemoved   TorrentOutcome = "remote_removed"
)

// TorrentFailureCode is a stable core-facing reason code.
type TorrentFailureCode string

const (
	TorrentFailureNone                TorrentFailureCode = ""
	TorrentFailureTerminalDeadSource  TorrentFailureCode = "torrent_terminal_dead_source"
	TorrentFailureTransientStall      TorrentFailureCode = "torrent_transient_stall"
	TorrentFailureProviderUnavailable TorrentFailureCode = "torrent_provider_unavailable"
	TorrentFailureCapacityLimited     TorrentFailureCode = "torrent_capacity_limited"
	TorrentFailureCancelled           TorrentFailureCode = "torrent_cancelled"
	TorrentFailureRemoteRemoved       TorrentFailureCode = "torrent_remote_removed"
	TorrentFailureSubmitRejected      TorrentFailureCode = "torrent_submit_rejected"
	// TorrentFailureSubmitBudgetExceeded (TS-3.2, T4) is a DH-side verdict,
	// not a provider-reported one: an uncached submission made literally
	// zero download progress within the configured acceptance-window
	// budget. Distinct from TorrentFailureTransientStall (COR-11), which
	// only engages once the provider explicitly reports a stalled/no-swarm
	// state and deliberately never blacklists since later swarm recovery
	// remains possible -- a source that cannot even begin transferring
	// within a bounded acceptance window is treated as a stronger,
	// blacklist-worthy signal instead.
	TorrentFailureSubmitBudgetExceeded TorrentFailureCode = "torrent_submit_budget_exceeded"
)

// TaskStatus is the resolved state of a submitted item at the provider.
// Download-progress fields (BytesTotal, BytesDone, DownloadFinished) are
type TaskStatus struct {
	RemoteID        string
	QueuedID        string
	QueueAuthID     string
	Hash            string
	Name            string
	State           string
	Label           string
	Progress        float64
	Seeds           int   // active seeders; 0 means no swarm
	ETA             int64 // seconds to completion; TorBox sentinel 8640000 = never
	DownloadPresent bool
	DownloadReady   bool
	Outcome         TorrentOutcome
	FailureCode     TorrentFailureCode
	Failed          bool
	Stalled         bool // true when provider reports no active swarm (COR-11)
	Inactive        bool
	Error           string
	Files           []RemoteFile
	CachedFiles     []CachedFile
}

// RetryableError wraps an error to signal that the caller may retry.
type RetryableError struct {
	Err error
}

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

func MarkRetryable(err error) error {
	if err == nil {
		return nil
	}
	var retryable *RetryableError
	if errors.As(err, &retryable) {
		return err
	}
	return &RetryableError{Err: err}
}

func IsRetryable(err error) bool {
	var retryable *RetryableError
	return errors.As(err, &retryable)
}
