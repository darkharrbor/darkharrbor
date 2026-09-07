package reactivepromotion

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type testRepo struct {
	commit    store.ReactiveCommit
	item      *store.Item
	proofs    []contentproof.Evidence
	state     string
	target    string
	size      int64
	completed int64
}

func (r *testRepo) GetReactiveCommit(context.Context, string) (store.ReactiveCommit, bool, error) {
	return r.commit, true, nil
}
func (r *testRepo) GetItemByID(context.Context, string) (*store.Item, error) { return r.item, nil }
func (r *testRepo) ListContentProofs(context.Context, []string, time.Time) ([]contentproof.Evidence, error) {
	return append([]contentproof.Evidence(nil), r.proofs...), nil
}
func (r *testRepo) BeginReactivePromotion(_ context.Context, _, target string, size int64, _ time.Time) error {
	if r.state == "staging" || r.state == "promoted" || r.state == "removed" {
		return errors.New("busy")
	}
	r.state = "staging"
	r.target = target
	r.size = size
	return nil
}
func (r *testRepo) GetReactivePromotion(context.Context, string) (store.ReactivePromotion, bool, error) {
	return store.ReactivePromotion{RepresentationID: "rep", State: r.state, TargetPath: r.target, SizeBytes: r.size}, r.state != "", nil
}
func (r *testRepo) CompleteReactivePromotion(_ context.Context, _ string, n int64, _ time.Time) error {
	r.state, r.completed = "promoted", n
	return nil
}
func (r *testRepo) FailReactivePromotion(context.Context, string, time.Time) error {
	r.state = "failed"
	return nil
}
func (r *testRepo) RemoveReactivePromotion(context.Context, string, time.Time) error {
	r.state = "removed"
	return nil
}
func (r *testRepo) SetReactivePromotionHeadroom(context.Context, int64, time.Time) error { return nil }
func (r *testRepo) UpsertContentProof(_ context.Context, e contentproof.Evidence, _ int) error {
	r.proofs = append(r.proofs, e)
	return nil
}
func (r *testRepo) DeleteContentProof(context.Context, contentproof.Key) error { return nil }

type testCache struct{ cancel bool }

func (c *testCache) YieldForPromotion(ctx context.Context, _ string, _ int64, _ int) (int64, error) {
	return 1234, ctx.Err()
}
func (c *testCache) GetChunk(ctx context.Context, src rangecache.ChunkSource, _ rangecache.Mode, ref rangecache.ChunkRef) ([]byte, error) {
	if c.cancel {
		return nil, context.Canceled
	}
	return src.Fetch(ctx, ref, rangecache.FetchDemand)
}

type testOpener struct{ source bytesource.ByteSource }

func (o testOpener) OpenPromotionSource(context.Context, *store.Item, string) (bytesource.ByteSource, error) {
	return o.source, nil
}

type testRescanner struct {
	err   error
	calls int
}

func (r *testRescanner) Rescan(context.Context, store.ReactiveCommit) error { r.calls++; return r.err }
func (r *testRescanner) Materialize(context.Context, store.ReactiveCommit, string, int64) error {
	r.calls++
	return r.err
}

func newTestService(t *testing.T, data []byte, proof bool) (*Service, *testRepo, *testCache, *testRescanner, string, string) {
	t.Helper()
	root := t.TempDir()
	strm, target := filepath.Join(root, "movie.strm"), filepath.Join(root, "movie.mkv")
	if err := os.WriteFile(strm, []byte("proxy"), 0o640); err != nil {
		t.Fatal(err)
	}
	repo := &testRepo{commit: store.ReactiveCommit{RepresentationID: "rep", ItemID: "item", FileID: "file", State: "committed", ArrName: "radarr", ArrKind: "movie", ArrItemID: 1}, item: &store.Item{ID: "item", State: store.StateReady, StrmPath: &strm}}
	if proof {
		sum := sha256.Sum256(data)
		repo.proofs = []contentproof.Evidence{{RepresentationID: "rep", Scope: contentproof.ScopeWhole, Length: int64(len(data)), Kind: contentproof.KindAuthoritative, Algorithm: contentproof.AlgorithmSHA256, Digest: sum[:], Provenance: contentproof.ProvenanceHTTPDigest, ObservedAt: time.Now()}}
	}
	graph, err := contentproof.New(repo, contentproof.Options{Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	cache, rescanner := &testCache{}, &testRescanner{}
	svc, err := New(repo, cache, testOpener{bytesource.NewMemSource("source", data)}, rescanner, graph, Options{Root: root, FloorPercent: 10, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	return svc, repo, cache, rescanner, strm, target
}

func TestPromotionProofCutoverAndNoResurrection(t *testing.T) {
	data := []byte("verified materialized bytes")
	svc, repo, _, rescanner, strm, target := newTestService(t, data, true)
	if err := svc.Promote(t.Context(), "rep", target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(data) || repo.state != "promoted" || repo.completed != int64(len(data)) || rescanner.calls != 1 {
		t.Fatalf("cutover state=%s bytes=%q err=%v rescans=%d", repo.state, got, err, rescanner.calls)
	}
	if _, err := os.Stat(strm); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("proxy remained after cutover")
	}
	if err := svc.Remove(t.Context(), "rep"); err != nil {
		t.Fatal(err)
	}
	if repo.state != "removed" {
		t.Fatalf("removal state=%s", repo.state)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deliberately removed target remained")
	}
	if err := svc.Promote(t.Context(), "rep", target); err == nil {
		t.Fatal("promoted deletion was resurrected")
	}
}

func TestPromotionNoProofAndRescanFailureRollBack(t *testing.T) {
	data := []byte("candidate")
	svc, repo, _, _, strm, target := newTestService(t, data, false)
	if err := svc.Promote(t.Context(), "rep", target); err == nil {
		t.Fatal("proofless promotion succeeded")
	}
	if _, err := os.Stat(strm); err != nil || repo.state != "failed" {
		t.Fatalf("proof rollback state=%s err=%v", repo.state, err)
	}

	svc, repo, _, rescanner, strm, target := newTestService(t, data, true)
	rescanner.err = errors.New("rescan failed")
	if err := svc.Promote(t.Context(), "rep", target); err == nil {
		t.Fatal("failed rescan succeeded")
	}
	if _, err := os.Stat(strm); err != nil || repo.state != "failed" {
		t.Fatalf("rescan rollback state=%s err=%v", repo.state, err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("materialized target survived failed rescan")
	}
}

func TestPromotionCancellationRemovesStage(t *testing.T) {
	svc, repo, cache, _, _, target := newTestService(t, []byte("candidate"), true)
	cache.cancel = true
	if err := svc.Promote(t.Context(), "rep", target); !errors.Is(err, context.Canceled) && err == nil {
		t.Fatalf("cancellation err=%v", err)
	}
	if repo.state != "failed" {
		t.Fatalf("state=%s", repo.state)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(target), ".movie.mkv.darkharrbor-part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stage survived cancellation")
	}
}

func TestPromotionRetryReconcilesCompletedMaterialization(t *testing.T) {
	data := []byte("verified materialized bytes")
	svc, repo, _, rescanner, _, target := newTestService(t, data, true)
	if err := svc.Promote(t.Context(), "rep", target); err != nil {
		t.Fatal(err)
	}
	if err := svc.Promote(t.Context(), "rep", target); err != nil {
		t.Fatal(err)
	}
	if rescanner.calls != 2 || repo.completed != int64(len(data)) {
		t.Fatalf("rescans=%d completed=%d", rescanner.calls, repo.completed)
	}
}
