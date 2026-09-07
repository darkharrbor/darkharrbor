// Package contentproof owns DarkHarrbor's shared Content Proof Graph.
//
// It records compact evidence about representations, never source URLs or
// media payloads, and answers one security-sensitive question: may bytes from
// two representations be treated as equivalent for a requested span?
package contentproof

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	DefaultMaxProofsPerRepresentation = 4096
	maxDigestBytes                    = 64
)

type Scope string

const (
	ScopeWhole Scope = "whole"
	ScopeBlock Scope = "block"
)

type Kind string

const (
	KindAuthoritative Kind = "authoritative"
	KindSameOrigin    Kind = "same_origin"
	KindTOFU          Kind = "tofu"
	KindHint          Kind = "hint"
)

type Provenance string

const (
	ProvenanceTorrentPiece     Provenance = "torrent_piece"
	ProvenanceTorrentMerkle    Provenance = "torrent_merkle"
	ProvenancePAR2IFSC         Provenance = "par2_ifsc"
	ProvenanceIADigest         Provenance = "ia_digest"
	ProvenanceHTTPDigest       Provenance = "http_digest"
	ProvenanceMetalink         Provenance = "metalink"
	ProvenanceSameOriginETag   Provenance = "same_origin_etag"
	ProvenancePlaybackObserved Provenance = "playback_observed"
	ProvenanceCandidateHint    Provenance = "candidate_hint"
)

type Algorithm string

const (
	AlgorithmMD5    Algorithm = "md5"
	AlgorithmSHA1   Algorithm = "sha1"
	AlgorithmSHA256 Algorithm = "sha256"
	// ETags and candidate hints are persisted only as SHA-256 digests of the
	// original value, keeping tokens and upstream strings out of the graph.
	AlgorithmETAGSHA256 Algorithm = "etag_sha256"
	AlgorithmHintSHA256 Algorithm = "hint_sha256"
)

// Evidence is one compact proof assertion for a whole object or exact block.
// RepresentationID and OriginID are opaque, secret-free identifiers.
type Evidence struct {
	RepresentationID string
	Scope            Scope
	Offset           int64
	Length           int64
	Kind             Kind
	Algorithm        Algorithm
	Digest           []byte
	Provenance       Provenance
	OriginID         string
	ExpiresAt        *time.Time
	ObservedAt       time.Time
}

// Key identifies one replaceable evidence assertion without including its
// digest. Replacing an assertion does not preserve mutation history; HR1.5's
// Representation Continuity Ledger owns that history.
type Key struct {
	RepresentationID string
	Scope            Scope
	Offset           int64
	Length           int64
	Kind             Kind
	Algorithm        Algorithm
	Provenance       Provenance
	OriginID         string
}

type Relation string

const (
	RelationProven   Relation = "proven"
	RelationConflict Relation = "conflict"
	RelationNoProof  Relation = "no_proof"
)

type Decision struct {
	Relation Relation
	Offset   int64
	Length   int64
	Left     Provenance
	Right    Provenance
}

type Repository interface {
	UpsertContentProof(context.Context, Evidence, int) error
	ListContentProofs(context.Context, []string, time.Time) ([]Evidence, error)
	DeleteContentProof(context.Context, Key) error
}

type Options struct {
	MaxProofsPerRepresentation int
	Now                        func() time.Time
}

type Graph struct {
	repo Repository
	max  int
	now  func() time.Time
}

func New(repo Repository, opts Options) (*Graph, error) {
	if repo == nil {
		return nil, errors.New("contentproof: nil repository")
	}
	if opts.MaxProofsPerRepresentation < 0 {
		return nil, errors.New("contentproof: negative proof limit")
	}
	if opts.MaxProofsPerRepresentation == 0 {
		opts.MaxProofsPerRepresentation = DefaultMaxProofsPerRepresentation
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Graph{repo: repo, max: opts.MaxProofsPerRepresentation, now: opts.Now}, nil
}

func (g *Graph) Record(ctx context.Context, evidence Evidence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if evidence.ObservedAt.IsZero() {
		evidence.ObservedAt = g.now().UTC()
	}
	if err := validateEvidence(evidence); err != nil {
		return err
	}
	evidence.Digest = append([]byte(nil), evidence.Digest...)
	return g.repo.UpsertContentProof(ctx, evidence, g.max)
}

func (g *Graph) Revoke(ctx context.Context, key Key) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateKey(key); err != nil {
		return err
	}
	return g.repo.DeleteContentProof(ctx, key)
}

