package nntp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/ladder"
	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

// Config holds NNTP provider configuration (from env vars).
type Config struct {
	Host        string
	Port        int
	TLS         bool
	Username    string
	Password    string
	Connections int  // HARRBOR_NNTP_CONNECTIONS, default 8
	SkipCRC     bool `json:"-"` // HARRBOR_NNTP_VERIFY_CRC=false
	// NegCacheTTLMin is how many minutes a definitive missing-article
	// (430/423) answer from this provider is remembered, so a repeat
	// demand for a known-dead segment short-circuits instead of spending
	// another BODY command on it (NS-1.1).
	// HARRBOR_NNTP_NEGCACHE_TTL_MIN; 0 or negative disables the cache
	// entirely. The operator-facing default (60) is applied in
	// internal/config, so a zero value here means disabled, not default.
	NegCacheTTLMin int
	// WarmConns is the NS-5.1 per-provider warm-pool target applied to
	// this provider's SegmentCache demand pool (see PoolConfig.WarmConns).
	// HARRBOR_NNTP_WARM_CONNS; 0 disables warm-keeping entirely. The
	// operator-facing default (1) is applied in internal/config.
	WarmConns int
	// PipelineDepth bounds simultaneous BODY commands per physical
	// connection. 1 is the serial compatibility/rollback setting.
	PipelineDepth int
}

// NNTPProvider handles probing and streaming for NZB items (SourceTypeNZB).
type NNTPProvider struct {
	pool    *Pool
	cache   *SegmentCache
	log     *slog.Logger
	skipCRC bool
	// empty means defaultProviderName. See provider_iface.go.
	name string

	// limiter is the shared SF-01 per-(item, class, key) WARN rate limiter,
	// used by Stream() to fold repeated per-segment fetch/decode failures
	// into one summary line instead of spamming one WARN per segment.
	limiter *outcome.Limiter

	// negCache is this provider's NS-1.1 missing-article cache, shared by
	// every Pool the provider owns (its own pool plus, via SetCache, the
	// segment cache's demand and readahead pools). Nil when disabled.
	negCache      *negativeCache
	pipelineState *pipelineState

	// finalRungReporter persists NS-1.3 item-scoped final-rung exhaustion
	// counters. Nil keeps the provider usable in tests and standalone tools
	// that have no item store.
	finalRungReporter FinalRungReporter

	// proofLookup, repairBudget, and repairNow are NS-4.2's rung-4
	// dependencies. A nil lookup or a disabled budget keeps every segment
	// fetch byte-identical to the pre-NS-4.2 ladder.
	proofLookup  PAR2ProofLookup
	repairBudget PAR2RepairBudget
	repairNow    func() time.Time
	// crossLaneRecover is NS-7.2's optional rung-5 callback into the shared
	// API/XR owners. crossLaneSlot bounds recovery across all streams served
	// by this provider; nil leaves pre-NS-7.2 behavior unchanged.
	crossLaneRecover CrossLaneRecover
	crossLaneSlot    chan struct{}
}

// FinalRungEvent is the secret-safe accounting record emitted when an
// item-associated NNTP acquisition walk reaches its final rung. The historical
// plan calls this a zero-fill event, but no literal zero bytes are written.
type FinalRungEvent struct {
	ItemID        string
	Provider      string
	Operation     string
	SegmentNumber int
	OutcomeClass  outcome.Class
	KnownMissing  bool
}

// FinalRungReporter persists one FinalRungEvent and returns the cumulative
// item count after the write.
type FinalRungReporter func(context.Context, FinalRungEvent) (int64, error)

type accountingItemIDKey struct{}

func withAccountingItemID(ctx context.Context, itemID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if itemID == "" {
		return ctx
	}
	return context.WithValue(ctx, accountingItemIDKey{}, itemID)
}

func accountingItemID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	itemID, _ := ctx.Value(accountingItemIDKey{}).(string)
	return itemID
}

// SetCache attaches a SegmentCache to this provider. When set, Stream() and
// StreamRAR() route through the cache for all segment fetches. Must be called
// before serving any requests (not goroutine-safe after startup).
func (p *NNTPProvider) SetCache(c *SegmentCache) {
	p.cache = c
	// NS-1.1: the segment cache runs its own demand/readahead pools
	// against the SAME provider account. They must share this provider's
	// one missing-article cache, or a dead segment found on the demand
	// path would be re-fetched by readahead (and vice versa) as though
	// nothing had been learned.
	if c != nil {
		c.useNegativeCache(p.negCache)
		c.usePipelineState(p.pipelineState)
		c.useFinalRungAccounting(p.Name(), p.finalRungReporter)
	}
}

// Cache returns the SegmentCache attached via SetCache, or nil if none has
// been attached yet. Read-only accessor for the NS-5.1 warm-pool driver
// and metrics/health exposure in cmd/darkharrbor and internal/api -- it
// does not mutate provider state and is safe to call at any time.
func (p *NNTPProvider) Cache() *SegmentCache { return p.cache }

// SetFinalRungReporter wires the NS-1.3 durable item counter. It is optional
// and must be set before serving requests. Every pool belonging to this
// provider receives the same reporter and provider identity.
func (p *NNTPProvider) SetFinalRungReporter(reporter FinalRungReporter) {
	p.finalRungReporter = reporter
	p.pool.setFinalRungAccounting(p.Name(), reporter)
	if p.cache != nil {
		p.cache.useFinalRungAccounting(p.Name(), reporter)
	}
}

