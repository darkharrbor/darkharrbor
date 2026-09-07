package contentproof_test

import (
	"context"
	"crypto/md5"  // #nosec G501 -- PAR2 known-answer fixture.
	"crypto/sha1" // #nosec G505 -- BitTorrent v1 known-answer fixture.
	"crypto/sha256"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func mappingGraph(t *testing.T) (*contentproof.Graph, *store.Store, time.Time) {
	t.Helper()
	graph, st, now := newGraph(t, 64)
	return graph, st, *now
}

func mappingCandidates(t *testing.T, size int64) (contentproof.LaneCandidate, contentproof.LaneCandidate) {
	t.Helper()
	httpCandidate, err := contentproof.NewLaneCandidate(contentproof.LaneHTTP, "http-item", "http-file", "http-rep", "Example.Release", size)
	if err != nil {
		t.Fatal(err)
	}
	proofCandidate, err := contentproof.NewLaneCandidate(contentproof.LaneTorrent, "torrent-item", "torrent-file", "torrent-rep", "example release", size)
	if err != nil {
		t.Fatal(err)
	}
	return httpCandidate, proofCandidate
}

func mappingEvidence(now time.Time, representationID string, data []byte, algorithm contentproof.Algorithm, provenance contentproof.Provenance) contentproof.Evidence {
	var digest []byte
	switch algorithm {
	case contentproof.AlgorithmMD5:
		sum := md5.Sum(data)
		digest = sum[:]
	case contentproof.AlgorithmSHA1:
		sum := sha1.Sum(data)
		digest = sum[:]
	case contentproof.AlgorithmSHA256:
		sum := sha256.Sum256(data)
		digest = sum[:]
	}
	return contentproof.Evidence{
		RepresentationID: representationID,
		Scope:            contentproof.ScopeBlock,
		Offset:           0,
		Length:           int64(len(data)),
		Kind:             contentproof.KindAuthoritative,
		Algorithm:        algorithm,
		Digest:           digest,
		Provenance:       provenance,
		ObservedAt:       now,
	}
}

func TestMapHTTPRepresentationKnownAnswers(t *testing.T) {
	tests := []struct {
		name       string
		lane       contentproof.Lane
		algorithm  contentproof.Algorithm
		provenance contentproof.Provenance
	}{
		{"torrent v1", contentproof.LaneTorrent, contentproof.AlgorithmSHA1, contentproof.ProvenanceTorrentPiece},
		{"torrent v2", contentproof.LaneTorrent, contentproof.AlgorithmSHA256, contentproof.ProvenanceTorrentMerkle},
		{"nntp par2", contentproof.LaneNNTP, contentproof.AlgorithmMD5, contentproof.ProvenancePAR2IFSC},
		{"http digest", contentproof.LaneHTTP, contentproof.AlgorithmSHA256, contentproof.ProvenanceHTTPDigest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			graph, st, now := mappingGraph(t)
			data := []byte("authoritative bytes")
			httpCandidate, _ := mappingCandidates(t, int64(len(data)))
			proofCandidate, err := contentproof.NewLaneCandidate(tc.lane, "proof-item", "proof-file", "proof-rep", "example release", int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			proof := mappingEvidence(now, proofCandidate.RepresentationID, data, tc.algorithm, tc.provenance)
			decision, err := graph.MapHTTPRepresentation(context.Background(), bytesource.NewMemSource("source", data), httpCandidate, proofCandidate, proof)
			if err != nil || decision.Relation != contentproof.RelationProven {
				t.Fatalf("decision = %+v, err = %v", decision, err)
			}
			proofs, err := st.ListContentProofs(context.Background(), []string{httpCandidate.RepresentationID}, now)
			if err != nil || len(proofs) != 1 || proofs[0].Provenance != tc.provenance {
				t.Fatalf("mapped proofs = %+v, err = %v", proofs, err)
			}
		})
	}
}

func TestMapHTTPRepresentationRejectsLaneProvenanceMismatchAndExpiredProof(t *testing.T) {
	graph, _, now := mappingGraph(t)
	data := []byte("authoritative bytes")
	httpCandidate, proofCandidate := mappingCandidates(t, int64(len(data)))
	proof := mappingEvidence(now, proofCandidate.RepresentationID, data, contentproof.AlgorithmMD5, contentproof.ProvenancePAR2IFSC)
	if decision, err := graph.MapHTTPRepresentation(context.Background(), bytesource.NewMemSource("source", data), httpCandidate, proofCandidate, proof); err == nil || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("lane mismatch decision = %+v, err = %v", decision, err)
	}

	proof = mappingEvidence(now.Add(-2*time.Hour), proofCandidate.RepresentationID, data, contentproof.AlgorithmSHA1, contentproof.ProvenanceTorrentPiece)
	expired := now.Add(-time.Hour)
	proof.ExpiresAt = &expired
	if decision, err := graph.MapHTTPRepresentation(context.Background(), bytesource.NewMemSource("source", data), httpCandidate, proofCandidate, proof); err != nil || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("expired decision = %+v, err = %v", decision, err)
	}
}

