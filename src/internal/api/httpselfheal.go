package api

// httpselfheal.go — HR3.4: bounded HTTP-lane self-heal with optional
// cooldown-governed Arr re-search escalation (frozen plan HR-D14's final
// rung: "... choose a proven equivalent mirror, choose a proven equivalent
// source in another lane, then request an Arr re-search when configured").
//
// HR-D10's full seven-rung ladder is: sparse cache, current origin,
// re-resolve, verified mirror, verified torrent representation, verified
// NNTP representation, legible failure + optional Arr re-search. HR3.1's
// own DONE entry states it implements rungs 1-3 (current origin + one
// forced same-source re-resolve) and explicitly defers rungs 4-6
// (proof-backed mirror/cross-lane candidates) to the future
// HR6.*/XR-01/XR-02 proof-producer rows -- none of which are dependencies
// of this row, confirmed live against the master plan's own dependency
// graph. This row owns rung 7 only: today every HR3.1 ladder exhaustion is
// an unconditional stream-time 503/502 (httpStreamUnavailable) with zero
// corrective action; this row adds a bounded, two-pass-reconfirmed decay
// signal on top of that existing terminal point, mirroring NS-6.2's own
// two-pass-reconfirm discipline -- NS-6.2's own live-gate correction found
// a single decayed observation can be transient origin trouble, not real
// content loss -- and independently required by this lane's own D13/D16
// rule ("a play-time outage is 5xx, NEVER a state transition"): the item
// stays Ready and its `.strm` is never touched by this row, matching
// NS-6.2's own hard-won correction to the same effect.
//
// Unlike NS-6.1 (an artificial periodic STAT-sampling cadence, needed
// because NNTP segment health is not otherwise observed), the HTTP lane
// already surfaces a real observation on every real playback attempt:
// httpStreamUnavailable is the single existing chokepoint for every live
// HTTP-lane terminal failure (post-cache-ladder-exhaustion in
// streamHTTPViaCache, and pre-cache resolve/handler-lookup failures in
// streamHTTPItem). The older direct-relay path
// (relayHTTPSource/attemptHTTPRelay/httpRelayExhausted) has zero live
// callers -- HR3.1 wired the shared cache ladder as the sole live path --
// so it is out of scope here, not a second hook point. No new periodic
// cycle, cursor, or DG-02 migration is introduced by this row.
//
// Reuse, not duplication (WORKFLOW.md rule 3 — do not add a second
// failure vocabulary): the actual Arr-escalation mechanics — media-identity
// resolution, owning-Arr lookup, marking the Arr's own file record missing
// before searching, and firing the search command — remain NS-6.2's
// existing `resolveDecayedItemIdentity`/`resolveArrSearchTarget`/
// `markArrFileMissing`/`fireArrSearchCommand` in cmd/darkharrbor/main.go,
// which already operate on a lane-agnostic `*store.Item`/
// `terminalFailureStore` and contain zero NNTP-specific logic. Rather than
// relocating them or writing a second copy, this row adds one small
// callback interface (mirroring TS-4.1's `ArrRefreshNotifier` convention
// exactly) that cmd/darkharrbor wires to a thin wrapper reusing
// triggerReSearchOnDecay verbatim, unmodified. Disclosed scope limitation:
// triggerReSearchOnDecay's own leading blacklist+SF-03-suppression
// sub-steps are gated on a populated item.SourceURI carrying literal NZB
// bytes (NS-6.2's own NZBBlacklistKey precondition) -- HTTP items never
// populate SourceURI that way, so those two sub-steps cleanly no-op for
// this lane (the function's own existing nil/empty-SourceURI branch,
// unmodified) and execution proceeds straight to identity resolution,
// owning-Arr lookup, mark-missing, and the search command, which ARE
// fully lane-agnostic and exercised. suppress.LaneHTTP remains reserved
// for a future row that gives the HTTP lane its own equivalent exact-
// identity blacklist primitive; not fabricated here.
import (
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

// HTTPDecayEscalator is satisfied by cmd/darkharrbor's thin wrapper around
// NS-6.2's existing triggerReSearchOnDecay. Nil-safe: an unset escalator
// simply disables the optional Arr re-search half of this row while the
// bounded reconfirm bookkeeping below still runs (observable via
// AttemptCount for WORKLOG/health-surface use).
type HTTPDecayEscalator interface {
	EscalateDecay(item *store.Item)
}

// SetHTTPDecayEscalator wires the optional Arr re-search escalation.
// Mirrors SetArrRefreshNotifier's post-construction setter convention.
func (s *Server) SetHTTPDecayEscalator(e HTTPDecayEscalator) {
	s.httpDecayEscalator = e
}

// httpSelfHealState is this row's own new, in-memory, per-item
// bookkeeping. No schema/migration is introduced (HR3.4 names no DG-02
// gate, matching TS-4.1/NS-6.2 precedent): process-lifetime-only.
type httpSelfHealState struct {
	mu        sync.Mutex
	suspected map[string]bool      // item ID -> one terminal failure already observed, awaiting reconfirmation
	cooldown  map[string]time.Time // item ID -> earliest time a new escalation may fire
	attempts  map[string]int       // item ID -> lifetime escalation counter (observability only)
	now       func() time.Time
}

func newHTTPSelfHealState(now func() time.Time) *httpSelfHealState {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &httpSelfHealState{
		suspected: make(map[string]bool),
		cooldown:  make(map[string]time.Time),
		attempts:  make(map[string]int),
		now:       now,
	}
}

// recordSuccess clears any pending single-strike suspicion for itemID. A
// real, live success between two failures means the earlier failure was
// transient origin trouble, not evidence of decay -- the same reasoning
// NS-6.2's shouldTriggerReSearch doc comment gives for requiring
// reconfirmation on a SEPARATE observation rather than any two failures
// ever recorded.
func (hs *httpSelfHealState) recordSuccess(itemID string) {
	if itemID == "" {
		return
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	delete(hs.suspected, itemID)
}

// recordFailure reports whether THIS failure is the second consecutive
// one for itemID (with no intervening success) and any prior escalation's
// cooldown has elapsed -- i.e. whether the caller should escalate now. The
// first failure only arms suspicion and returns false. A cooldown-
// suppressed reconfirmed failure also returns false but leaves suspicion
// armed, so a further failure once the cooldown lapses can still escalate
// without waiting for a fresh two-observation sequence.
func (hs *httpSelfHealState) recordFailure(itemID string, cooldown time.Duration) bool {
	if itemID == "" {
		return false
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if !hs.suspected[itemID] {
		hs.suspected[itemID] = true
		return false
	}
	now := hs.now()
	if until, ok := hs.cooldown[itemID]; ok && now.Before(until) {
		return false
	}
	hs.suspected[itemID] = false
	hs.attempts[itemID]++
	if cooldown > 0 {
		hs.cooldown[itemID] = now.Add(cooldown)
	} else {
		delete(hs.cooldown, itemID)
	}
	return true
}

// AttemptCount returns the lifetime escalation counter for itemID (0 if
// never escalated). Exposed for WORKLOG/health-surface use.
func (hs *httpSelfHealState) AttemptCount(itemID string) int {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.attempts[itemID]
}

// observeHTTPStreamFailure is HR3.4's entry point, called from
// httpStreamUnavailable for every failure EXCEPT a plain client-side
// cancellation (httpStreamUnavailable itself filters context.Canceled
// before calling in -- a live-gate proof this same session caught a real
// client abort routing through here and confirmed the filter is required:
// ordinary Jellyfin seek/probe disconnects must never count toward the
// two-pass reconfirm). Cheap and non-blocking: the bookkeeping check
// happens synchronously; escalation itself runs on a bounded background
// goroutine so a slow Arr call never delays the client-visible 503/502
// response that already committed.
func (s *Server) observeHTTPStreamFailure(item *store.Item) {
	if item == nil || s.httpSelfHeal == nil {
		return
	}
	if !s.cfg.HTTPStream.SelfHealEnabled {
		return
	}
	cooldown := time.Duration(s.cfg.HTTPStream.SelfHealCooldownMin) * time.Minute
	if !s.httpSelfHeal.recordFailure(item.ID, cooldown) {
		return
	}
	s.log.Info("http selfheal: reconfirmed failure, escalating",
		"event", "http_selfheal_escalate",
		"item_id", item.ID,
	)
	if s.httpDecayEscalator == nil {
		s.log.Info("http selfheal: escalation skipped (no escalator configured)",
			"event", "http_selfheal_escalate_skipped",
			"item_id", item.ID,
		)
		return
	}
	itemCopy := *item
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.httpDecayEscalator.EscalateDecay(&itemCopy)
	}()
}

// observeHTTPStreamSuccess clears HR3.4's single-strike suspicion for
// item. Called on every genuine successful play start (never on a mere
// client cancel -- callers only invoke this on a true r.Context().Err()
// == nil success).
func (s *Server) observeHTTPStreamSuccess(item *store.Item) {
	if item == nil || s.httpSelfHeal == nil {
		return
	}
	s.httpSelfHeal.recordSuccess(item.ID)
}
