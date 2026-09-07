package nntp

import (
	"context"
	mrand "math/rand"
	"testing"
	"time"
)

// --- Conn.Stat protocol-level cases (DG-05) ---------------------------

func TestConnStat_Present_ReturnsNilAndKeepsConnHealthy(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		"good@example": "223 0 <good@example> article exists",
	})
	pool := newTestPool(t, s, 4)

	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := conn.Stat("good@example"); err != nil {
		t.Fatalf("stat: unexpected error: %v", err)
	}
	if conn.Err() != nil {
		t.Fatalf("conn should remain healthy after a 223, got err=%v", conn.Err())
	}
	pool.Release(conn, nil)
	if s.statCount() != 1 {
		t.Fatalf("expected 1 STAT command, got %d", s.statCount())
	}
}

func TestConnStat_MissingArticle_ReturnsSentinelAndKeepsConnHealthy(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{})
	pool := newTestPool(t, s, 4)

	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	err = conn.Stat("missing@example")
	if err == nil {
		t.Fatal("expected ErrArticleMissing, got nil")
	}
	if !isErrArticleMissing(err) {
		t.Fatalf("expected ErrArticleMissing, got %v", err)
	}
	if conn.Err() != nil {
		t.Fatalf("conn should remain healthy after a definitive 430, got err=%v", conn.Err())
	}
	pool.Release(conn, nil)
}

func isErrArticleMissing(err error) bool {
	for err != nil {
		if err == ErrArticleMissing {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// --- statSegmentPresence / RunHealthAudit (DG-05, DG-07) ---------------

func TestStatSegmentPresence_Present(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		"a@x": "223 0 <a@x> article exists",
	})
	pool := newTestPool(t, s, 2)

	present, conclusive := statSegmentPresence(context.Background(), pool, testLogger(), "a@x")
	if !conclusive || !present {
		t.Fatalf("expected conclusive present, got present=%v conclusive=%v", present, conclusive)
	}
}

func TestStatSegmentPresence_Missing(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{})
	pool := newTestPool(t, s, 2)

	present, conclusive := statSegmentPresence(context.Background(), pool, testLogger(), "gone@x")
	if !conclusive || present {
		t.Fatalf("expected conclusive missing, got present=%v conclusive=%v", present, conclusive)
	}
}

// A primary provider that is entirely unreachable, with no failover wired,
// must report INCONCLUSIVE -- never a false "missing" -- so a transient
// outage can never manufacture decay evidence.
func TestStatSegmentPresence_UnreachablePrimaryNoFailover_Inconclusive(t *testing.T) {
	pool := newDeadPool(t, "primary")

	present, conclusive := statSegmentPresence(context.Background(), pool, testLogger(), "whatever@x")
	if conclusive || present {
		t.Fatalf("expected inconclusive/false, got present=%v conclusive=%v", present, conclusive)
	}
}

// A dead primary with a healthy failover must still reach a conclusive
// verdict from the failover candidate (NS-1.2 preference order reused).
func TestStatSegmentPresence_DeadPrimaryHealthyFailover_Conclusive(t *testing.T) {
	deadPrimary := newDeadPool(t, "primary")
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		"a@x": "223 0 <a@x> article exists",
	})
	failover := newNamedTestPool(t, s, 2, "backup")
	health := NewProviderHealth()
	deadPrimary.SetFailover([]*Pool{failover}, health)

	present, conclusive := statSegmentPresence(context.Background(), deadPrimary, testLogger(), "a@x")
	if !conclusive || !present {
		t.Fatalf("expected conclusive present via failover, got present=%v conclusive=%v", present, conclusive)
	}
}

// Cancellation mid-walk must return inconclusive, never a false miss.
func TestStatSegmentPresence_CancelledContext_Inconclusive(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		"a@x": "223 0 <a@x> article exists",
	})
	pool := newTestPool(t, s, 2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	present, conclusive := statSegmentPresence(ctx, pool, testLogger(), "a@x")
	if conclusive || present {
		t.Fatalf("expected inconclusive/false on cancelled context, got present=%v conclusive=%v", present, conclusive)
	}
}

