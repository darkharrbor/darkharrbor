package nntp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
)

// ErrConnCapRejected marks a connection attempt that failed because the
// provider rejected it at its own concurrent-connection ceiling (a "too
// many connections" style rejection), as distinct from an ordinary
// transient dial/auth failure. Pool.dial classifies greeting/AUTHINFO
// rejections into this sentinel (via errors.Is/errors.As on the wrapped
// chain) so Acquire can shrink the wired accountgov.ConnCeiling instead of
// leaving the caller's per-segment retry loop to keep re-hitting the same
// cap (HR1.4).
var ErrConnCapRejected = errors.New("nntp: provider connection ceiling rejected this connection")

// ErrArticleMissing marks a BODY rejection in which the provider answered
// correctly that it does not have the requested article (430 for the
// message-id form, 423 for the article-number form). This is a verdict
// about the ARTICLE on THIS provider, not about the connection and not
// about the post as a whole: the session stays healthy and reusable, and
// re-requesting the same message-id from the same provider can never
// succeed. Callers classify it outcome.ClassPermanentHere (NS-1.1), which
// stops the bounded retry loop immediately and seeds the per-process
// negative cache, instead of burning three redials on a certain failure.
var ErrArticleMissing = errors.New("nntp: provider does not have this article")

// nntpCodeNoSuchArticleID is the RFC 3977 response code for BODY
// <message-id> naming an article the server does not carry.
const nntpCodeNoSuchArticleID = 430

// nntpCodeNoSuchArticleNum is the RFC 3977 response code for the
// article-number form of the same condition. DarkHarrbor only ever issues
// the message-id form, so this is defensive rather than reachable today.
const nntpCodeNoSuchArticleNum = 423

// isArticleMissingCode reports whether an NNTP response code is a
// definitive "this server does not have that article" answer.
func isArticleMissingCode(code int) bool {
	return code == nntpCodeNoSuchArticleID || code == nntpCodeNoSuchArticleNum
}

// Conn is an authenticated NNTP connection.
type Conn struct {
	tp             *textproto.Conn
	netConn        net.Conn
	requestTimeout time.Duration

	// mu protects connection lifecycle state. The textproto pipeline owns
	// request/response ordering; mu only coordinates bounded logical leases
	// and failure/idle bookkeeping around those wire operations.
	mu       sync.Mutex
	lastUsed time.Time
	err      error
	dead     bool
	closed   bool
	replaced bool
	leases   int
	serial   bool
	// warm marks a connection dialed/kept open by the NS-5.1 warm-pool
	// keeper rather than an ordinary on-demand Acquire dial.
	warm bool
}

func (c *Conn) armDeadline() error {
	if c.requestTimeout <= 0 {
		return nil
	}
	return c.netConn.SetDeadline(time.Now().Add(c.requestTimeout))
}

