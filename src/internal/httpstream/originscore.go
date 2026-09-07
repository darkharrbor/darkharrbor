package httpstream

// Passive origin health scoring for HR3.3. Callers may rank only candidates
// they have already proved equivalent; this scorer is advisory and never
// performs I/O, acquires provider capacity, or weakens proof policy.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

const (
	originScoreHalfLife = 10 * time.Minute
	maxScoredOrigins    = 512
)

// OriginObservation is evidence from an operation that already happened.
// Bytes and Duration describe delivered payload; FirstByte is response-header
// latency. RangeSuccess and Expired record those exact origin behaviors.
type OriginObservation struct {
	FirstByte    time.Duration
	Duration     time.Duration
	Bytes        int64
	RangeSuccess bool
	Expired      bool
	// RetryStage is a fixed ladder stage. Unknown or free-form values fail
	// closed to "unknown" and never affect scoring.
	RetryStage string
	Err        error
}

type originScoreState struct {
	score float64
	at    time.Time
	// lifetimeEWMA/haveLifetime carry HR3.2's per-origin-class observed
	// source-token lifetime (ExpiresAt minus resolve time), smoothed and
	// bounded by the same shared, capped state this scorer already keeps --
	// no second per-origin store. Advisory only: it feeds a caller's own
	// predictive-lead calculation and never gates or retries anything here.
	lifetimeEWMA  time.Duration
	haveLifetime  bool
	firstByteEWMA time.Duration
	haveFirstByte bool
	observations  uint64
	expiries      uint64
	retryStage    string
}

// OriginHealthSnapshot is the bounded, URL-free diagnostic view of one
// scorer entry. Source is a digest of the already-opaque internal identity.
type OriginHealthSnapshot struct {
	Source             string `json:"source"`
	Health             string `json:"health"`
	Score              int64  `json:"score"`
	FirstByteMillis    int64  `json:"first_byte_ms,omitempty"`
	TokenLifetimeSecs  int64  `json:"token_lifetime_seconds,omitempty"`
	Observations       uint64 `json:"observations"`
	ExpiryObservations uint64 `json:"expiry_observations"`
	RetryStage         string `json:"retry_stage"`
}

// OriginScorer keeps bounded, process-local, URL-free passive evidence.
type OriginScorer struct {
	now func() time.Time

	mu     sync.Mutex
	scores map[string]originScoreState
}

// NewOriginScorer constructs one scorer using the injectable program clock.
func NewOriginScorer(now func() time.Time) *OriginScorer {
	if now == nil {
		now = time.Now
	}
	return &OriginScorer{now: now, scores: make(map[string]originScoreState)}
}