// SetFailoverProviders wires NS-1.2's per-segment provider failover across
// every configured usenet provider. order is the full preference order
// (usenetOrder); this provider's own name is filtered out of it
// automatically. all is every configured provider by name, including this
// one. health is the shared ProviderHealth instance -- callers MUST pass
// the SAME instance to every provider's SetFailoverProviders call, or a
// failure learned via one provider's segments would not protect another's
// calls against that same dead provider.
//
// Each of this provider's pools is wired to the matching kind of pool on
// every sibling: p.pool (used by the non-cached Probe path) fails over to
// siblings' own p.pool, and, when a SegmentCache is attached, its
// demandPool and raPool each fail over to the matching pool on every
// sibling that has one. This keeps a demand-class fetch failing over into
// a sibling's demand capacity rather than borrowing its readahead budget
// (and vice versa), preserving each pool's own HR1.4 connection ceiling.
//
// Call after SetCache and SetProviderName, once every configured provider
// exists (not safe to call concurrently with serving requests, same
// contract as every other SetXxx wiring method in this package). A single
// configured provider (len(all) < 2) is a no-op: there is nothing to fail
// over to, and pool.failover stays nil, reproducing NS-1.1's exact
// single-provider behavior.
func (p *NNTPProvider) SetFailoverProviders(order []string, all map[string]*NNTPProvider, health *ProviderHealth) {
	if p == nil || len(all) < 2 || health == nil {
		return
	}
	var siblingPool, siblingDemand, siblingRA []*Pool
	for _, name := range order {
		if name == p.Name() {
			continue
		}
		sib, ok := all[name]
		if !ok || sib == nil || sib.pool == nil {
			continue
		}
		siblingPool = append(siblingPool, sib.pool)
		if sib.cache != nil {
			if sib.cache.demandPool != nil {
				siblingDemand = append(siblingDemand, sib.cache.demandPool)
			}
			if sib.cache.raPool != nil {
				siblingRA = append(siblingRA, sib.cache.raPool)
			}
		}
	}
	if len(siblingPool) == 0 && len(siblingDemand) == 0 && len(siblingRA) == 0 {
		return
	}
	p.pool.SetFailover(siblingPool, health)
	if p.cache != nil {
		if p.cache.demandPool != nil {
			p.cache.demandPool.SetFailover(siblingDemand, health)
		}
		if p.cache.raPool != nil {
			p.cache.raPool.SetFailover(siblingRA, health)
		}
	}
	if p.log != nil {
		p.log.Info("nntp: failover wired",
			"provider", p.Name(),
			"pool_failover_count", len(siblingPool),
			"demand_failover_count", len(siblingDemand),
			"readahead_failover_count", len(siblingRA))
	}
}

// New creates an NNTPProvider with a bounded connection pool.
func New(cfg Config, log *slog.Logger) *NNTPProvider {
	if cfg.Connections <= 0 {
		cfg.Connections = 8
	}
	if cfg.Port <= 0 {
		cfg.Port = 563
	}
	pipelineState := &pipelineState{}
	pool := NewPool(PoolConfig{
		Host:          cfg.Host,
		Port:          cfg.Port,
		TLS:           cfg.TLS,
		Username:      cfg.Username,
		Password:      cfg.Password,
		MaxConns:      cfg.Connections,
		DialTimeout:   30 * time.Second,
		IdleTimeout:   5 * time.Minute,
		PipelineDepth: cfg.PipelineDepth,
		pipelineState: pipelineState,
	}, log)
	// HR1.4: AIMD ceiling starting at, and bounded by, the configured pool
	// size. Behavior is unchanged unless a real conn-cap rejection occurs.
	pool.SetCeiling(accountgov.NewConnCeiling(pool.MaxConns()))
	// NS-1.1: one missing-article cache per provider, wired into this
	// provider's own pool here and into the segment cache's pools by
	// SetCache. nil (TTL 0) disables it and restores pre-NS-1.1 behavior.
	// TTL <= 0 disables the cache outright (newNegativeCache returns nil,
	// every method is nil-safe, behavior is exactly pre-NS-1.1). The
	// operator-facing default lives in internal/config, not here, so that
	// HARRBOR_NNTP_NEGCACHE_TTL_MIN=0 means what it looks like it means
	// instead of being silently reinterpreted as "use the default".
	negCache := newNegativeCache(
		time.Duration(cfg.NegCacheTTLMin)*time.Minute,
		defaultNegCacheMaxEntries,
	)
	pool.SetNegativeCache(negCache)
	p := &NNTPProvider{
		pool:          pool,
		log:           log,
		skipCRC:       cfg.SkipCRC,
		limiter:       outcome.NewLimiter(),
		negCache:      negCache,
		pipelineState: pipelineState,
	}
	pool.setFinalRungAccounting(p.Name(), nil)
	return p
}

// ErrDeadPost indicates a post whose segments are consistently missing from the
// provider (430 No Such Article), i.e. removed/DMCA-taken content. Callers can
// errors.Is() against this to fail the item instead of retrying or inferring.
var ErrDeadPost = errors.New("nntp: dead post (consecutive missing segments)")

// Close shuts down the connection pool.

const (
	nntpArticleFetchMaxAttempts = 3
	nntpArticleRetryBaseDelay   = 10 * time.Millisecond
)

