package contentproof_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/crosslane"
	"github.com/darkharrbor/darkharrbor/internal/ladder"
)

func TestRecoveryCoordinatorFallsThroughAndEmitsOnlyProvenBlock(t *testing.T) {
	graph, _, now := newGraph(t, 8)
	data := []byte("verified cross-source block")
	target := recoveryCandidate(t, contentproof.LaneTorrent, "target", "target-rep", data)
	bad := recoveryCandidate(t, contentproof.LaneHTTP, "bad", "bad-rep", data)
	good := recoveryCandidate(t, contentproof.LaneNNTP, "good", "good-rep", data)
	proof := recoveryProof(target, data, *now)
	record(t, graph, proof)

	got, err := crosslane.NewCoordinator(graph, nil).RecoverBlock(context.Background(), crosslane.Request{
		Target: target, Proof: proof,
		Candidates: []crosslane.Candidate{
			{Handle: bad, Source: bytesource.NewMemSource("bad-source", []byte("corrupted cross-source data"))},
			{Handle: good, Source: bytesource.NewMemSource("good-source", data)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != string(data) || got.Candidate.RepresentationID != good.RepresentationID || got.Decision.Relation != contentproof.RelationProven {
		t.Fatalf("recovery result = %+v", got)
	}
	if len(got.Ladder.Rungs) != 2 || got.Ladder.Rungs[0].Attempts != 1 || !got.Ladder.Succeeded {
		t.Fatalf("ladder result = %+v", got.Ladder)
	}
}

func TestRecoveryCoordinatorUsesNativeCandidateDigestAndRejectsCorruption(t *testing.T) {
	graph, _, now := newGraph(t, 8)
	data := []byte("native proof transform")
	target := recoveryCandidate(t, contentproof.LaneTorrent, "target", "target-rep", data)
	candidate := recoveryCandidate(t, contentproof.LaneHTTP, "candidate", "candidate-rep", data)
	nativeDigest := func(ctx context.Context, candidateData []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sum := sha1.Sum(append([]byte("native:"), candidateData...)) // #nosec G401 -- native-proof test fixture.
		return sum[:], nil
	}
	proof := recoveryProof(target, data, *now)
	proof.Digest, _ = nativeDigest(context.Background(), data)
	record(t, graph, proof)

	req := crosslane.Request{
		Target: target, Proof: proof, CandidateDigest: nativeDigest,
		Candidates: []crosslane.Candidate{{Handle: candidate, Source: bytesource.NewMemSource("source", data)}},
	}
	got, err := crosslane.NewCoordinator(graph, nil).RecoverBlock(context.Background(), req)
	if err != nil || string(got.Data) != string(data) || got.Decision.Relation != contentproof.RelationProven {
		t.Fatalf("native-digest recovery = %+v, %v", got, err)
	}

	corrupt := append([]byte(nil), data...)
	corrupt[0] ^= 0xff
	req.Candidates[0].Source = bytesource.NewMemSource("source", corrupt)
	got, err = crosslane.NewCoordinator(graph, nil).RecoverBlock(context.Background(), req)
	if !errors.Is(err, ladder.ErrExhausted) || got.Data != nil || got.Decision.Relation != contentproof.RelationConflict {
		t.Fatalf("native-digest corruption = %+v, %v", got, err)
	}
}

func TestRecoveryCoordinatorFailsClosedWithoutProofOrOnConflict(t *testing.T) {
	graph, _, now := newGraph(t, 8)
	data := []byte("proof-gated bytes")
	target := recoveryCandidate(t, contentproof.LaneTorrent, "target", "target-rep", data)
	candidate := recoveryCandidate(t, contentproof.LaneHTTP, "candidate", "candidate-rep", data)
	proof := recoveryProof(target, data, *now)
	coordinator := crosslane.NewCoordinator(graph, nil)
	req := crosslane.Request{Target: target, Proof: proof, Candidates: []crosslane.Candidate{{Handle: candidate, Source: bytesource.NewMemSource("source", data)}}}

	got, err := coordinator.RecoverBlock(context.Background(), req)
	if err != nil || got.Data != nil || got.Decision.Relation != contentproof.RelationNoProof || len(got.Ladder.Rungs) != 0 {
		t.Fatalf("no-proof result = %+v, %v", got, err)
	}
	record(t, graph, proof)
	req.Candidates[0].Source = bytesource.NewMemSource("source", []byte("different payload"))
	got, err = coordinator.RecoverBlock(context.Background(), req)
	if !errors.Is(err, ladder.ErrExhausted) || got.Data != nil || got.Decision.Relation != contentproof.RelationConflict {
		t.Fatalf("conflict result = %+v, %v", got, err)
	}
}

func TestHR65SameTitleSizeRuntimeWithoutProofNeverTouchesSource(t *testing.T) {
	graph, _, now := newGraph(t, 8)
	data := []byte("similar but unproven bytes")
	similarity := struct {
		title   string
		size    int64
		runtime time.Duration
	}{title: "Same.Release.2026", size: int64(len(data)), runtime: 42 * time.Minute}
	// Runtime deliberately has no coordinator field: it cannot authorize bytes.
	target, err := contentproof.NewLaneCandidate(contentproof.LaneHTTP, "target", "target-file", "target-rep", similarity.title, similarity.size)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := contentproof.NewLaneCandidate(contentproof.LaneTorrent, "candidate", "candidate-file", "candidate-rep", similarity.title, similarity.size)
	if err != nil {
		t.Fatal(err)
	}
	proof := recoveryProof(target, data, *now)
	proof.Provenance = contentproof.ProvenanceHTTPDigest
	source := &untouchedSource{data: data}
	coordinator := crosslane.NewCoordinator(graph, nil)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := coordinator.RecoverBlock(context.Background(), crosslane.Request{
				Target: target, Proof: proof,
				Candidates: []crosslane.Candidate{{Handle: candidate, Source: source}},
			})
			if err != nil || got.Data != nil || got.Decision.Relation != contentproof.RelationNoProof || len(got.Ladder.Rungs) != 0 {
				t.Errorf("proofless similarity result = %+v, %v", got, err)
			}
		}()
	}
	wg.Wait()
	if got := source.calls.Load(); got != 0 {
		t.Fatalf("proofless similarity touched candidate source %d times", got)
	}
}

func TestRecoveryCoordinatorRejectsMalformedAndShortInputs(t *testing.T) {
	graph, _, now := newGraph(t, 8)
	data := []byte("bounded recovery bytes")
	target := recoveryCandidate(t, contentproof.LaneTorrent, "target", "target-rep", data)
	candidate := recoveryCandidate(t, contentproof.LaneHTTP, "candidate", "candidate-rep", data)
	proof := recoveryProof(target, data, *now)
	record(t, graph, proof)
	coordinator := crosslane.NewCoordinator(graph, nil)

	wrong := candidate
	wrong.Size++
	for name, req := range map[string]crosslane.Request{
		"no candidates":        {Target: target, Proof: proof},
		"mismatched candidate": {Target: target, Proof: proof, Candidates: []crosslane.Candidate{{Handle: wrong, Source: bytesource.NewMemSource("source", data)}}},
		"inexact source":       {Target: target, Proof: proof, Candidates: []crosslane.Candidate{{Handle: candidate, Source: &inexactSource{data: data}}}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := coordinator.RecoverBlock(context.Background(), req)
			if err == nil || got.Data != nil {
				t.Fatalf("result = %+v, %v", got, err)
			}
		})
	}

	got, err := coordinator.RecoverBlock(context.Background(), crosslane.Request{
		Target: target, Proof: proof,
		Candidates: []crosslane.Candidate{{Handle: candidate, Source: &shortSource{size: int64(len(data))}}},
	})
	if !errors.Is(err, ladder.ErrExhausted) || got.Data != nil {
		t.Fatalf("short-read result = %+v, %v", got, err)
	}
	if strings.Contains(fmt.Sprint(got), "source-private-detail") {
		t.Fatal("source error detail escaped coordinator")
	}
}

func TestRecoveryCoordinatorCancellationReleasesRecoveryLease(t *testing.T) {
	graph, _, now := newGraph(t, 8)
	data := []byte("cancelled recovery")
	target := recoveryCandidate(t, contentproof.LaneTorrent, "target", "target-rep", data)
	candidate := recoveryCandidate(t, contentproof.LaneHTTP, "candidate", "candidate-rep", data)
	proof := recoveryProof(target, data, *now)
	record(t, graph, proof)
	gov := accountgov.New("fixture")
	gov.SetCapacity(crosslane.Operation, 1)
	lease, err := gov.Acquire(context.Background(), crosslane.Operation, accountgov.PriorityPlayback, "playback")
	if err != nil {
		t.Fatal(err)
	}
	coordinator := crosslane.NewCoordinator(graph, gov)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := coordinator.RecoverBlock(ctx, crosslane.Request{Target: target, Proof: proof, Candidates: []crosslane.Candidate{{Handle: candidate, Source: bytesource.NewMemSource("source", data)}}})
		done <- runErr
	}()
	deadline := time.Now().Add(time.Second)
	for gov.Waiting(crosslane.Operation) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	lease.Release()
	if gov.Waiting(crosslane.Operation) != 0 || gov.InUse(crosslane.Operation) != 0 {
		t.Fatalf("leaked governor state: waiting=%d in_use=%d", gov.Waiting(crosslane.Operation), gov.InUse(crosslane.Operation))
	}
}

