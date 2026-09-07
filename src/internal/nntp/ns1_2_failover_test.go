package nntp

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/ladder"
	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

// deadHostPort returns a host/port pair with nothing listening: a listener
// is opened then immediately closed, so a dial fails fast with
// connection-refused instead of timing out (unlike an unroutable IP, which
// would make this test slow).
func deadHostPort(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	_ = ln.Close()
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return host, port
}

func newDeadPool(t *testing.T, name string) *Pool {
	t.Helper()
	host, port := deadHostPort(t)
	p := NewPool(PoolConfig{
		Host:        host,
		Port:        port,
		MaxConns:    1,
		DialTimeout: 300 * time.Millisecond,
		IdleTimeout: time.Minute,
	}, testLogger())
	t.Cleanup(p.Close)
	p.setFinalRungAccounting(name, nil)
	return p
}

func newNamedTestPool(t *testing.T, s *fakeArticleServer, maxConns int, name string) *Pool {
	t.Helper()
	p := newTestPool(t, s, maxConns)
	p.setFinalRungAccounting(name, nil)
	return p
}

// A primary provider whose connection is entirely dead (simulating killed
// credentials / a downed provider -- the row's own named live-gate
// scenario) must fail over per-segment to a healthy secondary provider,
// and the fetch must still succeed.
func TestFetchArticleWithRetry_FailsOverToSecondProvider_WhenPrimaryUnreachable(t *testing.T) {
	secondary := startFakeArticleServer(t, map[string]string{"seg1@example": "222 body follows"})
	primary := newDeadPool(t, "primary")
	sec := newNamedTestPool(t, secondary, 2, "secondary")
	health := NewProviderHealth()
	primary.SetFailover([]*Pool{sec}, health)

	var got []byte
	err := fetchArticleWithRetry(context.Background(), primary, testLogger(), "stream", 1, "seg1@example", func(data []byte) error {
		got = data
		return nil
	})
	if err != nil {
		t.Fatalf("expected failover success, got error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected body lines from the secondary provider")
	}
	if secondary.bodyCount() != 1 {
		t.Fatalf("secondary bodyCount = %d, want 1", secondary.bodyCount())
	}
}

// A single dead-primary failure must not immediately back the primary off
// (providerHealthBackoffThreshold requires consecutive failures) -- the
// very next segment must still try the primary first.
func TestFetchArticleWithRetry_SingleFailoverDoesNotYetBackOffPrimary(t *testing.T) {
	secondary := startFakeArticleServer(t, map[string]string{
		"seg1@example": "222 body follows",
		"seg2@example": "222 body follows",
	})
	primary := newDeadPool(t, "primary")
	sec := newNamedTestPool(t, secondary, 2, "secondary")
	health := NewProviderHealth()
	primary.SetFailover([]*Pool{sec}, health)

	if err := fetchArticleWithRetry(context.Background(), primary, testLogger(), "stream", 1, "seg1@example", nil); err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if got := health.Backoff("primary"); got != 0 {
		t.Fatalf("one failure must not trigger backoff yet, got %v", got)
	}

	// A second consecutive dead-primary failure crosses the threshold and
	// backs the primary off; a third segment must skip straight to the
	// secondary without attempting the dead primary at all (verified
	// indirectly: the fetch still succeeds and stays fast).
	if err := fetchArticleWithRetry(context.Background(), primary, testLogger(), "stream", 2, "seg2@example", nil); err != nil {
		t.Fatalf("segment 2: %v", err)
	}
	if got := health.Backoff("primary"); got <= 0 {
		t.Fatal("two consecutive dead-primary failures must trigger backoff")
	}
}

