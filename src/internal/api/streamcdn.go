package api

// streamcdn.go — HTTP/CDN ChunkSource for the torrent /stream byte-proxy
//
// With a shared rangecache configured, the byte proxy engages the same
// byte-range cache the NNTP path uses through a windowed ranged-GET source over
// the ephemeral TorBox CDN URL. Upstream URLs remain internal to DarkHarrbor.
//
// Capability honesty (F5): an origin that answers the bytes=0-0 probe without
// Accept-Ranges / 206 is treated as range-incapable; the caller falls back to
// the verbatim legacy passthrough copy so the 200-only seek-defeat is
// surfaced exactly as before rather than papered over.
//
// Expiry (C-expiry): an expired/rate-limited window triggers one reresolve of
// the CDN URL (provider.RequestDownloadURL + the same preflight-follow the
// proxy already performs) before the fetch is retried; a second failure in
// the same incident is fatal. A 416 is always fatal.
//
// TS-2.1: the above is formalized as torrent ladder rungs 1–3 of the T3
// chunk acquisition ladder (rangecache hit → current CDN URL → reresolve).
// Rung 1 ("cache") is rangecache.getChunk's own existing disk-hit/miss path
// (internal/rangecache/rangecache.go) — shared across every lane and not
// duplicated here, per this row's explicit "do not build a second cache"
// scope. Rungs 2 and 3 are Fetch's existing current-URL attempt and
// refreshMu-guarded reresolve-and-retry, unchanged in control flow, order,
// and locking, now run through two named internal/ladder.Rung walks purely
// for SF-02 accounting and structured logging (see Fetch below).
import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/ladder"
	"github.com/darkharrbor/darkharrbor/internal/outcome"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

// TS-2.1 rung names, used only for accounting/log labels — lane-owned
// meaning, matching the naming convention NS-1.1 established
// (rungNameNNTPPrimary etc.) and the frozen T3 spec's own rung numbering.
const (
	rungCDNCurrent   = "torrent_cdn_current"   // rung 2: current CDN URL
	rungCDNReresolve = "torrent_cdn_reresolve" // rung 3: reresolve same provider
	// rungCDNAltProviderPrefix names rung 4 (TS-2.2): bounded alternate-
	// provider same-hash midstream failover. One ladder rung is named per
	// candidate provider (rungAltProviderName) so per-provider outcomes are
	// individually accounted/logged, matching the rungCDNCurrent/Reresolve
	// per-rung accounting convention.
	rungCDNAltProviderPrefix = "torrent_cdn_altprovider_"
)

// rungAltProviderName returns the rung 4 accounting/log name for one
// alternate-provider candidate.
func rungAltProviderName(providerName string) string {
	return rungCDNAltProviderPrefix + providerName
}

// cdnReResolver re-fetches a fresh CDN URL after an expiry (403). It returns
// the clean, token-free CDN URL (post preflight-follow).
type cdnReResolver func(ctx context.Context) (string, error)

// cdnChunkSource adapts an HTTP origin (the TorBox CDN URL) to a
// rangecache.ChunkSource using fixed-size byte windows.
type cdnChunkSource struct {
	mu                sync.Mutex
	refreshMu         sync.Mutex
	key               string
	url               string
	client            *http.Client
	windowBytes       int64
	total             int64
	contentType       string
	reresolve         cdnReResolver
	reresolved        bool
	maxWorkers        int    // caps readahead parallelism (default 1 — CDN 429 avoidance)
	invalidateResolve func() // called when 429 persists after reresolve (bad node)

	// TS-2.1: optional structured logger for rung accounting (see
	// logRungOutcome). nil-safe: falls back to slog.Default() so a caller
	// that never sets it (e.g. TS-1.2's future archive ByteSource
	// consumers) is unaffected.
	log *slog.Logger

	// TS-2.3: passive, bounded CDN node evidence. sourceID is an opaque hash
	// of the last resolved remote IP; it is never a hostname, URL, or token.
	scores   *httpstream.OriginScorer
	sourceID string
	now      func() time.Time

	// TS-1.4: optional shared account governor. When gov is non-nil, each
	// ladder rung acquires a priority lease for (govOp, priority, session)
	// before issuing its HTTP range request — the mechanism that lets live
	// playback (accountgov.PriorityPlayback) and TS-1.4 prewarm
	// (accountgov.PriorityPrewarm) actually contend for something, so LC-06's
	// fixed priority has a real effect instead of staying dormant. nil-safe:
	// an unconfigured gov means every fetch runs exactly as before this row.
	//
	// TS-2.1 note: a lease is now acquired once per ladder rung rather than
	// once for the whole Fetch incident (the pre-TS-2.1 behavior). This
	// matches the NS-1.1/HR3.1 precedent for internal/ladder consumers and
	// is a disclosed, intentional refinement: it can only let a higher
	// priority waiter interleave between rung 2 and rung 3 of the same
	// incident, never starve one, so LC-06 fixed priority still holds.
	gov      *accountgov.Governor
	govOp    string
	priority accountgov.Priority
	session  string

	// TS-2.2: rung 4 alternate-provider candidates, set only by the real
	// playback call site (streamViaCache) -- nil for prewarm/RAR/ZIP
	// callers, which makes rung 4 a structural no-op and leaves Fetch's
	// pre-TS-2.2 behavior (fail after rung 3) exactly unchanged for them.
	altCandidates []altProviderCandidate

	// altMu guards the per-candidate bounded-confirm cache below. Confirm
	// (one CheckCached or one Submit+Poll add-then-verify round trip) runs
	// at most once per candidate for this cdnChunkSource's lifetime -- one
	// HTTP GET/stream request -- mirroring the "bounded confirm" the frozen
	// T3 spec calls for and the reresolved per-incident caching pattern
	// already established for rung 3.
	altMu        sync.Mutex
	altConfirmed map[string]cdnReResolver
	altFailed    map[string]bool

	// TS-3.4 (T12): optional TorrentMeta + this stream's own matched
	// FileEntry, set once at construction by the real playback call site
	// only (streamViaCache) -- nil for prewarm/RAR/ZIP callers, exactly
	// like altCandidates above, making verification a structural no-op for
	// them. Read-only after construction; never mutated during Fetch, so no
	// mutex protection is needed for these two fields specifically.
	meta     *torrentmeta.TorrentMeta
	metaFile *torrentmeta.FileEntry

	// TS-7.2 rung 5. Nil for every caller unless a Ready torrent file has a
	// native proof domain and cross-lane recovery is explicitly enabled.
	crossLaneRecover func(context.Context, int64, int64) (torrentCrossLaneResult, error)
}