func (c *Conn) setError(err error) error {
	if err == nil {
		return nil
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.dead = true
	c.mu.Unlock()
	return err
}

func (c *Conn) beginLease(depth int, idleTimeout time.Duration) (ok, pipelined, replace bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead || c.closed {
		if c.leases == 0 && !c.replaced {
			c.replaced = true
			return false, false, true
		}
		return false, false, false
	}
	if c.leases == 0 && idleTimeout > 0 && time.Since(c.lastUsed) > idleTimeout {
		c.dead = true
		c.replaced = true
		return false, false, true
	}
	if (c.serial && c.leases > 0) || c.leases >= depth {
		return false, false, false
	}
	pipelined = c.leases > 0
	c.leases++
	return true, pipelined, false
}

func (c *Conn) releaseLease(err error) (reusable, replace, wireFailure bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	wireFailure = c.err != nil
	if err != nil || wireFailure {
		c.dead = true
	}
	if c.leases > 0 {
		c.leases--
	}
	if !c.dead {
		c.lastUsed = time.Now()
		return true, false, false
	}
	if c.leases == 0 && !c.replaced {
		c.replaced = true
		return false, true, wireFailure
	}
	return false, false, wireFailure
}

func (c *Conn) markSerial() {
	c.mu.Lock()
	c.serial = true
	c.mu.Unlock()
}

func (c *Conn) state() (warm bool, leases int, lastUsed time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.warm, c.leases, c.lastUsed, c.err
}

func (c *Conn) setWarm() {
	c.mu.Lock()
	c.warm = true
	c.mu.Unlock()
}

func (c *Conn) touch() {
	c.mu.Lock()
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

// Body fetches the body of an article by message-id.
// Returns the lines of the body response (raw NNTP body lines).
func (c *Conn) Body(messageID string) ([]string, error) {
	if err := c.Err(); err != nil {
		return nil, err
	}
	if err := c.armDeadline(); err != nil {
		return nil, c.setError(err)
	}
	id, err := c.tp.Cmd("BODY <%s>", messageID)
	if err != nil {
		return nil, c.setError(err)
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)

	code, _, err := c.tp.ReadCodeLine(222)
	if err != nil {
		// A definitive missing article is a healthy protocol response. It
		// neither poisons this shared connection nor disables pipelining.
		if isArticleMissingCode(code) {
			return nil, fmt.Errorf("nntp: body: code %d: %w", code, ErrArticleMissing)
		}
		wireErr := fmt.Errorf("nntp: body: code %d: %w", code, err)
		return nil, c.setError(wireErr)
	}

	lines, err := c.tp.ReadDotLines()
	if err != nil {
		return nil, c.setError(err)
	}
	return lines, nil
}

// BodyDecoded incrementally dot-unstuffs and yEnc-decodes one BODY response.
// Unlike Body, it never materializes an intermediate []string per wire line.
func (c *Conn) BodyDecoded(ctx context.Context, messageID string, verifyCRC bool) ([]byte, error) {
	decoded, _, err := c.BodyDecodedWithTotal(ctx, messageID, verifyCRC)
	return decoded, err
}

// BodyDecodedWithTotal is BodyDecoded plus the exact decoded logical-file
// total reported by the yEnc =ybegin size= field when present.
func (c *Conn) BodyDecodedWithTotal(ctx context.Context, messageID string, verifyCRC bool) ([]byte, int64, error) {
	if err := c.Err(); err != nil {
		return nil, 0, err
	}
	if err := c.armDeadline(); err != nil {
		return nil, 0, c.setError(err)
	}
	id, err := c.tp.Cmd("BODY <%s>", messageID)
	if err != nil {
		return nil, 0, c.setError(err)
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)

	code, _, err := c.tp.ReadCodeLine(222)
	if err != nil {
		if isArticleMissingCode(code) {
			return nil, 0, fmt.Errorf("nntp: body: code %d: %w", code, ErrArticleMissing)
		}
		return nil, 0, c.setError(fmt.Errorf("nntp: body: code %d: %w", code, err))
	}
	var total int64
	decoded, err := decodeYEncReaderWithTotal(ctx, c.tp.DotReader(), verifyCRC, &total)
	if err != nil {
		if errors.Is(err, errYEncUndrained) {
			return nil, 0, c.setError(err)
		}
		return nil, 0, err
	}
	return decoded, total, nil
}

// nntpCodeArticleExists is the RFC 3977 response code for a successful STAT
// (article exists on this server; no body is transferred).
const nntpCodeArticleExists = 223

// Stat issues an RFC 3977 STAT <message-id> command and reports whether the
// article exists on this connection's provider, without transferring any
// body bytes. It is the NS-6.1 health-audit primitive: "header-only and
// nearly free" per the frozen plan's N13 rationale.
//
// Like Body, a definitive "no such article" answer (430/423) does NOT mark
// the connection broken -- the server answered correctly and the connection
// stays reusable -- so this returns ErrArticleMissing without poisoning
// c.err. Any other non-223 response, or a transport-level failure, marks
// the connection broken exactly like Body's transient-failure path, since
// the audit caller must be able to distinguish "definitely gone" from
// "could not ask" (DG-05).
func (c *Conn) Stat(messageID string) error {
	if err := c.Err(); err != nil {
		return err
	}
	if err := c.armDeadline(); err != nil {
		return c.setError(err)
	}
	id, err := c.tp.Cmd("STAT <%s>", messageID)
	if err != nil {
		return c.setError(err)
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)

	code, _, err := c.tp.ReadCodeLine(nntpCodeArticleExists)
	if err != nil {
		if isArticleMissingCode(code) {
			return fmt.Errorf("nntp: stat: code %d: %w", code, ErrArticleMissing)
		}
		return c.setError(fmt.Errorf("nntp: stat: code %d: %w", code, err))
	}
	return nil
}

// nntpCodeDate is the RFC 3977 response code for a successful DATE
// command (server UTC timestamp follows on the same line).
const nntpCodeDate = 111

// Ping issues an RFC 3977 DATE command and reports whether the connection
// (and the provider's session for it) is still alive and authenticated.
// This is the NS-5.1 warm-pool keepalive primitive: header-only, no
// article identity involved, safe to run on an otherwise-idle connection
// on a fixed cadence to keep it from going stale before the pool's own
// IdleTimeout would otherwise close it. Any non-111 response or transport
// failure marks the connection broken via c.err, exactly like Body/Stat's
// own transient-failure path, so the warm keeper can tell "still good" from
// "must be discarded and redialed" without guessing.
func (c *Conn) Ping() error {
	if err := c.Err(); err != nil {
		return err
	}
	if err := c.armDeadline(); err != nil {
		return c.setError(err)
	}
	id, err := c.tp.Cmd("DATE")
	if err != nil {
		return c.setError(err)
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)

	_, _, err = c.tp.ReadCodeLine(nntpCodeDate)
	if err != nil {
		return c.setError(fmt.Errorf("nntp: ping (date): %w", err))
	}
	return nil
}

// Close closes the underlying connection once.
func (c *Conn) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.dead = true
	c.mu.Unlock()
	_ = c.netConn.Close()
}

// Err returns any persistent wire/protocol error on this connection.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// PoolConfig holds configuration for the NNTP connection pool.
type pipelineState struct {
	disabled     atomic.Bool
	fallbackOnce sync.Once
}

type PoolConfig struct {
	Host        string
	Port        int
	TLS         bool
	Username    string
	Password    string
	MaxConns    int
	DialTimeout time.Duration
	IdleTimeout time.Duration
	// PipelineDepth bounds simultaneous BODY commands per physical
	// connection. Zero keeps package-level callers serial; application
	// config supplies the operator default of 4.
	PipelineDepth int
	pipelineState *pipelineState
	// WarmConns is the NS-5.1 warm-pool target: the number of already-
	// dialed, authenticated, keepalive-pinged connections WarmTick tries
	// to keep sitting idle in the pool so an Acquire at play-start finds
	// one ready instead of paying a fresh dial+TLS+AUTHINFO round trip.
	// 0 (the default) disables warm-keeping entirely, reproducing exactly
	// pre-NS-5.1 lazy-dial-on-demand behavior. Never exceeds MaxConns
	// (clamped in NewPool).
	WarmConns int
}

// Pool is a bounded pool of authenticated NNTP connections.
// A nil entry in the channel represents an available slot (create on demand).
type Pool struct {
	cfg    PoolConfig
	log    *slog.Logger
	ch     chan *Conn
	mu     sync.Mutex
	dialMu sync.Mutex
	open   []*Conn

	// ceiling is an optional HR1.4 AIMD-learned connection ceiling. When
	// nil (the default), the Pool behaves exactly as before this row:
	// gated solely by cfg.MaxConns. When wired (SetCeiling), the ceiling's
	// current value additionally bounds how many connections may be open
	// at once, and a conn-cap rejection detected in dial() shrinks it.
	ceiling *accountgov.ConnCeiling
	// parked counts available channel slots that were withheld (not
	// dialed, not returned to ch) because the ceiling was below cfg.MaxConns
	// at the time. They are returned to ch as the ceiling grows back.
	parked int

	// negCache is the provider's shared NS-1.1 missing-article cache. Nil
	// (the default) disables it; negativeCache is nil-receiver safe.
	negCache *negativeCache

	// providerName and finalRungReporter are NS-1.3 accounting metadata.
	// They are wired before serving and shared by every pool owned by the
	// same provider. The reporter persists item-scoped counts; the pool
	// itself owns the exact final-rung detection point.
	providerName      string
	finalRungReporter FinalRungReporter

	// failover and health are NS-1.2's per-segment provider failover
	// collaborators. Both nil (the default) reproduces exactly the
	// pre-NS-1.2 single-provider ladder (one rung, no health tracking).
	// failover lists OTHER providers' demand pools, in usenetOrder
	// preference order, excluding this Pool's own provider; health is
	// shared by every pool of every configured provider so a failure
	// observed via one provider's demand pool and its readahead pool (and
	// via another provider's own pools) all update the same per-name
	// state. Must be set before Acquire is first called (not safe to
	// mutate concurrently with in-flight requests), matching every other
	// SetXxx wiring method on Pool.
	failover []*Pool
	health   *ProviderHealth

	// warmConns is the NS-5.1 warm-pool target (PoolConfig.WarmConns,
	// clamped to MaxConns). 0 disables WarmTick entirely.
	warmConns int
	// coldAcquires/warmAcquires are NS-5.1's cumulative per-pool "cold vs
	// warm start" counters: coldAcquires increments every time Acquire had
	// to dial a brand-new connection; warmAcquires increments every time
	// Acquire returned an already-open, still-healthy pooled connection.
	// Plain int64 fields updated only via sync/atomic (never under p.mu),
	// so reading them concurrently with Acquire is always race-free.
	coldAcquires int64
	warmAcquires int64

	// NS-8.1: ch carries bounded logical leases. A physical connection
	// contributes pipelineDepth tokens, letting net/textproto order up to K
	// concurrent BODY responses on that connection without a dispatcher
	// goroutine. Any wire/protocol failure atomically returns the provider
	// pool to serial mode for the rest of the process.
	pipelineDepth    int
	pipelineState    *pipelineState
	pipelineCommands int64
	pipelineOnce     sync.Once
}

// SetFailover wires NS-1.2's per-segment provider failover: other
// providers' pools to fall through to, in preference order, plus the
// shared health/backoff tracker. Optional: a Pool without it behaves
// exactly as before this row (the ladder in fetchArticleWithRetry carries
// only its own one rung).
func (p *Pool) SetFailover(pools []*Pool, health *ProviderHealth) {
	p.failover = pools
	p.health = health
}

// MaxConns returns the configured pool bound (the NNTP pacing limit
// reported by the provider's connection-count Policy; provider_iface.go).
func (p *Pool) MaxConns() int { return p.cfg.MaxConns }

// SetCeiling wires an HR1.4 AIMD-learned connection ceiling. Optional: a
// Pool without a wired ceiling behaves exactly as before this row, gated
// solely by cfg.MaxConns. Must be called before the first Acquire (not
// safe to call concurrently with in-flight Acquire/Release).
func (p *Pool) SetCeiling(c *accountgov.ConnCeiling) { p.ceiling = c }

// SetNegativeCache wires the provider's shared missing-article cache
// (NS-1.1). Optional: a Pool without one behaves exactly as before that
// row. Every Pool belonging to the same provider must be given the SAME
// instance so a dead segment found by one pool is known to the others.
// Must be called before the first Acquire.
func (p *Pool) SetNegativeCache(nc *negativeCache) { p.negCache = nc }

func (p *Pool) setPipelineState(state *pipelineState) {
	if p != nil && state != nil {
		p.pipelineState = state
	}
}

// setFinalRungAccounting wires the provider identity and optional NS-1.3
// reporter. Must be called before the pool serves requests.
func (p *Pool) setFinalRungAccounting(providerName string, reporter FinalRungReporter) {
	if p == nil {
		return
	}
	p.providerName = providerName
	p.finalRungReporter = reporter
}

// knownMissing reports whether this provider has already definitively
// answered that it does not carry messageID, within the cache TTL.
func (p *Pool) knownMissing(messageID string) bool {
	if p == nil {
		return false
	}
	return p.negCache.Has(messageID)
}

// recordMissing remembers a definitive missing-article verdict.
func (p *Pool) recordMissing(messageID string) {
	if p == nil {
		return
	}
	p.negCache.Put(messageID)
}

// EffectiveMaxConns returns the currently effective connection ceiling: the
// AIMD-adjusted value when a ConnCeiling is wired, otherwise the static
// configured cfg.MaxConns.
func (p *Pool) EffectiveMaxConns() int {
	if p.ceiling != nil {
		return p.ceiling.Current()
	}
	return p.cfg.MaxConns
}

func (p *Pool) effectivePipelineDepth() int {
	if p == nil || p.pipelineState == nil || p.pipelineState.disabled.Load() {
		return 1
	}
	return p.pipelineDepth
}

// PipelineSnapshot exposes bounded, secret-free transport evidence for tests
// and health plumbing: configured/effective depth and commands that actually
// joined an already-active connection.
func (p *Pool) PipelineSnapshot() (configured, effective int, commands int64) {
	if p == nil {
		return 0, 0, 0
	}
	return p.pipelineDepth, p.effectivePipelineDepth(), atomic.LoadInt64(&p.pipelineCommands)
}

func (p *Pool) disablePipelining() {
	if p == nil || p.pipelineDepth <= 1 || !p.pipelineState.disabled.CompareAndSwap(false, true) {
		return
	}
	p.mu.Lock()
	for _, c := range p.open {
		c.markSerial()
	}
	p.mu.Unlock()
	p.pipelineState.fallbackOnce.Do(func() {
		if p.log != nil {
			p.log.Warn("nntp: BODY pipelining disabled; using serial provider fallback",
				"event", "nntp_pipeline_serial_fallback",
				"provider", p.providerName)
		}
	})
}

// AcquireStats returns this pool's cumulative NS-5.1 cold/warm Acquire
// counts: cold is the number of Acquire calls that required a fresh dial,
// warm is the number that were satisfied by an already-open pooled
// connection. Both start at zero and only grow for the process lifetime
// (no persistence, matching every other in-memory pool counter). Safe to
// call concurrently with Acquire/Release; nil-receiver safe.
func (p *Pool) AcquireStats() (cold, warm int64) {
	if p == nil {
		return 0, 0
	}
	return atomic.LoadInt64(&p.coldAcquires), atomic.LoadInt64(&p.warmAcquires)
}

// WarmTarget returns the configured NS-5.1 warm-pool target for this pool
// (0 when warm-keeping is disabled). Nil-receiver safe.
func (p *Pool) WarmTarget() int {
	if p == nil {
		return 0
	}
	return p.warmConns
}

// warmOpenCount reports how many connections currently in p.open are
// marked warm (dialed/refreshed by WarmTick). Held under p.mu since it
// walks p.open, exactly like removeConn/Close.
func (p *Pool) warmOpenCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.open {
		warm, _, _, _ := c.state()
		if warm {
			n++
		}
	}
	return n
}

