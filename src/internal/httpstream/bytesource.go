package httpstream

// HTTP ByteSource adapter (HR1.1; HTTP-plan HR-D12, "one shared ByteSource
// owner"). Exposes one resolved HTTP source as a bytesource.ByteSource so
// archive parsers, container seek-index parsing, media truth, and
// verification consume HTTP bytes through the same lane-agnostic contract
// they already use for NNTP segments and debrid CDN windows (TS-1.1).
//
// Scope discipline, per the consolidated master plan section 3 (one owner per
// shared subsystem):
//
//   - NO cache and NO in-flight coalescing live here. Sparse cache identity
//     and global range coalescing belong to HR1.3 in internal/rangecache;
//     adaptive prewarm/readahead belongs to TS-1.4/HR5.3. This adapter issues
//     exactly the ranged GETs its caller asks for.
//   - NO continuity ledger and NO proof graph. Mutation handling here is
//     strictly LOCAL to one source's own lifetime: if the origin's
//     representation changes underneath an active source, reads fail rather
//     than splice across representations (LC-05). The durable ledger is
//     HR1.5; the proof vocabulary is HR1.2.
//   - Resolution is INJECTED. The adapter never talks to a backend registry
//     itself; the caller supplies a Resolver, which in the running server is
//     the existing per-(item,file) resolve coordinator, so concurrent
//     resolves stay coalesced by their existing owner (HS-3.8/HS-3.9).
//
// Security invariants (D11, DG-04, LG-10): the transient source URL and its
// request headers live in memory for the life of the source and are never
// persisted, never logged, and never rendered into an error. This file emits
// no log lines at all; every error is a sanitized *Error. Redirects are
// followed internally by the shared SecurityPolicy on the supplied client and
// upstream Location is never read here, let alone relayed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

const (
	// byteSourceMaxAttempts bounds the upstream requests one ReadAt may
	// issue. A well-behaved origin needs one; a short-reading origin needs a
	// few more to deliver the tail of the requested range. Exhaustion is an
	// error, never an unbounded loop.
	byteSourceMaxAttempts = 8

	// byteSourceMaxSkip bounds how far into a Range-ignoring origin's 200
	// body the adapter will discard to reach the requested offset (DG-09).
	// Past this, a random read would degenerate into a whole-file download,
	// which is a lazy-materialization violation - fail distinctly instead.
	byteSourceMaxSkip = 8 << 20

	// byteSourceExpirySkew is subtracted from a handler-supplied ExpiresAt
	// before it is trusted, matching the resolve coordinator's own margin.
	byteSourceExpirySkew = 30 * time.Second

	// predictiveResolveTimeout bounds HR3.2's background predictive
	// re-resolve. It runs on its own detached, bounded context -- never the
	// caller's per-ReadAt ctx, whose lifetime is unrelated and typically far
	// shorter -- and always terminates within this budget.
	predictiveResolveTimeout = 20 * time.Second

	// predictiveCooldown bounds how often a predictive attempt (success or
	// failure) may retry, so a source sitting inside its lead window across
	// many ReadAt calls cannot spawn goroutines faster than this.
	predictiveCooldown = 3 * time.Second

	// velocityEWMAAlpha smooths the playhead-velocity estimate used to skip
	// a predictive re-resolve when playback is expected to finish (reach
	// EOF) before the current token would expire anyway.
	velocityEWMAAlpha = 0.3
)

// ErrSourceMutated reports that the origin's representation changed while a
// ByteSource was live: a different total length, a different strong entity
// tag, or a different Last-Modified. Bytes from before and after such a
// change are never combined (LC-05). Errors carrying this sentinel are also
// *Error values of class ClassUpstreamMalformed, so the existing sanitized
// status mapping and SF-01 outcome vocabulary apply unchanged.
var ErrSourceMutated = errors.New("source representation changed under an active read")

type mutatedError struct{ inner *Error }

func (m *mutatedError) Error() string   { return m.inner.Error() }
func (m *mutatedError) Unwrap() []error { return []error{m.inner, ErrSourceMutated} }

func mutated(detail string) error {
	return &mutatedError{inner: NewError(ClassUpstreamMalformed, detail)}
}

// Resolver supplies the transient source coordinates for one logical HTTP
// file. forced=true must bypass any cache the implementation keeps and
// produce freshly resolved coordinates. The returned ResolvedFile's URL and
// headers are transient by contract and must never be persisted or logged by
// either side.
type Resolver func(ctx context.Context, forced bool) (ResolvedFile, error)

