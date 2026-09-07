package contentproof

import (
	"context"
	"crypto/md5"  // #nosec G501 -- PAR2 IFSC proof verification requires MD5.
	"crypto/sha1" // #nosec G505 -- BitTorrent v1 proof verification requires SHA-1.
	"crypto/sha256"
	"errors"
	"hash"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// MaxMappingProofBytes bounds one HTTP proof-domain verification read.
const MaxMappingProofBytes int64 = 64 << 20

// MapHTTPRepresentation maps one exact flat-digest proof-covered span of an
// HTTP representation. Native formats that need specialized hashing use
// MapHTTPRepresentationWithDigest.
func (g *Graph) MapHTTPRepresentation(ctx context.Context, source bytesource.ByteSource, httpCandidate, proofCandidate LaneCandidate, proof Evidence) (Decision, error) {
	return g.mapHTTPRepresentation(ctx, source, httpCandidate, proofCandidate, proof, nil)
}

// MapHTTPRepresentationWithDigest maps native proof formats such as BitTorrent
// v2 Merkle pieces while retaining the same bounds and fail-closed checks.
func (g *Graph) MapHTTPRepresentationWithDigest(ctx context.Context, source bytesource.ByteSource, httpCandidate, proofCandidate LaneCandidate, proof Evidence, candidateDigest func(context.Context, []byte) ([]byte, error)) (Decision, error) {
	if candidateDigest == nil {
		return Decision{Relation: RelationNoProof, Offset: proof.Offset, Length: proof.Length}, errors.New("contentproof: nil native candidate digest")
	}
	return g.mapHTTPRepresentation(ctx, source, httpCandidate, proofCandidate, proof, candidateDigest)
}

func (g *Graph) mapHTTPRepresentation(ctx context.Context, source bytesource.ByteSource, httpCandidate, proofCandidate LaneCandidate, proof Evidence, candidateDigest func(context.Context, []byte) ([]byte, error)) (Decision, error) {
	decision := Decision{Relation: RelationNoProof, Offset: proof.Offset, Length: proof.Length}
	if err := ctx.Err(); err != nil {
		return decision, err
	}
	if g == nil {
		return decision, errors.New("contentproof: nil graph")
	}
	if err := validateLaneCandidate(httpCandidate); err != nil {
		return decision, err
	}
	if err := validateLaneCandidate(proofCandidate); err != nil {
		return decision, err
	}
	if httpCandidate.Lane != LaneHTTP {
		return decision, errors.New("contentproof: mapping source is not HTTP")
	}
	if httpCandidate.ReleaseKey != proofCandidate.ReleaseKey || httpCandidate.Size != proofCandidate.Size {
		return decision, nil
	}
	if proof.RepresentationID != proofCandidate.RepresentationID || proof.Kind != KindAuthoritative {
		return decision, errors.New("contentproof: proof does not belong to candidate")
	}
	if proof.ObservedAt.IsZero() {
		proof.ObservedAt = g.now().UTC()
	}
	if err := validateEvidence(proof); err != nil {
		return decision, err
	}
	if !ProofMatchesLane(proofCandidate.Lane, proof.Provenance) {
		return decision, errors.New("contentproof: proof provenance does not match candidate lane")
	}
	if proof.Offset > proofCandidate.Size || proof.Length > proofCandidate.Size-proof.Offset {
		return decision, errors.New("contentproof: proof exceeds candidate size")
	}
	now := g.now().UTC()
	if proof.ExpiresAt != nil && !proof.ExpiresAt.After(now) {
		return decision, nil
	}

	native, err := g.VerifyDigest(ctx, proof.RepresentationID, proof.Scope, proof.Offset, proof.Length, proof.Algorithm, proof.Digest)
	if err != nil {
		return decision, err
	}
	if native.Relation == RelationConflict {
		return native, nil
	}
	if native.Relation == RelationNoProof {
		if err := g.Record(ctx, proof); err != nil {
			return decision, err
		}
	}

	mapped, err := g.Compare(ctx, httpCandidate.RepresentationID, proofCandidate.RepresentationID, proof.Offset, proof.Length)
	if err != nil {
		return decision, err
	}
	if mapped.Relation != RelationNoProof {
		return mapped, nil
	}

	if source == nil || !source.Caps().RangeSupport || !source.Caps().ExactSize ||
		source.Size() != httpCandidate.Size || proof.Length > MaxMappingProofBytes {
		return decision, nil
	}
	data := make([]byte, int(proof.Length))
	n, err := source.ReadAt(ctx, data, proof.Offset)
	if err != nil || n != len(data) {
		if ctx.Err() != nil {
			return decision, ctx.Err()
		}
		return decision, nil
	}
	digestFn := candidateDigest
	if digestFn == nil {
		digestFn = func(digestCtx context.Context, candidateData []byte) ([]byte, error) {
			return Digest(digestCtx, proof.Algorithm, candidateData)
		}
	}
	digest, err := digestFn(ctx, data)
	if err != nil {
		return decision, err
	}
	verified, err := g.VerifyDigest(ctx, proofCandidate.RepresentationID, proof.Scope, proof.Offset, proof.Length, proof.Algorithm, digest)
	if err != nil || verified.Relation != RelationProven {
		return verified, err
	}

	httpProof := proof
	httpProof.RepresentationID = httpCandidate.RepresentationID
	httpProof.ObservedAt = now
	existing, err := g.VerifyDigest(ctx, httpProof.RepresentationID, httpProof.Scope, httpProof.Offset, httpProof.Length, httpProof.Algorithm, httpProof.Digest)
	if err != nil {
		return decision, err
	}
	if existing.Relation == RelationConflict {
		return existing, nil
	}
	if existing.Relation == RelationNoProof {
		if err := g.Record(ctx, httpProof); err != nil {
			return decision, err
		}
	}
	return g.Compare(ctx, httpCandidate.RepresentationID, proofCandidate.RepresentationID, proof.Offset, proof.Length)
}

// ProofMatchesLane reports whether authoritative proof provenance is native to a lane.
func ProofMatchesLane(lane Lane, provenance Provenance) bool {
	switch lane {
	case LaneTorrent:
		return provenance == ProvenanceTorrentPiece || provenance == ProvenanceTorrentMerkle
	case LaneNNTP:
		return provenance == ProvenancePAR2IFSC
	case LaneHTTP:
		return provenance == ProvenanceIADigest || provenance == ProvenanceHTTPDigest || provenance == ProvenanceMetalink
	default:
		return false
	}
}

// Digest hashes a bounded proof block with a supported authoritative algorithm.
// It checks cancellation between fixed-size chunks.
func Digest(ctx context.Context, algorithm Algorithm, data []byte) ([]byte, error) {
	var h hash.Hash
	switch algorithm {
	case AlgorithmMD5:
		h = md5.New() // #nosec G401 -- PAR2 IFSC verification requires MD5.
	case AlgorithmSHA1:
		h = sha1.New() // #nosec G401 -- BitTorrent v1 verification requires SHA-1.
	case AlgorithmSHA256:
		h = sha256.New()
	default:
		return nil, errors.New("contentproof: unsupported mapping algorithm")
	}
	const chunk = 64 << 10
	for offset := 0; offset < len(data); offset += chunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := offset + chunk
		if end > len(data) {
			end = len(data)
		}
		_, _ = h.Write(data[offset:end])
	}
	return h.Sum(nil), nil
}
