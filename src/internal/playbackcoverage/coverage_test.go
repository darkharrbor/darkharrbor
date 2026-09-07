package playbackcoverage

import (
	"context"
	"testing"
	"time"
)

type recordingRepository struct {
	ctxErr error
	span   Interval
	// lastDeclaredSize records RX-9.6's declared extent so a test can assert
	// the store receives it.
	lastDeclaredSize int64
}

func (r *recordingRepository) MergePlaybackCoverage(ctx context.Context, _ string, span Interval, at time.Time, _ int, declaredSize int64) (Snapshot, error) {
	r.lastDeclaredSize = declaredSize
	r.ctxErr = ctx.Err()
	r.span = span
	return Snapshot{Intervals: []Interval{span}, DeliveredBytes: span.End - span.Start, UpdatedAt: at}, nil
}
func (*recordingRepository) GetPlaybackCoverage(context.Context, string, int) (Snapshot, bool, error) {
	return Snapshot{}, false, nil
}

func (r *recordingRepository) MergePlaybackMediaCoverage(ctx context.Context, _ string, span Interval, at time.Time, _ int) (MediaSnapshot, error) {
	r.ctxErr = ctx.Err()
	r.span = span
	return MediaSnapshot{Intervals: []Interval{span}, DeliveredNS: span.End - span.Start, UpdatedAt: at}, nil
}

func (*recordingRepository) GetPlaybackMediaCoverage(context.Context, string, int) (MediaSnapshot, bool, error) {
	return MediaSnapshot{}, false, nil
}

func TestObservePreservesCompletedWriteAfterCancellation(t *testing.T) {
	repo := &recordingRepository{}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	tracker, _ := New(repo, Options{Now: func() time.Time { return now }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot, err := tracker.Observe(ctx, "nntp:delivered", 5, 7)
	if err != nil || repo.ctxErr != nil || repo.span != (Interval{Start: 5, End: 12}) || snapshot.DeliveredBytes != 7 {
		t.Fatalf("observation = %+v repo=%+v err=%v", snapshot, repo, err)
	}
}

func TestObserveCarriesDeclaredSizeWithoutProposalTarget(t *testing.T) {
	repo := &recordingRepository{}
	tracker, _ := New(repo, Options{})
	ctx := WithDeclaredSize(context.Background(), 987654321)
	if _, err := tracker.Observe(ctx, "mediaflow:declared", 0, 1); err != nil {
		t.Fatal(err)
	}
	if repo.lastDeclaredSize != 987654321 {
		t.Fatalf("declared size = %d", repo.lastDeclaredSize)
	}
}

func TestMalformedCoverageInputsFailClosed(t *testing.T) {
	tracker, _ := New(&recordingRepository{}, Options{})
	for _, test := range []struct {
		id     string
		offset int64
		length int64
	}{
		{"", 0, 1}, {"http://private", 0, 1}, {"id?query", 0, 1}, {"valid", -1, 1}, {"valid", 0, 0},
	} {
		if _, err := tracker.Observe(context.Background(), test.id, test.offset, test.length); err == nil {
			t.Fatalf("accepted malformed input: %+v", test)
		}
	}
}

func TestObserveMediaPreservesCompletedWriteAfterCancellation(t *testing.T) {
	repo := &recordingRepository{}
	tracker, _ := New(repo, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot, err := tracker.ObserveMedia(ctx, "http:hls", 2*time.Second.Nanoseconds(), 6*time.Second.Nanoseconds())
	if err != nil || repo.ctxErr != nil || snapshot.DeliveredNS != 4*time.Second.Nanoseconds() {
		t.Fatalf("media observation = %+v repo context=%v err=%v", snapshot, repo.ctxErr, err)
	}
}

func TestMalformedMediaCoverageInputsFailClosed(t *testing.T) {
	tracker, _ := New(&recordingRepository{}, Options{})
	for _, test := range []struct {
		id             string
		startNS, endNS int64
	}{
		{"", 0, 1}, {"http://private", 0, 1}, {"valid", -1, 1}, {"valid", 1, 1}, {"valid", 2, 1},
	} {
		if _, err := tracker.ObserveMedia(context.Background(), test.id, test.startNS, test.endNS); err == nil {
			t.Fatalf("accepted malformed media input: %+v", test)
		}
	}
}
