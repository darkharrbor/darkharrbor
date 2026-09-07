package nntp

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ProviderHealth is NS-1.2's shared per-provider-name health/backoff
// tracker. It is keyed by provider name (Pool.providerName), not by *Pool
// instance, because a single provider's demand and readahead pools (see
// cache.go) share one account and must be treated as one health subject: a
// killed credential fails both identically, and either one succeeding is
// evidence the account is fine.
//
// A "failure" here means a rung exhausted its own bounded internal retry
// (fetchArticleFromPoolTotal's 3 attempts) and still could not complete --
// i.e. a real dial/auth/transport problem, not a single definitive
// missing-article answer (ClassPermanentHere), which is evidence the
// provider IS reachable and is never counted as unhealthy.
//
// The backoff schedule is deliberately simple and in-memory only (N10: "all
// scheduling state is in-memory"): consecutive failures double the backoff
// window up to a cap, and a single success clears it immediately. This is
// not meant to be a sophisticated circuit breaker -- its only job is to
// stop a per-segment ladder from spending a full 3-attempt dial/auth cycle
// against a provider whose credentials were just killed, on every single
// segment of an in-progress stream, once the first couple of segments have
// already proven it's down.
type ProviderHealth struct {
	mu            sync.Mutex
	state         map[string]*providerHealthState
	now           func() time.Time
	stripe        bool
	retentionDays map[string]int
	observedRoute map[string]bool
	stripeCursor  atomic.Uint64

	// lagYoungAgeSec, lagMinSamples, and lagState are NS-9.2's
	// per-provider article-propagation lag model. lagYoungAgeSec is the
	// age (seconds) below which a post is "young" and eligible for
	// lag-based ordering; 0 (the default, set by every existing
	// constructor) disables the model entirely, reproducing exactly the
	// pre-NS-9.2 preference/retention/stripe behavior. Set via
	// SetLagModel, matching every other SetXxx wiring method's
	// before-first-Acquire contract.
	lagYoungAgeSec int64
	lagMinSamples  int
	lagState       map[string]*providerLagAvg
}

type providerHealthState struct {
	consecutiveFailures int
	backoffUntil        time.Time
}

// providerLagAvg is NS-9.2's bounded per-provider observation: a
// recency-weighted average age-at-first-success (seconds) plus a sample
// count gating whether that average is trusted for ordering decisions
// yet. O(1) memory per provider regardless of traffic volume -- no
// per-item history is retained (matches N10: in-memory scheduling state
// only, and ProviderHealth's own existing bounded-state convention).
type providerLagAvg struct {
	samples int
	meanSec float64
}

const (
	// providerHealthBackoffBase is the backoff window after the first
	// observed failure.
	providerHealthBackoffBase = 2 * time.Second
	// providerHealthBackoffCap bounds exponential growth so a long-dead
	// provider is retried periodically rather than abandoned forever --
	// N10 makes item.Provider a preference, not a lock, and a provider
	// whose credentials are later fixed must be recoverable without a
	// process restart.
	providerHealthBackoffCap = 2 * time.Minute
	// providerHealthBackoffThreshold is the number of consecutive
	// failures before any backoff is applied at all. A single flaky
	// failure should not exile a provider from the very next segment --
	// only a run of them.
	providerHealthBackoffThreshold = 2
)

// NewProviderHealth constructs an empty tracker.
func NewProviderHealth() *ProviderHealth {
	return NewProviderHealthWithRouting(false, nil)
}

// NewProviderHealthWithRouting constructs the existing shared health tracker
// with NS-8.4's in-memory candidate-order policy. retentionDays is copied so
// runtime scheduling cannot race with startup configuration assembly.
func NewProviderHealthWithRouting(stripe bool, retentionDays map[string]int) *ProviderHealth {
	h := &ProviderHealth{
		state:         make(map[string]*providerHealthState),
		now:           time.Now,
		stripe:        stripe,
		retentionDays: make(map[string]int, len(retentionDays)),
		observedRoute: make(map[string]bool),
	}
	for name, days := range retentionDays {
		if name != "" && days > 0 {
			h.retentionDays[name] = days
		}
	}
	return h
}

