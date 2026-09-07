package torrentmeta

import (
	"context"
	"crypto/sha1" // #nosec G505 -- BitTorrent v1 piece hashes are defined as SHA-1.
	"encoding/hex"
	"errors"
	"hash"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
)

// MaxProofBlockBytes is the largest native torrent proof span this adapter
// will ask a cross-lane consumer to materialize at once.
const MaxProofBlockBytes int64 = 64 << 20

// ProofDomain is a lazy, URL-free view of one TorrentMeta file's native v1
// piece and/or v2 Merkle proof material. It owns no persistence or payload.
type ProofDomain struct {
	meta      *TorrentMeta
	file      FileEntry
	totalSize int64
	v1        bool
	v2Root    []byte
	v2Layer   []byte
}

// NativeProof describes one exact candidate-byte span and its authoritative
// torrent digest.
type NativeProof struct {
	Scope      contentproof.Scope
	Offset     int64
	Length     int64
	Algorithm  contentproof.Algorithm
	Provenance contentproof.Provenance

	digest      []byte
	pieceLength int64
}

// CrossLaneCandidate pairs a generic URL-free candidate with its torrent
// proof domain. It still performs no discovery or byte fetch.
type CrossLaneCandidate struct {
	Candidate contentproof.LaneCandidate
	Proofs    ProofDomain
}

// NewProofDomain validates TorrentMeta geometry once. Invalid, contradictory,
// oversized, or proofless metadata safely abstains.
func NewProofDomain(meta *TorrentMeta, file *FileEntry) (ProofDomain, bool) {
	if meta == nil || file == nil || meta.PieceLength <= 0 || meta.PieceLength > MaxProofBlockBytes ||
		file.Size <= 0 || file.StartOffset < 0 || file.EndOffset < file.StartOffset ||
		file.EndOffset-file.StartOffset != file.Size {
		return ProofDomain{}, false
	}

	matches := 0
	var totalSize int64
	for i := range meta.Files {
		entry := meta.Files[i]
		if entry.Size < 0 || entry.StartOffset != totalSize || entry.EndOffset < entry.StartOffset ||
			entry.EndOffset-entry.StartOffset != entry.Size {
			return ProofDomain{}, false
		}
		totalSize = entry.EndOffset
		if entry == *file {
			matches++
		}
	}
	if matches != 1 || totalSize <= 0 {
		return ProofDomain{}, false
	}

	domain := ProofDomain{meta: meta, file: *file, totalSize: totalSize}
	if len(meta.PieceHashesV1) > 0 && len(meta.PieceHashesV1)%sha1.Size == 0 {
		expected := (totalSize + meta.PieceLength - 1) / meta.PieceLength
		domain.v1 = int64(len(meta.PieceHashesV1)/sha1.Size) == expected
	}

	if rootHex, ok := meta.V2MerkleRoots[file.Path]; ok && validV2PieceLength(meta.PieceLength) {
		if root, err := hex.DecodeString(rootHex); err == nil && len(root) == 32 {
			domain.v2Root = append([]byte(nil), root...)
			if file.Size > meta.PieceLength {
				layer := meta.PieceLayers[rootHex]
				expected := (file.Size + meta.PieceLength - 1) / meta.PieceLength
				if len(layer)%32 == 0 && int64(len(layer)/32) == expected {
					domain.v2Layer = layer
				}
			}
		}
	}

	if (!domain.v1 && len(domain.v2Root) == 0) || !domain.hasProof() {
		return ProofDomain{}, false
	}
	return domain, true
}

// NewCrossLaneCandidate creates a usable torrent candidate only when exact
// file size and at least one native proof agree.
func NewCrossLaneCandidate(meta *TorrentMeta, file *FileEntry, itemID, fileID, representationID, releaseName string, size int64) (CrossLaneCandidate, error) {
	if file == nil || size != file.Size {
		return CrossLaneCandidate{}, errors.New("torrentmeta: candidate file size mismatch")
	}
	domain, ok := NewProofDomain(meta, file)
	if !ok {
		return CrossLaneCandidate{}, errors.New("torrentmeta: candidate has no usable proof")
	}
	candidate, err := contentproof.NewLaneCandidate(contentproof.LaneTorrent, itemID, fileID, representationID, releaseName, size)
	if err != nil {
		return CrossLaneCandidate{}, err
	}
	return CrossLaneCandidate{Candidate: candidate, Proofs: domain}, nil
}

// ProofAt returns the exact native proof span covering file-relative offset.
// Missing, cross-file, malformed, or out-of-range material safely abstains.
func (d ProofDomain) ProofAt(offset int64) (NativeProof, bool) {
	if offset < 0 || offset >= d.file.Size || d.meta == nil {
		return NativeProof{}, false
	}
	if d.v1 {
		if proof, ok := d.proofAtV1(offset); ok {
			return proof, true
		}
	}
	return d.proofAtV2(offset)
}

