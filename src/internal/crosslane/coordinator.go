// Package crosslane implements XR-01's shared proof-gated recovery and
// block-striping coordinator.
package crosslane

import (
	"context"
	"errors"
	"strconv"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/ladder"
	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

// Operation is the shared account-governor class used by verified cross-source
// recovery. PriorityRecovery keeps it behind live playback per LC-06.
const Operation = "cross-source-recovery"

// Candidate pairs one transient URL-free handle with its canonical ByteSource.
type Candidate struct {
	Handle contentproof.LaneCandidate
	Source bytesource.ByteSource
}

// Request describes one proof-bounded block recovery. Proof must already exist
// as authoritative evidence in Graph; name and size only nominate candidates.
type Request struct {
	Target     contentproof.LaneCandidate
	Proof      contentproof.Evidence
	Candidates []Candidate
	// CandidateDigest adapts native proof formats such as BitTorrent v2
	// Merkle pieces. Nil preserves the shared flat-digest behavior.
	CandidateDigest func(context.Context, []byte) ([]byte, error)
}

// Block is emitted only after Graph proves Data against Request.Proof.
type Block struct {
	Data           []byte
	Candidate      contentproof.LaneCandidate
	Decision       contentproof.Decision
	Ladder         ladder.Result
	CanonicalID    string
	ConvergenceErr error
}

// Coordinator reuses the Content Proof Graph, shared acquisition ladder, and
// optional account governor. It creates no background work or durable state.
type Coordinator struct {
	graph     *contentproof.Graph
	gov       *accountgov.Governor
	converger *contentproof.Converger
}

// NewCoordinator constructs the shared XR-01 coordinator.
func NewCoordinator(graph *contentproof.Graph, gov *accountgov.Governor, converger ...*contentproof.Converger) *Coordinator {
	c := &Coordinator{graph: graph, gov: gov}
	if len(converger) > 0 {
		c.converger = converger[0]
	}
	return c
}

// RecoverBlock returns the first exact candidate block proven by the target's
// authoritative evidence. Independent calls may choose different sources.
func (c *Coordinator) RecoverBlock(ctx context.Context, req Request) (Block, error) {
	result := Block{Decision: noProof(req)}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if c == nil || c.graph == nil {
		return result, errors.New("crosslane: nil proof graph")
	}
	if err := validateRequest(req); err != nil {
		return result, err
	}

	decision, err := c.graph.VerifyDigest(ctx, req.Target.RepresentationID, req.Proof.Scope, req.Proof.Offset, req.Proof.Length, req.Proof.Algorithm, req.Proof.Digest)
	if err != nil {
		return result, err
	}
	result.Decision = decision
	if decision.Relation != contentproof.RelationProven {
		return result, nil
	}
	for _, candidate := range req.Candidates {
		caps := candidate.Source.Caps()
		if !caps.RangeSupport || !caps.ExactSize || candidate.Source.Size() != req.Target.Size {
			return result, errors.New("crosslane: source lacks an exact bounded range")
		}
	}

	var verified []byte
	var verifiedCandidate contentproof.LaneCandidate
	lastDecision := noProof(req)
	rungs := make([]ladder.Rung, 0, len(req.Candidates))
	for i := range req.Candidates {
		candidate := req.Candidates[i]
		rungs = append(rungs, ladder.Rung{
			Name:        string(candidate.Handle.Lane) + "-" + strconv.Itoa(i),
			Op:          Operation,
			Priority:    accountgov.PriorityRecovery,
			MaxAttempts: 1,
			Attempt: func(attemptCtx context.Context) error {
				data := make([]byte, int(req.Proof.Length))
				n, readErr := candidate.Source.ReadAt(attemptCtx, data, req.Proof.Offset)
				if readErr != nil || n != len(data) {
					if err := attemptCtx.Err(); err != nil {
						return err
					}
					return outcome.Transient(errors.New("crosslane: source read failed"))
				}
				digestFn := req.CandidateDigest
				if digestFn == nil {
					digestFn = func(digestCtx context.Context, candidateData []byte) ([]byte, error) {
						return contentproof.Digest(digestCtx, req.Proof.Algorithm, candidateData)
					}
				}
				digest, digestErr := digestFn(attemptCtx, data)
				if digestErr != nil {
					if err := attemptCtx.Err(); err != nil {
						return err
					}
					return outcome.Permanent(digestErr)
				}
				candidateDecision, verifyErr := c.graph.VerifyDigest(attemptCtx, req.Target.RepresentationID, req.Proof.Scope, req.Proof.Offset, req.Proof.Length, req.Proof.Algorithm, digest)
				if verifyErr != nil {
					if err := attemptCtx.Err(); err != nil {
						return err
					}
					return outcome.Transient(verifyErr)
				}
				lastDecision = candidateDecision
				if candidateDecision.Relation != contentproof.RelationProven {
					return outcome.Permanent(errors.New("crosslane: recovery block not proven"))
				}
				verified = data
				verifiedCandidate = candidate.Handle
				return nil
			},
		})
	}

	ladderResult, runErr := ladder.New(c.gov, rungs...).Run(ctx, req.Target.ItemID)
	result.Ladder = ladderResult
	if err := ctx.Err(); err != nil {
		return Block{Decision: noProof(req), Ladder: ladderResult}, err
	}
	if runErr != nil {
		result.Decision = lastDecision
		return result, runErr
	}
	result.Data = verified
	result.Candidate = verifiedCandidate
	result.Decision = lastDecision
	if c.converger != nil {
		result.CanonicalID, result.ConvergenceErr = c.converger.ObserveVerified(ctx, req.Target, verifiedCandidate,
			contentproof.VerifiedSpan{Offset: req.Proof.Offset, Length: req.Proof.Length}, lastDecision)
	}
	return result, nil
}

func validateRequest(req Request) error {
	if err := contentproof.ValidateLaneCandidate(req.Target); err != nil {
		return err
	}
	if req.Proof.RepresentationID != req.Target.RepresentationID || req.Proof.Kind != contentproof.KindAuthoritative {
		return errors.New("crosslane: proof does not belong to target")
	}
	if !proofMayAuthorizeTarget(req.Target.Lane, req.Proof.Provenance) {
		return errors.New("crosslane: proof provenance does not match target lane")
	}
	if req.Proof.Offset < 0 || req.Proof.Length <= 0 || req.Proof.Length > contentproof.MaxMappingProofBytes ||
		req.Proof.Offset > req.Target.Size || req.Proof.Length > req.Target.Size-req.Proof.Offset {
		return errors.New("crosslane: invalid proof span")
	}
	if len(req.Candidates) == 0 || len(req.Candidates) > contentproof.MaxLaneCandidates {
		return errors.New("crosslane: invalid candidate count")
	}
	for _, candidate := range req.Candidates {
		if err := contentproof.ValidateLaneCandidate(candidate.Handle); err != nil {
			return err
		}
		if candidate.Handle.ReleaseKey != req.Target.ReleaseKey || candidate.Handle.Size != req.Target.Size {
			return errors.New("crosslane: candidate does not exactly match target")
		}
		if candidate.Source == nil {
			return errors.New("crosslane: nil candidate source")
		}
	}
	return nil
}

func proofMayAuthorizeTarget(lane contentproof.Lane, provenance contentproof.Provenance) bool {
	if contentproof.ProofMatchesLane(lane, provenance) {
		return true
	}
	// HR6.1 records a verified torrent/PAR2 proof on the HTTP representation
	// without relabeling its native provenance. The graph lookup below still
	// requires that exact authoritative digest to exist on the HTTP target.
	return lane == contentproof.LaneHTTP && (provenance == contentproof.ProvenanceTorrentPiece ||
		provenance == contentproof.ProvenanceTorrentMerkle || provenance == contentproof.ProvenancePAR2IFSC)
}

func noProof(req Request) contentproof.Decision {
	return contentproof.Decision{Relation: contentproof.RelationNoProof, Offset: req.Proof.Offset, Length: req.Proof.Length}
}