const (
	cdnEarlyReresolveScore = -20
	maxCDNRemoteAddrBytes  = 256
)

func cdnRefreshStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// torrentCDNScoreID turns only a resolved IP into a bounded opaque identity.
// Unknown, malformed, or hostname-only remote addresses safely abstain.
func torrentCDNScoreID(remoteAddr string) string {
	if len(remoteAddr) == 0 || len(remoteAddr) > maxCDNRemoteAddrBytes {
		return ""
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(ip.String()))
	return fmt.Sprintf("torrent-cdn-%x", sum[:12])
}

func (c *cdnChunkSource) observeCDN(id string, observation httpstream.OriginObservation) {
	if id == "" || c.scores == nil {
		return
	}
	c.mu.Lock()
	c.sourceID = id
	c.mu.Unlock()
	c.scores.Observe(id, observation)
}

func (c *cdnChunkSource) shouldEarlyReresolve() bool {
	c.mu.Lock()
	id, canReresolve := c.sourceID, c.reresolve != nil
	c.mu.Unlock()
	return canReresolve && id != "" && c.scores.Score(id) <= cdnEarlyReresolveScore
}

// verifyPieceWindow opportunistically checks one freshly fetched window's
// bytes against this stream's TorrentMeta (TS-3.4/T12: "opportunistic
// aligned v1 piece/v2 Merkle proof and corruption rejection/penalty").
//
// ok=true covers BOTH the "verified and matched" case and the "nothing to
// check" abstention (unaligned window, or no meta/hash material) -- exactly
// the frozen T12 design's "never block streaming on unaligned reads;
// verification is opportunistic by design". A caller must never treat
// ok=true as proof the bytes are good; it only means this function raised
// no objection.
//
// ok=false means a piece hash mismatched: the CDN host score is penalized
// (T9's existing passive scorer, wired torrent-lane in TS-2.3) at the same
// severity as any other permanent-here fetch failure, and the caller must
// treat this exactly like a refresh-worthy failure -- try the next T3 rung
// -- never serve the corrupt bytes to the cache or a player.
func (c *cdnChunkSource) verifyPieceWindow(start int64, data []byte) (ok bool) {
	if c.meta == nil || c.metaFile == nil {
		return true
	}
	checked, verified, badIndex := torrentmeta.VerifyWindow(c.meta, c.metaFile, start, data)
	if checked == 0 || verified {
		return true
	}
	c.mu.Lock()
	id := c.sourceID
	c.mu.Unlock()
	if c.scores != nil && id != "" {
		c.scores.Observe(id, httpstream.OriginObservation{
			Err: outcome.Permanent(fmt.Errorf("cdn source: window %d-%d piece %d verification failed", start, start+int64(len(data))-1, badIndex)),
		})
	}
	log := c.log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("torrent: cdn piece verification failed (T12)", "key", c.key, "piece", badIndex)
	return false
}

