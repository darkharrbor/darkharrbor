package nntp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/ladder"
	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestPool(t *testing.T, s *fakeArticleServer, maxConns int) *Pool {
	t.Helper()
	host, port := hostPort(t, s.address())
	p := NewPool(PoolConfig{
		Host:        host,
		Port:        port,
		Username:    "u",
		Password:    "p",
		MaxConns:    maxConns,
		DialTimeout: 5 * time.Second,
		IdleTimeout: time.Minute,
	}, testLogger())
	t.Cleanup(p.Close)
	return p
}

// A 430 answer must surface as ErrArticleMissing and must NOT poison the
// connection: Conn.Err() stays nil so Pool.Release keeps the connection
// instead of closing and redialing it.
func TestConnBody_MissingArticle_ReturnsSentinelAndKeepsConnHealthy(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{})
	pool := newTestPool(t, s, 4)

	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	_, bodyErr := conn.Body("missing@example")
	if bodyErr == nil {
		t.Fatal("expected an error for a 430 answer, got nil")
	}
	if !errors.Is(bodyErr, ErrArticleMissing) {
		t.Fatalf("expected ErrArticleMissing, got %v", bodyErr)
	}
	if conn.Err() != nil {
		t.Fatalf("430 must not poison the connection; Conn.Err() = %v", conn.Err())
	}
	pool.Release(conn, nil)
}

// An ordinary non-missing failure code stays a connection-level error, as
// before this row.
func TestConnBody_TransientCode_StillPoisonsConnection(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"boom@example": "400 service temporarily unavailable"})
	pool := newTestPool(t, s, 4)

	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_, bodyErr := conn.Body("boom@example")
	if bodyErr == nil {
		t.Fatal("expected an error for a 400 answer, got nil")
	}
	if errors.Is(bodyErr, ErrArticleMissing) {
		t.Fatal("a 400 must not classify as ErrArticleMissing")
	}
	if conn.Err() == nil {
		t.Fatal("a non-missing failure must still mark the connection broken")
	}
	pool.Release(conn, bodyErr)
}

// Permanent-here means the bounded retry loop stops immediately: exactly
// one BODY command, not nntpArticleFetchMaxAttempts of them.
func TestFetchArticleWithRetry_MissingArticle_StopsAfterOneBodyCommand(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{})
	pool := newTestPool(t, s, 4)

	err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", 1, "missing@example", nil)
	if err == nil {
		t.Fatal("expected an error for a missing article")
	}
	if !errors.Is(err, ErrArticleMissing) {
		t.Fatalf("expected wrapped ErrArticleMissing, got %v", err)
	}
	if got := s.bodyCount(); got != 1 {
		t.Fatalf("missing article must cost exactly 1 BODY command, got %d", got)
	}
	if got := segmentFetchFailureClass(err); got != outcome.ClassPermanentHere {
		t.Fatalf("expected ClassPermanentHere, got %q", got)
	}
}

// The connection survives a missing-article answer, so repeated fetches of
// dead segments reuse one TCP connection instead of thrashing the pool.
//
// MaxConns is 1 deliberately: Pool.ch is a FIFO of MaxConns slots where a
// nil entry means "dial on demand", so with a larger pool a released
// connection sits behind the remaining nil slots and the test would
// measure slot ordering rather than whether the connection survived.
// With a single slot, a reused connection is the only way connCount can
// stay at 1.
func TestFetchArticleWithRetry_MissingArticle_ReusesConnection(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{})
	pool := newTestPool(t, s, 1)

	for i := 0; i < 3; i++ {
		err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", i, "missing@example", nil)
		if !errors.Is(err, ErrArticleMissing) {
			t.Fatalf("fetch %d: expected ErrArticleMissing, got %v", i, err)
		}
	}
	if got := s.bodyCount(); got != 3 {
		t.Fatalf("expected 3 BODY commands, got %d", got)
	}
	if got := s.connCount(); got != 1 {
		t.Fatalf("three dead-segment fetches must reuse one connection, got %d connections", got)
	}
}

