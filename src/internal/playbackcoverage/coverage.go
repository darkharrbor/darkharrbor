// Package playbackcoverage owns RX-3.1's durable unions of byte and native
// VOD HLS media-time intervals successfully delivered to clients.
package playbackcoverage

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
)

const (
	MaxRepresentationIDBytes = 256
	MaxTargetIDBytes         = 256
	DefaultMaxIntervals      = 4096
	// Coverage is durable evidence of bytes already delivered to a viewer.
	// Give it the same bounded SQLite admission window as other streaming
	// metadata writers so transient use of the store's single connection does
	// not discard a completed observation before a transaction can begin.
	DefaultWriteTimeout = 5 * time.Second
)

type ExtentKind string

const (
	ExtentDeclaredBytes ExtentKind = "declared_bytes"
	ExtentMediaTruth    ExtentKind = "media_truth_bytes"
	ExtentVODHLS        ExtentKind = "vod_hls_duration"
)

type Target struct {
	ItemID string
	FileID string
	Kind   ExtentKind
	Total  int64
}

type Proposal struct {
	RepresentationID string
	ItemID           string
	FileID           string
	ExtentKind       ExtentKind
	Delivered        int64
	Total            int64
	Threshold        float64
	CreatedAt        time.Time
}

type Interval struct {
	Start int64
	End   int64
}

type Snapshot struct {
	RepresentationID string
	Intervals        []Interval
	DeliveredBytes   int64
	UpdatedAt        time.Time
}

type MediaSnapshot struct {
	RepresentationID string
	Intervals        []Interval
	DeliveredNS      int64
	UpdatedAt        time.Time
}

type Repository interface {
	MergePlaybackCoverage(context.Context, string, Interval, time.Time, int, int64) (Snapshot, error)
	GetPlaybackCoverage(context.Context, string, int) (Snapshot, bool, error)
}

type MediaRepository interface {
	MergePlaybackMediaCoverage(context.Context, string, Interval, time.Time, int) (MediaSnapshot, error)
	GetPlaybackMediaCoverage(context.Context, string, int) (MediaSnapshot, bool, error)
}

type ProposalRepository interface {
	CreatePlaybackProposal(context.Context, Proposal) (bool, error)
}

type Options struct {
	Now               func() time.Time
	MaxIntervals      int
	WriteTimeout      time.Duration
	ProposalEnabled   bool
	ProposalThreshold float64
}

type Tracker struct {
	repo              Repository
	now               func() time.Time
	maxIntervals      int
	writeTimeout      time.Duration
	proposalRepo      ProposalRepository
	proposalThreshold float64
}