// Observe records evidence for an opaque origin/pathway ID. Invalid,
// URL-shaped identities and cancellations are ignored so scoring can never
// fail or alter playback.
func (s *OriginScorer) Observe(id string, observation OriginObservation) {
	if s == nil || !validOriginScoreID(id) || errorsIsCancellation(observation.Err) {
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.scores[id]; !exists && len(s.scores) >= maxScoredOrigins {
		s.evictOldestLocked()
	}
	state := s.scores[id]
	state.score = decayedOriginScore(state, now) + originObservationValue(observation)
	state.score = math.Max(-100, math.Min(100, state.score))
	if observation.FirstByte > 0 {
		const alpha = 0.3
		if !state.haveFirstByte {
			state.firstByteEWMA = observation.FirstByte
		} else {
			state.firstByteEWMA = time.Duration(float64(state.firstByteEWMA)*(1-alpha) + float64(observation.FirstByte)*alpha)
		}
		state.haveFirstByte = true
	}
	state.observations++
	if observation.Expired {
		state.expiries++
	}
	state.retryStage = validRetryStage(observation.RetryStage)
	state.at = now
	s.scores[id] = state
}

// Score returns the origin's current decayed score. Unknown origins are
// neutral. Reading a score performs no probes and starts no background work.
func (s *OriginScorer) Score(id string) float64 {
	if s == nil || !validOriginScoreID(id) {
		return 0
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return decayedOriginScore(s.scores[id], now)
}

// Rank returns a stable score-descending copy. The caller remains responsible
// for proof and provider-policy filtering before passing candidates here.
func (s *OriginScorer) Rank(ids []string) []string {
	ranked := append([]string(nil), ids...)
	now := s.now()
	s.mu.Lock()
	values := make(map[string]float64, len(ranked))
	for _, id := range ranked {
		if validOriginScoreID(id) {
			values[id] = decayedOriginScore(s.scores[id], now)
		}
	}
	s.mu.Unlock()
	sort.SliceStable(ranked, func(i, j int) bool {
		return values[ranked[i]] > values[ranked[j]]
	})
	return ranked
}

// ObserveTokenLifetime records one observed source-token lifetime (HR3.2)
// for an opaque origin/pathway ID, smoothed with the same bounded, capped
// state Observe already maintains. Non-positive lifetimes and invalid or
// URL-shaped IDs are ignored so this can never fail or alter playback.
func (s *OriginScorer) ObserveTokenLifetime(id string, lifetime time.Duration) {
	if s == nil || !validOriginScoreID(id) || lifetime <= 0 {
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.scores[id]; !exists && len(s.scores) >= maxScoredOrigins {
		s.evictOldestLocked()
	}
	state := s.scores[id]
	const lifetimeAlpha = 0.3
	if !state.haveLifetime {
		state.lifetimeEWMA = lifetime
	} else {
		state.lifetimeEWMA = time.Duration(float64(state.lifetimeEWMA)*(1-lifetimeAlpha) + float64(lifetime)*lifetimeAlpha)
	}
	state.haveLifetime = true
	state.at = now
	s.scores[id] = state
}

// EstimatedLifetime returns the origin's smoothed observed token lifetime, or
// 0 when no observation has been recorded yet. It performs no I/O.
func (s *OriginScorer) EstimatedLifetime(id string) time.Duration {
	if s == nil || !validOriginScoreID(id) {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scores[id].lifetimeEWMA
}

// Len exposes bounded-state size for deterministic and health-owner tests;
// it contains no source identities or measurements.
func (s *OriginScorer) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.scores)
}

// Snapshot returns a stable, bounded, secret-safe copy for diagnostics.
func (s *OriginScorer) Snapshot() []OriginHealthSnapshot {
	if s == nil {
		return []OriginHealthSnapshot{}
	}
	now := s.now()
	s.mu.Lock()
	out := make([]OriginHealthSnapshot, 0, len(s.scores))
	for id, state := range s.scores {
		score := decayedOriginScore(state, now)
		sum := sha256.Sum256([]byte(id))
		entry := OriginHealthSnapshot{
			Source:             hex.EncodeToString(sum[:8]),
			Health:             originHealth(score, state.observations),
			Score:              int64(math.Round(score)),
			Observations:       state.observations,
			ExpiryObservations: state.expiries,
			RetryStage:         validRetryStage(state.retryStage),
		}
		if state.haveFirstByte {
			entry.FirstByteMillis = state.firstByteEWMA.Milliseconds()
		}
		if state.haveLifetime {
			entry.TokenLifetimeSecs = int64(state.lifetimeEWMA.Seconds())
		}
		out = append(out, entry)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

func originHealth(score float64, observations uint64) string {
	if observations == 0 {
		return "unknown"
	}
	if score >= 10 {
		return "healthy"
	}
	if score > -10 {
		return "degraded"
	}
	return "unhealthy"
}

func validRetryStage(stage string) string {
	switch stage {
	case "current", "reresolve", "alternate", "cross_lane", "expiry":
		return stage
	default:
		return "unknown"
	}
}

func validOriginScoreID(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && len(id) <= 128 &&
		!strings.Contains(id, "://") && !strings.ContainsAny(id, "?&") &&
		ScrubURLs(id) == id
}

func errorsIsCancellation(err error) bool {
	return errors.Is(err, context.Canceled)
}

func originObservationValue(observation OriginObservation) float64 {
	value := 0.0
	if observation.Err == nil && !observation.Expired {
		value += 8
	}
	if observation.RangeSuccess {
		value += 4
	}
	switch {
	case observation.FirstByte > 0 && observation.FirstByte <= 250*time.Millisecond:
		value += 4
	case observation.FirstByte >= 2*time.Second:
		value -= 4
	}
	if observation.Bytes > 0 && observation.Duration > 0 {
		bytesPerSecond := float64(observation.Bytes) / observation.Duration.Seconds()
		switch {
		case bytesPerSecond >= 8<<20:
			value += 4
		case bytesPerSecond >= 2<<20:
			value += 2
		case bytesPerSecond < 256<<10:
			value -= 3
		}
	}
	if errors.Is(observation.Err, context.DeadlineExceeded) {
		value -= 12
	} else {
		switch outcome.Classify(observation.Err) {
		case outcome.ClassPermanentHere:
			value -= 20
		case outcome.ClassTransientHere:
			value -= 12
		case outcome.ClassAccountLevel:
			value -= 24
		}
	}
	if observation.Expired {
		value -= 16
	}
	return value
}

func decayedOriginScore(state originScoreState, now time.Time) float64 {
	if state.at.IsZero() || !now.After(state.at) {
		return state.score
	}
	return state.score * math.Exp2(-float64(now.Sub(state.at))/float64(originScoreHalfLife))
}

func (s *OriginScorer) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	for id, state := range s.scores {
		if oldestID == "" || state.at.Before(oldest) || (state.at.Equal(oldest) && id < oldestID) {
			oldestID, oldest = id, state.at
		}
	}
	delete(s.scores, oldestID)
}