// WarmSnapshot returns this pool's current NS-5.1 warm-pool state: open is
// the number of warm connections currently held, target is the configured
// WarmConns goal, and cold/warm are the cumulative AcquireStats counters.
// Nil-receiver safe; a pool with warm-keeping disabled reports target==0
// and open==0, exactly matching pre-NS-5.1 absence of this concept.
func (p *Pool) WarmSnapshot() (open, target int, cold, warm int64) {
	if p == nil {
		return 0, 0, 0, 0
	}
	cold, warm = p.AcquireStats()
	return p.warmOpenCount(), p.warmConns, cold, warm
}

// WarmTick runs one bounded NS-5.1 warm-pool maintenance pass: it tops up
// idle connections toward warmConns and refreshes already-warm idle
// connections with a Ping (DATE) so they survive as long as the operator's
// configured WarmConns wants them to, rather than lapsing at IdleTimeout
// purely from inactivity. A no-op (returns immediately) when warm-keeping
// is disabled (warmConns<=0) or the pool is nil, exactly preserving
// pre-NS-5.1 behavior.
//
// The pass visits at most cap(p.ch) channel items -- each item (a nil free
// slot or an existing *Conn) is received and always returned to the
// channel exactly once, via non-blocking select+default, so this can never
// block a concurrent Acquire/Release and always terminates in bounded time
// regardless of how busy the pool is (DG-07).
func (p *Pool) WarmTick(log *slog.Logger) {
	if p == nil || p.warmConns <= 0 {
		return
	}
	warmOpen := p.warmOpenCount()
	limit := len(p.ch)
	seen := make(map[*Conn]struct{}, p.cfg.MaxConns)
	for i := 0; i < limit; i++ {
		var item *Conn
		select {
		case item = <-p.ch:
		default:
			return
		}

		if item == nil {
			if warmOpen >= p.warmConns {
				p.ch <- nil
				continue
			}
			c, err := p.dial()
			if err != nil {
				p.ch <- nil
				if log != nil {
					log.Warn("nntp: warm pool: dial failed (will retry next tick)",
						"host", p.cfg.Host, "error", err)
				}
				return
			}
			c.setWarm()
			p.mu.Lock()
			p.open = append(p.open, c)
			p.mu.Unlock()
			warmOpen++
			p.returnConnTokens(c, p.effectivePipelineDepth())
			seen[c] = struct{}{}
			continue
		}

		if _, duplicate := seen[item]; duplicate {
			p.ch <- item
			continue
		}
		seen[item] = struct{}{}
		warm, leases, _, err := item.state()
		if err != nil {
			// A failing in-flight lease owns retirement/replacement. Drop
			// this spare logical token so no new request can join it.
			continue
		}
		if leases > 0 {
			// Only truly idle physical connections receive keepalives.
			p.ch <- item
			continue
		}
		if warm {
			if err := item.Ping(); err != nil {
				_, replace, _ := item.releaseLease(err)
				if replace {
					item.Close()
					p.removeConn(item)
					p.ch <- nil
				}
				if warmOpen > 0 {
					warmOpen--
				}
				p.disablePipelining()
				if log != nil {
					log.Warn("nntp: warm pool: keepalive ping failed; discarding",
						"host", p.cfg.Host, "error", err)
				}
				continue
			}
			item.touch()
		}
		p.ch <- item
	}
}