// Compare returns Conflict when comparable authoritative evidence disagrees,
// Proven when it agrees, and NoProof otherwise. A whole-object proof may cover
// a subrange; a block proof covers only spans wholly inside that exact block.
// Callers split larger reads at proof boundaries.
func (g *Graph) Compare(ctx context.Context, leftID, rightID string, offset, length int64) (Decision, error) {
	decision := Decision{Relation: RelationNoProof, Offset: offset, Length: length}
	if err := ctx.Err(); err != nil {
		return decision, err
	}
	if err := validateOpaqueID("left representation", leftID); err != nil {
		return decision, err
	}
	if err := validateOpaqueID("right representation", rightID); err != nil {
		return decision, err
	}
	if offset < 0 || length <= 0 || offset > int64(^uint64(0)>>1)-length {
		return decision, errors.New("contentproof: invalid comparison span")
	}

	leftIDs, leftSize, err := representationAliases(ctx, g.repo, leftID)
	if err != nil {
		return decision, err
	}
	rightIDs, rightSize, err := representationAliases(ctx, g.repo, rightID)
	if err != nil {
		return decision, err
	}
	leftSet := stringSet(leftIDs)
	rightSet := stringSet(rightIDs)
	if sameStringSet(leftSet, rightSet) && leftSize > 0 && leftSize == rightSize && offset <= leftSize-length {
		decision.Relation = RelationProven
		return decision, nil
	}
	evidence, err := g.repo.ListContentProofs(ctx, append(leftIDs, rightIDs...), g.now().UTC())
	if err != nil {
		return decision, err
	}
	var matched *Decision
	for _, left := range evidence {
		if !leftSet[left.RepresentationID] || left.Kind != KindAuthoritative || !covers(left, offset, length) {
			continue
		}
		for _, right := range evidence {
			if !rightSet[right.RepresentationID] || right.Kind != KindAuthoritative ||
				left.Scope != right.Scope || left.Offset != right.Offset || left.Length != right.Length ||
				left.Algorithm != right.Algorithm || !covers(right, offset, length) {
				continue
			}
			candidate := Decision{Relation: RelationProven, Offset: offset, Length: length, Left: left.Provenance, Right: right.Provenance}
			if !equalBytes(left.Digest, right.Digest) {
				candidate.Relation = RelationConflict
				return candidate, nil
			}
			matched = &candidate
		}
	}
	if matched != nil {
		return *matched, nil
	}
	return decision, nil
}

// VerifyDigest checks candidate bytes (hashed by the caller) against an
// authoritative proof for one exact whole object or block. This is how a
// torrent piece or PAR2 IFSC proof can authorize candidate bytes even when
// the candidate's own lane uses a different native hash algorithm.
func (g *Graph) VerifyDigest(ctx context.Context, representationID string, scope Scope, offset, length int64, algorithm Algorithm, digest []byte) (Decision, error) {
	decision := Decision{Relation: RelationNoProof, Offset: offset, Length: length}
	if err := ctx.Err(); err != nil {
		return decision, err
	}
	if err := validateOpaqueID("representation", representationID); err != nil {
		return decision, err
	}
	if scope != ScopeWhole && scope != ScopeBlock {
		return decision, errors.New("contentproof: invalid verification scope")
	}
	if offset < 0 || length <= 0 || (scope == ScopeWhole && offset != 0) || len(digest) != digestLength(algorithm) {
		return decision, errors.New("contentproof: invalid verification digest or span")
	}

	representationIDs, size, err := representationAliases(ctx, g.repo, representationID)
	if err != nil {
		return decision, err
	}
	if size > 0 && (offset > size || length > size-offset) {
		return decision, errors.New("contentproof: verification exceeds canonical representation")
	}
	evidence, err := g.repo.ListContentProofs(ctx, representationIDs, g.now().UTC())
	if err != nil {
		return decision, err
	}
	matched := false
	for _, proof := range evidence {
		if proof.Kind != KindAuthoritative || proof.Scope != scope || proof.Offset != offset ||
			proof.Length != length || proof.Algorithm != algorithm {
			continue
		}
		decision.Left = proof.Provenance
		if !equalBytes(proof.Digest, digest) {
			decision.Relation = RelationConflict
			return decision, nil
		}
		matched = true
	}
	if matched {
		decision.Relation = RelationProven
	}
	return decision, nil
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sameStringSet(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if !right[value] {
			return false
		}
	}
	return true
}