// Contrast case proving the assertion above is meaningful: a genuine
// connection-level failure over the same single-slot pool still tears the
// connection down and redials, exactly as before this row. Three attempts
// against one dead-connection reply cost three connections.
func TestFetchArticleWithRetry_TransientFailure_StillRedials(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"boom@example": "400 service temporarily unavailable"})
	pool := newTestPool(t, s, 1)

	err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", 1, "boom@example", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := s.connCount(); got != nntpArticleFetchMaxAttempts {
		t.Fatalf("expected %d connections for %d transient attempts, got %d",
			nntpArticleFetchMaxAttempts, nntpArticleFetchMaxAttempts, got)
	}
}

// A transient failure keeps its full bounded retry budget, unchanged.
func TestFetchArticleWithRetry_TransientFailure_RetriesFullBudget(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"boom@example": "400 service temporarily unavailable"})
	pool := newTestPool(t, s, 4)

	err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", 1, "boom@example", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrArticleMissing) {
		t.Fatal("a 400 must not classify as ErrArticleMissing")
	}
	if got := s.bodyCount(); got != nntpArticleFetchMaxAttempts {
		t.Fatalf("expected %d BODY commands, got %d", nntpArticleFetchMaxAttempts, got)
	}
	if got := segmentFetchFailureClass(err); got != outcome.ClassTransientHere {
		t.Fatalf("expected ClassTransientHere, got %q", got)
	}
}

// A successful fetch is unaffected by this row.
func TestFetchArticleWithRetry_Success_Unchanged(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"good@example": "222 body follows"})
	pool := newTestPool(t, s, 4)

	var got []byte
	err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", 1, "good@example", func(data []byte) error {
		got = data
		return nil
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if string(got) != "ABC" {
		t.Fatalf("unexpected decoded body: %#v", got)
	}
	if n := s.bodyCount(); n != 1 {
		t.Fatalf("expected 1 BODY command, got %d", n)
	}
}

func TestSegmentFetchFailureClass_Taxonomy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want outcome.Class
	}{
		{"article missing", ErrArticleMissing, outcome.ClassPermanentHere},
		{"wrapped article missing", errors.Join(errors.New("ctx"), ErrArticleMissing), outcome.ClassPermanentHere},
		{"dead post", ErrDeadPost, outcome.ClassPermanentHere},
		{"other", errors.New("connection reset"), outcome.ClassTransientHere},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := segmentFetchFailureClass(tc.err); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- NS-1.1 part 2: the negative cache itself ----

func newTestPoolWithNegCache(t *testing.T, s *fakeArticleServer, maxConns int, nc *negativeCache) *Pool {
	t.Helper()
	p := newTestPool(t, s, maxConns)
	p.SetNegativeCache(nc)
	return p
}

func TestNegativeCache_HitWithinTTLAndExpiryAfter(t *testing.T) {
	base := time.Now()
	now := base
	nc := newNegativeCache(30*time.Minute, 100)
	nc.now = func() time.Time { return now }

	if nc.Has("a@example") {
		t.Fatal("empty cache must not report a hit")
	}
	nc.Put("a@example")

	now = base.Add(29 * time.Minute)
	if !nc.Has("a@example") {
		t.Fatal("entry must still be live one minute before its TTL")
	}

	now = base.Add(30 * time.Minute)
	if nc.Has("a@example") {
		t.Fatal("entry must be expired exactly at its TTL")
	}
	if n, _, _ := nc.Stats(); n != 0 {
		t.Fatalf("expired entry must be dropped on read, %d left", n)
	}
}

func TestNegativeCache_DisabledIsNilAndNilSafe(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Minute} {
		nc := newNegativeCache(ttl, 100)
		if nc != nil {
			t.Fatalf("ttl %v must produce a disabled (nil) cache", ttl)
		}
		// All methods must be safe on the nil cache.
		nc.Put("a@example")
		if nc.Has("a@example") {
			t.Fatal("disabled cache must never report a hit")
		}
		if n, h, w := nc.Stats(); n != 0 || h != 0 || w != 0 {
			t.Fatal("disabled cache must report zero stats")
		}
	}
}