// NewPool creates a connection pool. Connections are created lazily.
func NewPool(cfg PoolConfig, log *slog.Logger) *Pool {
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 8
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 30 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	if cfg.WarmConns < 0 {
		cfg.WarmConns = 0
	}
	if cfg.WarmConns > cfg.MaxConns {
		cfg.WarmConns = cfg.MaxConns
	}
	if cfg.PipelineDepth < 1 {
		cfg.PipelineDepth = 1
	}
	if cfg.PipelineDepth > 32 {
		cfg.PipelineDepth = 32
	}
	if cfg.pipelineState == nil {
		cfg.pipelineState = &pipelineState{}
	}
	p := &Pool{
		cfg:           cfg,
		log:           log,
		ch:            make(chan *Conn, cfg.MaxConns*cfg.PipelineDepth),
		warmConns:     cfg.WarmConns,
		pipelineDepth: cfg.PipelineDepth,
		pipelineState: cfg.pipelineState,
	}
	// One nil is one not-yet-dialed PHYSICAL connection. A dialed
	// connection contributes PipelineDepth logical BODY lease tokens.
	for i := 0; i < cfg.MaxConns; i++ {
		p.ch <- nil
	}
	return p
}

func (p *Pool) returnConnTokens(conn *Conn, n int) {
	for i := 0; i < n; i++ {
		p.ch <- conn
	}
}