func TestRecoveryCoordinatorConcurrentBlocksChooseIndependently(t *testing.T) {
	graph, _, now := newGraph(t, 8)
	data := []byte("abcdefgh")
	target := recoveryCandidate(t, contentproof.LaneTorrent, "target", "target-rep", data)
	first := recoveryCandidate(t, contentproof.LaneHTTP, "first", "first-rep", data)
	second := recoveryCandidate(t, contentproof.LaneNNTP, "second", "second-rep", data)
	coordinator := crosslane.NewCoordinator(graph, nil)

	var wg sync.WaitGroup
	for offset := int64(0); offset < int64(len(data)); offset += 4 {
		block := append([]byte(nil), data[offset:offset+4]...)
		proof := recoveryProof(target, block, *now)
		proof.Offset, proof.Length = offset, 4
		digest := sha1.Sum(block)
		proof.Digest = digest[:]
		record(t, graph, proof)
		wg.Add(1)
		go func(proof contentproof.Evidence) {
			defer wg.Done()
			got, err := coordinator.RecoverBlock(context.Background(), crosslane.Request{
				Target: target, Proof: proof,
				Candidates: []crosslane.Candidate{{Handle: first, Source: bytesource.NewMemSource("first", data)}, {Handle: second, Source: bytesource.NewMemSource("second", data)}},
			})
			if err != nil || len(got.Data) != 4 || got.Decision.Relation != contentproof.RelationProven {
				t.Errorf("block recovery = %+v, %v", got, err)
			}
		}(proof)
	}
	wg.Wait()
}