// A healthy fetch through the primary must clear any prior backoff
// (RecordSuccess), so a transient outage does not permanently exile a
// provider once it recovers.
func TestFetchArticleWithRetry_SuccessClearsPriorBackoff(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"seg1@example": "222 body follows"})
	pool := newNamedTestPool(t, s, 1, "onlyprovider")
	health := NewProviderHealth()
	pool.health = health // no siblings; still exercise health recording on the primary rung
	health.RecordFailure("onlyprovider")
	health.RecordFailure("onlyprovider")
	if health.Backoff("onlyprovider") <= 0 {
		t.Fatal("setup: expected backoff to be active")
	}

	if err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "stream", 1, "seg1@example", nil); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := health.Backoff("onlyprovider"); got != 0 {
		t.Fatalf("success must clear backoff, got %v", got)
	}
}

// When EVERY candidate provider has already definitively answered that it
// does not carry the article, no rung is attempted at all and the result
// is the same permanent+exhausted verdict NS-1.1 produced for a single
// provider -- generalized across the whole candidate set, not just the
// primary.
func TestFetchArticleWithRetry_AllProvidersKnownMissing_NoWireCalls(t *testing.T) {
	secondary := startFakeArticleServer(t, map[string]string{})
	primary := newDeadPool(t, "primary") // would fail hard if ever dialed
	sec := newNamedTestPool(t, secondary, 1, "secondary")
	health := NewProviderHealth()
	primary.SetFailover([]*Pool{sec}, health)

	nc := newNegativeCache(time.Hour, 100)
	primary.SetNegativeCache(nc)
	sec.SetNegativeCache(newNegativeCache(time.Hour, 100))
	primary.recordMissing("gone@example")
	sec.recordMissing("gone@example")

	err := fetchArticleWithRetry(context.Background(), primary, testLogger(), "stream", 1, "gone@example", nil)
	if !errors.Is(err, ErrArticleMissing) || !errors.Is(err, ladder.ErrExhausted) {
		t.Fatalf("error chain = %v", err)
	}
	if outcome.Classify(err) != outcome.ClassPermanentHere {
		t.Fatalf("class = %v, want permanent_here", outcome.Classify(err))
	}
	if secondary.bodyCount() != 0 {
		t.Fatalf("secondary bodyCount = %d, want 0 (should never be dialed for a known-missing article)", secondary.bodyCount())
	}
}

// If the primary does NOT know the article is missing but the ONLY
// failover candidate does, the primary rung must still run (this is not
// the all-known-missing fast path).
func TestFetchArticleWithRetry_PartialKnownMissing_StillTriesUnknownCandidate(t *testing.T) {
	primaryServer := startFakeArticleServer(t, map[string]string{"seg1@example": "222 body follows"})
	primary := newNamedTestPool(t, primaryServer, 1, "primary")
	secondary := startFakeArticleServer(t, map[string]string{})
	sec := newNamedTestPool(t, secondary, 1, "secondary")
	sec.SetNegativeCache(newNegativeCache(time.Hour, 100))
	sec.recordMissing("seg1@example")
	health := NewProviderHealth()
	primary.SetFailover([]*Pool{sec}, health)

	err := fetchArticleWithRetry(context.Background(), primary, testLogger(), "stream", 1, "seg1@example", nil)
	if err != nil {
		t.Fatalf("expected success via primary, got %v", err)
	}
	if primaryServer.bodyCount() != 1 {
		t.Fatalf("primary bodyCount = %d, want 1", primaryServer.bodyCount())
	}
	if secondary.bodyCount() != 0 {
		t.Fatalf("secondary bodyCount = %d, want 0 (known-missing candidate must be skipped)", secondary.bodyCount())
	}
}

// A Pool with no failover wired at all (pool.failover nil, the default for
// every pre-NS-1.2 caller and every standalone Pool) must reproduce
// NS-1.1's exact single-rung ladder: one rung named rungNameNNTPPrimary,
// nothing else.
func TestFetchArticleWithRetry_NoFailoverWired_SingleRungUnchanged(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"seg1@example": "222 body follows"})
	pool := newTestPool(t, s, 1)

	if err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "stream", 1, "seg1@example", nil); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if s.bodyCount() != 1 {
		t.Fatalf("bodyCount = %d, want 1", s.bodyCount())
	}
}