// takeConnToken prefers an already-authenticated connection over consuming a
// nil physical-connection slot. This is what makes K concurrent requests pack
// onto one pipeline before opening another TCP/TLS/authenticated connection.
func (p *Pool) takeConnToken() *Conn {
	limit := len(p.ch)
	for i := 0; i < limit; i++ {
		select {
		case conn := <-p.ch:
			if conn != nil {
				return conn
			}
			p.ch <- nil
		default:
			return nil
		}
	}
	return nil
}

// Acquire returns a ready NNTP connection. Blocks until one is available
// or ctx is cancelled. Caller MUST call Release when done.
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	p.unparkIfRoom()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case conn := <-p.ch:
			depth := p.effectivePipelineDepth()
			if conn == nil {
				// Serialize physical dial admission, then rescan: another
				// caller may have authenticated and published K pipeline
				// tokens after this caller reserved its nil slot.
				p.dialMu.Lock()
				if existing := p.takeConnToken(); existing != nil {
					p.ch <- nil
					conn = existing
					p.dialMu.Unlock()
				} else {
					if p.parkIfOverCeiling() {
						p.dialMu.Unlock()
						continue
					}
					c, err := p.dial()
					if err != nil {
						if p.ceiling != nil && isConnCapRejection(err) {
							p.ceiling.OnRejected()
							if p.log != nil {
								p.log.Warn("nntp: pool: provider connection ceiling rejection; shrinking effective ceiling",
									"host", p.cfg.Host,
									"effective_max", p.ceiling.Current())
							}
						}
						p.ch <- nil
						p.dialMu.Unlock()
						return nil, fmt.Errorf("nntp: pool: dial: %w", err)
					}
					ok, _, _ := c.beginLease(depth, p.cfg.IdleTimeout)
					if !ok {
						c.Close()
						p.ch <- nil
						p.dialMu.Unlock()
						continue
					}
					p.mu.Lock()
					p.open = append(p.open, c)
					p.mu.Unlock()
					p.returnConnTokens(c, depth-1)
					p.dialMu.Unlock()
					atomic.AddInt64(&p.coldAcquires, 1)
					return c, nil
				}
			}

			ok, pipelined, replace := conn.beginLease(depth, p.cfg.IdleTimeout)
			if replace {
				conn.Close()
				p.removeConn(conn)
				p.ch <- nil
				continue
			}
			if !ok {
				// A serial-fallback connection already has one active lease,
				// or this is a stale token for a retired connection. Dropping
				// the token collapses its former K-token allowance to one.
				continue
			}
			if pipelined {
				atomic.AddInt64(&p.pipelineCommands, 1)
				p.pipelineOnce.Do(func() {
					if p.log != nil {
						p.log.Info("nntp: BODY pipeline active",
							"event", "nntp_pipeline_active",
							"provider", p.providerName,
							"depth", depth)
					}
				})
			}
			atomic.AddInt64(&p.warmAcquires, 1)
			return conn, nil
		}
	}
}

