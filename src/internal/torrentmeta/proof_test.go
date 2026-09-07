package torrentmeta_test

import (
	"context"
	"crypto/sha1" // #nosec G505 -- test vectors use BitTorrent v1 SHA-1.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

type proofRepo struct {
	mu       sync.Mutex
	evidence []contentproof.Evidence
}

func (r *proofRepo) UpsertContentProof(_ context.Context, evidence contentproof.Evidence, _ int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	evidence.Digest = append([]byte(nil), evidence.Digest...)
	r.evidence = append(r.evidence, evidence)
	return nil
}

func (r *proofRepo) ListContentProofs(_ context.Context, ids []string, _ time.Time) ([]contentproof.Evidence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	var out []contentproof.Evidence
	for _, evidence := range r.evidence {
		if wanted[evidence.RepresentationID] {
			evidence.Digest = append([]byte(nil), evidence.Digest...)
			out = append(out, evidence)
		}
	}
	return out, nil
}

func (r *proofRepo) DeleteContentProof(_ context.Context, key contentproof.Key) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, evidence := range r.evidence {
		if evidence.RepresentationID == key.RepresentationID && evidence.Offset == key.Offset &&
			evidence.Length == key.Length && evidence.Provenance == key.Provenance {
			r.evidence = append(r.evidence[:i], r.evidence[i+1:]...)
			break
		}
	}
	return nil
}