func covers(e Evidence, offset, length int64) bool {
	return e.Offset <= offset && e.Length >= length && offset-e.Offset <= e.Length-length
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

var opaqueIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func validateOpaqueID(field, value string) error {
	if !opaqueIDPattern.MatchString(value) {
		return fmt.Errorf("contentproof: %s must be an opaque secret-free ID", field)
	}
	return nil
}

func validateKey(key Key) error {
	return validateEvidence(Evidence{
		RepresentationID: key.RepresentationID,
		Scope:            key.Scope, Offset: key.Offset, Length: key.Length,
		Kind: key.Kind, Algorithm: key.Algorithm, Digest: make([]byte, digestLength(key.Algorithm)),
		Provenance: key.Provenance, OriginID: key.OriginID, ObservedAt: time.Unix(1, 0),
	})
}

func validateEvidence(e Evidence) error {
	if err := validateOpaqueID("representation", e.RepresentationID); err != nil {
		return err
	}
	if e.Scope != ScopeWhole && e.Scope != ScopeBlock {
		return errors.New("contentproof: invalid scope")
	}
	if e.Offset < 0 || e.Length <= 0 || (e.Scope == ScopeWhole && e.Offset != 0) {
		return errors.New("contentproof: invalid evidence span")
	}
	if e.ObservedAt.IsZero() {
		return errors.New("contentproof: missing observed time")
	}
	if e.ExpiresAt != nil && !e.ExpiresAt.After(e.ObservedAt) {
		return errors.New("contentproof: expiry must follow observation")
	}
	if len(e.Digest) == 0 || len(e.Digest) > maxDigestBytes || len(e.Digest) != digestLength(e.Algorithm) {
		return errors.New("contentproof: invalid digest length or algorithm")
	}
	if e.OriginID != "" {
		if err := validateOpaqueID("origin", e.OriginID); err != nil {
			return err
		}
	}
	if !validKindProvenance(e) {
		return errors.New("contentproof: invalid kind/provenance combination")
	}
	return nil
}

func digestLength(algorithm Algorithm) int {
	switch algorithm {
	case AlgorithmMD5:
		return 16
	case AlgorithmSHA1:
		return 20
	case AlgorithmSHA256, AlgorithmETAGSHA256, AlgorithmHintSHA256:
		return 32
	default:
		return 0
	}
}

func validKindProvenance(e Evidence) bool {
	switch e.Kind {
	case KindAuthoritative:
		if e.OriginID != "" {
			return false
		}
		switch e.Provenance {
		case ProvenanceTorrentPiece:
			return e.Scope == ScopeBlock && e.Algorithm == AlgorithmSHA1
		case ProvenanceTorrentMerkle:
			return e.Algorithm == AlgorithmSHA256
		case ProvenancePAR2IFSC:
			return e.Scope == ScopeBlock && e.Algorithm == AlgorithmMD5
		case ProvenanceIADigest, ProvenanceMetalink:
			return e.Scope == ScopeWhole && isAuthoritativeAlgorithm(e.Algorithm)
		case ProvenanceHTTPDigest:
			return isAuthoritativeAlgorithm(e.Algorithm)
		}
	case KindSameOrigin:
		return e.Provenance == ProvenanceSameOriginETag && e.Algorithm == AlgorithmETAGSHA256 && e.OriginID != ""
	case KindTOFU:
		return e.Provenance == ProvenancePlaybackObserved && e.Algorithm == AlgorithmSHA256 && e.OriginID == ""
	case KindHint:
		return e.Provenance == ProvenanceCandidateHint && e.Algorithm == AlgorithmHintSHA256 && e.OriginID == ""
	}
	return false
}

func isAuthoritativeAlgorithm(algorithm Algorithm) bool {
	return algorithm == AlgorithmMD5 || algorithm == AlgorithmSHA1 || algorithm == AlgorithmSHA256
}
