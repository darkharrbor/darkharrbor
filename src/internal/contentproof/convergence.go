package contentproof

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"time"
)

const (
	DefaultMaxRoutesPerRepresentation = 32
	DefaultMaxPendingSpansPerPair     = 4096
)

// VerifiedSpan is one exact range already proven byte-identical by the shared
// recovery coordinator. Partial spans never imply whole-representation identity.
type VerifiedSpan struct {
	Offset int64
	Length int64
}

// ConvergenceRepository atomically accumulates verified spans and promotes a
// fully covered pair into one canonical group with durable URL-free routes.
type ConvergenceRepository interface {
	ObserveRepresentationConvergence(context.Context, LaneCandidate, LaneCandidate, VerifiedSpan, string, time.Time, int, int) (string, error)
	ListRepresentationRoutes(context.Context, string, int) ([]LaneCandidate, error)
	ListRepresentationAliases(context.Context, string, int) ([]string, int64, error)
	ResolveRepresentationCanonical(context.Context, string) (string, bool, error)
}

func (c *Converger) CanonicalRepresentationID(ctx context.Context, representationID string) (string, bool, error) {
	if err := validateOpaqueID("representation", representationID); err != nil {
		return "", false, err
	}
	return c.repo.ResolveRepresentationCanonical(ctx, representationID)
}

type Converger struct {
	repo      ConvergenceRepository
	now       func() time.Time
	maxRoutes int
	maxSpans  int
}

func NewConverger(repo ConvergenceRepository, opts Options) (*Converger, error) {
	if repo == nil {
		return nil, errors.New("contentproof: nil convergence repository")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Converger{
		repo: repo, now: opts.Now,
		maxRoutes: DefaultMaxRoutesPerRepresentation,
		maxSpans:  DefaultMaxPendingSpansPerPair,
	}, nil
}

// ObserveVerified records one coordinator-proven span. canonical is non-empty
// only after the union covers the entire exact-size representation.
func (c *Converger) ObserveVerified(ctx context.Context, left, right LaneCandidate, span VerifiedSpan, decision Decision) (canonical string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if decision.Relation != RelationProven || decision.Offset != span.Offset || decision.Length != span.Length {
		return "", errors.New("contentproof: convergence span is not proven")
	}
	if err := validateLaneCandidate(left); err != nil {
		return "", err
	}
	if err := validateLaneCandidate(right); err != nil {
		return "", err
	}
	if left.RepresentationID == right.RepresentationID || left.ReleaseKey != right.ReleaseKey || left.Size != right.Size {
		return "", errors.New("contentproof: convergence candidates disagree")
	}
	if span.Offset < 0 || span.Length <= 0 || span.Offset > left.Size || span.Length > left.Size-span.Offset {
		return "", errors.New("contentproof: invalid convergence span")
	}
	return c.repo.ObserveRepresentationConvergence(ctx, left, right, span,
		canonicalID(left.RepresentationID, right.RepresentationID), c.now().UTC(), c.maxRoutes, c.maxSpans)
}

func (c *Converger) Routes(ctx context.Context, representationID string) ([]LaneCandidate, error) {
	if err := validateOpaqueID("representation", representationID); err != nil {
		return nil, err
	}
	routes, err := c.repo.ListRepresentationRoutes(ctx, representationID, c.maxRoutes)
	if err != nil {
		return nil, err
	}
	for _, route := range routes {
		if err := validateLaneCandidate(route); err != nil {
			return nil, err
		}
	}
	return routes, nil
}

func canonicalID(left, right string) string {
	ids := []string{left, right}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte("darkharrbor/canonical-representation/v1\x00" + ids[0] + "\x00" + ids[1]))
	return "canonical:" + hex.EncodeToString(sum[:])
}

func representationAliases(ctx context.Context, repo any, representationID string) ([]string, int64, error) {
	resolver, ok := repo.(interface {
		ListRepresentationAliases(context.Context, string, int) ([]string, int64, error)
	})
	if !ok {
		return []string{representationID}, 0, nil
	}
	aliases, size, err := resolver.ListRepresentationAliases(ctx, representationID, DefaultMaxRoutesPerRepresentation)
	if err != nil {
		return nil, 0, err
	}
	if len(aliases) == 0 {
		return []string{representationID}, 0, nil
	}
	return aliases, size, nil
}