func fetchArticleBytesWithRetry(ctx context.Context, pool *Pool, log *slog.Logger, op string, segNumber int, messageID string, verifyCRC bool) ([]byte, error) {
	return fetchArticleBytesWithRetryAt(ctx, pool, log, op, segNumber, messageID, 0, verifyCRC)
}

func fetchArticleBytesWithRetryAt(ctx context.Context, pool *Pool, log *slog.Logger, op string, segNumber int, messageID string, postedAt int64, verifyCRC bool) ([]byte, error) {
	var data []byte
	err := fetchArticleWithRetryAtCRC(ctx, pool, log, op, segNumber, messageID, postedAt, verifyCRC, func(body []byte) error {
		data = body
		return nil
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// rungNameNNTPPrimary is the single NNTP ladder rung that exists today.
// NS-1.2 adds per-provider failover rungs; when it does, rungs get real
// per-provider names and this constant goes away. The name is accounting
// only -- ladder treats it as an opaque label.
const rungNameNNTPPrimary = "nntp_primary"

// rungPriorityForOp maps a fetch call site to the accountgov priority its
// attempts should lease at.
//
// INERT TODAY: the NNTP ladder is built with a nil Governor (see
// fetchArticleWithRetry), because the connection Pool is already this
// lane's admission bound and layering a second lease over it would risk
// deadlock for no gain -- HR1.4 deliberately left generic-Governor lane
// wiring to the consuming rows. The mapping is set correctly now so that
// whichever row wires a Governor inherits the right values rather than
// discovering an unset field.
func rungPriorityForOp(op string) accountgov.Priority {
	switch op {
	case "probe", "build", "rar manifest":
		// Grab-time preparation: bounded, not user-visible.
		return accountgov.PriorityGrab
	case "prewarm":
		return accountgov.PriorityPrewarm
	case "recovery":
		return accountgov.PriorityRecovery
	case "audit":
		// NS-6.1 health-audit STAT sampling: background, lowest priority
		// above prewarm, must never contend with live playback.
		return accountgov.PriorityAudit
	default:
		// "stream", "cache", "rar seg", "rar stream": live playback and
		// seek, which must never queue behind background work.
		return accountgov.PriorityPlayback
	}
}

// fetchArticleWithRetry fetches one article body, routing every NNTP read
// in DarkHarrbor through a single internal/ladder walk (NS-1.1).
//
// All five surfaces named by the row -- Probe, Stream, StreamRAR,
// StreamRARManifest, and ZIP entry reads -- reach the network through this
// one function (ZIP via fetchRARSegment), so wrapping it here is what
// makes the ladder the sole acquisition path rather than five parallel
// ones. NS-1.2 adds a second provider rung by appending to the rung slice
// below; no call site changes.
//
// The ladder walk carries exactly one rung today, and that rung's Attempt
// is the pre-existing bounded 3-attempt loop against this pool
// (fetchArticleFromPoolTotal). Rung.MaxAttempts is deliberately 1: the rung
// already owns its own retry budget, so a rung budget above 1 would
// multiply into 3xN real BODY commands against the provider.
func fetchArticleWithRetry(ctx context.Context, pool *Pool, log *slog.Logger, op string, segNumber int, messageID string, consume func([]byte) error) error {
	return fetchArticleWithRetryAt(ctx, pool, log, op, segNumber, messageID, 0, consume)
}

func fetchArticleWithRetryAt(ctx context.Context, pool *Pool, log *slog.Logger, op string, segNumber int, messageID string, postedAt int64, consume func([]byte) error) error {
	return fetchArticleWithRetryAtCRC(ctx, pool, log, op, segNumber, messageID, postedAt, true, consume)
}

func fetchArticleWithRetryAtCRC(ctx context.Context, pool *Pool, log *slog.Logger, op string, segNumber int, messageID string, postedAt int64, verifyCRC bool, consume func([]byte) error) error {
	return fetchArticleWithRetryAtCRCTotal(ctx, pool, log, op, segNumber, messageID, postedAt, verifyCRC, func(data []byte, _ int64) error {
		if consume == nil {
			return nil
		}
		return consume(data)
	})
}

func fetchArticleWithRetryAtCRCTotal(ctx context.Context, pool *Pool, log *slog.Logger, op string, segNumber int, messageID string, postedAt int64, verifyCRC bool, consume func([]byte, int64) error) error {
	if pool == nil {
		return fmt.Errorf("nntp: %s: nil pool", op)
	}

	// NS-1.2: candidate pools in preference order -- this provider first
	// (the item's recorded provider, or the configured default; see
	// api/router.go's pickUsenetProvider), then any wired failover
	// siblings. A Pool with no failover wired (pool.failover is nil, the
	// pre-NS-1.2 default and every standalone/test Pool) produces exactly
	// the single-candidate list NS-1.1 always used.
	candidates := append([]*Pool{pool}, pool.failover...)
	policy := "preference"
	if pool.health != nil {
		candidates, policy = pool.health.orderCandidates(candidates, postedAt)
	}
	// NS-1.1's short-circuit generalizes across candidates: skip a real
	// wire round trip against any provider that has already definitively
	// answered it does not carry this article, costing zero BODY commands
	// and zero connections for that candidate. If EVERY candidate has
	// already said so, no rung is servable and the fast path below
	// preserves NS-1.1's exact permanent+exhausted behavior without ever
	// entering the ladder.
	rungs := make([]ladder.Rung, 0, len(candidates))
	rungProviders := make(map[string]string, len(candidates))
	selectedProvider := ""
	allKnownMissing := true
	for _, c := range candidates {
		if c == nil {
			continue
		}
		if c.knownMissing(messageID) {
			continue
		}
		allKnownMissing = false
		// A candidate deep in a backoff window (NS-1.2: repeated real
		// dial/auth/transport failures, e.g. killed credentials) is
		// skipped UNLESS it is the only surviving candidate -- a false
		// positive must still be retriable eventually, and this function
		// must never build zero rungs when a candidate exists that isn't
		// definitively known-missing.
		if c.health != nil && c != pool && c.health.Backoff(c.providerName) > 0 {
			continue
		}
		cc := c // capture
		name := rungNameNNTPPrimary
		if cc != pool {
			name = "nntp_failover_" + cc.providerName
		}
		if selectedProvider == "" {
			selectedProvider = cc.providerName
		}
		rungProviders[name] = cc.providerName
		rungs = append(rungs, ladder.Rung{
			Name:        name,
			Op:          "nntp_article",
			Priority:    rungPriorityForOp(op),
			MaxAttempts: 1,
			Attempt: func(ctx context.Context) error {
				return fetchArticleFromPoolTotal(ctx, cc, log, op, segNumber, messageID, verifyCRC, consume)
			},
		})
	}
	if len(rungs) == 0 {
		if allKnownMissing {
			finalErr := outcome.Permanent(
				fmt.Errorf("nntp: %s: segment %d: %w (known missing): %w",
					op, segNumber, ErrArticleMissing, ladder.ErrExhausted))
			pool.reportFinalRung(ctx, op, segNumber, finalErr, true)
			return finalErr
		}
		// Every remaining candidate is in backoff (health false positive
		// guard failed to keep at least one): fall back to the primary
		// alone rather than fail with zero rungs attempted.
		selectedProvider = pool.providerName
		rungProviders[rungNameNNTPPrimary] = pool.providerName
		rungs = append(rungs, ladder.Rung{
			Name:        rungNameNNTPPrimary,
			Op:          "nntp_article",
			Priority:    rungPriorityForOp(op),
			MaxAttempts: 1,
			Attempt: func(ctx context.Context) error {
				return fetchArticleFromPoolTotal(ctx, pool, log, op, segNumber, messageID, verifyCRC, consume)
			},
		})
	}
	if log != nil {
		log.Debug("nntp: segment provider selected",
			"event", "nntp_provider_selected",
			"provider", selectedProvider,
			"policy", policy)
	}

	l := ladder.New(nil, rungs...)

	// session is empty: it feeds accountgov fair-share bookkeeping only,
	// and this ladder has no Governor. It is also the right default for a
	// value that would otherwise carry item identity into shared state.
	res, err := l.Run(ctx, "")
	recordFailoverHealth(pool, candidates, res, postedAt)
	switch {
	case err == nil:
		if log != nil {
			provider := rungProviders[res.StoppedAt]
			if pool.health.firstSuccessfulRoute(provider, policy) {
				log.Info("nntp: provider route active",
					"event", "nntp_provider_route_active",
					"provider", provider,
					"policy", policy)
			}
			log.Debug("nntp: segment provider succeeded",
				"event", "nntp_provider_succeeded",
				"provider", provider,
				"policy", policy)
		}
		return nil

	case errors.Is(err, ladder.ErrExhausted):
		// FINAL-RUNG EXHAUSTION. This is the "zero-fill signal" the row
		// requires preserved, and it is emphatically NOT zero bytes:
		// zero-filled payload produces invalid MKV that ffmpeg rejects,
		// so this path returns the last rung's already-classified error
		// and lets the stream stop legibly, exactly as before this row.
		//
		// What NS-1.1 adds is that the result is now RECOGNIZABLE as
		// ladder exhaustion -- errors.Is(err, ladder.ErrExhausted) holds
		// on the returned error -- so NS-1.3 can count final-rung
		// zero-fill events without changing any caller's behavior.
		// Wrapping preserves both the underlying sentinel identity
		// (errors.Is against ErrArticleMissing/ErrDeadPost) and the
		// outcome class, so segmentFetchFailureClass and outcome.Classify
		// keep returning what they returned before.
		if last := lastRungErr(res); last != nil {
			finalErr := fmt.Errorf("%w: %w", last, ladder.ErrExhausted)
			pool.reportFinalRung(ctx, op, segNumber, finalErr, false)
			return finalErr
		}
		finalErr := outcome.Transient(
			fmt.Errorf("nntp: %s: segment %d: %w", op, segNumber, ladder.ErrExhausted))
		pool.reportFinalRung(ctx, op, segNumber, finalErr, false)
		return finalErr

	case errors.Is(err, ladder.ErrAccountLevel):
		// Unreachable today: nothing in the NNTP fetch path classifies
		// account-level yet (conn-cap rejections are handled at the pool
		// by HR1.4's AIMD ceiling, not surfaced here). Handled explicitly
		// so that when a row does add an account-level classification,
		// the walk stops rather than falling into the default branch.
		if last := lastRungErr(res); last != nil {
			return fmt.Errorf("%w: %w", last, ladder.ErrAccountLevel)
		}
		return err

	default:
		// Context cancellation or lease failure: returned unchanged, as
		// the pre-ladder code returned ctx.Err() unchanged.
		return err
	}
}

// reportFinalRung persists and logs one item-associated final-rung event.
// Empty item identity or a nil reporter intentionally makes this a no-op for
// standalone tools/tests. Persistence uses a detached context so a client
// disconnect immediately after the provider verdict cannot erase the event.
// Reporter failure never masks or replaces the original stream error.
func (p *Pool) reportFinalRung(ctx context.Context, op string, segNumber int, finalErr error, knownMissing bool) {
	if p == nil || p.finalRungReporter == nil {
		return
	}
	itemID := accountingItemID(ctx)
	if itemID == "" {
		return
	}
	event := FinalRungEvent{
		ItemID:        itemID,
		Provider:      p.providerName,
		Operation:     op,
		SegmentNumber: segNumber,
		OutcomeClass:  outcome.Classify(finalErr),
		KnownMissing:  knownMissing,
	}
	count, err := p.finalRungReporter(context.WithoutCancel(ctx), event)
	if err != nil {
		if p.log != nil {
			p.log.Warn("nntp: final-rung exhaustion accounting failed",
				"event", "nntp_final_rung_exhausted_accounting_failed",
				"item_id", itemID,
				"provider", p.providerName,
				"operation", op,
				"segment_number", segNumber,
				"outcome_class", string(event.OutcomeClass),
				"known_missing", knownMissing,
				"error", err)
		}
		return
	}
	if p.log != nil {
		p.log.Warn("nntp: final rung exhausted",
			"event", "nntp_final_rung_exhausted",
			"item_id", itemID,
			"provider", p.providerName,
			"operation", op,
			"segment_number", segNumber,
			"outcome_class", string(event.OutcomeClass),
			"known_missing", knownMissing,
			"event_count", count)
	}
}

// recordFailoverHealth updates NS-1.2's shared health/backoff tracker from
// one ladder walk's per-rung results. A rung whose Attempt eventually
// succeeded, or definitively answered ClassPermanentHere (proof the
// provider IS reachable and correctly authenticated -- it answered "I do
// not have this article", which requires a working connection), clears
// that provider's backoff. A rung that exhausted its own bounded attempts
// with ClassTransientHere (or any other non-success classification) counts
// as one real connectivity failure toward that provider's backoff.
// Candidates never added as a rung at all (skipped in fetchArticleWithRetry
// as known-missing, or already backed off) are left untouched -- no rung
// ran against them this call, so there is nothing new to learn.
//
// postedAt (NS-9.2) is threaded through from the caller's own postedAt so
// a genuine success can also feed the lag model's age-at-first-success
// observation; RecordSuccessAt safely no-ops the lag half when the model
// is disabled or postedAt is unusable, so this is a strict superset of
// the pre-NS-9.2 behavior.
func recordFailoverHealth(pool *Pool, candidates []*Pool, res ladder.Result, postedAt int64) {
	if pool == nil || pool.health == nil {
		return
	}
	byName := make(map[string]*Pool, len(candidates))
	for _, c := range candidates {
		if c == nil {
			continue
		}
		name := rungNameNNTPPrimary
		if c != pool {
			name = "nntp_failover_" + c.providerName
		}
		byName[name] = c
	}
	for _, rr := range res.Rungs {
		c, ok := byName[rr.Name]
		if !ok {
			continue
		}
		switch rr.Class {
		case "", outcome.ClassPermanentHere:
			pool.health.RecordSuccessAt(c.providerName, postedAt)
		case outcome.ClassAccountLevel:
			// Account-level (429/quota) is not a per-provider connectivity
			// verdict; leave health untouched.
		default:
			pool.health.RecordFailure(c.providerName)
		}
	}
}

// lastRungErr returns the error recorded by the last rung the ladder
// actually attempted, or nil if it recorded none.
func lastRungErr(res ladder.Result) error {
	if len(res.Rungs) == 0 {
		return nil
	}
	return res.Rungs[len(res.Rungs)-1].Err
}

func fetchArticleFromPoolTotal(ctx context.Context, pool *Pool, log *slog.Logger, op string, segNumber int, messageID string, verifyCRC bool, consume func([]byte, int64) error) error {
	var lastErr error
	for attempt := 1; attempt <= nntpArticleFetchMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := pool.Acquire(ctx)
		if err != nil {
			lastErr = err
		} else {
			data, fileTotal, bodyErr := conn.BodyDecodedWithTotal(ctx, messageID, verifyCRC)
			if bodyErr == nil && ctx.Err() != nil {
				// The response was fully drained, so this shared pipelined
				// connection remains reusable; cancelled bytes never reach
				// decode, cache, or the caller.
				pool.Release(conn, nil)
				return ctx.Err()
			}
			if bodyErr != nil {
				if errors.Is(bodyErr, ErrArticleMissing) {
					pool.recordMissing(messageID)
					// Permanent-here (NS-1.1): the provider has answered
					// that it does not carry this article, so the two
					// remaining attempts are certain to fail. Stop now,
					// and release the connection as a clean success --
					// Conn.Body left it unpoisoned precisely so the pool
					// keeps it instead of closing and redialing.
					pool.Release(conn, nil)
					return outcome.Permanent(
						fmt.Errorf("nntp: %s: segment %d: %w", op, segNumber, bodyErr))
				}
				pool.Release(conn, bodyErr)
				lastErr = bodyErr
			} else {
				consumeErr := error(nil)
				if consume != nil {
					consumeErr = consume(data, fileTotal)
				}
				pool.Release(conn, consumeErr)
				if consumeErr != nil {
					// A yEnc/CRC decode failure means THIS COPY of the
					// article arrived corrupt, not that the article is
					// gone: transient-here, so a future provider rung
					// (NS-1.2) may still recover the same segment.
					return outcome.Transient(consumeErr)
				}
				return nil
			}
		}
		if attempt == nntpArticleFetchMaxAttempts {
			break
		}
		if log != nil {
			log.Warn("nntp: article fetch failed; retrying",
				"op", op,
				"seg", segNumber,
				"attempt", attempt,
				"max_attempts", nntpArticleFetchMaxAttempts,
				"error", lastErr)
		}
		delay := time.Duration(attempt) * nntpArticleRetryBaseDelay
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	// The rung exhausted its own bounded budget without a definitive
	// missing-article verdict: transient-here, so the ladder would advance
	// to a further provider rung once NS-1.2 adds one.
	return outcome.Transient(
		fmt.Errorf("nntp: %s: segment %d failed after %d attempts: %w",
			op, segNumber, nntpArticleFetchMaxAttempts, lastErr))
}

// Close shuts down the connection pool.
func (p *NNTPProvider) Close() { p.pool.Close() }

// Probe fetches the first probeBytes bytes of the main file in the NZB and
// writes them to a temp file for ffprobe. Caller must remove the temp file.
// Segments are fetched sequentially until probeBytes is accumulated.
func (p *NNTPProvider) Probe(ctx context.Context, nzbData []byte, probeBytes int64) (tmpPath string, err error) {
	nzb, err := ParseNZB(nzbData)
	if err != nil {
		return "", err
	}
	if len(nzb.Files) == 0 {
		return "", fmt.Errorf("nntp: probe: no files in NZB")
	}
	files := nzb.Files
	main := files[0] // already sorted largest-first by ParseNZB

	tmp, err := os.CreateTemp("", "darkharrbor-nntp-probe-*.bin")
	if err != nil {
		return "", fmt.Errorf("nntp: probe: create temp: %w", err)
	}
	tmpPath = tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
			tmpPath = ""
		}
	}()

	// Dead-post fail-fast (found live, P11 gate): a DMCA-removed post 430s on
	// every segment. Without a bound, this loop grinds through every segment of
	// the main file (~2s each with retries — hours for a remux), and because the
	// resolver is serial, it head-of-line-blocks every other resolving item.
	// A handful of consecutive missing segments means the post is dead; abort.
	const probeMaxConsecutiveFailures = 8
	consecutiveFailures := 0

	var accumulated int64
	for _, seg := range main.Segments {
		if accumulated >= probeBytes {
			break
		}
		decoded, fErr := fetchArticleBytesWithRetryAt(ctx, p.pool, p.log, "probe", seg.Number, seg.MessageID, seg.PostedAt, !p.skipCRC)
		if fErr != nil {
			p.log.Warn("nntp: probe: segment fetch failed", "seg", seg.Number, "error", fErr)
			consecutiveFailures++
			if consecutiveFailures >= probeMaxConsecutiveFailures {
				// %w on BOTH: the dead-post verdict AND the last
				// underlying segment error. Using %v on the latter (as
				// this did before NS-1.1) severed the error chain at the
				// Probe boundary, so outcome.Classify saw ClassUnknown
				// and errors.Is could not reach ErrArticleMissing or
				// ladder.ErrExhausted -- found by the CG-01 live gate.
				// The rendered message is unchanged.
				return "", fmt.Errorf("nntp: probe: %d consecutive segment failures: %w (last: %w)",
					consecutiveFailures, ErrDeadPost, fErr)
			}
			continue
		}
		consecutiveFailures = 0
		// Truncate to exactly probeBytes if this segment overshoots
		need := probeBytes - accumulated
		if int64(len(decoded)) > need {
			decoded = decoded[:need]
		}
		if _, wErr := tmp.Write(decoded); wErr != nil {
			return "", fmt.Errorf("nntp: probe: write: %w", wErr)
		}
		accumulated += int64(len(decoded))
	}

	if accumulated == 0 {
		return "", fmt.Errorf("nntp: probe: no bytes fetched")
	}
	return tmpPath, nil
}

// ProbeForItem is the resolve-path variant of Probe. It preserves the public
// provider interface while associating any final-rung event with the item being
// prepared (NS-1.3).
func (p *NNTPProvider) ProbeForItem(ctx context.Context, itemID string, nzbData []byte, probeBytes int64) (string, error) {
	return p.Probe(withAccountingItemID(ctx, itemID), nzbData, probeBytes)
}

// segResult is the result of fetching and decoding one segment in parallel.
type segResult struct {
	number int
	data   []byte
	err    error
}

// filterNZBVideoFiles returns only NZB files whose subject indicates a video
// file by extension. Mirrors the filterVideoFiles logic in main.go.
// Used by Stream() so fileIndex refers to video files, not all NZB entries.
func filterNZBVideoFiles(files []NZBFile) []NZBFile {
	videoExts := []string{".mkv", ".mp4", ".avi", ".ts", ".m2ts", ".webm"}
	var out []NZBFile
	for _, f := range files {
		subj := strings.ToLower(f.Subject)
		for _, ext := range videoExts {
			if strings.Contains(subj, ext) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// segmentFetchFailureClass classifies a segment-fetch failure into the
// shared SF-01 vocabulary (§ outcome package). Two failures are
// permanent-here, meaning retrying this exact article against this exact
// provider can never help:
//
//   - ErrArticleMissing: the provider answered 430/423, definitively
//     stating it does not carry this article (NS-1.1).
//   - ErrDeadPost: consecutive missing-article evidence upstream, e.g.
//     from a caller that already ran the probe's consecutive-failure
//     check.
//
// Any other fetch failure — a dropped connection, one momentary provider
// hiccup — is transient-here: fetchArticleWithRetry already exhausted its
// own bounded retry against this provider, but a future rung (a different
// provider, NS-1.2) may still recover the same segment, so this is not a
// terminal verdict on the source itself.
func segmentFetchFailureClass(err error) outcome.Class {
	if errors.Is(err, ErrArticleMissing) || errors.Is(err, ErrDeadPost) {
		return outcome.ClassPermanentHere
	}
	return outcome.ClassTransientHere
}

// Stream assembles and pipes an NZB file to the http.ResponseWriter.
// Segments are fetched in parallel (up to pool size), decoded from yEnc,
// and written to w in segment order. Supports HTTP Range for Jellyfin seek.
// On segment error: does NOT write zeros for the gap. That was the v1
// design and the comment outlived it -- zero-filled payload produces an
// invalid MKV that ffmpeg rejects outright, which is worse than a legible
// stop. The fetch returns a classified error (permanent_here for a
// definitive missing-article answer, transient_here otherwise), the WARN
// is folded through the SF-01 limiter, and the stream stops. NS-1.1 makes
// that result additionally recognizable as ladder exhaustion
// (errors.Is(err, ladder.ErrExhausted)) so NS-1.3 can account for it as a
// zero-fill EVENT without any zero bytes ever being produced. Real gap
// recovery is par2 (NS-4.x), not zero-fill.
func (p *NNTPProvider) Stream(ctx context.Context, itemID string, nzbData []byte, fileIndex int, w http.ResponseWriter, rangeHeader string) error {
	ctx = withAccountingItemID(ctx, itemID)
	// SF-01: fold any per-segment WARNs suppressed within an open window into
	// one summary line at the natural end of this bounded operation, so a
	// final burst is never silently dropped. Safe to defer unconditionally —
	// it is a no-op when the cache-backed path below (which does not use
	// this provider's limiter) served the request instead.
	defer p.limiter.FlushItem(p.log, itemID)

	nzb, err := ParseNZB(nzbData)
	if err != nil {
		return err
	}
	if len(nzb.Files) == 0 {
		return fmt.Errorf("nntp: stream: no files in NZB")
	}
	files := nzb.Files
	// in the filtered video-file list (matching writeNZBStrm in main.go), not the
	// full NZB file list which may include NFO/SFV/PAR2 files at index 0.
	videoFiles := filterNZBVideoFiles(files)
	if len(videoFiles) > 0 {
		files = videoFiles
	}
	if fileIndex < 0 || fileIndex >= len(files) {
		p.log.Warn("nntp: stream: fileIndex out of range, falling back to 0",
			"fileIndex", fileIndex, "fileCount", len(files))
		fileIndex = 0
	}
	main := files[fileIndex]
	if len(main.Segments) == 0 {
		return fmt.Errorf("nntp: stream: no segments")
	}

	// The cache owns range parsing, read-ahead, and the write loop.
	// Content-Type header is set here before delegating so the cache
	// does not need to know about NZB subject parsing.
	if p.cache != nil {
		contentType := "video/x-matroska"
		sub := strings.ToLower(main.Subject)
		switch {
		case strings.Contains(sub, ".mp4"):
			contentType = "video/mp4"
		case strings.Contains(sub, ".avi"):
			contentType = "video/x-msvideo"
		case strings.Contains(sub, ".webm"):
			contentType = "video/webm"
		}
		w.Header().Set("Content-Type", contentType)
		// NS-4.2 rung 4. A nil session (no proof index, no budget, ambiguous
		// file identity) leaves the fetch ladder exactly as it was.
		repair := p.newPAR2RepairSession(ctx, itemID, ContentKey(nzbData), nzbData, main)
		return p.cache.StreamWithCache(ctx, itemID, ContentKey(nzbData), fileIndex, main.Segments, main.TotalBytes, w, rangeHeader, repair)
	}

	// Content-Type from subject extension
	contentType := "video/x-matroska"
	sub := strings.ToLower(main.Subject)
	switch {
	case strings.Contains(sub, ".mp4"):
		contentType = "video/mp4"
	case strings.Contains(sub, ".avi"):
		contentType = "video/x-msvideo"
	case strings.Contains(sub, ".webm"):
		contentType = "video/webm"
	}

	// Parse Range header: "bytes=START-" or "bytes=START-END"
	var rangeStart, rangeEnd int64
	isPartial := false
	rangeEnd = main.TotalBytes - 1
	if rangeHeader != "" && strings.HasPrefix(rangeHeader, "bytes=") {
		spec := strings.TrimPrefix(rangeHeader, "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		if len(parts) == 2 {
			_, _ = fmt.Sscanf(parts[0], "%d", &rangeStart)
			if parts[1] != "" {
				_, _ = fmt.Sscanf(parts[1], "%d", &rangeEnd)
			}
			isPartial = true
		}
	}

	// Set headers before first Write
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	if isPartial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d",
			rangeStart, rangeEnd, main.TotalBytes))
		// and diverges from actual decoded payload size. Setting Content-Length here
		// causes FFmpeg/Jellyfin to report premature EOF when actual decoded bytes
		// are fewer than declared. Chunked transfer handles the boundary correctly.
		w.WriteHeader(http.StatusPartialContent)
	} else {
		// main.TotalBytes is NZB-declared (sum of seg.Bytes), not actual
		// decoded payload size. Setting it causes FFmpeg to report premature
		// EOF when actual decoded bytes fall ~170-211MB short of declared.
		// Chunked transfer (no Content-Length) lets FFmpeg stop cleanly at
		// real EOF instead of waiting for bytes that don't exist.
		w.WriteHeader(http.StatusOK)
	}

	// Determine byte range covered by each segment for range-seek support
	// segStart[i] = cumulative bytes before segments[i]
	segStart := make([]int64, len(main.Segments))
	cumulative := int64(0)
	for i, seg := range main.Segments {
		segStart[i] = cumulative
		cumulative += seg.Bytes
	}

	// Filter to only segments that overlap [rangeStart, rangeEnd]
	type segWork struct {
		idx int
		seg NZBSegment
	}
	work := make([]segWork, 0, len(main.Segments))
	for i, seg := range main.Segments {
		segEnd := segStart[i] + seg.Bytes - 1
		if segEnd < rangeStart {
			continue // entirely before range
		}
		if segStart[i] > rangeEnd {
			break // entirely after range
		}
		work = append(work, segWork{idx: i, seg: seg})
	}

	// in order. Window size = MaxConns so goroutine count == connection count.
	// No goroutines sit idle; each goroutine holds an NNTP connection, fetches
	// one segment, posts to its slot, then exits. The write loop launches
	// the next goroutine as each slot is consumed, keeping exactly windowSize
	// goroutines in flight at any time. This prevents 1738-goroutine explosions
	// that caused NNTP pool starvation and zero-fill corruption.
	//
	// Segment failure returns an error instead of writing zeros — zeros produce
	// invalid MKV bytes that ffmpeg rejects with "invalid data found".
	//
	// Flush after each write so ffmpeg receives bytes immediately.
	flusher, canFlush := w.(http.Flusher)
	windowSize := p.pool.cfg.MaxConns
	if windowSize < 1 {
		windowSize = 1
	}

	slots := make([]chan segResult, len(work))
	for i := range slots {
		slots[i] = make(chan segResult, 1)
	}

	launchSeg := func(wi int, sw segWork) {
		go func() {
			var res segResult
			res.number = sw.seg.Number
			if ctx.Err() != nil {
				res.err = ctx.Err()
				slots[wi] <- res
				return
			}
			decoded, err := fetchArticleBytesWithRetryAt(ctx, p.pool, p.log, "stream", sw.seg.Number, sw.seg.MessageID, sw.seg.PostedAt, !p.skipCRC)
			if err != nil {
				class := segmentFetchFailureClass(err)
				p.limiter.Warn(p.log, itemID, class, "stream_segment_fetch",
					"nntp: stream: fetch failed",
					"seg", sw.seg.Number, "class", string(class), "error", err)
				res.err = err
				slots[wi] <- res
				return
			}
			res.data = decoded
			slots[wi] <- res
		}()
	}

	// Pre-launch first windowSize goroutines.
	nextLaunch := 0
	for nextLaunch < len(work) && nextLaunch < windowSize {
		launchSeg(nextLaunch, work[nextLaunch])
		nextLaunch++
	}

	// Write loop: read each slot in order; launch next goroutine as each is consumed.
	//
	// actual decoded yEnc payload length. All Range skip/trim math uses
	// decodedOffset — the actual cumulative bytes emitted — not seg.Bytes.
	// segStart[] is still used only for segment pre-filtering (good enough
	// approximation for which segments to fetch); it is never used for byte math.
	decodedOffset := int64(0) // actual bytes decoded so far across all segments
	for wi := range slots {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		res := <-slots[wi]

		// Launch next goroutine to keep window full.
		if nextLaunch < len(work) {
			launchSeg(nextLaunch, work[nextLaunch])
			nextLaunch++
		}

		if res.err != nil {
			return fmt.Errorf("nntp: stream: segment %d failed: %w", res.number, res.err)
		}
		data := res.data
		segDecodedStart := decodedOffset
		decodedOffset += int64(len(data))

		if isPartial {
			// Skip: this segment ends before rangeStart — nothing to write.
			if decodedOffset-1 < rangeStart {
				continue
			}
			// Trim front: segment starts before rangeStart.
			if segDecodedStart < rangeStart {
				skip := rangeStart - segDecodedStart
				if skip >= int64(len(data)) {
					continue
				}
				data = data[skip:]
			}
			// Trim tail: segment extends past rangeEnd.
			segWriteEnd := segDecodedStart + int64(len(res.data)) - 1
			if segWriteEnd > rangeEnd {
				keep := rangeEnd - max64(segDecodedStart, rangeStart) + 1
				if keep <= 0 {
					break
				}
				if keep > int64(len(data)) {
					keep = int64(len(data))
				}
				data = data[:keep]
			}
		}

		if _, werr := w.Write(data); werr != nil {
			return fmt.Errorf("nntp: stream: write: %w", werr)
		}
		if canFlush {
			flusher.Flush()
		}

		// Stop once rangeEnd is satisfied.
		if isPartial && decodedOffset > rangeEnd {
			break
		}
	}
	return nil
}

// max64 returns the larger of two int64 values.
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