// ByteSourceConfig configures NewByteSource. Size is the already-verified
// total (the D9 preflight's Content-Range total for HTTP items) and is
// authoritative: the adapter treats a later disagreement from the origin as
// mutation rather than silently adopting the new value.
type ByteSourceConfig struct {
	// Key is a stable, secret-free identity for the source. It must not
	// contain a URL or a query string.
	Key string
	// Size is the verified total byte length. Required and positive.
	Size int64
	// RangeVerified reports whether ranged reads are known to work (the
	// persisted per-file truth established at grab time).
	RangeVerified bool
	// Client is the outbound client. Callers pass the shared relay client
	// built by NewHTTPClient with the D11 SecurityPolicy so SSRF policy,
	// capped internal redirects, and the idle-read stream budget all apply.
	Client *http.Client
	// Resolve supplies transient source coordinates. Required.
	Resolve Resolver
	// Now overrides the clock (standing injectable-clock convention).
	Now func() time.Time
	// OriginScores receives only passive observations from requests this
	// ByteSource already makes. It is advisory and nil-safe.
	OriginScores *OriginScorer
	// PredictiveLeadMin enables HR3.2 predictive midstream re-resolve when
	// positive: sourceFor proactively kicks off one bounded, non-blocking
	// background re-resolve once now+lead reaches the current source's
	// ExpiresAt, so a live read need not stall exactly at the expiry
	// boundary. Zero (the default) disables predictive resolve entirely --
	// the existing reactive re-resolve-on-expiry/mutation path in ReadAt is
	// unchanged either way and remains the sole correctness guarantee; this
	// is a latency optimization layered on top of it, never a replacement.
	PredictiveLeadMin time.Duration
	// PredictiveLeadMax caps the lead computed from OriginScores'
	// per-origin-class observed token lifetimes. Zero means no cap beyond
	// PredictiveLeadMin itself.
	PredictiveLeadMax time.Duration
}

type representation struct {
	total        int64
	etag         string
	lastModified string
}

type httpByteSource struct {
	key     string
	size    int64
	caps    bytesource.Capabilities
	client  *http.Client
	resolve Resolver
	now     func() time.Time
	scores  *OriginScorer

	predictLeadMin time.Duration
	predictLeadMax time.Duration

	mu             sync.Mutex
	cur            ResolvedFile
	haveCur        bool
	expiryObserved bool
	rep            representation
	haveRep        bool

	// Playhead-velocity tracking (HR3.2): a cheap EWMA over successive
	// ReadAt offsets, used only to skip a predictive re-resolve when
	// playback is expected to reach EOF before the current token expires.
	haveVelocity bool
	lastReadAt   time.Time
	lastOff      int64
	velocityBps  float64

	// Predictive-resolve bookkeeping (HR3.2): a single bounded, self-
	// terminating background attempt at a time, rate-limited by cooldown
	// regardless of outcome so a source sitting inside its lead window
	// cannot spawn goroutines faster than predictiveCooldown allows.
	predictiveInFlight      bool
	predictiveCooldownUntil time.Time
}