func (c *cdnChunkSource) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// initCDNSource performs the bytes=0-0 capability probe and returns a ready
// source plus (total, contentType, supportsRanges). supportsRanges==false
// means the caller must use the legacy passthrough.
func initCDNSource(ctx context.Context, cdnURL string, windowBytes int64, reresolve cdnReResolver) (*cdnChunkSource, int64, string, bool, error) {
	src := &cdnChunkSource{
		url:         cdnURL,
		client:      &http.Client{Timeout: 0}, // streaming; no overall timeout
		windowBytes: windowBytes,
		reresolve:   reresolve,
		maxWorkers:  1, // safe default; caller overrides via SetMaxWorkers
	}
	probe := func(url string) (status int, total int64, contentType string, err error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, 0, "", err
		}
		req.Header.Set("Range", "bytes=0-0")
		resp, err := src.client.Do(req)
		if err != nil {
			return 0, 0, "", err
		}
		defer func() { _, _ = io.CopyN(io.Discard, resp.Body, 4<<10); _ = resp.Body.Close() }()
		return resp.StatusCode, parseContentRangeTotal(resp.Header.Get("Content-Range")), resp.Header.Get("Content-Type"), nil
	}

	status, total, contentType, err := probe(cdnURL)
	if err != nil {
		return nil, 0, "", false, err
	}
	// An expired or rate-limited probe gets one fresh URL. This initialization
	// retry is separate from Fetch's per-incident reresolve allowance.
	if cdnRefreshStatus(status) {
		if reresolve == nil {
			return nil, 0, "", false, fmt.Errorf("cdn source: capability probe status %d", status)
		}
		newURL, rerr := reresolve(ctx)
		if rerr != nil {
			return nil, 0, "", false, fmt.Errorf("cdn source: probe reresolve failed: %w", rerr)
		}
		if newURL == "" {
			return nil, 0, "", false, fmt.Errorf("cdn source: probe reresolve returned empty URL")
		}
		src.url = newURL
		status, total, contentType, err = probe(newURL)
		if err != nil {
			return nil, 0, "", false, err
		}
		if cdnRefreshStatus(status) {
			return nil, 0, "", false, fmt.Errorf("cdn source: capability probe status %d after reresolve", status)
		}
	}
	// A range-capable origin answers a single-byte range with 206 and a valid
	// Content-Range total. Anything else stays on the legacy passthrough path.
	if status == http.StatusOK || (status == http.StatusPartialContent && total <= 0) {
		return src, 0, contentType, false, nil
	}
	if status != http.StatusPartialContent {
		return nil, 0, "", false, fmt.Errorf("cdn source: unexpected capability probe status %d", status)
	}
	src.total = total
	src.contentType = contentType
	return src, total, contentType, true, nil
}

func (c *cdnChunkSource) Key() string      { return c.key }
func (c *cdnChunkSource) ExactSizes() bool { return true }

// MaxWorkers implements rangecache.WorkerLimiter. CDN fetches are fast but
// concurrent requests to the same node trigger per-IP 429s. Default is 1;
// configurable via HARRBOR_STREAM_READAHEAD_WORKERS.
func (c *cdnChunkSource) MaxWorkers() int {
	if c.maxWorkers > 0 {
		return c.maxWorkers
	}
	return 1
}

func (c *cdnChunkSource) Chunks() []rangecache.ChunkRef {
	n := c.total / c.windowBytes
	if c.total%c.windowBytes != 0 {
		n++
	}
	refs := make([]rangecache.ChunkRef, 0, n)
	for i := int64(0); i < n; i++ {
		start := i * c.windowBytes
		size := c.windowBytes
		if start+size > c.total {
			size = c.total - start
		}
		refs = append(refs, rangecache.ChunkRef{
			Index: int(i),
			Key:   fmt.Sprintf("w%012d", start),
			Size:  size,
		})
	}
	return refs
}

// logRungOutcome emits one secret-safe structured log line per ladder rung
// attempted (TS-2.1 "rung accounting + structured logs"). Only the DH-owned
// item/file key, the rung name, and the SF-01 outcome class are logged —
// never a URL, token, or header (DG-04). Success is logged at Debug (the
// common case, every readahead chunk); a rung that did not succeed is logged
// at Info so an operator can see reresolve-incident frequency without
// enabling debug logging.
func (c *cdnChunkSource) logRungOutcome(name string, res ladder.Result) {
	log := c.log
	if log == nil {
		log = slog.Default()
	}
	if res.Succeeded {
		log.Debug("torrent: cdn ladder rung", "rung", name, "key", c.key, "outcome", "success")
		return
	}
	var class outcome.Class
	if n := len(res.Rungs); n > 0 {
		class = res.Rungs[n-1].Class
	}
	log.Info("torrent: cdn ladder rung", "rung", name, "key", c.key, "outcome", "failed", "class", string(class))
}

// lastRungErr returns the most recent rung's recorded error, the same value
// internal/ladder's Run already used to decide advance/stop, regardless of
// whether that value came from a classified Attempt failure, a governor
// lease failure, or ctx cancellation (see ladder.Ladder.runRung).
func lastRungErr(res ladder.Result) error {
	if n := len(res.Rungs); n > 0 {
		return res.Rungs[n-1].Err
	}
	return nil
}