func TestMapHTTPRepresentationConflictDoesNotMap(t *testing.T) {
	graph, st, now := mappingGraph(t)
	good := []byte("authoritative bytes")
	bad := append([]byte(nil), good...)
	bad[0] ^= 0xff
	httpCandidate, proofCandidate := mappingCandidates(t, int64(len(good)))
	proof := mappingEvidence(now, proofCandidate.RepresentationID, good, contentproof.AlgorithmSHA1, contentproof.ProvenanceTorrentPiece)
	decision, err := graph.MapHTTPRepresentation(context.Background(), bytesource.NewMemSource("source", bad), httpCandidate, proofCandidate, proof)
	if err != nil || decision.Relation != contentproof.RelationConflict {
		t.Fatalf("decision = %+v, err = %v", decision, err)
	}
	proofs, err := st.ListContentProofs(context.Background(), []string{httpCandidate.RepresentationID}, now)
	if err != nil || len(proofs) != 0 {
		t.Fatalf("HTTP proofs = %+v, err = %v", proofs, err)
	}
}

func TestMapHTTPRepresentationHintsNeverAuthorizeBytes(t *testing.T) {
	graph, _, now := mappingGraph(t)
	data := []byte("authoritative bytes")
	httpCandidate, proofCandidate := mappingCandidates(t, int64(len(data)))
	wrongName, err := contentproof.NewLaneCandidate(contentproof.LaneTorrent, "other-item", "other-file", proofCandidate.RepresentationID, "Different.Release", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	proof := mappingEvidence(now, proofCandidate.RepresentationID, data, contentproof.AlgorithmSHA1, contentproof.ProvenanceTorrentPiece)
	decision, err := graph.MapHTTPRepresentation(context.Background(), bytesource.NewMemSource("source", data), httpCandidate, wrongName, proof)
	if err != nil || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("decision = %+v, err = %v", decision, err)
	}
}

func TestMapHTTPRepresentationRejectsExistingNativeConflict(t *testing.T) {
	graph, st, now := mappingGraph(t)
	data := []byte("authoritative bytes")
	httpCandidate, proofCandidate := mappingCandidates(t, int64(len(data)))
	existing := mappingEvidence(now, proofCandidate.RepresentationID, []byte("different evidence"), contentproof.AlgorithmSHA1, contentproof.ProvenanceTorrentPiece)
	existing.Length = int64(len(data))
	if err := graph.Record(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	incoming := mappingEvidence(now, proofCandidate.RepresentationID, data, contentproof.AlgorithmSHA1, contentproof.ProvenanceTorrentPiece)
	decision, err := graph.MapHTTPRepresentation(context.Background(), bytesource.NewMemSource("source", data), httpCandidate, proofCandidate, incoming)
	if err != nil || decision.Relation != contentproof.RelationConflict {
		t.Fatalf("decision = %+v, err = %v", decision, err)
	}
	proofs, err := st.ListContentProofs(context.Background(), []string{proofCandidate.RepresentationID}, now)
	if err != nil || len(proofs) != 1 || string(proofs[0].Digest) != string(existing.Digest) {
		t.Fatalf("native proof was overwritten: %+v, err = %v", proofs, err)
	}
}

func TestMapHTTPRepresentationAbstainsOnShortInexactAndOversizedSources(t *testing.T) {
	graph, _, now := mappingGraph(t)
	data := []byte("authoritative bytes")
	httpCandidate, proofCandidate := mappingCandidates(t, int64(len(data)))
	proof := mappingEvidence(now, proofCandidate.RepresentationID, data, contentproof.AlgorithmSHA1, contentproof.ProvenanceTorrentPiece)

	short := &mappingSource{data: data[:4], size: int64(len(data)), caps: bytesource.Capabilities{RangeSupport: true, ExactSize: true}}
	decision, err := graph.MapHTTPRepresentation(context.Background(), short, httpCandidate, proofCandidate, proof)
	if err != nil || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("short decision = %+v, err = %v", decision, err)
	}

	inexact := &mappingSource{data: data, size: int64(len(data)), caps: bytesource.Capabilities{RangeSupport: true}}
	decision, err = graph.MapHTTPRepresentation(context.Background(), inexact, httpCandidate, proofCandidate, proof)
	if err != nil || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("inexact decision = %+v, err = %v", decision, err)
	}

	proof.Length = contentproof.MaxMappingProofBytes + 1
	proofCandidate.Size = proof.Length
	httpCandidate.Size = proof.Length
	decision, err = graph.MapHTTPRepresentation(context.Background(), nil, httpCandidate, proofCandidate, proof)
	if err != nil || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("oversized decision = %+v, err = %v", decision, err)
	}
}