func TestHR64StripesOnlyVerifiedBlocksAcrossSources(t *testing.T) {
	graph, _, now := newGraph(t, 8)
	data := []byte("abcdefgh")
	target := recoveryCandidate(t, contentproof.LaneTorrent, "target", "target-rep", data)
	first := recoveryCandidate(t, contentproof.LaneHTTP, "first", "first-rep", data)
	second := recoveryCandidate(t, contentproof.LaneNNTP, "second", "second-rep", data)
	corruptFirst := append([]byte(nil), data...)
	corruptFirst[4] ^= 0xff
	coordinator := crosslane.NewCoordinator(graph, nil)

	var assembled []byte
	var selected []string
	for offset := int64(0); offset < int64(len(data)); offset += 4 {
		block := data[offset : offset+4]
		proof := recoveryProof(target, block, *now)
		proof.Offset, proof.Length = offset, 4
		digest := sha1.Sum(block)
		proof.Digest = digest[:]
		record(t, graph, proof)

		got, err := coordinator.RecoverBlock(context.Background(), crosslane.Request{
			Target: target,
			Proof:  proof,
			Candidates: []crosslane.Candidate{
				{Handle: first, Source: bytesource.NewMemSource("first", corruptFirst)},
				{Handle: second, Source: bytesource.NewMemSource("second", data)},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Decision.Relation != contentproof.RelationProven || !bytes.Equal(got.Data, block) {
			t.Fatalf("block at %d = %+v", offset, got)
		}
		assembled = append(assembled, got.Data...)
		selected = append(selected, got.Candidate.RepresentationID)
	}
	if !bytes.Equal(assembled, data) {
		t.Fatalf("striped bytes = %q, want %q", assembled, data)
	}
	if len(selected) != 2 || selected[0] != first.RepresentationID || selected[1] != second.RepresentationID {
		t.Fatalf("selected representations = %v, want [%s %s]", selected, first.RepresentationID, second.RepresentationID)
	}
}

func FuzzRecoveryCoordinatorFailsClosed(f *testing.F) {
	for _, seed := range [][]byte{[]byte("abcd"), nil, []byte("different"), make([]byte, 65)} {
		f.Add(seed, int64(0), int64(4))
	}
	f.Fuzz(func(t *testing.T, data []byte, offset, length int64) {
		if len(data) > 128 {
			data = data[:128]
		}
		graph, _, now := newGraph(t, 4)
		base := []byte("abcd")
		target := recoveryCandidate(t, contentproof.LaneTorrent, "target", "target-rep", base)
		candidate := recoveryCandidate(t, contentproof.LaneHTTP, "candidate", "candidate-rep", base)
		proof := recoveryProof(target, base, *now)
		proof.Offset, proof.Length = offset, length
		_, _ = crosslane.NewCoordinator(graph, nil).RecoverBlock(context.Background(), crosslane.Request{
			Target: target, Proof: proof, Candidates: []crosslane.Candidate{{Handle: candidate, Source: bytesource.NewMemSource("source", data)}},
		})
	})
}

func recoveryCandidate(t testing.TB, lane contentproof.Lane, itemID, representationID string, data []byte) contentproof.LaneCandidate {
	t.Helper()
	candidate, err := contentproof.NewLaneCandidate(lane, itemID, itemID+"-file", representationID, "shared release", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func recoveryProof(target contentproof.LaneCandidate, data []byte, now time.Time) contentproof.Evidence {
	digest := sha1.Sum(data)
	return contentproof.Evidence{RepresentationID: target.RepresentationID, Scope: contentproof.ScopeBlock, Length: int64(len(data)), Kind: contentproof.KindAuthoritative, Algorithm: contentproof.AlgorithmSHA1, Digest: digest[:], Provenance: contentproof.ProvenanceTorrentPiece, ObservedAt: now}
}

type shortSource struct{ size int64 }

func (s *shortSource) ReadAt(context.Context, []byte, int64) (int, error) {
	return 0, errors.New("source-private-detail")
}
func (s *shortSource) Size() int64 { return s.size }
func (s *shortSource) Key() string { return "short" }
func (s *shortSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: true}
}

type inexactSource struct{ data []byte }

func (s *inexactSource) ReadAt(context.Context, []byte, int64) (int, error) { return 0, nil }
func (s *inexactSource) Size() int64                                        { return int64(len(s.data)) }
func (s *inexactSource) Key() string                                        { return "inexact" }
func (s *inexactSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: false}
}

type untouchedSource struct {
	data  []byte
	calls atomic.Int64
}

func (s *untouchedSource) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	s.calls.Add(1)
	return copy(p, s.data[off:]), nil
}
func (s *untouchedSource) Size() int64 {
	s.calls.Add(1)
	return int64(len(s.data))
}
func (s *untouchedSource) Key() string { return "untouched" }
func (s *untouchedSource) Caps() bytesource.Capabilities {
	s.calls.Add(1)
	return bytesource.Capabilities{RangeSupport: true, ExactSize: true}
}