// NewByteSource builds a bytesource.ByteSource over one resolved HTTP source.
func NewByteSource(cfg ByteSourceConfig) (bytesource.ByteSource, error) {
	key := strings.TrimSpace(cfg.Key)
	if key == "" {
		return nil, NewError(ClassInvalidKey, "byte source requires a key")
	}
	// A ByteSource key reaches caches, indexes, and evidence. A URL-shaped or
	// query-bearing key would carry a source URL or a signed tok= value into
	// exactly those surfaces (DG-04, LG-10).
	if strings.Contains(key, "://") || strings.ContainsAny(key, "?&") || ScrubURLs(key) != key {
		return nil, NewError(ClassInvalidKey, "byte source key must not contain a URL or query string")
	}
	if cfg.Size <= 0 {
		return nil, NewError(ClassUpstreamMalformed, "byte source requires a positive verified size")
	}
	if cfg.Client == nil {
		return nil, NewError(ClassBackendUnavailable, "byte source requires an http client")
	}
	if cfg.Resolve == nil {
		return nil, NewError(ClassNoSource, "byte source requires a resolver")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	predictLeadMax := cfg.PredictiveLeadMax
	if predictLeadMax < 0 {
		predictLeadMax = 0
	}
	predictLeadMin := cfg.PredictiveLeadMin
	if predictLeadMin < 0 {
		predictLeadMin = 0
	}
	return &httpByteSource{
		key:            key,
		size:           cfg.Size,
		client:         cfg.Client,
		now:            now,
		scores:         cfg.OriginScores,
		predictLeadMin: predictLeadMin,
		predictLeadMax: predictLeadMax,
		caps: bytesource.Capabilities{
			// Ranges are byte-granular: no window or segment alignment, and a
			// tail read costs exactly one ranged GET.
			RangeSupport: cfg.RangeVerified,
			ExactSize:    true,
			TailCost:     bytesource.TailCostCheap,
			Alignment:    0,
		},
		resolve: cfg.Resolve,
	}, nil
}

func (h *httpByteSource) Size() int64                   { return h.size }
func (h *httpByteSource) Caps() bytesource.Capabilities { return h.caps }
func (h *httpByteSource) Key() string                   { return h.key }

// ReadAt implements io.ReaderAt semantics over ranged GETs. A body that ends
// before the validated span is filled (short read) is completed by
// re-requesting only the remainder, within a bounded attempt budget. A
// refreshable upstream failure gets exactly one forced re-resolve - the same
// single-refresh discipline the shipped relay status matrix uses - and a
// mutation is terminal and never retried.
func (h *httpByteSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, bytesource.ErrNegativeOffset
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off >= h.size {
		return 0, io.EOF
	}

	h.recordPlayhead(off, h.now())

	want := int64(len(p))
	short := false
	if off+want > h.size {
		want = h.size - off
		short = true
	}

	var n int64
	retried := false
	forceNext := false
	for attempt := 0; n < want; attempt++ {
		if err := ctx.Err(); err != nil {
			return int(n), err
		}
		if attempt >= byteSourceMaxAttempts {
			return int(n), NewError(ClassBackendUnavailable,
				"source did not deliver the requested range within the attempt budget")
		}
		rf, err := h.sourceFor(ctx, forceNext)
		forceNext = false
		if err != nil {
			return int(n), err
		}
		stage := "current"
		if retried {
			stage = "reresolve"
		}
		got, ferr := h.fetchInto(ctx, p[n:want], off+n, rf, stage)
		n += int64(got)
		if ferr == nil {
			continue
		}
		if errors.Is(ferr, ErrSourceMutated) {
			return int(n), ferr
		}
		// A 429 is account-level, not evidence that this representation URL
		// expired. Re-resolving against the same account would spend another
		// request while the cooldown is authoritative.
		if ClassOf(ferr) == ClassRateLimited {
			return int(n), ferr
		}
		if cerr := ctx.Err(); cerr != nil {
			return int(n), cerr
		}
		if retried {
			return int(n), ferr
		}
		retried = true
		forceNext = true
		h.invalidate()
	}
	if short {
		return int(n), io.EOF
	}
	return int(n), nil
}

// sourceFor returns usable coordinates, re-resolving when forced or when the
// held ones are at/past their skew-adjusted expiry. Concurrent readers may
// each call the Resolver; coalescing is the Resolver's own responsibility
// (the running server's resolve coordinator already provides it) and is
// deliberately not duplicated here.
func (h *httpByteSource) sourceFor(ctx context.Context, forced bool) (ResolvedFile, error) {
	if !forced {
		h.mu.Lock()
		rf := h.cur
		expired := h.haveCur && h.expiredLocked()
		observeExpiry := expired && !h.expiryObserved
		if observeExpiry {
			h.expiryObserved = true
		}
		ok := h.haveCur && !expired
		h.mu.Unlock()
		if observeExpiry {
			h.observeOrigin(rf, OriginObservation{Expired: true, RetryStage: "expiry"})
		}
		if ok {
			h.maybeTriggerPredictive(rf)
			return rf, nil
		}
	}
	rf, err := h.resolve(ctx, forced)
	if err != nil {
		return ResolvedFile{}, err
	}
	rf, err = ValidateResolvedFile(rf, h.now())
	if err != nil {
		return ResolvedFile{}, err
	}
	h.mu.Lock()
	h.cur, h.haveCur, h.expiryObserved = rf, true, false
	h.mu.Unlock()
	return rf, nil
}

func (h *httpByteSource) expiredLocked() bool {
	if h.cur.ExpiresAt.IsZero() {
		return false
	}
	return !h.now().Add(byteSourceExpirySkew).Before(h.cur.ExpiresAt)
}

func (h *httpByteSource) invalidate() {
	h.mu.Lock()
	h.cur, h.haveCur = ResolvedFile{}, false
	h.mu.Unlock()
}

// fetchInto performs exactly one upstream ranged GET and copies what it
// returns into dst. A returned count smaller than len(dst) with a nil error
// is a short read the caller completes; it never means fabricated bytes.
func (h *httpByteSource) fetchInto(ctx context.Context, dst []byte, off int64, rf ResolvedFile, retryStage string) (n int, retErr error) {
	started := h.now()
	var firstByte time.Duration
	rangeSuccess := false
	defer func() {
		h.observeOrigin(rf, OriginObservation{
			FirstByte:    firstByte,
			Duration:     h.now().Sub(started),
			Bytes:        int64(n),
			RangeSuccess: rangeSuccess,
			RetryStage:   retryStage,
			Err:          retErr,
		})
	}()

	want := ByteRange{Start: off, End: off + int64(len(dst)) - 1}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rf.URL, nil)
	if err != nil {
		return 0, NewError(ClassUpstreamMalformed, "source URL not requestable")
	}
	// Transient source headers are applied to the upstream request only.
	// Hop-by-hop, identity-bearing, and DH-owned headers are filtered (D11).
	for k, v := range rf.RequestHeaders {
		if badByteSourceHeader(k) {
			continue
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", UpstreamRangeHeader(want))

	resp, err := h.client.Do(req)
	firstByte = h.now().Sub(started)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return 0, cerr
		}
		// url.Error embeds the full request URL; it is never wrapped, only
		// replaced with static text (DG-04).
		return 0, NewError(ClassBackendUnavailable, "source fetch failed")
	}
	defer drainAndClose(resp)

	switch resp.StatusCode {
	case http.StatusPartialContent:
		total, verr := ValidateUpstream206(resp.Header.Get("Content-Range"), want)
		if verr != nil {
			return 0, verr
		}
		if merr := h.observeRepresentation(total, resp.Header); merr != nil {
			return 0, merr
		}
		rangeSuccess = true
		return h.copyBody(ctx, resp.Body, dst)

	case http.StatusOK:
		// The origin ignored the Range header (DG-09). Reaching off means
		// discarding a prefix, which is only acceptable inside a bounded
		// window - beyond it, a random read would become a full download.
		if isNonMediaContentType(resp.Header.Get("Content-Type")) {
			return 0, NewError(ClassUpstreamMalformed, "source returned non-media body")
		}
		if off > byteSourceMaxSkip {
			return 0, NewError(ClassNoSource, "source ignores Range beyond the bounded skip window")
		}
		if resp.ContentLength > 0 {
			if merr := h.observeRepresentation(resp.ContentLength, resp.Header); merr != nil {
				return 0, merr
			}
		}
		if _, derr := io.CopyN(io.Discard, resp.Body, off); derr != nil {
			if cerr := ctx.Err(); cerr != nil {
				return 0, cerr
			}
			return 0, NewError(ClassBackendUnavailable, "source body read failed")
		}
		return h.copyBody(ctx, resp.Body, dst)

	case http.StatusRequestedRangeNotSatisfiable:
		// DH only ever requests ranges inside the verified size, so an
		// upstream 416 means the representation is no longer the one that
		// was verified. Terminal, never refreshed.
		return 0, mutated("source rejected a range inside the verified size")

	case http.StatusTooManyRequests:
		return 0, NewRateLimitError(resp.Header.Get("Retry-After"))

	default:
		return 0, Wrapf(ClassBackendUnavailable, "source responded %d", resp.StatusCode)
	}
}