// Digest returns a defensive copy of the authoritative native digest.
func (p NativeProof) Digest() []byte {
	return append([]byte(nil), p.digest...)
}

// Evidence adapts a native torrent proof into the existing shared proof graph.
// The caller supplies the program's injected observation clock.
func (p NativeProof) Evidence(representationID string, observedAt time.Time) contentproof.Evidence {
	return contentproof.Evidence{
		RepresentationID: representationID,
		Scope:            p.Scope,
		Offset:           p.Offset,
		Length:           p.Length,
		Kind:             contentproof.KindAuthoritative,
		Algorithm:        p.Algorithm,
		Digest:           p.Digest(),
		Provenance:       p.Provenance,
		ObservedAt:       observedAt,
	}
}

// CandidateDigest computes the digest a later coordinator passes to the
// shared graph. It is bounded, context-cancellable, and exact-length only.
func (p NativeProof) CandidateDigest(ctx context.Context, data []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Length <= 0 || p.Length > MaxProofBlockBytes || int64(len(data)) != p.Length {
		return nil, errors.New("torrentmeta: candidate bytes do not match proof span")
	}
	switch p.Provenance {
	case contentproof.ProvenanceTorrentPiece:
		return hashCancellable(ctx, sha1.New(), data)
	case contentproof.ProvenanceTorrentMerkle:
		sum, err := computeV2PieceHashContext(ctx, data, p.pieceLength)
		if err != nil {
			return nil, err
		}
		return sum[:], nil
	default:
		return nil, errors.New("torrentmeta: unknown native proof provenance")
	}
}

func (d ProofDomain) proofAtV1(offset int64) (NativeProof, bool) {
	absolute := d.file.StartOffset + offset
	if absolute < d.file.StartOffset {
		return NativeProof{}, false
	}
	index := absolute / d.meta.PieceLength
	start := index * d.meta.PieceLength
	end := start + d.meta.PieceLength
	if end < start || end > d.totalSize {
		end = d.totalSize
	}
	if start < d.file.StartOffset || end > d.file.EndOffset || end <= start {
		return NativeProof{}, false
	}
	digestStart := index * sha1.Size
	if digestStart < 0 || digestStart+sha1.Size > int64(len(d.meta.PieceHashesV1)) {
		return NativeProof{}, false
	}
	return NativeProof{
		Scope: contentproof.ScopeBlock, Offset: start - d.file.StartOffset, Length: end - start,
		Algorithm: contentproof.AlgorithmSHA1, Provenance: contentproof.ProvenanceTorrentPiece,
		digest: append([]byte(nil), d.meta.PieceHashesV1[digestStart:digestStart+sha1.Size]...),
	}, true
}

func (d ProofDomain) proofAtV2(offset int64) (NativeProof, bool) {
	if len(d.v2Root) != 32 {
		return NativeProof{}, false
	}
	if d.file.Size <= d.meta.PieceLength {
		return NativeProof{
			Scope: contentproof.ScopeWhole, Offset: 0, Length: d.file.Size,
			Algorithm: contentproof.AlgorithmSHA256, Provenance: contentproof.ProvenanceTorrentMerkle,
			digest: append([]byte(nil), d.v2Root...), pieceLength: d.meta.PieceLength,
		}, true
	}
	index := offset / d.meta.PieceLength
	start := index * d.meta.PieceLength
	length := d.meta.PieceLength
	if remaining := d.file.Size - start; remaining < length {
		length = remaining
	}
	digestStart := index * 32
	if length <= 0 || digestStart < 0 || digestStart+32 > int64(len(d.v2Layer)) {
		return NativeProof{}, false
	}
	return NativeProof{
		Scope: contentproof.ScopeBlock, Offset: start, Length: length,
		Algorithm: contentproof.AlgorithmSHA256, Provenance: contentproof.ProvenanceTorrentMerkle,
		digest: append([]byte(nil), d.v2Layer[digestStart:digestStart+32]...), pieceLength: d.meta.PieceLength,
	}, true
}

func (d ProofDomain) hasProof() bool {
	if _, ok := d.ProofAt(0); ok {
		return true
	}
	if d.v1 {
		next := ((d.file.StartOffset + d.meta.PieceLength - 1) / d.meta.PieceLength) * d.meta.PieceLength
		if next >= d.file.StartOffset && next < d.file.EndOffset {
			_, ok := d.ProofAt(next - d.file.StartOffset)
			return ok
		}
	}
	return false
}

func validV2PieceLength(pieceLength int64) bool {
	if pieceLength < v2BlockSize || pieceLength > MaxProofBlockBytes || pieceLength%v2BlockSize != 0 {
		return false
	}
	blocks := pieceLength / v2BlockSize
	return blocks&(blocks-1) == 0
}

func hashCancellable(ctx context.Context, h hash.Hash, data []byte) ([]byte, error) {
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