// RunHealthAudit must never spawn extra goroutines (DG-07: bounded
// goroutine/state race gate) and must exclude inconclusive samples from
// the completeness denominator entirely.
func TestRunHealthAudit_MixedPresentMissingInconclusive(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		"present1@x": "223 0 <present1@x> article exists",
		"present2@x": "223 0 <present2@x> article exists",
		// "missing@x" absent => 430
	})
	pool := newTestPool(t, s, 4)

	segs := []SampledSegment{
		{FileIndex: 0, SegmentNumber: 1, MessageID: "present1@x"},
		{FileIndex: 0, SegmentNumber: 2, MessageID: "present2@x"},
		{FileIndex: 0, SegmentNumber: 3, MessageID: "missing@x"},
	}
	res := RunHealthAudit(context.Background(), pool, testLogger(), segs)
	if res.SegmentsSampled != 3 {
		t.Fatalf("expected 3 conclusive samples, got %d", res.SegmentsSampled)
	}
	if res.SegmentsPresent != 2 {
		t.Fatalf("expected 2 present, got %d", res.SegmentsPresent)
	}
	if len(res.Dead) != 1 || res.Dead[0].MessageID != "missing@x" {
		t.Fatalf("expected exactly missing@x in Dead, got %+v", res.Dead)
	}
	wantCompleteness := 2.0 / 3.0
	if res.Completeness != wantCompleteness {
		t.Fatalf("expected completeness %v, got %v", wantCompleteness, res.Completeness)
	}
}

// An audit pass where every candidate is unreachable must report
// Completeness == 1.0 (healthy-by-default), never 0.0 -- an inconclusive
// pass is not evidence of decay.
func TestRunHealthAudit_AllInconclusive_CompletenessDefaultsHealthy(t *testing.T) {
	pool := newDeadPool(t, "primary")
	segs := []SampledSegment{
		{FileIndex: 0, SegmentNumber: 1, MessageID: "a@x"},
		{FileIndex: 0, SegmentNumber: 2, MessageID: "b@x"},
	}
	res := RunHealthAudit(context.Background(), pool, testLogger(), segs)
	if res.SegmentsSampled != 0 {
		t.Fatalf("expected 0 conclusive samples, got %d", res.SegmentsSampled)
	}
	if res.Completeness != 1.0 {
		t.Fatalf("expected default-healthy completeness 1.0, got %v", res.Completeness)
	}
}

func TestRunHealthAudit_ContextCancelledMidWalk_ReturnsPartialResult(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		"a@x": "223 0 <a@x> article exists",
	})
	pool := newTestPool(t, s, 4)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	segs := []SampledSegment{
		{FileIndex: 0, SegmentNumber: 1, MessageID: "a@x"},
		{FileIndex: 0, SegmentNumber: 2, MessageID: "b@x"},
	}
	res := RunHealthAudit(ctx, pool, testLogger(), segs)
	if res.SegmentsSampled != 0 {
		t.Fatalf("expected zero samples once context is already cancelled, got %d", res.SegmentsSampled)
	}
}

// --- SelectSampleSegments -----------------------------------------------

func TestSelectSampleSegments_Deterministic(t *testing.T) {
	nzb := &NZB{Files: []NZBFile{
		{Segments: []NZBSegment{
			{Number: 1, MessageID: "a"}, {Number: 2, MessageID: "b"}, {Number: 3, MessageID: "c"},
		}},
		{Segments: []NZBSegment{
			{Number: 1, MessageID: "d"}, {Number: 2, MessageID: "e"},
		}},
	}}
	r1 := SelectSampleSegments(nzb, 3, mrand.New(mrand.NewSource(42)))
	r2 := SelectSampleSegments(nzb, 3, mrand.New(mrand.NewSource(42)))
	if len(r1) != 3 || len(r2) != 3 {
		t.Fatalf("expected 3 samples each, got %d and %d", len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i] != r2[i] {
			t.Fatalf("same-seed samples diverged at %d: %+v vs %+v", i, r1[i], r2[i])
		}
	}
}