func (h *httpByteSource) observeOrigin(rf ResolvedFile, observation OriginObservation) {
	if h.scores == nil {
		return
	}
	if id := opaqueOriginID(rf.URL); id != "" {
		h.scores.Observe(id, observation)
	}
}

// opaqueOriginID separates origins without retaining their URL, host, path,
// query, credentials, or headers in scorer state.
func opaqueOriginID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil || u.Host == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(u.Scheme) + "\x00" + strings.ToLower(u.Host)))
	return "origin:" + hex.EncodeToString(sum[:])
}

// copyBody fills dst as far as the body allows. A truncated body yields a
// short count with a nil error so the caller re-requests only the remainder.
func (h *httpByteSource) copyBody(ctx context.Context, body io.Reader, dst []byte) (int, error) {
	n, err := io.ReadFull(body, dst)
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return n, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return n, cerr
	}
	return n, NewError(ClassBackendUnavailable, "source body read failed")
}

// observeRepresentation records the origin's identity on first sight and
// compares it on every later response. Only conclusive disagreements are
// mutation: a weak entity tag is not a byte-range continuity signal and is
// ignored, an unknown ("*") total is not compared, and a validator that
// merely appears or disappears is inconclusive. Validators are learned but
// never unlearned.
func (h *httpByteSource) observeRepresentation(total int64, hdr http.Header) error {
	seen := representation{
		total:        total,
		etag:         strongETag(hdr.Get("ETag")),
		lastModified: strings.TrimSpace(hdr.Get("Last-Modified")),
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.haveRep {
		h.rep, h.haveRep = seen, true
		// The verified size is authoritative: an origin that already
		// disagrees changed since grab-time verification.
		if seen.total > 0 && seen.total != h.size {
			return mutated("source total length disagrees with the verified size")
		}
		return nil
	}

	prev := h.rep
	switch {
	case seen.total > 0 && prev.total > 0 && seen.total != prev.total:
		return mutated("source total length changed")
	case prev.etag != "" && seen.etag != "" && prev.etag != seen.etag:
		return mutated("source entity tag changed")
	case prev.etag == "" && seen.etag == "" &&
		prev.lastModified != "" && seen.lastModified != "" &&
		prev.lastModified != seen.lastModified:
		return mutated("source last-modified changed")
	}

	if prev.total == 0 && seen.total > 0 {
		h.rep.total = seen.total
	}
	if prev.etag == "" && seen.etag != "" {
		h.rep.etag = seen.etag
	}
	if prev.lastModified == "" && seen.lastModified != "" {
		h.rep.lastModified = seen.lastModified
	}
	return nil
}

// strongETag returns the entity tag only when it is strong. A weak validator
// does not guarantee octet equality and must never authorize byte continuity.
func strongETag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(strings.ToLower(v), "w/") {
		return ""
	}
	return v
}

