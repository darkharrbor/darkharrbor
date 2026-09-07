package contentproof

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

const DefaultMaxContinuityEntriesPerRepresentation = DefaultMaxProofsPerRepresentation

type ContinuityRelation string

const (
	ContinuityTOFU          ContinuityRelation = "tofu"
	ContinuityAuthoritative ContinuityRelation = "authoritative"
	ContinuityConflict      ContinuityRelation = "conflict"
)

// Observation is the SHA-256 digest of one exact span actually served.
// RepresentationID is opaque and secret-free; media bytes are never retained.
type Observation struct {
	RepresentationID string
	Offset           int64
	Length           int64
	Digest           []byte
}

// ContinuityEntry records one observation and how it related to prior bytes
// and authoritative proof at observation time.
type ContinuityEntry struct {
	Observation
	Relation   ContinuityRelation
	Provenance Provenance
	Mutation   bool
	ObservedAt time.Time
}

type ContinuityRepository interface {
	ListContentProofs(context.Context, []string, time.Time) ([]Evidence, error)
	LatestContinuity(context.Context, string, int64, int64) (*ContinuityEntry, error)
	AppendContinuity(context.Context, ContinuityEntry, int) error
	ListContinuity(context.Context, string) ([]ContinuityEntry, error)
}

type Ledger struct {
	repo ContinuityRepository
	max  int
	now  func() time.Time
	mu   sync.Mutex
}

func NewLedger(repo ContinuityRepository, opts Options) (*Ledger, error) {
	if repo == nil {
		return nil, errors.New("contentproof: nil continuity repository")
	}
	if opts.MaxProofsPerRepresentation < 0 {
		return nil, errors.New("contentproof: negative continuity limit")
	}
	if opts.MaxProofsPerRepresentation == 0 {
		opts.MaxProofsPerRepresentation = DefaultMaxContinuityEntriesPerRepresentation
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Ledger{repo: repo, max: opts.MaxProofsPerRepresentation, now: opts.Now}, nil
}

// Observe records a served span. A changed digest is retained as mutation
// history. Only an exact authoritative SHA-256 proof can upgrade TOFU; a
// disagreement with any such proof is a conflict.
func (l *Ledger) Observe(ctx context.Context, observation Observation) (ContinuityEntry, error) {
	entry := ContinuityEntry{Observation: observation, Relation: ContinuityTOFU}
	if err := ctx.Err(); err != nil {
		return entry, err
	}
	if err := validateObservation(observation); err != nil {
		return entry, err
	}
	entry.ObservedAt = l.now().UTC()
	entry.Digest = append([]byte(nil), entry.Digest...)

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return entry, err
	}

	aliases, _, err := representationAliases(ctx, l.repo, entry.RepresentationID)
	if err != nil {
		return entry, err
	}
	var latest *ContinuityEntry
	for _, alias := range aliases {
		candidate, latestErr := l.repo.LatestContinuity(ctx, alias, entry.Offset, entry.Length)
		if latestErr != nil {
			return entry, latestErr
		}
		if candidate != nil && (latest == nil || candidate.ObservedAt.After(latest.ObservedAt)) {
			latest = candidate
		}
	}
	entry.Mutation = latest != nil && !equalBytes(latest.Digest, entry.Digest)

	proofs, err := l.repo.ListContentProofs(ctx, aliases, l.now().UTC())
	if err != nil {
		return entry, err
	}
	for _, proof := range proofs {
		if proof.Kind != KindAuthoritative || proof.Algorithm != AlgorithmSHA256 ||
			proof.Offset != entry.Offset || proof.Length != entry.Length {
			continue
		}
		entry.Provenance = proof.Provenance
		if !equalBytes(proof.Digest, entry.Digest) {
			entry.Relation = ContinuityConflict
			break
		}
		entry.Relation = ContinuityAuthoritative
	}

	if err := l.repo.AppendContinuity(ctx, entry, l.max); err != nil {
		return entry, err
	}
	return entry, nil
}

func (l *Ledger) History(ctx context.Context, representationID string) ([]ContinuityEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateOpaqueID("representation", representationID); err != nil {
		return nil, err
	}
	aliases, _, err := representationAliases(ctx, l.repo, representationID)
	if err != nil {
		return nil, err
	}
	var history []ContinuityEntry
	for _, alias := range aliases {
		entries, listErr := l.repo.ListContinuity(ctx, alias)
		if listErr != nil {
			return nil, listErr
		}
		history = append(history, entries...)
	}
	sort.SliceStable(history, func(i, j int) bool { return history[i].ObservedAt.Before(history[j].ObservedAt) })
	return history, nil
}

func validateObservation(observation Observation) error {
	if err := validateOpaqueID("representation", observation.RepresentationID); err != nil {
		return err
	}
	if observation.Offset < 0 || observation.Length <= 0 ||
		observation.Offset > int64(^uint64(0)>>1)-observation.Length {
		return errors.New("contentproof: invalid continuity span")
	}
	if len(observation.Digest) != digestLength(AlgorithmSHA256) {
		return errors.New("contentproof: continuity digest must be SHA-256")
	}
	return nil
}
