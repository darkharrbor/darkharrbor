package playbackcoverage

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"
)

type proposalRepository struct {
	recordingRepository
	mu        sync.Mutex
	delivered int64
	created   []Proposal
	ctxErr    error
}

func (r *proposalRepository) MergePlaybackCoverage(ctx context.Context, id string, span Interval, at time.Time, max int, declaredSize int64) (Snapshot, error) {
	snapshot, err := r.recordingRepository.MergePlaybackCoverage(ctx, id, span, at, max, declaredSize)
	snapshot.DeliveredBytes = r.delivered
	return snapshot, err
}

func (r *proposalRepository) MergePlaybackMediaCoverage(ctx context.Context, id string, span Interval, at time.Time, max int) (MediaSnapshot, error) {
	snapshot, err := r.recordingRepository.MergePlaybackMediaCoverage(ctx, id, span, at, max)
	snapshot.DeliveredNS = r.delivered
	return snapshot, err
}

func (r *proposalRepository) CreatePlaybackProposal(ctx context.Context, proposal Proposal) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctxErr = ctx.Err()
	r.created = append(r.created, proposal)
	return true, nil
}

func TestProposalDisabledByDefault(t *testing.T) {
	repo := &proposalRepository{delivered: 100}
	tracker, err := New(repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTarget(context.Background(), Target{ItemID: "item", FileID: "file", Kind: ExtentDeclaredBytes, Total: 100})
	if _, err := tracker.Observe(ctx, "http:representation", 0, 100); err != nil {
		t.Fatal(err)
	}
	if len(repo.created) != 0 {
		t.Fatalf("disabled tracker created %d proposals", len(repo.created))
	}
}

func TestProposalCrossesThresholdOncePerObservation(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	repo := &proposalRepository{delivered: 49}
	tracker, err := New(repo, Options{
		Now: func() time.Time { return now }, ProposalEnabled: true, ProposalThreshold: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTarget(ctx, Target{ItemID: "item", FileID: "file", Kind: ExtentDeclaredBytes, Total: 100})
	cancel()
	if _, err := tracker.Observe(ctx, "http:representation", 0, 49); err != nil {
		t.Fatal(err)
	}
	if len(repo.created) != 0 {
		t.Fatal("proposal created below threshold")
	}
	repo.delivered = 50
	if _, err := tracker.Observe(ctx, "http:representation", 49, 1); err != nil {
		t.Fatal(err)
	}
	if len(repo.created) != 1 {
		t.Fatalf("proposals=%d, want 1", len(repo.created))
	}
	got := repo.created[0]
	if got.Delivered != 50 || got.Total != 100 || got.Threshold != 0.5 || got.CreatedAt != now || repo.ctxErr != nil {
		t.Fatalf("proposal=%+v context_err=%v", got, repo.ctxErr)
	}
}

func TestProposalExtentKindsFailClosed(t *testing.T) {
	repo := &proposalRepository{delivered: 6 * int64(time.Second)}
	tracker, err := New(repo, Options{ProposalEnabled: true, ProposalThreshold: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	byteCtx := WithTarget(context.Background(), Target{ItemID: "item", FileID: "file", Kind: ExtentVODHLS, Total: 10})
	if _, err := tracker.Observe(byteCtx, "http:representation", 0, 6); err != nil {
		t.Fatal(err)
	}
	if len(repo.created) != 0 {
		t.Fatal("byte observation used VOD duration target")
	}
	mediaCtx := WithTarget(context.Background(), Target{ItemID: "item", FileID: "file", Kind: ExtentVODHLS, Total: 10 * int64(time.Second)})
	if _, err := tracker.ObserveMedia(mediaCtx, "http:hls", 0, 6*int64(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(repo.created) != 1 || repo.created[0].ExtentKind != ExtentVODHLS {
		t.Fatalf("VOD proposals=%+v", repo.created)
	}
}

func TestProposalConfigurationAndTargetsRejectAmbiguity(t *testing.T) {
	if _, err := New(&recordingRepository{}, Options{ProposalEnabled: true, ProposalThreshold: 0.5}); err == nil {
		t.Fatal("enabled proposal accepted repository without proposal owner")
	}
	for _, threshold := range []float64{0, -1, 1.01, math.NaN(), math.Inf(1)} {
		if _, err := New(&proposalRepository{}, Options{ProposalEnabled: true, ProposalThreshold: threshold}); err == nil {
			t.Fatalf("accepted threshold %v", threshold)
		}
	}
	ctx := WithTarget(context.Background(), Target{ItemID: "item with space", FileID: "file", Kind: ExtentDeclaredBytes, Total: 1})
	if _, ok := targetFromContext(ctx); ok {
		t.Fatal("accepted ambiguous target")
	}
}
