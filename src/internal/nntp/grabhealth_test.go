package nntp

import (
	"context"
	"testing"
	"time"
)

// This file tests NS-6.3's GrabTimeHealthCheck (bounded, synchronous
// grab-time STAT dead-post rejection), reusing the exact same fake-server
// fixtures (startFakeArticleServerWithStat, newTestPool, newDeadPool,
// testLogger) NS-6.1's own RunHealthAudit/statSegmentPresence tests already
// established -- no new test harness.

func validNZBFixture(messageIDs ...string) []byte {
	body := `<?xml version="1.0"?><nzb><file poster="p" date="1" subject="video.mkv"><groups><group>alt.binaries.test</group></groups><segments>`
	for i, mid := range messageIDs {
		body += `<segment bytes="100" number="` + string(rune('1'+i)) + `">` + mid + `</segment>`
	}
	body += `</segments></file></nzb>`
	return []byte(body)
}

func TestGrabTimeHealthCheck_AllDead(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		// no entries => every STAT comes back 430 (missing)
	})
	pool := newTestPool(t, s, 4)
	p := &NNTPProvider{pool: pool, log: testLogger()}

	data := validNZBFixture("dead1@x", "dead2@x", "dead3@x")
	allDead, sampled, err := p.GrabTimeHealthCheck(context.Background(), data, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sampled != 3 {
		t.Fatalf("sampled = %d, want 3", sampled)
	}
	if !allDead {
		t.Fatal("expected allDead=true when every conclusive sample is missing")
	}
}

func TestGrabTimeHealthCheck_MixedPresentMissing_NotAllDead(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		"present@x": "223 0 <present@x> article exists",
		// "missing@x" absent => 430
	})
	pool := newTestPool(t, s, 4)
	p := &NNTPProvider{pool: pool, log: testLogger()}

	data := validNZBFixture("present@x", "missing@x")
	allDead, sampled, err := p.GrabTimeHealthCheck(context.Background(), data, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sampled != 2 {
		t.Fatalf("sampled = %d, want 2", sampled)
	}
	if allDead {
		t.Fatal("expected allDead=false for a partial (mixed) sample -- that is NS-6.1/NS-4.2's threshold job, not this row's")
	}
}

func TestGrabTimeHealthCheck_AllPresent_NotDead(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		"a@x": "223 0 <a@x> article exists",
		"b@x": "223 0 <b@x> article exists",
	})
	pool := newTestPool(t, s, 4)
	p := &NNTPProvider{pool: pool, log: testLogger()}

	data := validNZBFixture("a@x", "b@x")
	allDead, sampled, err := p.GrabTimeHealthCheck(context.Background(), data, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sampled != 2 {
		t.Fatalf("sampled = %d, want 2", sampled)
	}
	if allDead {
		t.Fatal("expected allDead=false when every sample is present")
	}
}

func TestGrabTimeHealthCheck_Inconclusive_NeverDead(t *testing.T) {
	pool := newDeadPool(t, "primary")
	p := &NNTPProvider{pool: pool, log: testLogger()}

	data := validNZBFixture("a@x", "b@x")
	allDead, sampled, err := p.GrabTimeHealthCheck(context.Background(), data, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sampled != 0 {
		t.Fatalf("sampled = %d, want 0 (every candidate unreachable)", sampled)
	}
	if allDead {
		t.Fatal("an entirely inconclusive pass must never report allDead=true")
	}
}

func TestGrabTimeHealthCheck_ContextTimeoutMidSample_DegradesGracefully(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		"a@x": "223 0 <a@x> article exists",
	})
	pool := newTestPool(t, s, 4)
	p := &NNTPProvider{pool: pool, log: testLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled -- mirrors TestRunHealthAudit_ContextCancelledMidWalk_ReturnsPartialResult
	data := validNZBFixture("a@x", "b@x")
	allDead, sampled, err := p.GrabTimeHealthCheck(ctx, data, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sampled != 0 {
		t.Fatalf("sampled = %d, want 0 once context is already cancelled", sampled)
	}
	if allDead {
		t.Fatal("a cancelled-before-any-sample pass must never report allDead=true")
	}
}

func TestGrabTimeHealthCheck_MalformedNZB_AbstainsWithError(t *testing.T) {
	p := &NNTPProvider{pool: nil, log: testLogger()}
	allDead, sampled, err := p.GrabTimeHealthCheck(context.Background(), []byte("not xml at all"), 4)
	if err == nil {
		t.Fatal("expected a parse error for malformed NZB data")
	}
	if allDead {
		t.Fatal("a parse-failure abstain must never report allDead=true")
	}
	if sampled != 0 {
		t.Fatalf("sampled = %d, want 0 on parse failure", sampled)
	}
}

func TestGrabTimeHealthCheck_EmptyNZB_AbstainsNoError(t *testing.T) {
	p := &NNTPProvider{pool: nil, log: testLogger()}
	// Valid NZB shell with zero segments -- SelectSampleSegments returns nil.
	data := []byte(`<?xml version="1.0"?><nzb><file poster="p" date="1" subject="x"><groups><group>g</group></groups><segments></segments></file></nzb>`)
	allDead, sampled, err := p.GrabTimeHealthCheck(context.Background(), data, 4)
	if err != nil {
		t.Fatalf("unexpected error for a structurally-valid empty NZB: %v", err)
	}
	if allDead || sampled != 0 {
		t.Fatalf("allDead=%v sampled=%d, want false/0 for an unsampleable NZB", allDead, sampled)
	}
}

// Sanity: the function respects a bounded bound reasonably fast even under
// normal (non-cancelled) conditions -- not a strict timing assertion (CI
// hosts vary), just confirms no unbounded blocking on a healthy fake server.
func TestGrabTimeHealthCheck_CompletesPromptlyAgainstFakeServer(t *testing.T) {
	s := startFakeArticleServerWithStat(t, nil, map[string]string{
		"a@x": "223 0 <a@x> article exists",
	})
	pool := newTestPool(t, s, 4)
	p := &NNTPProvider{pool: pool, log: testLogger()}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := p.GrabTimeHealthCheck(ctx, validNZBFixture("a@x"), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("GrabTimeHealthCheck took %v, expected to complete well within the bounded timeout", elapsed)
	}
}