func TestNegativeCache_EvictsAtBound(t *testing.T) {
	nc := newNegativeCache(time.Hour, 10)
	for i := 0; i < 40; i++ {
		nc.Put(fmt.Sprintf("seg-%d@example", i))
	}
	n, _, writes := nc.Stats()
	if n > 10 {
		t.Fatalf("cache must stay within its bound, got %d entries", n)
	}
	if writes != 40 {
		t.Fatalf("expected 40 writes recorded, got %d", writes)
	}
}

// The whole point of the row: a repeat demand for a known-dead segment
// costs zero BODY commands.
func TestFetchArticleWithRetry_KnownMissing_ShortCircuits(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{})
	nc := newNegativeCache(time.Hour, 100)
	pool := newTestPoolWithNegCache(t, s, 4, nc)

	for i := 0; i < 5; i++ {
		err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", i, "dead@example", nil)
		if !errors.Is(err, ErrArticleMissing) {
			t.Fatalf("fetch %d: expected ErrArticleMissing, got %v", i, err)
		}
	}
	if got := s.bodyCount(); got != 1 {
		t.Fatalf("five demands for one dead segment must cost 1 BODY command, got %d", got)
	}
	if _, hits, writes := nc.Stats(); hits != 4 || writes != 1 {
		t.Fatalf("expected 4 hits / 1 write, got %d hits / %d writes", hits, writes)
	}
}

// Once the TTL lapses the segment is retried, so a reversed takedown or a
// late-propagating article is recoverable.
func TestFetchArticleWithRetry_NegCacheExpiry_RefetchesOnce(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{})
	base := time.Now()
	now := base
	nc := newNegativeCache(30*time.Minute, 100)
	nc.now = func() time.Time { return now }
	pool := newTestPoolWithNegCache(t, s, 4, nc)

	_ = fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", 1, "dead@example", nil)
	_ = fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", 1, "dead@example", nil)
	if got := s.bodyCount(); got != 1 {
		t.Fatalf("second fetch within TTL must short-circuit, got %d BODY commands", got)
	}

	now = base.Add(31 * time.Minute)
	_ = fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", 1, "dead@example", nil)
	if got := s.bodyCount(); got != 2 {
		t.Fatalf("fetch after TTL must go back to the provider, got %d BODY commands", got)
	}
}

// A transient failure must never populate the cache: doing so would turn a
// momentary hiccup into a TTL-long refusal to even try.
func TestFetchArticleWithRetry_TransientFailure_DoesNotPopulateNegCache(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"boom@example": "400 service temporarily unavailable"})
	nc := newNegativeCache(time.Hour, 100)
	pool := newTestPoolWithNegCache(t, s, 4, nc)

	_ = fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", 1, "boom@example", nil)
	if n, _, w := nc.Stats(); n != 0 || w != 0 {
		t.Fatalf("transient failure must not be cached: %d entries, %d writes", n, w)
	}
	if got := s.bodyCount(); got != nntpArticleFetchMaxAttempts {
		t.Fatalf("transient budget unchanged: expected %d BODY commands, got %d",
			nntpArticleFetchMaxAttempts, got)
	}
}

// A success is never cached, and a later fetch of the same article still
// works -- the cache can only ever record absence.
func TestFetchArticleWithRetry_Success_NotCached(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"good@example": "222 body follows"})
	nc := newNegativeCache(time.Hour, 100)
	pool := newTestPoolWithNegCache(t, s, 4, nc)

	for i := 0; i < 2; i++ {
		if err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "test", 1, "good@example", nil); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if n, _, _ := nc.Stats(); n != 0 {
		t.Fatalf("successful fetches must not populate the negative cache, got %d entries", n)
	}
	if got := s.bodyCount(); got != 2 {
		t.Fatalf("expected 2 real BODY commands, got %d", got)
	}
}