// SetLagModel enables NS-9.2's per-provider article-propagation lag
// model. youngAgeSec is the age (seconds) below which a post is
// considered young and eligible for lag-based ordering; 0 or negative
// disables the model, reproducing exactly the pre-NS-9.2 behavior (the
// default for every existing constructor, so callers that never invoke
// this method are unaffected). minSamples is the number of observed
// age-at-first-success samples a provider needs before its average is
// trusted enough to influence ordering; values below 1 are clamped to 1.
// Must be called before Acquire is first called, matching every other
// SetXxx wiring method on ProviderHealth/Pool.
func (h *ProviderHealth) SetLagModel(youngAgeSec int64, minSamples int) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if youngAgeSec <= 0 {
		h.lagYoungAgeSec = 0
		return
	}
	if minSamples < 1 {
		minSamples = 1
	}
	h.lagYoungAgeSec = youngAgeSec
	h.lagMinSamples = minSamples
	if h.lagState == nil {
		h.lagState = make(map[string]*providerLagAvg)
	}
}

// SetClock overrides the scheduler/health clock for deterministic tests. It
// must be called during startup, before requests are served.
func (h *ProviderHealth) SetClock(now func() time.Time) {
	if h == nil || now == nil {
		return
	}
	h.mu.Lock()
	h.now = now
	h.mu.Unlock()
}

// RecordSuccess clears any accumulated failure/backoff state for name. Safe
// to call on a nil receiver (no-op), matching the package's existing
// nil-safe-optional-collaborator convention (negativeCache, ConnCeiling).
func (h *ProviderHealth) RecordSuccess(name string) {
	if h == nil || name == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, name)
}

// RecordSuccessAt is RecordSuccess plus NS-9.2's lag-model observation:
// when the lag model is enabled (SetLagModel called with a positive
// youngAgeSec) and postedAt is a usable past timestamp, the elapsed age
// at this first success is folded into name's recency-weighted average
// age-at-first-success via a fixed-weight EWMA (bounded O(1) memory, no
// per-item history retained). A zero, negative, or future postedAt is
// ambiguous and abstains from the lag observation while RecordSuccess's
// existing backoff-clearing behavior still applies. Safe to call on a
// nil receiver.
func (h *ProviderHealth) RecordSuccessAt(name string, postedAt int64) {
	h.RecordSuccess(name)
	if h == nil || name == "" || postedAt <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lagYoungAgeSec == 0 {
		return
	}
	nowUnix := h.now().Unix()
	if postedAt > nowUnix {
		return
	}
	ageSec := float64(nowUnix - postedAt)
	st, ok := h.lagState[name]
	if !ok {
		st = &providerLagAvg{}
		h.lagState[name] = st
	}
	st.samples++
	const lagEWMAAlpha = 0.2
	if st.samples == 1 {
		st.meanSec = ageSec
	} else {
		st.meanSec = lagEWMAAlpha*ageSec + (1-lagEWMAAlpha)*st.meanSec
	}
}

// lagYoungThreshold returns the configured young-post age threshold under
// h.mu, so orderCandidates never races with a concurrent SetLagModel
// call. 0 means the lag model is disabled.
func (h *ProviderHealth) lagYoungThreshold() int64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lagYoungAgeSec
}