// isNonMediaContentType reports an HTML/JSON body, which is an expired or
// erroring source rather than media - the same judgment the shipped relay
// makes on a 200.
func isNonMediaContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	return strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json")
}

// badByteSourceHeader filters hop-by-hop, identity-bearing, and DH-owned
// headers out of a transient source header map, reusing the single forbidden
// set already enforced by ValidateResolvedFile.
func badByteSourceHeader(name string) bool {
	n := http.CanonicalHeaderKey(strings.TrimSpace(name))
	if _, bad := forbiddenTransientHeaders[n]; bad {
		return true
	}
	return n == "Accept-Encoding"
}

// drainAndClose reads a bounded remainder so the connection can be reused,
// then closes. The drain is capped: an abandoned movie-sized body is dropped,
// never consumed.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

// ── HR3.2: predictive midstream re-resolve ──────────────────────────────────
//
// The reactive path above (sourceFor's forced re-resolve on expiry/mutation)
// is the correctness guarantee and is unchanged by everything below. This
// section only ever shortens the window in which a live read would otherwise
// have to synchronously re-resolve right at the expiry boundary: a bounded,
// best-effort background attempt fires once the caller's own configured lead
// (informed by OriginScores' observed per-origin-class token lifetimes) is
// reached, and its result is adopted only when it provably still matches the
// same verified representation identity (Selector, Size). Any mismatch, any
// error, or PredictiveLeadMin==0 (the default) leaves the reactive path as
// the sole mechanism, byte-identical to pre-HR3.2 behavior.

// recordPlayhead updates the smoothed playhead-velocity estimate from
// successive requested offsets. It is a cheap, bounded, in-place EWMA over a
// handful of scalars -- no growth, no I/O.
func (h *httpByteSource) recordPlayhead(off int64, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.haveVelocity {
		h.lastReadAt, h.lastOff, h.haveVelocity = now, off, true
		return
	}
	dt := now.Sub(h.lastReadAt)
	doff := off - h.lastOff
	if dt > 0 && doff > 0 {
		inst := float64(doff) / dt.Seconds()
		if h.velocityBps <= 0 {
			h.velocityBps = inst
		} else {
			h.velocityBps = h.velocityBps*(1-velocityEWMAAlpha) + inst*velocityEWMAAlpha
		}
	}
	if off > h.lastOff {
		h.lastOff = off
	}
	h.lastReadAt = now
}

