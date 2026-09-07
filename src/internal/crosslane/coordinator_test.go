package crosslane_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/crosslane"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type proofRepo struct {
	mu     sync.Mutex
	proofs []contentproof.Evidence
}

func (r *proofRepo) UpsertContentProof(_ context.Context, evidence contentproof.Evidence, _ int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proofs = append(r.proofs, evidence)
	return nil
}
func (r *proofRepo) ListContentProofs(_ context.Context, ids []string, _ time.Time) ([]contentproof.Evidence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []contentproof.Evidence
	for _, proof := range r.proofs {
		for _, id := range ids {
			if proof.RepresentationID == id {
				result = append(result, proof)
			}
		}
	}
	return result, nil
}
func (*proofRepo) DeleteContentProof(context.Context, contentproof.Key) error { return nil }

type exactSource struct {
	data  []byte
	reads atomic.Int32
}

func (s *exactSource) Size() int64 { return int64(len(s.data)) }
func (*exactSource) Key() string   { return "candidate" }
func (*exactSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: true}
}
func (s *exactSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	s.reads.Add(1)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 || off+int64(len(p)) > int64(len(s.data)) {
		return 0, errors.New("range")
	}
	return copy(p, s.data[off:off+int64(len(p))]), nil
}

func TestHR62MappedPAR2ProofAuthorizesHTTPRecovery(t *testing.T) {
	data := []byte("verified")
	sum := md5.Sum(data)
	repo := &proofRepo{}
	graph, err := contentproof.New(repo, contentproof.Options{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := contentproof.NewLaneCandidate(contentproof.LaneHTTP, "http-item", "file", "http:target", "Example.Release", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := contentproof.NewLaneCandidate(contentproof.LaneNNTP, "nntp-item", "0", "nntp:candidate", "Example.Release", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	proof := contentproof.Evidence{
		RepresentationID: target.RepresentationID,
		Scope:            contentproof.ScopeBlock,
		Offset:           0,
		Length:           int64(len(data)),
		Kind:             contentproof.KindAuthoritative,
		Algorithm:        contentproof.AlgorithmMD5,
		Digest:           sum[:],
		Provenance:       contentproof.ProvenancePAR2IFSC,
		ObservedAt:       time.Now().UTC(),
	}
	if err := graph.Record(context.Background(), proof); err != nil {
		t.Fatal(err)
	}
	source := &exactSource{data: data}
	block, err := crosslane.NewCoordinator(graph, nil).RecoverBlock(context.Background(), crosslane.Request{
		Target: target,
		Proof:  proof,
		Candidates: []crosslane.Candidate{{
			Handle: handle,
			Source: source,
		}},
	})
	if err != nil || block.Decision.Relation != contentproof.RelationProven || !bytes.Equal(block.Data, data) {
		t.Fatalf("RecoverBlock = %#v, %v", block, err)
	}
	if source.reads.Load() != 1 {
		t.Fatalf("candidate reads = %d, want 1", source.reads.Load())
	}
}

func TestCoordinatorConvergesOnlyAfterWholeRepresentationIsProven(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/coordinator.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	repository := store.New(db)
	graph, _ := contentproof.New(repository, contentproof.Options{})
	converger, _ := contentproof.NewConverger(repository, contentproof.Options{})
	data := []byte("whole-representation")
	target, _ := contentproof.NewLaneCandidate(contentproof.LaneTorrent, "torrent-item", "0", "torrent:whole", "Same.Release", int64(len(data)))
	candidate, _ := contentproof.NewLaneCandidate(contentproof.LaneHTTP, "http-item", "file", "http:whole", "Same.Release", int64(len(data)))
	coordinator := crosslane.NewCoordinator(graph, nil, converger)
	for _, span := range [][2]int{{0, 5}, {5, len(data) - 5}} {
		part := data[span[0] : span[0]+span[1]]
		digest := sha1.Sum(part)
		proof := contentproof.Evidence{
			RepresentationID: target.RepresentationID,
			Scope:            contentproof.ScopeBlock,
			Offset:           int64(span[0]),
			Length:           int64(span[1]),
			Kind:             contentproof.KindAuthoritative,
			Algorithm:        contentproof.AlgorithmSHA1,
			Digest:           digest[:],
			Provenance:       contentproof.ProvenanceTorrentPiece,
			ObservedAt:       time.Now().UTC(),
		}
		if err := graph.Record(ctx, proof); err != nil {
			t.Fatal(err)
		}
		block, err := coordinator.RecoverBlock(ctx, crosslane.Request{
			Target: target, Proof: proof,
			Candidates: []crosslane.Candidate{{Handle: candidate, Source: &exactSource{data: data}}},
		})
		if err != nil || block.ConvergenceErr != nil {
			t.Fatalf("RecoverBlock = %+v, %v", block, err)
		}
		if span[0] == 0 && block.CanonicalID != "" {
			t.Fatal("partial proof promoted a canonical representation")
		}
		if span[0] != 0 && block.CanonicalID == "" {
			t.Fatal("whole proof coverage did not promote a canonical representation")
		}
	}
	routes, err := converger.Routes(ctx, target.RepresentationID)
	if err != nil || len(routes) != 2 {
		t.Fatalf("routes = %d, %v", len(routes), err)
	}
}