// parkIfOverCeiling reports whether the number of already-open connections
// meets or exceeds the ceiling's current effective value; if so it withholds
// the just-received available slot (increments parked, does not dial and
// does not return the slot to ch).
func (p *Pool) parkIfOverCeiling() bool {
	if p.ceiling == nil {
		return false
	}
	current := p.ceiling.Current()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.open) >= current {
		p.parked++
		return true
	}
	return false
}

// unparkIfRoom returns previously withheld slots to ch as the ceiling grows
// back (AIMD additive increase) or as open connections close, so recovered
// capacity becomes usable again without waiting for an unrelated Acquire to
// notice.
func (p *Pool) unparkIfRoom() {
	if p.ceiling == nil {
		return
	}
	current := p.ceiling.Current()
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.parked > 0 && len(p.open) < current {
		select {
		case p.ch <- nil:
			p.parked--
		default:
			// ch is at capacity (shouldn't normally happen: total buffered
			// slots + open connections never exceed cfg.MaxConns), stop
			// rather than block.
			return
		}
	}
}

// Release returns a connection to the pool or discards it if err != nil.
func (p *Pool) Release(conn *Conn, err error) {
	if conn == nil {
		return
	}
	reusable, replace, wireFailure := conn.releaseLease(err)
	if wireFailure {
		p.disablePipelining()
	}
	if reusable {
		p.ch <- conn
		return
	}
	if replace {
		conn.Close()
		p.removeConn(conn)
		p.ch <- nil
		p.unparkIfRoom()
	}
}