// willFinishBeforeExpiry reports whether, at the current smoothed velocity,
// playback is expected to reach EOF before expiresAt lands -- in which case
// a predictive refresh would never be used and is skipped. Unknown velocity
// (no samples yet, or already effectively at EOF) is treated conservatively
// as "will not finish first", so the predictive attempt still proceeds.
func (h *httpByteSource) willFinishBeforeExpiry(expiresAt, now time.Time) bool {
	h.mu.Lock()
	have := h.haveVelocity
	velocity := h.velocityBps
	remaining := h.size - h.lastOff
	h.mu.Unlock()
	if remaining <= 0 {
		// Playhead has already reached effective EOF: no further reads are
		// expected, so a predictive refresh has no remaining benefit either.
		return true
	}
	if !have || velocity <= 0 {
		return false
	}
	etaSeconds := float64(remaining) / velocity
	if etaSeconds < 0 {
		return false
	}
	eta := now.Add(time.Duration(etaSeconds * float64(time.Second)))
	return eta.Before(expiresAt)
}

// predictiveLead returns the lead time to apply before rf.ExpiresAt,
// starting from the configured floor and raised toward 10% of the origin
// class's own observed token lifetime (HR3.2's "observed token lifetimes ...
// inform the lead"), capped at PredictiveLeadMax when configured.
func (h *httpByteSource) predictiveLead(originID string) time.Duration {
	lead := h.predictLeadMin
	if h.scores != nil && originID != "" {
		if observed := h.scores.EstimatedLifetime(originID); observed > 0 {
			if frac := observed / 10; frac > lead {
				lead = frac
			}
		}
	}
	if h.predictLeadMax > 0 && lead > h.predictLeadMax {
		lead = h.predictLeadMax
	}
	return lead
}

// maybeTriggerPredictive starts at most one bounded background predictive
// re-resolve when rf is inside its configured lead window and playback is
// not expected to finish before rf.ExpiresAt anyway. It never blocks the
// caller and never returns an error; any failure simply leaves the reactive
// path to handle the eventual real expiry.
func (h *httpByteSource) maybeTriggerPredictive(rf ResolvedFile) {
	if h.predictLeadMin <= 0 || rf.ExpiresAt.IsZero() {
		return
	}
	now := h.now()
	originID := opaqueOriginID(rf.URL)
	lead := h.predictiveLead(originID)
	if now.Add(lead).Before(rf.ExpiresAt) {
		return // not yet inside the predictive lead window
	}
	if h.willFinishBeforeExpiry(rf.ExpiresAt, now) {
		return // playback is expected to end before the token does
	}
	h.mu.Lock()
	if h.predictiveInFlight || now.Before(h.predictiveCooldownUntil) {
		h.mu.Unlock()
		return
	}
	h.predictiveInFlight = true
	h.mu.Unlock()
	go h.runPredictiveResolve(originID)
}

// runPredictiveResolve performs exactly one forced background re-resolve on
// a detached, bounded context (never the caller's own per-ReadAt ctx, whose
// lifetime is unrelated) and adopts the result only when it provably matches
// the source's already-verified identity: the same Selector, and -- when
// both sides know a size -- the same verified Size. A shorter or equal
// ExpiresAt is not an improvement and is dropped rather than adopted, so a
// predictive attempt can never make the held coordinates worse. Every path
// clears predictiveInFlight and starts the next cooldown window.
func (h *httpByteSource) runPredictiveResolve(originID string) {
	ctx, cancel := context.WithTimeout(context.Background(), predictiveResolveTimeout)
	defer cancel()
	started := h.now()
	rf, err := h.resolve(ctx, true)
	if err == nil {
		rf, err = ValidateResolvedFile(rf, h.now())
	}

	adopted := false
	h.mu.Lock()
	if err == nil {
		sameIdentity := rf.Selector == h.cur.Selector &&
			(h.size <= 0 || rf.Size <= 0 || rf.Size == h.size)
		improves := rf.ExpiresAt.IsZero() ||
			h.cur.ExpiresAt.IsZero() ||
			rf.ExpiresAt.After(h.cur.ExpiresAt)
		if sameIdentity && improves {
			h.cur = rf
			h.expiryObserved = false
			adopted = true
		}
	}
	h.predictiveInFlight = false
	h.predictiveCooldownUntil = h.now().Add(predictiveCooldown)
	h.mu.Unlock()

	if adopted && h.scores != nil && originID != "" && !rf.ExpiresAt.IsZero() {
		h.scores.ObserveTokenLifetime(originID, rf.ExpiresAt.Sub(started))
	}
}