func New(repo Repository, opts Options) (*Tracker, error) {
	if repo == nil {
		return nil, errors.New("playbackcoverage: nil repository")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxIntervals <= 0 {
		opts.MaxIntervals = DefaultMaxIntervals
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = DefaultWriteTimeout
	}
	tracker := &Tracker{repo: repo, now: opts.Now, maxIntervals: opts.MaxIntervals, writeTimeout: opts.WriteTimeout}
	if opts.ProposalEnabled {
		proposalRepo, ok := repo.(ProposalRepository)
		if !ok {
			return nil, errors.New("playbackcoverage: proposal repository unavailable")
		}
		if opts.ProposalThreshold <= 0 || opts.ProposalThreshold > 1 || math.IsNaN(opts.ProposalThreshold) || math.IsInf(opts.ProposalThreshold, 0) {
			return nil, errors.New("playbackcoverage: invalid proposal threshold")
		}
		tracker.proposalRepo = proposalRepo
		tracker.proposalThreshold = opts.ProposalThreshold
	}
	return tracker, nil
}

// Observe records bytes after a successful client write. The bounded detached
// context preserves that completed write if the client closes immediately
// afterward, without allowing database work to outlive the request indefinitely.
func (t *Tracker) Observe(ctx context.Context, representationID string, offset, length int64) (Snapshot, error) {
	if t == nil {
		return Snapshot{}, errors.New("playbackcoverage: nil tracker")
	}
	if err := ValidateRepresentationID(representationID); err != nil {
		return Snapshot{}, err
	}
	if offset < 0 || length <= 0 || offset > math.MaxInt64-length {
		return Snapshot{}, errors.New("playbackcoverage: invalid delivered span")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.writeTimeout)
	defer cancel()
	// RX-9.6 (RD-32): pass the declared extent so the store can discard
	// coverage whose byte offsets belong to a different release of the same
	// identity. A caller may carry that extent without a proposal target;
	// absent both contexts yields 0 (unknown), preserving the old posture.
	declaredSize := declaredSizeFromContext(writeCtx)
	if target, ok := targetFromContext(writeCtx); ok && target.Kind == ExtentDeclaredBytes {
		declaredSize = target.Total
	}
	snapshot, err := t.repo.MergePlaybackCoverage(writeCtx, representationID,
		Interval{Start: offset, End: offset + length}, t.now().UTC(), t.maxIntervals, declaredSize)
	if err != nil {
		return Snapshot{}, err
	}
	if err := t.maybePropose(writeCtx, representationID, snapshot.DeliveredBytes, ExtentDeclaredBytes); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (t *Tracker) Snapshot(ctx context.Context, representationID string) (Snapshot, bool, error) {
	if t == nil {
		return Snapshot{}, false, errors.New("playbackcoverage: nil tracker")
	}
	if err := ValidateRepresentationID(representationID); err != nil {
		return Snapshot{}, false, err
	}
	return t.repo.GetPlaybackCoverage(ctx, representationID, t.maxIntervals)
}

// ObserveMedia records a complete native VOD HLS segment or part after its
// entire body was successfully written. Partial delivery must never call it.
func (t *Tracker) ObserveMedia(ctx context.Context, representationID string, startNS, endNS int64) (MediaSnapshot, error) {
	if t == nil {
		return MediaSnapshot{}, errors.New("playbackcoverage: nil tracker")
	}
	if err := ValidateRepresentationID(representationID); err != nil {
		return MediaSnapshot{}, err
	}
	if startNS < 0 || endNS <= startNS {
		return MediaSnapshot{}, errors.New("playbackcoverage: invalid media span")
	}
	repo, ok := t.repo.(MediaRepository)
	if !ok {
		return MediaSnapshot{}, errors.New("playbackcoverage: media repository unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.writeTimeout)
	defer cancel()
	snapshot, err := repo.MergePlaybackMediaCoverage(writeCtx, representationID,
		Interval{Start: startNS, End: endNS}, t.now().UTC(), t.maxIntervals)
	if err != nil {
		return MediaSnapshot{}, err
	}
	if err := t.maybePropose(writeCtx, representationID, snapshot.DeliveredNS, ExtentVODHLS); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (t *Tracker) MediaSnapshot(ctx context.Context, representationID string) (MediaSnapshot, bool, error) {
	if t == nil {
		return MediaSnapshot{}, false, errors.New("playbackcoverage: nil tracker")
	}
	if err := ValidateRepresentationID(representationID); err != nil {
		return MediaSnapshot{}, false, err
	}
	repo, ok := t.repo.(MediaRepository)
	if !ok {
		return MediaSnapshot{}, false, errors.New("playbackcoverage: media repository unavailable")
	}
	return repo.GetPlaybackMediaCoverage(ctx, representationID, t.maxIntervals)
}

func ValidateRepresentationID(value string) error {
	if value == "" || len(value) > MaxRepresentationIDBytes || strings.Contains(value, "://") ||
		strings.ContainsAny(value, "\r\n\x00?&=") {
		return errors.New("playbackcoverage: invalid representation identity")
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return errors.New("playbackcoverage: invalid representation identity")
		}
	}
	return nil
}

type representationKey struct{}
type targetKey struct{}
type declaredSizeKey struct{}

func WithRepresentation(ctx context.Context, representationID string) context.Context {
	if ctx == nil || ValidateRepresentationID(representationID) != nil {
		return ctx
	}
	return context.WithValue(ctx, representationKey{}, representationID)
}

func Representation(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	value, ok := ctx.Value(representationKey{}).(string)
	return value, ok && ValidateRepresentationID(value) == nil
}

// WithDeclaredSize carries an observed byte extent without inventing a
// proposal target. RX-9.6 still resets stale coverage when a representation's
// release changes, while RX-3.2 remains unable to propose until a real Ready
// item/file target exists.
func WithDeclaredSize(ctx context.Context, total int64) context.Context {
	if ctx == nil || total <= 0 {
		return ctx
	}
	return context.WithValue(ctx, declaredSizeKey{}, total)
}

func declaredSizeFromContext(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	total, _ := ctx.Value(declaredSizeKey{}).(int64)
	if total <= 0 {
		return 0
	}
	return total
}

func WithTarget(ctx context.Context, target Target) context.Context {
	if ctx == nil || ValidateTarget(target) != nil {
		return ctx
	}
	return context.WithValue(ctx, targetKey{}, target)
}

func targetFromContext(ctx context.Context) (Target, bool) {
	if ctx == nil {
		return Target{}, false
	}
	target, ok := ctx.Value(targetKey{}).(Target)
	return target, ok && ValidateTarget(target) == nil
}

func ValidateTarget(target Target) error {
	if target.ItemID == "" || target.FileID == "" || len(target.ItemID) > MaxTargetIDBytes || len(target.FileID) > MaxTargetIDBytes || target.Total <= 0 {
		return errors.New("playbackcoverage: invalid proposal target")
	}
	for _, value := range []string{target.ItemID, target.FileID} {
		if strings.Contains(value, "://") || strings.ContainsAny(value, "\r\n\x00?&=") {
			return errors.New("playbackcoverage: invalid proposal target")
		}
		for _, r := range value {
			if r < 0x21 || r > 0x7e {
				return errors.New("playbackcoverage: invalid proposal target")
			}
		}
	}
	switch target.Kind {
	case ExtentDeclaredBytes, ExtentMediaTruth, ExtentVODHLS:
		return nil
	default:
		return errors.New("playbackcoverage: invalid proposal extent")
	}
}

func (t *Tracker) maybePropose(ctx context.Context, representationID string, delivered int64, observedKind ExtentKind) error {
	if t.proposalRepo == nil || delivered <= 0 {
		return nil
	}
	target, ok := targetFromContext(ctx)
	if !ok || (observedKind == ExtentVODHLS) != (target.Kind == ExtentVODHLS) || float64(delivered)/float64(target.Total) < t.proposalThreshold {
		return nil
	}
	_, err := t.proposalRepo.CreatePlaybackProposal(ctx, Proposal{
		RepresentationID: representationID,
		ItemID:           target.ItemID, FileID: target.FileID, ExtentKind: target.Kind,
		Delivered: delivered, Total: target.Total, Threshold: t.proposalThreshold,
		CreatedAt: t.now().UTC(),
	})
	return err
}