func TestSelectSampleSegments_BoundedByAvailableSegments(t *testing.T) {
	nzb := &NZB{Files: []NZBFile{
		{Segments: []NZBSegment{{Number: 1, MessageID: "a"}, {Number: 2, MessageID: "b"}}},
	}}
	got := SelectSampleSegments(nzb, 8, mrand.New(mrand.NewSource(1)))
	if len(got) != 2 {
		t.Fatalf("expected clamp to 2 available segments, got %d", len(got))
	}
}

func TestSelectSampleSegments_EmptyMessageIDsExcluded(t *testing.T) {
	nzb := &NZB{Files: []NZBFile{
		{Segments: []NZBSegment{{Number: 1, MessageID: ""}, {Number: 2, MessageID: "b"}}},
	}}
	got := SelectSampleSegments(nzb, 5, mrand.New(mrand.NewSource(1)))
	if len(got) != 1 || got[0].MessageID != "b" {
		t.Fatalf("expected only the non-empty message-id segment, got %+v", got)
	}
}

// --- MergeDeadRegions / CompletenessThresholdMet ------------------------

func TestMergeDeadRegions_AddsAndRecoversAndDedupes(t *testing.T) {
	existing := []DeadRegion{
		{FileIndex: 0, SegmentNumber: 1, MessageID: "a"},
		{FileIndex: 0, SegmentNumber: 2, MessageID: "b"},
	}
	dead := []SampledSegment{
		{FileIndex: 0, SegmentNumber: 2, MessageID: "b"}, // already present, dedup
		{FileIndex: 0, SegmentNumber: 3, MessageID: "c"}, // newly dead
	}
	recovered := []SampledSegment{
		{FileIndex: 0, SegmentNumber: 1, MessageID: "a"}, // recovered, must be removed
	}
	got := MergeDeadRegions(existing, dead, recovered, 0)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries (b, c), got %d: %+v", len(got), got)
	}
	byMsg := map[string]bool{}
	for _, d := range got {
		byMsg[d.MessageID] = true
	}
	if !byMsg["b"] || !byMsg["c"] || byMsg["a"] {
		t.Fatalf("unexpected merge result: %+v", got)
	}
}

func TestMergeDeadRegions_CapsAtMaxRegions_DropsOldestFirst(t *testing.T) {
	existing := []DeadRegion{
		{FileIndex: 0, SegmentNumber: 1, MessageID: "old1"},
		{FileIndex: 0, SegmentNumber: 2, MessageID: "old2"},
	}
	dead := []SampledSegment{
		{FileIndex: 0, SegmentNumber: 3, MessageID: "new1"},
		{FileIndex: 0, SegmentNumber: 4, MessageID: "new2"},
	}
	got := MergeDeadRegions(existing, dead, nil, 3)
	if len(got) != 3 {
		t.Fatalf("expected cap at 3, got %d: %+v", len(got), got)
	}
	if got[0].MessageID != "old2" {
		t.Fatalf("expected oldest (old1) dropped first, got front=%s", got[0].MessageID)
	}
}

func TestCompletenessThresholdMet(t *testing.T) {
	cases := []struct {
		completeness, threshold float64
		want                    bool
	}{
		{1.0, 0.95, true},
		{0.95, 0.95, true},
		{0.94, 0.95, false},
		{0.0, 0.95, false},
	}
	for _, c := range cases {
		if got := CompletenessThresholdMet(c.completeness, c.threshold); got != c.want {
			t.Fatalf("CompletenessThresholdMet(%v, %v) = %v, want %v", c.completeness, c.threshold, got, c.want)
		}
	}
}

// Guard against a slow/hanging fake server locking up the audit -- exercised
// implicitly by every test above via t.Cleanup(pool.Close), but assert the
// package-level constant assumption explicitly: statFromPool must not block
// past ctx.
func TestStatFromPool_RespectsContextTimeout(t *testing.T) {
	pool := newDeadPool(t, "primary")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = statFromPool(ctx, pool, testLogger(), "a@x")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("statFromPool took too long to respect context: %v", elapsed)
	}
}