func TestMapHTTPRepresentationCancellationAndConcurrentUse(t *testing.T) {
	graph, _, now := mappingGraph(t)
	data := []byte("authoritative bytes")
	httpCandidate, proofCandidate := mappingCandidates(t, int64(len(data)))
	proof := mappingEvidence(now, proofCandidate.RepresentationID, data, contentproof.AlgorithmSHA1, contentproof.ProvenanceTorrentPiece)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decision, err := graph.MapHTTPRepresentation(ctx, bytesource.NewMemSource("source", data), httpCandidate, proofCandidate, proof)
	if !errors.Is(err, context.Canceled) || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("cancelled decision = %+v, err = %v", decision, err)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := graph.MapHTTPRepresentation(context.Background(), bytesource.NewMemSource("source", data), httpCandidate, proofCandidate, proof)
			if err != nil || decision.Relation != contentproof.RelationProven {
				t.Errorf("decision = %+v, err = %v", decision, err)
			}
		}()
	}
	wg.Wait()
}

func TestMapHTTPRepresentationWithNativeDigest(t *testing.T) {
	graph, _, now := mappingGraph(t)
	data := []byte("native merkle bytes")
	httpCandidate, proofCandidate := mappingCandidates(t, int64(len(data)))
	native := sha256.Sum256(append([]byte("merkle-domain:"), data...))
	proof := contentproof.Evidence{
		RepresentationID: proofCandidate.RepresentationID,
		Scope:            contentproof.ScopeBlock,
		Offset:           0,
		Length:           int64(len(data)),
		Kind:             contentproof.KindAuthoritative,
		Algorithm:        contentproof.AlgorithmSHA256,
		Digest:           native[:],
		Provenance:       contentproof.ProvenanceTorrentMerkle,
		ObservedAt:       now,
	}
	digest := func(ctx context.Context, candidate []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(append([]byte("merkle-domain:"), candidate...))
		return sum[:], nil
	}
	decision, err := graph.MapHTTPRepresentationWithDigest(context.Background(), bytesource.NewMemSource("source", data), httpCandidate, proofCandidate, proof, digest)
	if err != nil || decision.Relation != contentproof.RelationProven {
		t.Fatalf("native decision = %+v, err = %v", decision, err)
	}
	if decision, err = graph.MapHTTPRepresentationWithDigest(context.Background(), bytesource.NewMemSource("source", data), httpCandidate, proofCandidate, proof, nil); err == nil || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("nil digest decision = %+v, err = %v", decision, err)
	}
}

type mappingSource struct {
	data []byte
	size int64
	caps bytesource.Capabilities
}

func (s *mappingSource) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *mappingSource) Size() int64                   { return s.size }
func (s *mappingSource) Caps() bytesource.Capabilities { return s.caps }
func (s *mappingSource) Key() string                   { return "mapping-source" }