// Close closes all connections in the pool.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.open {
		c.Close()
	}
	p.open = nil
}

func (p *Pool) removeConn(conn *Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, c := range p.open {
		if c == conn {
			p.open = append(p.open[:i], p.open[i+1:]...)
			return
		}
	}
}

func (p *Pool) dial() (*Conn, error) {
	addr := net.JoinHostPort(p.cfg.Host, fmt.Sprintf("%d", p.cfg.Port))
	dialer := &net.Dialer{Timeout: p.cfg.DialTimeout}

	var netConn net.Conn
	var err error
	if p.cfg.TLS {
		netConn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: p.cfg.Host,
		})
	} else {
		netConn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	tp := textproto.NewConn(netConn)

	// Read greeting. RFC 3977 defines only 200 (posting allowed) and 201
	// (posting prohibited) as success codes here; anything outside the 2xx
	// class is a rejection at connect time, which for a provider with a
	// hard concurrent-connection ceiling is commonly how "too many
	// connections" surfaces (e.g. 400/502 with an explanatory message).
	code, message, err := tp.ReadCodeLine(0)
	if err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("nntp: greeting: %w", err)
	}
	if code/100 != 2 {
		_ = netConn.Close()
		if looksLikeConnCapRejection(code, message) {
			return nil, fmt.Errorf("nntp: greeting rejected (%d %s): %w", code, message, ErrConnCapRejected)
		}
		return nil, fmt.Errorf("nntp: greeting rejected: %d %s", code, message)
	}

	// Authenticate
	if p.cfg.Username != "" {
		if err := nntpCmd(tp, 381, "AUTHINFO USER %s", p.cfg.Username); err != nil {
			// Some servers return 281 directly
			if !strings.Contains(err.Error(), "281") {
				_ = netConn.Close()
				return nil, fmt.Errorf("nntp: authinfo user: %w", err)
			}
		}
		if err := nntpCmd(tp, 281, "AUTHINFO PASS %s", p.cfg.Password); err != nil {
			_ = netConn.Close()
			return nil, fmt.Errorf("nntp: authinfo pass: %w", err)
		}
	}

	return &Conn{
		tp:             tp,
		netConn:        netConn,
		requestTimeout: p.cfg.DialTimeout,
		lastUsed:       time.Now(),
	}, nil
}