// Demand and readahead pools of one provider share a single instance, so a
// dead segment found on one is immediately known to the other.
func TestNegativeCache_SharedAcrossProviderPools(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{})
	nc := newNegativeCache(time.Hour, 100)
	demand := newTestPoolWithNegCache(t, s, 4, nc)
	readahead := newTestPoolWithNegCache(t, s, 4, nc)

	if err := fetchArticleWithRetry(context.Background(), demand, testLogger(), "demand", 1, "dead@example", nil); !errors.Is(err, ErrArticleMissing) {
		t.Fatalf("demand fetch: unexpected %v", err)
	}
	if err := fetchArticleWithRetry(context.Background(), readahead, testLogger(), "readahead", 1, "dead@example", nil); !errors.Is(err, ErrArticleMissing) {
		t.Fatalf("readahead fetch: unexpected %v", err)
	}
	if got := s.bodyCount(); got != 1 {
		t.Fatalf("readahead must reuse the demand pool's verdict, got %d BODY commands", got)
	}
}

// SetCache is the wiring join point: attaching a SegmentCache must hand it
// the provider's instance, not leave its pools with caches of their own.
func TestSetCache_SharesProviderNegativeCacheWithBothPools(t *testing.T) {
	p := New(Config{Host: "127.0.0.1", Port: 1, Connections: 2, NegCacheTTLMin: 60}, testLogger())
	t.Cleanup(p.Close)
	if p.negCache == nil {
		t.Fatal("provider must build a negative cache when TTL > 0")
	}

	sc := &SegmentCache{
		demandPool: NewPool(PoolConfig{Host: "127.0.0.1", Port: 1, MaxConns: 1}, testLogger()),
		raPool:     NewPool(PoolConfig{Host: "127.0.0.1", Port: 1, MaxConns: 1}, testLogger()),
	}
	p.SetCache(sc)

	if sc.demandPool.negCache != p.negCache {
		t.Fatal("demand pool must share the provider's negative cache instance")
	}
	if sc.raPool.negCache != p.negCache {
		t.Fatal("readahead pool must share the provider's negative cache instance")
	}
}

func TestNew_NegCacheDisabledWhenTTLZero(t *testing.T) {
	p := New(Config{Host: "127.0.0.1", Port: 1, Connections: 2, NegCacheTTLMin: 0}, testLogger())
	t.Cleanup(p.Close)
	if p.negCache != nil {
		t.Fatal("TTL 0 must disable the negative cache")
	}
	if p.pool.knownMissing("anything@example") {
		t.Fatal("a disabled cache must never report a hit")
	}
}

// ---- NS-1.1 part 3: single ladder walk, classification, no zero-fill ----