func TestV1ProofDomainKnownAnswerConflictAndFinalPiece(t *testing.T) {
	first := []byte("abcd")
	last := []byte("ef")
	firstHash := sha1.Sum(first)
	lastHash := sha1.Sum(last)
	meta := &torrentmeta.TorrentMeta{
		PieceLength:   4,
		Files:         []torrentmeta.FileEntry{{Path: "video.mkv", Size: 6, StartOffset: 0, EndOffset: 6}},
		PieceHashesV1: append(append([]byte(nil), firstHash[:]...), lastHash[:]...),
	}
	domain, ok := torrentmeta.NewProofDomain(meta, &meta.Files[0])
	if !ok {
		t.Fatal("expected v1 proof domain")
	}
	proof, ok := domain.ProofAt(4)
	if !ok || proof.Offset != 4 || proof.Length != 2 || proof.Provenance != contentproof.ProvenanceTorrentPiece {
		t.Fatalf("proof = %+v, %v", proof, ok)
	}

	now := time.Unix(1, 0).UTC()
	repo := &proofRepo{}
	graph, err := contentproof.New(repo, contentproof.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Record(context.Background(), proof.Evidence("torrent-file", now)); err != nil {
		t.Fatal(err)
	}
	digest, err := proof.CandidateDigest(context.Background(), last)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := graph.VerifyDigest(context.Background(), "torrent-file", proof.Scope, proof.Offset, proof.Length, proof.Algorithm, digest)
	if err != nil || decision.Relation != contentproof.RelationProven {
		t.Fatalf("matching decision = %+v, %v", decision, err)
	}
	digest, err = proof.CandidateDigest(context.Background(), []byte("zz"))
	if err != nil {
		t.Fatal(err)
	}
	decision, err = graph.VerifyDigest(context.Background(), "torrent-file", proof.Scope, proof.Offset, proof.Length, proof.Algorithm, digest)
	if err != nil || decision.Relation != contentproof.RelationConflict {
		t.Fatalf("corrupt decision = %+v, %v", decision, err)
	}
}

func TestV1ProofDomainRefusesCrossFilePiece(t *testing.T) {
	hashes := make([]byte, 3*sha1.Size)
	meta := &torrentmeta.TorrentMeta{
		PieceLength: 4,
		Files: []torrentmeta.FileEntry{
			{Path: "a.bin", Size: 6, StartOffset: 0, EndOffset: 6},
			{Path: "b.mkv", Size: 6, StartOffset: 6, EndOffset: 12},
		},
		PieceHashesV1: hashes,
	}
	domain, ok := torrentmeta.NewProofDomain(meta, &meta.Files[0])
	if !ok {
		t.Fatal("expected domain with one usable piece")
	}
	if _, ok := domain.ProofAt(4); ok {
		t.Fatal("cross-file v1 piece must abstain")
	}
}

func TestV2ProofDomainKnownAnswerAndNoProof(t *testing.T) {
	const pieceLength = 16384
	data := []byte("v2 candidate")
	root := v2PieceRoot(data, pieceLength)
	rootHex := hex.EncodeToString(root[:])
	meta := &torrentmeta.TorrentMeta{
		PieceLength:   pieceLength,
		Files:         []torrentmeta.FileEntry{{Path: "video.mkv", Size: int64(len(data)), StartOffset: 0, EndOffset: int64(len(data))}},
		V2MerkleRoots: map[string]string{"video.mkv": rootHex},
	}
	domain, ok := torrentmeta.NewProofDomain(meta, &meta.Files[0])
	if !ok {
		t.Fatal("expected v2 proof domain")
	}
	proof, ok := domain.ProofAt(0)
	if !ok || proof.Scope != contentproof.ScopeWhole || proof.Provenance != contentproof.ProvenanceTorrentMerkle {
		t.Fatalf("proof = %+v, %v", proof, ok)
	}

	now := time.Unix(2, 0).UTC()
	repo := &proofRepo{}
	graph, err := contentproof.New(repo, contentproof.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Record(context.Background(), proof.Evidence("v2-file", now)); err != nil {
		t.Fatal(err)
	}
	digest, err := proof.CandidateDigest(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := graph.VerifyDigest(context.Background(), "v2-file", proof.Scope, proof.Offset, proof.Length, proof.Algorithm, digest)
	if err != nil || decision.Relation != contentproof.RelationProven {
		t.Fatalf("matching decision = %+v, %v", decision, err)
	}
	corrupt := append([]byte(nil), data...)
	corrupt[0] ^= 0xff
	corruptDigest, err := proof.CandidateDigest(context.Background(), corrupt)
	if err != nil {
		t.Fatal(err)
	}
	decision, err = graph.VerifyDigest(context.Background(), "v2-file", proof.Scope, proof.Offset, proof.Length, proof.Algorithm, corruptDigest)
	if err != nil || decision.Relation != contentproof.RelationConflict {
		t.Fatalf("corrupt decision = %+v, %v", decision, err)
	}
	decision, err = graph.VerifyDigest(context.Background(), "missing-file", proof.Scope, proof.Offset, proof.Length, proof.Algorithm, digest)
	if err != nil || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("no-proof decision = %+v, %v", decision, err)
	}
}

func TestV2MultiPieceProofAndHybridFallback(t *testing.T) {
	const pieceLength = 16384
	first := make([]byte, pieceLength)
	for i := range first {
		first[i] = 'a'
	}
	tail := []byte("tail")
	firstRoot := v2PieceRoot(first, pieceLength)
	tailRoot := v2PieceRoot(tail, pieceLength)
	var pair [64]byte
	copy(pair[:32], firstRoot[:])
	copy(pair[32:], tailRoot[:])
	fileRoot := sha256.Sum256(pair[:])
	rootHex := hex.EncodeToString(fileRoot[:])
	layer := append(append([]byte(nil), firstRoot[:]...), tailRoot[:]...)
	meta := &torrentmeta.TorrentMeta{
		PieceLength: pieceLength,
		Files: []torrentmeta.FileEntry{{
			Path: "multi.mkv", Size: pieceLength + int64(len(tail)), StartOffset: 0, EndOffset: pieceLength + int64(len(tail)),
		}},
		V2MerkleRoots: map[string]string{"multi.mkv": rootHex},
		PieceLayers:   map[string][]byte{rootHex: layer},
	}
	domain, ok := torrentmeta.NewProofDomain(meta, &meta.Files[0])
	if !ok {
		t.Fatal("expected multi-piece v2 domain")
	}
	proof, ok := domain.ProofAt(pieceLength)
	if !ok || proof.Scope != contentproof.ScopeBlock || proof.Offset != pieceLength || proof.Length != int64(len(tail)) {
		t.Fatalf("multi-piece proof = %+v, %v", proof, ok)
	}
	digest, err := proof.CandidateDigest(context.Background(), tail)
	if err != nil || !equalDigest(digest, tailRoot[:]) {
		t.Fatalf("tail digest = %x, %v", digest, err)
	}

	secondData := make([]byte, pieceLength)
	for i := range secondData {
		secondData[i] = 'b'
	}
	secondRoot := v2PieceRoot(secondData, pieceLength)
	secondRootHex := hex.EncodeToString(secondRoot[:])
	hybrid := &torrentmeta.TorrentMeta{
		PieceLength: pieceLength,
		Files: []torrentmeta.FileEntry{
			{Path: "padding.bin", Size: pieceLength + 1, StartOffset: 0, EndOffset: pieceLength + 1},
			{Path: "video.mkv", Size: pieceLength, StartOffset: pieceLength + 1, EndOffset: 2*pieceLength + 1},
		},
		PieceHashesV1: make([]byte, 3*sha1.Size),
		V2MerkleRoots: map[string]string{"video.mkv": secondRootHex},
	}
	hybridDomain, ok := torrentmeta.NewProofDomain(hybrid, &hybrid.Files[1])
	if !ok {
		t.Fatal("expected hybrid proof domain")
	}
	hybridProof, ok := hybridDomain.ProofAt(0)
	if !ok || hybridProof.Provenance != contentproof.ProvenanceTorrentMerkle {
		t.Fatalf("hybrid proof = %+v, %v", hybridProof, ok)
	}
}

func TestProofDomainRejectsContradictoryExtents(t *testing.T) {
	meta := &torrentmeta.TorrentMeta{
		PieceLength: 4,
		Files: []torrentmeta.FileEntry{
			{Path: "a.bin", Size: 4, StartOffset: 0, EndOffset: 4},
			{Path: "b.bin", Size: 4, StartOffset: 5, EndOffset: 9},
		},
		PieceHashesV1: make([]byte, 3*sha1.Size),
	}
	if _, ok := torrentmeta.NewProofDomain(meta, &meta.Files[0]); ok {
		t.Fatal("expected non-contiguous extent rejection")
	}
}

func equalDigest(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestCrossLaneCandidateRequiresExactSizeAndUsableProof(t *testing.T) {
	data := []byte("abcd")
	sum := sha1.Sum(data)
	meta := &torrentmeta.TorrentMeta{
		PieceLength:   4,
		Files:         []torrentmeta.FileEntry{{Path: "video.mkv", Size: 4, StartOffset: 0, EndOffset: 4}},
		PieceHashesV1: append([]byte(nil), sum[:]...),
	}
	candidate, err := torrentmeta.NewCrossLaneCandidate(meta, &meta.Files[0], "item", "file", "representation", "Example.Release", 4)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Candidate.Lane != contentproof.LaneTorrent || candidate.Candidate.ReleaseKey == "" {
		t.Fatalf("candidate = %+v", candidate.Candidate)
	}
	if _, err := torrentmeta.NewCrossLaneCandidate(meta, &meta.Files[0], "item", "file", "representation", "Example.Release", 5); err == nil {
		t.Fatal("expected exact-size rejection")
	}

	proofless := *meta
	proofless.PieceHashesV1 = nil
	if _, err := torrentmeta.NewCrossLaneCandidate(&proofless, &proofless.Files[0], "item", "file", "representation", "Example.Release", 4); err == nil {
		t.Fatal("expected no-proof rejection")
	}
}

func TestProofDomainRejectsMalformedAndCandidateDigestCancels(t *testing.T) {
	data := []byte("abcd")
	sum := sha1.Sum(data)
	meta := &torrentmeta.TorrentMeta{
		PieceLength:   4,
		Files:         []torrentmeta.FileEntry{{Path: "video.mkv", Size: 4, StartOffset: 0, EndOffset: 4}},
		PieceHashesV1: append([]byte(nil), sum[:]...),
	}
	domain, ok := torrentmeta.NewProofDomain(meta, &meta.Files[0])
	if !ok {
		t.Fatal("expected proof domain")
	}
	proof, ok := domain.ProofAt(0)
	if !ok {
		t.Fatal("expected proof")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := proof.CandidateDigest(ctx, data); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, err := proof.CandidateDigest(context.Background(), data[:3]); err == nil {
		t.Fatal("expected short candidate rejection")
	}

	malformed := *meta
	malformed.PieceHashesV1 = malformed.PieceHashesV1[:19]
	if _, ok := torrentmeta.NewProofDomain(&malformed, &malformed.Files[0]); ok {
		t.Fatal("expected malformed hash rejection")
	}
	oversized := *meta
	oversized.PieceLength = torrentmeta.MaxProofBlockBytes + 1
	if _, ok := torrentmeta.NewProofDomain(&oversized, &oversized.Files[0]); ok {
		t.Fatal("expected oversized proof rejection")
	}
}

func TestNativeProofDigestIsDefensiveCopy(t *testing.T) {
	data := []byte("abcd")
	sum := sha1.Sum(data)
	meta := &torrentmeta.TorrentMeta{
		PieceLength:   4,
		Files:         []torrentmeta.FileEntry{{Path: "video.mkv", Size: 4, StartOffset: 0, EndOffset: 4}},
		PieceHashesV1: append([]byte(nil), sum[:]...),
	}
	domain, _ := torrentmeta.NewProofDomain(meta, &meta.Files[0])
	proof, _ := domain.ProofAt(0)
	first := proof.Digest()
	first[0] ^= 0xff
	second := proof.Digest()
	if first[0] == second[0] {
		t.Fatal("digest mutation escaped defensive copy")
	}
}

func v2PieceRoot(data []byte, pieceLength int) [32]byte {
	const blockSize = 16384
	leavesNeeded := pieceLength / blockSize
	leaves := make([][32]byte, leavesNeeded)
	used := 0
	for offset := 0; offset < len(data); offset += blockSize {
		end := offset + blockSize
		if end > len(data) {
			end = len(data)
		}
		leaves[used] = sha256.Sum256(data[offset:end])
		used++
	}
	zero := sha256.Sum256(make([]byte, blockSize))
	for used < len(leaves) {
		leaves[used] = zero
		used++
	}
	for len(leaves) > 1 {
		next := make([][32]byte, len(leaves)/2)
		for i := range next {
			var pair [64]byte
			copy(pair[:32], leaves[2*i][:])
			copy(pair[32:], leaves[2*i+1][:])
			next[i] = sha256.Sum256(pair[:])
		}
		leaves = next
	}
	return leaves[0]
}