// looksLikeConnCapRejection reports whether an NNTP response code/message
// pair matches the common "too many connections" shape providers use when
// rejecting a connection at their own concurrent-connection ceiling. This
// stays a narrow, explicit pattern match (no invented provider-specific
// scraping): unmatched non-2xx responses still fail the dial, just without
// being classified as a conn-cap rejection.
func looksLikeConnCapRejection(code int, message string) bool {
	if code != 400 && code != 502 {
		return false
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "too many connections"),
		strings.Contains(lower, "max connections"),
		strings.Contains(lower, "maximum connections"),
		strings.Contains(lower, "connection limit"),
		strings.Contains(lower, "concurrent connections"):
		return true
	default:
		return false
	}
}

// isConnCapRejection reports whether err (possibly wrapped) is or carries a
// conn-cap rejection, either via the ErrConnCapRejected sentinel wrapped by
// dial()'s greeting check, or via a *textproto.Error from an AUTHINFO
// exchange whose code/message matches the same pattern.
func isConnCapRejection(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrConnCapRejected) {
		return true
	}
	var pe *textproto.Error
	if errors.As(err, &pe) {
		return looksLikeConnCapRejection(pe.Code, pe.Msg)
	}
	return false
}

func nntpCmd(tp *textproto.Conn, expectCode int, format string, args ...any) error {
	id, err := tp.Cmd(format, args...)
	if err != nil {
		return err
	}
	tp.StartResponse(id)
	defer tp.EndResponse(id)
	_, _, err = tp.ReadCodeLine(expectCode)
	return err
}