// Ladder exhaustion must stay RECOGNIZABLE for NS-1.3 accounting while
// preserving both the underlying sentinel identity and the outcome class
// every existing caller already depends on.
func TestLadderExhaustion_RecognizableAndClassified(t *testing.T) {
	t.Run("transient exhaustion", func(t *testing.T) {
		s := startFakeArticleServer(t, map[string]string{"boom@example": "400 service temporarily unavailable"})
		pool := newTestPool(t, s, 4)

		err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "stream", 1, "boom@example", nil)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !errors.Is(err, ladder.ErrExhausted) {
			t.Fatalf("exhaustion must be recognizable for NS-1.3, got %v", err)
		}
		if got := outcome.Classify(err); got != outcome.ClassTransientHere {
			t.Fatalf("outcome.Classify = %q, want transient_here (SF-01 forbids unknown)", got)
		}
		if got := segmentFetchFailureClass(err); got != outcome.ClassTransientHere {
			t.Fatalf("segmentFetchFailureClass = %q, want transient_here", got)
		}
	})

	t.Run("permanent exhaustion preserves sentinel", func(t *testing.T) {
		s := startFakeArticleServer(t, map[string]string{})
		pool := newTestPool(t, s, 4)

		err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "stream", 1, "dead@example", nil)
		if !errors.Is(err, ErrArticleMissing) {
			t.Fatalf("sentinel identity must survive the ladder wrap, got %v", err)
		}
		if !errors.Is(err, ladder.ErrExhausted) {
			t.Fatalf("exhaustion must be recognizable, got %v", err)
		}
		if got := outcome.Classify(err); got != outcome.ClassPermanentHere {
			t.Fatalf("outcome.Classify = %q, want permanent_here", got)
		}
	})

	t.Run("negative-cache hit is also exhausted", func(t *testing.T) {
		s := startFakeArticleServer(t, map[string]string{})
		nc := newNegativeCache(time.Hour, 100)
		pool := newTestPoolWithNegCache(t, s, 4, nc)

		_ = fetchArticleWithRetry(context.Background(), pool, testLogger(), "stream", 1, "dead@example", nil)
		err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "stream", 1, "dead@example", nil)
		if !errors.Is(err, ladder.ErrExhausted) {
			t.Fatalf("a cache-decided verdict must count as exhaustion too, got %v", err)
		}
		if !errors.Is(err, ErrArticleMissing) {
			t.Fatalf("sentinel identity must survive, got %v", err)
		}
		if got := outcome.Classify(err); got != outcome.ClassPermanentHere {
			t.Fatalf("outcome.Classify = %q, want permanent_here", got)
		}
	})
}

// SF-01 requires zero ClassUnknown outcomes. Before this row the NNTP fetch
// path returned bare fmt.Errorf values, which classify as unknown.
func TestFetchArticleWithRetry_NeverClassifiesUnknown(t *testing.T) {
	cases := []struct {
		name  string
		reply map[string]string
		id    string
	}{
		{"missing article", map[string]string{}, "dead@example"},
		{"transient failure", map[string]string{"boom@example": "400 unavailable"}, "boom@example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := startFakeArticleServer(t, tc.reply)
			pool := newTestPool(t, s, 4)
			err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "stream", 1, tc.id, nil)
			if got := outcome.Classify(err); got == outcome.ClassUnknown || got == "" {
				t.Fatalf("outcome.Classify = %q, must be a real SF-01 class", got)
			}
		})
	}
}

// The zero-fill non-regression: a failed fetch must never hand the consumer
// bytes. It returns a classified error and the consume callback is never
// invoked, because zero-filled payload produces invalid MKV.
func TestFetchArticleWithRetry_FailureNeverDeliversBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply map[string]string
		id    string
	}{
		{"missing article", map[string]string{}, "dead@example"},
		{"transient failure", map[string]string{"boom@example": "400 unavailable"}, "boom@example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startFakeArticleServer(t, tc.reply)
			pool := newTestPool(t, s, 4)

			called := 0
			err := fetchArticleWithRetry(context.Background(), pool, testLogger(), "stream", 1, tc.id,
				func([]byte) error {
					called++
					return nil
				})
			if err == nil {
				t.Fatal("expected a classified error, got nil")
			}
			if called != 0 {
				t.Fatalf("consume must never run on failure (no zero-fill), ran %d times", called)
			}
		})
	}

	// And the lines-returning wrapper hands back nil, not an empty buffer
	// that a caller could mistake for a real short article.
	s := startFakeArticleServer(t, map[string]string{})
	pool := newTestPool(t, s, 4)
	data, err := fetchArticleBytesWithRetry(context.Background(), pool, testLogger(), "stream", 1, "dead@example", true)
	if err == nil {
		t.Fatal("expected an error")
	}
	if data != nil {
		t.Fatalf("failed fetch must return nil data, got %#v", data)
	}
}

