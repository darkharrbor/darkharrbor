package contentproof_test

import (
	"bytes"
	"context"
	"crypto/md5"  // #nosec G501 -- PAR2 IFSC is defined as MD5.
	"crypto/sha1" // #nosec G505 -- BitTorrent v1 is defined as SHA-1.
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/crosslane"
)

func TestXR02SymmetricRecoveryMatrix(t *testing.T) {
	tests := []struct {
		name       string
		targetLane contentproof.Lane
		sourceLane contentproof.Lane
		provenance contentproof.Provenance
		algorithm  contentproof.Algorithm
	}{
		{"torrent_from_nntp", contentproof.LaneTorrent, contentproof.LaneNNTP, contentproof.ProvenanceTorrentPiece, contentproof.AlgorithmSHA1},
		{"torrent_from_http", contentproof.LaneTorrent, contentproof.LaneHTTP, contentproof.ProvenanceTorrentPiece, contentproof.AlgorithmSHA1},
		{"nntp_from_torrent", contentproof.LaneNNTP, contentproof.LaneTorrent, contentproof.ProvenancePAR2IFSC, contentproof.AlgorithmMD5},
		{"nntp_from_http", contentproof.LaneNNTP, contentproof.LaneHTTP, contentproof.ProvenancePAR2IFSC, contentproof.AlgorithmMD5},
		{"http_from_torrent", contentproof.LaneHTTP, contentproof.LaneTorrent, contentproof.ProvenanceTorrentPiece, contentproof.AlgorithmSHA1},
		{"http_from_nntp", contentproof.LaneHTTP, contentproof.LaneNNTP, contentproof.ProvenancePAR2IFSC, contentproof.AlgorithmMD5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			graph, _, now := newGraph(t, 8)
			data := []byte("abcdefgh")
			corruptSecond := append([]byte(nil), data...)
			corruptSecond[4] ^= 0xff
			target := recoveryCandidate(t, test.targetLane, "target", "target-rep", data)
			first := recoveryCandidate(t, test.sourceLane, "first", "first-rep", data)
			second := recoveryCandidate(t, test.sourceLane, "second", "second-rep", data)
			coordinator := crosslane.NewCoordinator(graph, nil)

			noProofSource := &untouchedSource{data: data}
			noProof := xr02Proof(target, data[:4], 0, test.provenance, test.algorithm, *now)
			got, err := coordinator.RecoverBlock(context.Background(), crosslane.Request{
				Target: target,
				Proof:  noProof,
				Candidates: []crosslane.Candidate{{
					Handle: first,
					Source: noProofSource,
				}},
			})
			if err != nil || got.Data != nil || got.Decision.Relation != contentproof.RelationNoProof || noProofSource.calls.Load() != 0 {
				t.Fatalf("no-proof result = %+v, err=%v, source_calls=%d", got, err, noProofSource.calls.Load())
			}

			var assembled []byte
			var selected []string
			for offset := int64(0); offset < int64(len(data)); offset += 4 {
				proof := xr02Proof(target, data[offset:offset+4], offset, test.provenance, test.algorithm, *now)
				record(t, graph, proof)
				block, recoverErr := coordinator.RecoverBlock(context.Background(), crosslane.Request{
					Target: target,
					Proof:  proof,
					Candidates: []crosslane.Candidate{
						{Handle: first, Source: bytesource.NewMemSource("first", corruptSecond)},
						{Handle: second, Source: bytesource.NewMemSource("second", data)},
					},
				})
				if recoverErr != nil {
					t.Fatal(recoverErr)
				}
				if block.Decision.Relation != contentproof.RelationProven || !bytes.Equal(block.Data, data[offset:offset+4]) {
					t.Fatalf("block at %d = %+v", offset, block)
				}
				assembled = append(assembled, block.Data...)
				selected = append(selected, block.Candidate.RepresentationID)
			}
			if !bytes.Equal(assembled, data) {
				t.Fatalf("assembled bytes = %q, want %q", assembled, data)
			}
			if len(selected) != 2 || selected[0] != first.RepresentationID || selected[1] != second.RepresentationID {
				t.Fatalf("selected representations = %v, want [%s %s]", selected, first.RepresentationID, second.RepresentationID)
			}
		})
	}
}

func xr02Proof(target contentproof.LaneCandidate, data []byte, offset int64, provenance contentproof.Provenance, algorithm contentproof.Algorithm, now time.Time) contentproof.Evidence {
	var digest []byte
	switch algorithm {
	case contentproof.AlgorithmMD5:
		sum := md5.Sum(data) // #nosec G401 -- PAR2 IFSC is defined as MD5.
		digest = sum[:]
	default:
		sum := sha1.Sum(data) // #nosec G401 -- BitTorrent v1 is defined as SHA-1.
		digest = sum[:]
	}
	return contentproof.Evidence{
		RepresentationID: target.RepresentationID,
		Scope:            contentproof.ScopeBlock,
		Offset:           offset,
		Length:           int64(len(data)),
		Kind:             contentproof.KindAuthoritative,
		Algorithm:        algorithm,
		Digest:           digest,
		Provenance:       provenance,
		ObservedAt:       now,
	}
}