// Fetch issues a ranged GET for one window, walking T3 rungs 2-5 in order
// after rangecache rung 1 misses: current CDN, same-provider reresolve,
// alternate provider, then proof-gated NNTP/HTTP recovery. Each provider rung
// has one attempt and is accounted by internal/ladder; rung 5 is absent unless
// the request has exact native torrent proofs and cross-lane recovery enabled.
//
// Behavior is unchanged from pre-TS-2.1: rung 2 is exactly the prior
// unconditional first doRange against the current URL; rung 3 is exactly the
// prior refreshMu-guarded race-check-then-reresolve block, moved verbatim
// into reresolveAndRetry. A 416 or any status that isn't refresh-worthy
// remains fatal without ever attempting rung 3, preserving "a 416 is always
// fatal" and "an unexpected status never reresolves" exactly.
func (c *cdnChunkSource) Fetch(ctx context.Context, ref rangecache.ChunkRef, class rangecache.FetchClass) ([]byte, error) {
	start, err := strconv.ParseInt(strings.TrimPrefix(ref.Key, "w"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("cdn source: bad window key %q: %w", ref.Key, err)
	}
	end := start + ref.Size - 1

	var (
		data   []byte
		status int
	)
	var failedURL string
	currentLadder := ladder.New(c.gov, ladder.Rung{
		Name:        rungCDNCurrent,
		Op:          c.govOp,
		Priority:    c.priority,
		MaxAttempts: 1,
		Attempt: func(attemptCtx context.Context) error {
			if c.shouldEarlyReresolve() {
				c.mu.Lock()
				failedURL = c.url
				c.mu.Unlock()
				status = http.StatusGone
				return outcome.Permanent(fmt.Errorf("cdn source: passive score requested reresolve"))
			}
			var ferr error
			data, status, failedURL, ferr = c.doRange(attemptCtx, start, end, "current")
			if ferr != nil {
				// A response can advertise 200/206 and then truncate its body.
				// The transport failure, not the stale header status, controls
				// recovery: clear status so reresolve/alternate-provider rungs
				// run exactly as they do for a pre-header connection failure.
				status = 0
				return ferr
			}
			if status == http.StatusOK || status == http.StatusPartialContent {
				if int64(len(data)) != ref.Size {
					// A successful header with a truncated body is a transport
					// failure, not usable media. Clear the stale status so the
					// recovery ladder advances and never serves partial bytes.
					status = 0
					return outcome.Transient(fmt.Errorf("cdn source: short window %d-%d", start, end))
				}
				// TS-3.4 (T12): a corrupt window is treated exactly like a
				// bad/expired node -- reuse the same synthetic-StatusGone
				// refresh-worthy signal shouldEarlyReresolve() already
				// establishes below, so the existing rung-3/rung-4
				// fallthrough control flow in Fetch needs no changes at all.
				if !c.verifyPieceWindow(start, data) {
					c.mu.Lock()
					failedURL = c.url
					c.mu.Unlock()
					status = http.StatusGone
					return outcome.Permanent(fmt.Errorf("cdn source: window %d-%d corrupt (T12)", start, end))
				}
				c.mu.Lock()
				c.reresolved = false
				c.mu.Unlock()
				return nil
			}
			if status == http.StatusRequestedRangeNotSatisfiable {
				return outcome.Permanent(fmt.Errorf("cdn source: window %d-%d unsatisfiable (416)", start, end))
			}
			if !cdnRefreshStatus(status) {
				return outcome.Permanent(fmt.Errorf("cdn source: window %d-%d unexpected status %d", start, end, status))
			}
			return outcome.Permanent(fmt.Errorf("cdn source: window %d-%d refresh-worthy status %d", start, end, status))
		},
	})
	res1, _ := currentLadder.Run(ctx, c.session)
	c.logRungOutcome(rungCDNCurrent, res1)
	if res1.Succeeded {
		return data, nil
	}
	// A cancelled client must stop immediately rather than spending recovery
	// budget after nobody is waiting for the bytes.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 416 and any non-refresh-worthy status are fatal without ever
	// attempting rung 3. A transport failure has no HTTP status (0), so it
	// must advance: provider unreachability is exactly what rungs 3 and 4 are
	// meant to recover from.
	if status == http.StatusRequestedRangeNotSatisfiable || (status != 0 && !cdnRefreshStatus(status)) {
		return nil, lastRungErr(res1)
	}

	reresolveLadder := ladder.New(c.gov, ladder.Rung{
		Name:        rungCDNReresolve,
		Op:          c.govOp,
		Priority:    c.priority,
		MaxAttempts: 1,
		Attempt: func(attemptCtx context.Context) error {
			var rerr error
			data, rerr = c.reresolveAndRetry(attemptCtx, start, end, failedURL, status)
			return rerr
		},
	})
	res2, _ := reresolveLadder.Run(ctx, c.session)
	c.logRungOutcome(rungCDNReresolve, res2)
	if res2.Succeeded {
		return data, nil
	}

	// TS-2.2 rung 4: bounded alternate-provider same-hash midstream
	// failover. altCandidates is nil for every caller except real playback
	// (streamViaCache), so this loop is a structural no-op there --
	// identical to pre-TS-2.2 behavior (return the rung-3 error).
	for _, cand := range c.altCandidates {
		cand := cand
		altLadder := ladder.New(c.gov, ladder.Rung{
			Name:        rungAltProviderName(cand.name),
			Op:          c.govOp,
			Priority:    accountgov.PriorityRecovery,
			MaxAttempts: 1,
			Attempt: func(attemptCtx context.Context) error {
				resolveFn, cerr := c.confirmAltCandidate(attemptCtx, cand)
				if cerr != nil {
					return outcome.Permanent(fmt.Errorf("cdn source: altprovider %s confirm: %w", cand.name, cerr))
				}
				newURL, rerr := resolveFn(attemptCtx)
				if rerr != nil {
					return outcome.Permanent(fmt.Errorf("cdn source: altprovider %s resolve: %w", cand.name, rerr))
				}
				if newURL == "" {
					return outcome.Permanent(fmt.Errorf("cdn source: altprovider %s resolve returned empty URL", cand.name))
				}
				d, st, _, ferr := c.doRangeAt(attemptCtx, newURL, start, end, "alternate")
				if ferr != nil {
					return ferr
				}
				if st != http.StatusOK && st != http.StatusPartialContent {
					return outcome.Permanent(fmt.Errorf("cdn source: altprovider %s window %d-%d status %d", cand.name, start, end, st))
				}
				// TS-3.4 (T12): never adopt an alternate provider whose
				// bytes fail verification -- a corrupt rung-4 candidate is
				// exactly as disqualifying as one that returns a bad status.
				if !c.verifyPieceWindow(start, d) {
					return outcome.Permanent(fmt.Errorf("cdn source: altprovider %s window %d-%d corrupt (T12)", cand.name, start, end))
				}
				// Adopt: this alternate provider becomes the current source
				// for the remainder of this incident, and every later
				// window in this stream, exactly as the frozen T3 spec's
				// "resolve + continue byte-exact ... playback continues on
				// the alternate" describes.
				c.mu.Lock()
				c.url = newURL
				c.reresolve = resolveFn
				c.reresolved = false
				c.mu.Unlock()
				data = d
				return nil
			},
		})
		res4, _ := altLadder.Run(ctx, c.session)
		c.logRungOutcome(rungAltProviderName(cand.name), res4)
		if res4.Succeeded {
			return data, nil
		}
	}

	// TS-7.2 rung 5: the shared XR coordinator owns recovery-priority
	// admission and proof verification. A nil callback is the pre-row no-op.
	if c.crossLaneRecover != nil {
		recovered, recoverErr := c.crossLaneRecover(ctx, start, end)
		if recoverErr == nil && int64(len(recovered.data)) == ref.Size {
			log := c.log
			if log == nil {
				log = slog.Default()
			}
			log.Info("torrent: cross-lane recovery accepted",
				"blocks", recovered.blocks,
				"nntp_blocks", recovered.nntpBlocks,
				"http_blocks", recovered.httpBlocks,
				"bytes", recovered.bytes,
			)
			return recovered.data, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return nil, lastRungErr(res2)
}

// reresolveAndRetry is rung 3 of the T3 chunk-fetch ladder (TS-2.1),
// unchanged verbatim from Fetch's pre-TS-2.1 refreshMu-guarded block: only
// one goroutine performs an actual reresolve at a time; a concurrent
// incident first rechecks whether another goroutine already replaced the
// URL (retrying that winner's URL before ever calling reresolve itself) —
// this is what TestCDNChunkSourceSharesConcurrentReresolve proves costs
// exactly one real reresolve() call for two simultaneous expiries.
func (c *cdnChunkSource) reresolveAndRetry(ctx context.Context, start, end int64, failedURL string, status int) ([]byte, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.Lock()
	currentURL := c.url
	c.mu.Unlock()
	if currentURL != failedURL {
		data, st, _, ferr := c.doRange(ctx, start, end, "reresolve")
		if ferr != nil {
			return nil, ferr
		}
		if st == http.StatusOK || st == http.StatusPartialContent {
			// TS-3.4 (T12): same rule as rung 2 -- a corrupt window here is a
			// fetch failure, not a success, and must not stop this incident;
			// falling through to the reresolve-and-retry path below (or, if
			// this is itself the reresolve-and-retry path already, to rung
			// 4 via Fetch's own unconditional fallthrough after this
			// function returns an error).
			if !c.verifyPieceWindow(start, data) {
				return nil, outcome.Permanent(fmt.Errorf("cdn source: window %d-%d corrupt (T12)", start, end))
			}
			c.mu.Lock()
			c.reresolved = false
			c.mu.Unlock()
			return data, nil
		}
		if st == http.StatusRequestedRangeNotSatisfiable {
			return nil, outcome.Permanent(fmt.Errorf("cdn source: window %d-%d unsatisfiable (416)", start, end))
		}
		if !cdnRefreshStatus(st) {
			return nil, outcome.Permanent(fmt.Errorf("cdn source: window %d-%d unexpected status %d", start, end, st))
		}
		status = st
	}

	c.mu.Lock()
	alreadyReResolved := c.reresolved
	c.mu.Unlock()
	if alreadyReResolved || c.reresolve == nil {
		// A node that stays bad after refresh must not remain in the resolve cache.
		if c.invalidateResolve != nil {
			c.invalidateResolve()
		}
		return nil, outcome.Permanent(fmt.Errorf("cdn source: window %d-%d expired/rate-limited (status %d) after reresolve", start, end, status))
	}
	newURL, rerr := c.reresolve(ctx)
	if rerr != nil {
		return nil, outcome.Transient(fmt.Errorf("cdn source: reresolve failed: %w", rerr))
	}
	if newURL == "" {
		return nil, outcome.Permanent(fmt.Errorf("cdn source: reresolve returned empty URL"))
	}
	c.mu.Lock()
	c.url = newURL
	c.reresolved = true
	c.mu.Unlock()
	data, st, _, ferr := c.doRange(ctx, start, end, "reresolve")
	if ferr != nil {
		return nil, ferr
	}
	if st == http.StatusOK || st == http.StatusPartialContent {
		// TS-3.4 (T12): verify before declaring the reresolved fetch a
		// success -- a corrupt window here has already exhausted rung 3's
		// own retry allowance, so it falls straight to rung 4 (or fails).
		if !c.verifyPieceWindow(start, data) {
			return nil, outcome.Permanent(fmt.Errorf("cdn source: window %d-%d corrupt after reresolve (T12)", start, end))
		}
		// COR-1: a later URL expiry is a new incident and may refresh again.
		c.mu.Lock()
		c.reresolved = false
		c.mu.Unlock()
		return data, nil
	}
	return nil, outcome.Permanent(fmt.Errorf("cdn source: window %d-%d status %d after reresolve", start, end, st))
}

func (c *cdnChunkSource) doRange(ctx context.Context, start, end int64, retryStage string) ([]byte, int, string, error) {
	c.mu.Lock()
	url := c.url
	c.mu.Unlock()
	return c.doRangeAt(ctx, url, start, end, retryStage)
}

// doRangeAt is doRange against an explicit URL rather than c.url (TS-2.2:
// rung 4 fetches a candidate alternate provider's CDN URL before deciding
// whether to adopt it, so it must not read or mutate c.url on the attempt
// path -- only a successful attempt adopts, exactly like rung 3's own
// c.url swap only happens after a confirmed successful retry).
func (c *cdnChunkSource) doRangeAt(ctx context.Context, url string, start, end int64, retryStage string) ([]byte, int, string, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()

	started := c.nowTime()
	var sourceID string
	var firstByte time.Duration
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			sourceID = torrentCDNScoreID(info.Conn.RemoteAddr().String())
		},
		GotFirstResponseByte: func() { firstByte = c.nowTime().Sub(started) },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, url, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := client.Do(req)
	if err != nil {
		c.observeCDN(sourceID, httpstream.OriginObservation{
			FirstByte:  firstByte,
			Duration:   c.nowTime().Sub(started),
			RetryStage: retryStage,
			Err:        outcome.Transient(err),
		})
		return nil, 0, url, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		statusErr := outcome.Permanent(fmt.Errorf("cdn status %d", resp.StatusCode))
		if resp.StatusCode == http.StatusTooManyRequests {
			statusErr = outcome.AccountLevel(fmt.Errorf("cdn status %d", resp.StatusCode))
		} else if resp.StatusCode >= 500 {
			statusErr = outcome.Transient(fmt.Errorf("cdn status %d", resp.StatusCode))
		}
		c.observeCDN(sourceID, httpstream.OriginObservation{
			FirstByte:  firstByte,
			Duration:   c.nowTime().Sub(started),
			Expired:    cdnRefreshStatus(resp.StatusCode),
			RetryStage: retryStage,
			Err:        statusErr,
		})
		return nil, resp.StatusCode, url, nil
	}
	// PERF-10: pre-size the read buffer to the expected window size instead of
	// io.ReadAll's repeated doubling (saves ~4 allocs per 16MB window).
	expected := end - start + 1
	data := make([]byte, expected)
	n, err := io.ReadFull(resp.Body, data)
	observation := httpstream.OriginObservation{
		FirstByte:    firstByte,
		Duration:     c.nowTime().Sub(started),
		Bytes:        int64(n),
		RangeSuccess: resp.StatusCode == http.StatusPartialContent && n == len(data),
		RetryStage:   retryStage,
	}
	if err == io.ErrUnexpectedEOF {
		// Preserve the established short-read behavior while scoring the
		// partial response as transient evidence rather than guessing success.
		observation.Err = outcome.Transient(fmt.Errorf("short CDN range response"))
		c.observeCDN(sourceID, observation)
		return data[:n], resp.StatusCode, url, nil
	}
	if err != nil {
		observation.Err = outcome.Transient(err)
		c.observeCDN(sourceID, observation)
		return nil, resp.StatusCode, url, err
	}
	c.observeCDN(sourceID, observation)
	return data[:n], resp.StatusCode, url, nil
}

// confirmAltCandidate runs cand's bounded confirm at most once per
// cdnChunkSource lifetime (TS-2.2 "bounded"): the result (success resolver
// or failure) is cached so repeated windows within the same stream never
// re-issue a real CheckCached/Submit/Poll round trip against an already-
// decided candidate.
func (c *cdnChunkSource) confirmAltCandidate(ctx context.Context, cand altProviderCandidate) (cdnReResolver, error) {
	c.altMu.Lock()
	if c.altConfirmed == nil {
		c.altConfirmed = map[string]cdnReResolver{}
		c.altFailed = map[string]bool{}
	}
	if r, ok := c.altConfirmed[cand.name]; ok {
		c.altMu.Unlock()
		return r, nil
	}
	if c.altFailed[cand.name] {
		c.altMu.Unlock()
		return nil, fmt.Errorf("cdn source: altprovider %s already failed confirm this stream", cand.name)
	}
	c.altMu.Unlock()

	resolveFn, err := cand.confirm(ctx)

	c.altMu.Lock()
	defer c.altMu.Unlock()
	if err != nil {
		c.altFailed[cand.name] = true
		return nil, err
	}
	c.altConfirmed[cand.name] = resolveFn
	return resolveFn, nil
}

// parseContentRangeTotal extracts the total from "bytes START-END/TOTAL".
func parseContentRangeTotal(cr string) int64 {
	i := strings.LastIndex(cr, "/")
	if i < 0 {
		return 0
	}
	tail := strings.TrimSpace(cr[i+1:])
	if tail == "*" {
		return 0
	}
	n, err := strconv.ParseInt(tail, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// makeCDNSourceFromCache constructs a cdnChunkSource directly from a warm
// resolve cache entry, skipping the bytes=0-0 capability probe (PERF-3).
func makeCDNSourceFromCache(cdnURL string, windowBytes int64, total int64, contentType string, reresolve cdnReResolver) *cdnChunkSource {
	return &cdnChunkSource{
		url:         cdnURL,
		client:      &http.Client{Timeout: 0},
		windowBytes: windowBytes,
		reresolve:   reresolve,
		maxWorkers:  1,
		total:       total,
		contentType: contentType,
	}
}

// NewCDNChunkSourceProbe (TS-1.4) performs the real bytes=0-0 capability
// probe (DG-09: a grab-time prewarm caller has no prior proof the origin is
// Range-capable, unlike streamViaCache's warm resolve-cache fast path) and
// returns a ready rangecache.ChunkSource wired to the shared account
// governor at accountgov.PriorityPrewarm. rangeCapable reports whether the
// origin actually answered a Range with 206 + a valid Content-Range total;
// callers must skip prewarm entirely when false, exactly like streamViaCache
// does for the live path — a Range-ignoring origin is never assumed capable.
//
// log (TS-2.1) may be nil; Fetch's rung-accounting logs fall back to
// slog.Default() when unset.
func NewCDNChunkSourceProbe(ctx context.Context, key, cdnURL string, windowBytes int64, reresolve func(context.Context) (string, error), invalidate func(), gov *accountgov.Governor, session string, log *slog.Logger) (rangecache.ChunkSource, bool, error) {
	src, _, _, rangeCapable, err := initCDNSource(ctx, cdnURL, windowBytes, cdnReResolver(reresolve))
	if err != nil {
		return nil, false, err
	}
	src.key = key
	src.invalidateResolve = invalidate
	src.gov = gov
	src.govOp = TorrentCDNGovOp
	src.priority = accountgov.PriorityPrewarm
	src.session = session
	src.log = log
	return src, rangeCapable, nil
}

// streamViaCache serves a torrent byte-proxy request through the shared
// rangecache when the origin supports ranges. Returns served=false (without
// writing anything) when the origin is not range-capable, so the caller runs
// the legacy passthrough. The reresolve closure performs a fresh
// RequestDownloadURL + preflight-follow.
// altCandidates is TS-2.2's rung 4 alternate-provider list, built by the
// caller (router.go, real playback only) from the item's configured debrid
// providers other than its bound primary. nil is always safe: it makes
// rung 4 a no-op, matching every pre-TS-2.2 caller and every non-playback
// caller (prewarm, CDN-RAR/ZIP) of this same cdnChunkSource type.
func (s *Server) streamViaCache(w http.ResponseWriter, r *http.Request, item *store.Item, itemID, fileID, cdnURL string, reresolve cdnReResolver, invalidate func(), cachedTotal int64, cachedContentType string, cachedRangeCapable *bool, altCandidates []altProviderCandidate, tsMeta *torrentmeta.TorrentMeta, tsMetaFile *torrentmeta.FileEntry) (served bool) {
	if s.streamCache == nil {
		return false
	}
	if cachedRangeCapable != nil && !*cachedRangeCapable {
		return false
	}
	ctx := r.Context()
	windowBytes := int64(s.cfg.Cache.StreamChunkSizeMB) * 1024 * 1024
	if windowBytes <= 0 {
		windowBytes = 16 * 1024 * 1024
	}
	var src *cdnChunkSource
	var total int64
	var contentType string
	// PERF-3: skip the bytes=0-0 capability probe only when a prior probe
	// proved range support. File-list size alone is not capability evidence.
	if cachedRangeCapable != nil && *cachedRangeCapable && cachedTotal > 0 {
		src = makeCDNSourceFromCache(cdnURL, windowBytes, cachedTotal, cachedContentType, reresolve)
		total = cachedTotal
		contentType = cachedContentType
	} else {
		var ranged bool
		var err error
		src, total, contentType, ranged, err = initCDNSource(ctx, cdnURL, windowBytes, reresolve)
		if err != nil {
			s.log.Warn("stream: cdn cache probe failed, falling back to passthrough", "error", httpstream.Sanitize(err))
			// TS-4.1: this is the OTHER real "stream-time" broken/removed
			// detection trigger the frozen T5 decision names, distinct from
			// the mid-stream Stream() error path below -- a fresh playback
			// attempt whose very first capability probe fails is the more
			// common real-world manifestation of a provider-deleted item
			// (a user presses play on something whose provider link already
			// died) than an already-successfully-streaming session breaking
			// mid-play. Same non-client-cancelled + real-playback gating as
			// the mid-stream trigger below.
			if ctx.Err() == nil && altCandidates != nil {
				s.TriggerRepairAsync(item)
				// TS-5.1: the same genuine non-client-cancelled real-
				// playback failure signal TS-4.1 already established is
				// exactly a negative own-traffic hash observation.
				s.recordStreamAvailability(ctx, item, false)
			}
			return false
		}
		capable := ranged
		cacheTotal := total
		if cacheTotal <= 0 {
			cacheTotal = cachedTotal
		}
		cacheContentType := contentType
		if cacheContentType == "" {
			cacheContentType = cachedContentType
		}
		s.cacheResolveEntry(itemID, fileID, src.url, cacheContentType, cacheTotal, &capable)
		if !ranged || total <= 0 {
			// F5: range-incapable origin — surface seek-defeat via legacy passthrough.
			s.log.Info("stream: cdn origin not range-capable, using passthrough (F5)")
			return false
		}
		// TS-5.1: a fresh live bytes=0-0 capability probe that actually
		// succeeded is a real, positive own-traffic observation that this
		// provider still serves this hash -- recorded only here (never on
		// the cachedRangeCapable fast path above, which performs no live
		// provider-facing call) and only for real playback (altCandidates
		// is non-nil only at this call site, matching the TS-4.1 negative
		// signal's own gating immediately above).
		if altCandidates != nil {
			s.recordStreamAvailability(ctx, item, true)
		}
	}
	src.key = "torrent|" + resolveKey(itemID, fileID)
	// TS-2.2: rung 4 candidates for this stream (nil unless the caller
	// built any -- see altCandidates doc comment above).
	src.altCandidates = altCandidates
	// TS-3.4 (T12): opportunistic piece/merkle verification material for
	// this stream (nil unless the caller resolved this item's TorrentMeta
	// and matched this exact file -- see the router.go call site's own
	// lookup, mirroring TS-0.3's established by-size matching convention).
	src.meta = tsMeta
	src.metaFile = tsMetaFile
	if item != nil && tsMeta != nil && tsMetaFile != nil && s.cfg.NNTP.CrossLaneSplice {
		src.crossLaneRecover = func(recoverCtx context.Context, start, end int64) (torrentCrossLaneResult, error) {
			return s.recoverTorrentCrossLaneWindow(recoverCtx, item, fileID, tsMeta, tsMetaFile, start, end)
		}
	}
	// TS-1.4: playback fetches contend for the shared account governor at
	// PriorityPlayback, so a concurrent prewarm (PriorityPrewarm, same
	// TorrentCDNGovOp class) can never take capacity a live play is waiting
	// on (LC-06). Both nil-safe: an unconfigured torrentGov leaves every
	// fetch exactly as unleased as before this row.
	src.gov = s.torrentGov
	src.govOp = TorrentCDNGovOp
	src.priority = accountgov.PriorityPlayback
	src.session = itemID
	// TS-2.1: rung-accounting logs use the server's own structured logger.
	src.log = s.log
	// TS-2.3: real playback traffic feeds the one lane-scoped passive scorer.
	src.scores = s.torrentCDNScores
	src.now = s.nowUTC
	// Apply the per-stream worker cap: TS-1.4's adaptive recommendation when
	// this source has a prior prewarm measurement (bitrate + achieved
	// single-connection throughput), otherwise the static configured
	// default — unchanged behavior for a source that was never prewarmed.
	workers := s.cfg.Cache.StreamReadaheadWorkers
	if s.experienceSvc != nil {
		workers = s.experienceSvc.Workers(src, workers)
	}
	if workers > 0 {
		src.maxWorkers = workers
	}
	src.invalidateResolve = invalidate
	mode := s.streamMode()
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	minBuf := s.cfg.Cache.StreamMinBufferSegments
	if minBuf <= 0 {
		minBuf = 2 // default: 2 × StreamChunkSizeMB prefetched before first byte
	}
	if err := s.streamCache.Stream(ctx, src, mode, total, w, r.Header.Get("Range"), minBuf); err != nil { // HARRBOR_STREAM_MIN_BUFFER_SEGMENTS (default 2)
		// Bytes may already be in flight; status is committed. Log only.
		s.log.Warn("stream: cdn cache stream error", "error", httpstream.Sanitize(err))
		// TS-4.1: a genuine (non-client-cancelled) terminal fetch failure on
		// a real-playback stream (altCandidates is non-nil only at this real
		// playback call site, never for prewarm/RAR/ZIP — the same signal
		// TS-2.2's own rung 4 already uses) is exactly T5's "stream-time"
		// broken/removed detection trigger. ctx.Err() != nil means the
		// client went away or the request context expired — not a source
		// failure — so repair is skipped in that case.
		if ctx.Err() == nil && altCandidates != nil {
			s.TriggerRepairAsync(item)
			// TS-5.1: same genuine-failure signal as the capability-probe
			// site above.
			s.recordStreamAvailability(ctx, item, false)
		}
	}
	return true
}

// streamMode resolves the effective cache mode for the /stream path:
// HARRBOR_STREAM_CACHE_MODE overrides HARRBOR_CACHE_MODE when set.
func (s *Server) streamMode() rangecache.Mode {
	m := s.cfg.Cache.Mode
	if s.cfg.Cache.StreamMode != "" {
		m = s.cfg.Cache.StreamMode
	}
	return rangecache.Mode(m)
}