// lagAverage returns name's current recency-weighted average
// age-at-first-success and whether it has reached the configured minimum
// sample count yet. Safe to call on a nil receiver or unknown name
// (returns !ok either way, meaning "treat as unobserved").
func (h *ProviderHealth) lagAverage(name string) (meanSec float64, ok bool) {
	if h == nil || name == "" {
		return 0, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, exists := h.lagState[name]
	if !exists || st.samples < h.lagMinSamples {
		return 0, false
	}
	return st.meanSec, true
}

// RecordFailure records one exhausted-rung failure for name, growing its
// backoff window if the consecutive-failure threshold is met.
func (h *ProviderHealth) RecordFailure(name string) {
	if h == nil || name == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.state[name]
	if !ok {
		st = &providerHealthState{}
		h.state[name] = st
	}
	st.consecutiveFailures++
	if st.consecutiveFailures < providerHealthBackoffThreshold {
		return
	}
	shift := st.consecutiveFailures - providerHealthBackoffThreshold
	backoff := providerHealthBackoffBase << shift
	if backoff <= 0 || backoff > providerHealthBackoffCap {
		backoff = providerHealthBackoffCap
	}
	st.backoffUntil = h.now().Add(backoff)
}

// Backoff returns the remaining backoff duration for name, or 0 if name is
// currently healthy (or unknown, or h is nil).
func (h *ProviderHealth) Backoff(name string) time.Duration {
	if h == nil || name == "" {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.state[name]
	if !ok {
		return 0
	}
	remaining := st.backoffUntil.Sub(h.now())
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// orderCandidates applies NS-8.4 and NS-9.2 without changing the candidate
// set. Healthy providers are considered before backed-off providers. When
// an article is older than any known provider horizon, known horizons
// sort deepest first. Otherwise, when the article is young (NS-9.2, off
// by default) and at least two healthy candidates have a trusted observed
// average age-at-first-success, candidates sort by that average
// ascending. Otherwise optional striping rotates healthy providers.
// Missing, future, or ambiguous timestamps abstain and retain ordinary
// order (or striping).
func (h *ProviderHealth) orderCandidates(candidates []*Pool, postedAt int64) (ordered []*Pool, policy string) {
	if h == nil || len(candidates) < 2 {
		return candidates, "preference"
	}

	healthy := make([]*Pool, 0, len(candidates))
	backedOff := make([]*Pool, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if h.Backoff(candidate.providerName) > 0 {
			backedOff = append(backedOff, candidate)
		} else {
			healthy = append(healthy, candidate)
		}
	}
	if len(healthy) == 0 {
		return append([]*Pool(nil), candidates...), "preference"
	}

	nowUnix := h.clockNow().Unix()
	ageDays := int64(-1)
	if postedAt > 0 && postedAt <= nowUnix {
		ageDays = (nowUnix - postedAt) / int64((24*time.Hour)/time.Second)
	}
	retentionApplies := false
	if ageDays >= 0 {
		for _, candidate := range healthy {
			if days := h.retentionDays[candidate.providerName]; days > 0 && ageDays > int64(days) {
				retentionApplies = true
				break
			}
		}
	}

	// NS-9.2: for young posts (below the configured lag-model threshold,
	// disabled by default), order by observed per-provider average
	// age-at-first-success instead of static preference/stripe, but only
	// once at least two healthy candidates have reached the minimum
	// sample count -- with fewer than two comparable candidates there is
	// nothing to usefully reorder, so it falls through to existing
	// preference/stripe behavior unchanged. Retention (old posts) is
	// checked first and always takes priority; the two conditions are
	// not expected to overlap in practice (young posts are, by
	// definition, far inside every real provider's retention horizon).
	lagApplies := false
	if !retentionApplies {
		if youngAgeSec := h.lagYoungThreshold(); youngAgeSec > 0 && postedAt > 0 && postedAt <= nowUnix {
			if nowUnix-postedAt < youngAgeSec {
				known := 0
				for _, candidate := range healthy {
					if _, ok := h.lagAverage(candidate.providerName); ok {
						known++
					}
				}
				lagApplies = known >= 2
			}
		}
	}

	policy = "preference"
	if retentionApplies {
		sort.SliceStable(healthy, func(i, j int) bool {
			left := h.retentionDays[healthy[i].providerName]
			right := h.retentionDays[healthy[j].providerName]
			switch {
			case left == right:
				return false
			case left == 0:
				return false
			case right == 0:
				return true
			default:
				return left > right
			}
		})
		policy = "retention"
	} else if lagApplies {
		sort.SliceStable(healthy, func(i, j int) bool {
			leftMean, leftOK := h.lagAverage(healthy[i].providerName)
			rightMean, rightOK := h.lagAverage(healthy[j].providerName)
			switch {
			case leftOK && rightOK:
				return leftMean < rightMean
			case leftOK && !rightOK:
				return true
			default:
				return false
			}
		})
		policy = "lag"
	} else if h.stripe && len(healthy) > 1 {
		start := int((h.stripeCursor.Add(1) - 1) % uint64(len(healthy)))
		healthy = append(healthy[start:], healthy[:start]...)
		policy = "stripe"
	}
	return append(healthy, backedOff...), policy
}

// firstSuccessfulRoute returns true once per bounded policy/provider pair for
// the life of this in-memory scheduler. It supports one non-flooding INFO proof
// of each active route; per-segment detail remains DEBUG-only.
func (h *ProviderHealth) firstSuccessfulRoute(provider, policy string) bool {
	if h == nil || provider == "" || policy == "" {
		return false
	}
	key := policy + "\x00" + provider
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.observedRoute[key] {
		return false
	}
	h.observedRoute[key] = true
	return true
}

func (h *ProviderHealth) clockNow() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now()
}