// The MaxAttempts trap: the rung owns its own 3-attempt budget, so the
// ladder rung budget must stay 1. A rung budget above 1 would multiply into
// 3xN real BODY commands against the provider.
func TestLadderRungBudget_DoesNotMultiplyProviderLoad(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"boom@example": "400 unavailable"})
	pool := newTestPool(t, s, 4)

	_ = fetchArticleWithRetry(context.Background(), pool, testLogger(), "stream", 1, "boom@example", nil)
	if got := s.bodyCount(); got != nntpArticleFetchMaxAttempts {
		t.Fatalf("ladder must not multiply the rung's budget: expected %d BODY commands, got %d",
			nntpArticleFetchMaxAttempts, got)
	}
}

// Cancellation returns ctx.Err() unchanged, not dressed up as exhaustion.
func TestFetchArticleWithRetry_CancellationUnchanged(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{"good@example": "222 body follows"})
	pool := newTestPool(t, s, 4)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := fetchArticleWithRetry(ctx, pool, testLogger(), "stream", 1, "good@example", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if errors.Is(err, ladder.ErrExhausted) {
		t.Fatal("cancellation must not be reported as ladder exhaustion")
	}
}

func TestRungPriorityForOp(t *testing.T) {
	grab := []string{"probe", "build", "rar manifest"}
	playback := []string{"stream", "cache", "rar seg", "rar stream"}
	for _, op := range grab {
		if got := rungPriorityForOp(op); got != accountgov.PriorityGrab {
			t.Fatalf("op %q: got %v, want PriorityGrab", op, got)
		}
	}
	for _, op := range playback {
		if got := rungPriorityForOp(op); got != accountgov.PriorityPlayback {
			t.Fatalf("op %q: got %v, want PriorityPlayback", op, got)
		}
	}
}

// Regression guard for a defect the CG-01 live gate caught: Probe's
// aggregate dead-post error used %v on the last segment error, severing
// the chain so outcome.Classify saw ClassUnknown and errors.Is could not
// reach ErrArticleMissing or ladder.ErrExhausted.
func TestProbe_DeadPostError_PreservesChainAndClass(t *testing.T) {
	s := startFakeArticleServer(t, map[string]string{})
	p := New(Config{
		Host: "127.0.0.1", Connections: 2, NegCacheTTLMin: 60,
	}, testLogger())
	t.Cleanup(p.Close)
	host, port := hostPort(t, s.address())
	p.pool = NewPool(PoolConfig{
		Host: host, Port: port, Username: "u", Password: "p", MaxConns: 2,
		DialTimeout: 5 * time.Second, IdleTimeout: time.Minute,
	}, testLogger())
	p.pool.SetNegativeCache(p.negCache)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8" ?>` + "\n<nzb>\n")
	b.WriteString(`<file poster="p@x" date="1700000000" subject="t (1/1)">` + "\n")
	b.WriteString("<groups><group>alt.binaries.test</group></groups>\n<segments>\n")
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&b, `<segment bytes="700000" number="%d">dead-%d@invalid.example</segment>`+"\n", i, i)
	}
	b.WriteString("</segments>\n</file>\n</nzb>\n")

	path, err := p.Probe(context.Background(), []byte(b.String()), 1<<20)
	if path != "" {
		t.Fatalf("a dead post must produce no payload file, got %q", path)
	}
	if err == nil {
		t.Fatal("expected a dead-post error")
	}
	if !errors.Is(err, ErrDeadPost) {
		t.Fatalf("expected ErrDeadPost, got %v", err)
	}
	if !errors.Is(err, ErrArticleMissing) {
		t.Fatalf("chain to ErrArticleMissing must survive Probe's wrapper, got %v", err)
	}
	if !errors.Is(err, ladder.ErrExhausted) {
		t.Fatalf("chain to ladder.ErrExhausted must survive Probe's wrapper, got %v", err)
	}
	if got := outcome.Classify(err); got != outcome.ClassPermanentHere {
		t.Fatalf("outcome.Classify = %q, want permanent_here (SF-01 forbids unknown)", got)
	}
}
