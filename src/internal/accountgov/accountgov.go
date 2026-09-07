// Package accountgov implements HR1.4: the shared plan-aware
// account+operation priority governor and leases.
//
// It generalizes two things that previously existed only as separate,
// lane-specific arrangements:
//
//  1. A fixed LC-06 operation-priority ordering, applied as priority- and
//     session-fair-share-ordered admission to a configured concurrent
//     capacity, so lower-priority background work (prewarm, audits,
//     repair) can never starve live playback demand of a scarce account
//     lease (the "demand starvation gate").
//  2. A per-provider AIMD-learned connection ceiling for NNTP-style
//     connection pools: the operator-configured value is the ceiling's
//     starting point and upper bound. A real conn-cap rejection from the
//     provider multiplicatively lowers the effective ceiling; a sustained
//     rejection-free window afterward additively probes back up toward the
//     configured bound. The adjustment happens once, here, at the pool
//     level -- it must never surface as churn on individual per-segment
//     fetch retries.
//
// accountgov does not replace provider.Policy (the existing per-op-class
// token-bucket/Retry-After rate limiter used by TorBox and Real-Debrid) or
// internal/governor.Governor (the store-backed uncached-item-budget gate,
// an unrelated per-item concern). A caller normally acquires an accountgov
// Lease first (ordering/fairness), then calls the provider's own
// provider.Policy.Acquire for the same operation (rate/quota enforcement).
//
// This package is dependency-free (stdlib only) by design so any lane
// (NNTP, torrent, HTTP) can consume it without introducing an import cycle.
package accountgov

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Priority is the fixed LC-06 operation-priority ordering. Lower values are
// served first when multiple waiters contend for the same operation-class
// capacity.
type Priority int

const (
	// PriorityPlayback is live playback and seek -- the highest priority.
	PriorityPlayback Priority = iota
	// PriorityRecovery is active playback recovery.
	PriorityRecovery
	// PriorityGrab is grabs and import validation.
	PriorityGrab
	// PriorityRepair is repair work.
	PriorityRepair
	// PriorityAudit is audits and health checks.
	PriorityAudit
	// PriorityPrewarm is prewarm and speculative background work -- the
	// lowest priority; it is cancellable and cannot take the last usable
	// capacity while any higher-priority demand waits.
	PriorityPrewarm
)

func (p Priority) String() string {
	switch p {
	case PriorityPlayback:
		return "playback"
	case PriorityRecovery:
		return "recovery"
	case PriorityGrab:
		return "grab"
	case PriorityRepair:
		return "repair"
	case PriorityAudit:
		return "audit"
	case PriorityPrewarm:
		return "prewarm"
	default:
		return fmt.Sprintf("priority(%d)", int(p))
	}
}

// Lease is a granted admission to one unit of an operation class's
// configured capacity. Callers must call Release exactly once.
type Lease struct {
	g   *Governor
	op  string
	rel sync.Once
}

// Release returns the lease's capacity unit and admits the next eligible
// waiter, if any, in priority-then-fair-share order.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.rel.Do(func() {
		l.g.release(l.op)
	})
}

// waiter is one blocked Acquire call.
type waiter struct {
	priority Priority
	session  string
	seq      int64
	ready    chan struct{}
}

const (
	denialPriorities = 7 // six fixed priorities plus unknown
	denialReasons    = 2 // canceled, deadline
)

type denialCounts [denialPriorities][denialReasons]uint64

// DenialSnapshot is one fixed-vocabulary governor denial counter.
type DenialSnapshot struct {
	Priority string `json:"priority"`
	Reason   string `json:"reason"`
	Count    uint64 `json:"count"`
}

// Governor is the shared, per-account operation-priority lease manager. One
// Governor instance represents one provider account's shared operation
// capacity profile; every DarkHarrbor feature contending for that account's
// operations acquires leases through it.
//
// Capacity is configured per operation-class name (a caller-chosen string --
// e.g. a provider.Op value's string form, or an NNTP pool identifier). An
// operation class with no configured capacity (SetCapacity never called, or
// called with a non-positive value) is treated as unbounded: Acquire always
// grants immediately and Release is a no-op for it.
type Governor struct {
	name string

	mu      sync.Mutex
	caps    map[string]int
	inUse   map[string]int
	waiting map[string][]*waiter
	served  map[string]map[string]int64 // op -> session -> times granted
	denials map[string]denialCounts
	seq     int64
}

// New creates a Governor for one named provider account.
func New(name string) *Governor {
	return &Governor{
		name:    name,
		caps:    map[string]int{},
		inUse:   map[string]int{},
		waiting: map[string][]*waiter{},
		served:  map[string]map[string]int64{},
		denials: map[string]denialCounts{},
	}
}

// Name returns the account name this Governor was created for.
func (g *Governor) Name() string { return g.name }

// SetCapacity configures the concurrent-lease capacity for an operation
// class. capacity <= 0 makes the class unbounded (no admission gating).
func (g *Governor) SetCapacity(op string, capacity int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if capacity <= 0 {
		delete(g.caps, op)
		return
	}
	g.caps[op] = capacity
}

// Capacity returns the configured capacity for op (0 = unbounded).
func (g *Governor) Capacity(op string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.caps[op]
}

// InUse returns the number of leases currently held for op.
func (g *Governor) InUse(op string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inUse[op]
}

// Waiting returns the number of blocked Acquire callers for op.
func (g *Governor) Waiting(op string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.waiting[op])
}

// Acquire blocks until a lease for operation class op is admitted, honoring
// LC-06 priority order and per-session fair share among waiters of equal
// priority. session identifies the caller for fair-share bookkeeping (e.g. a
// playback session ID or item ID); it may be empty if the caller does not
// distinguish sessions.
func (g *Governor) Acquire(ctx context.Context, op string, priority Priority, session string) (*Lease, error) {
	g.mu.Lock()
	capacity := g.caps[op]
	if capacity <= 0 {
		g.mu.Unlock()
		return &Lease{g: g, op: op}, nil
	}
	if g.inUse[op] < capacity {
		g.inUse[op]++
		g.markServedLocked(op, session)
		g.mu.Unlock()
		return &Lease{g: g, op: op}, nil
	}
	g.seq++
	w := &waiter{priority: priority, session: session, seq: g.seq, ready: make(chan struct{})}
	g.waiting[op] = append(g.waiting[op], w)
	g.mu.Unlock()

	select {
	case <-ctx.Done():
		g.cancelWaiter(op, w, ctx.Err())
		return nil, ctx.Err()
	case <-w.ready:
		return &Lease{g: g, op: op}, nil
	}
}

// cancelWaiter removes w from op's queue if it has not yet been granted. If
// it was already granted concurrently with cancellation, the granted lease
// is released immediately rather than leaked.
func (g *Governor) cancelWaiter(op string, w *waiter, err error) {
	g.mu.Lock()
	counts := g.denials[op]
	counts[denialPriorityIndex(w.priority)][denialReasonIndex(err)]++
	g.denials[op] = counts
	queue := g.waiting[op]
	for i, cand := range queue {
		if cand == w {
			g.waiting[op] = append(queue[:i], queue[i+1:]...)
			g.mu.Unlock()
			return
		}
	}
	g.mu.Unlock()
	// Not found in the queue: it was already granted (raced with ctx
	// cancellation between select cases). Release the capacity we were
	// just handed so it is not leaked.
	select {
	case <-w.ready:
		g.release(op)
	default:
	}
}

// release returns one capacity unit for op and admits the next eligible
// waiter, chosen by lowest Priority value first, then by the session with
// the fewest grants so far (fair share within the same priority tier), then
// by arrival order.
func (g *Governor) release(op string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.caps[op]; !ok {
		return
	}
	queue := g.waiting[op]
	if len(queue) == 0 {
		if g.inUse[op] > 0 {
			g.inUse[op]--
		}
		return
	}
	best := 0
	for i := 1; i < len(queue); i++ {
		if betterWaiter(queue[i], queue[best], g.served[op]) {
			best = i
		}
	}
	w := queue[best]
	g.waiting[op] = append(queue[:best], queue[best+1:]...)
	g.markServedLocked(op, w.session)
	close(w.ready)
	// inUse is unchanged: the released unit transfers directly to w.
}

// betterWaiter reports whether candidate should be admitted ahead of
// current: lower Priority value wins; ties broken by fewer grants so far
// for the candidate's session (fair share); remaining ties broken by
// arrival order (FIFO).
func betterWaiter(candidate, current *waiter, served map[string]int64) bool {
	if candidate.priority != current.priority {
		return candidate.priority < current.priority
	}
	if served[candidate.session] != served[current.session] {
		return served[candidate.session] < served[current.session]
	}
	return candidate.seq < current.seq
}

func (g *Governor) markServedLocked(op, session string) {
	m := g.served[op]
	if m == nil {
		m = map[string]int64{}
		g.served[op] = m
	}
	m[session]++
}

// Denials returns all non-zero denial counters for op using only fixed
// priority and reason values. It never exposes session or account identity.
func (g *Governor) Denials(op string) []DenialSnapshot {
	g.mu.Lock()
	counts := g.denials[op]
	g.mu.Unlock()
	out := make([]DenialSnapshot, 0, denialPriorities*denialReasons)
	for priority := 0; priority < denialPriorities; priority++ {
		for reason := 0; reason < denialReasons; reason++ {
			if counts[priority][reason] == 0 {
				continue
			}
			out = append(out, DenialSnapshot{
				Priority: denialPriorityName(priority),
				Reason:   [...]string{"canceled", "deadline"}[reason],
				Count:    counts[priority][reason],
			})
		}
	}
	return out
}

func denialPriorityIndex(priority Priority) int {
	if priority < PriorityPlayback || priority > PriorityPrewarm {
		return denialPriorities - 1
	}
	return int(priority)
}

func denialPriorityName(index int) string {
	if index == denialPriorities-1 {
		return "unknown"
	}
	return Priority(index).String()
}

func denialReasonIndex(err error) int {
	if err == context.DeadlineExceeded {
		return 1
	}
	return 0
}

// ConnCeiling is an AIMD-learned effective connection-count ceiling for one
// provider connection pool. The operator-configured value is the ceiling's
// static upper bound and starting point. A real conn-cap rejection
// multiplicatively halves the effective ceiling (floored at MinCeiling); a
// sustained rejection-free window afterward additively grows it back by one
// connection at a time toward the configured bound.
//
// Time-dependent behavior uses an injectable clock (SetClock) per the
// program's standing convention so tests remain deterministic.
type ConnCeiling struct {
	mu         sync.Mutex
	configured int
	current    int
	minCeiling int
	growEvery  time.Duration
	lastChange time.Time
	now        func() time.Time
}

// NewConnCeiling creates a ConnCeiling starting at, and bounded above by,
// configured (floored at 1). It grows back toward configured by one
// connection every 30 seconds of rejection-free operation after a shrink.
func NewConnCeiling(configured int) *ConnCeiling {
	if configured < 1 {
		configured = 1
	}
	return &ConnCeiling{
		configured: configured,
		current:    configured,
		minCeiling: 1,
		growEvery:  30 * time.Second,
		now:        time.Now,
	}
}

// SetClock overrides the injectable clock (tests only).
func (c *ConnCeiling) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// SetGrowInterval overrides the additive-increase probe interval (tests
// only; production uses the 30s default).
func (c *ConnCeiling) SetGrowInterval(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d > 0 {
		c.growEvery = d
	}
}

// Configured returns the static operator-configured upper bound.
func (c *ConnCeiling) Configured() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.configured
}

// Current returns the effective connection ceiling, first applying any
// additive-increase probe that has become due.
func (c *ConnCeiling) Current() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maybeGrowLocked()
	return c.current
}

func (c *ConnCeiling) maybeGrowLocked() {
	if c.current >= c.configured {
		return
	}
	now := c.now()
	if c.lastChange.IsZero() {
		c.lastChange = now
		return
	}
	if now.Sub(c.lastChange) >= c.growEvery {
		c.current++
		c.lastChange = now
	}
}

// OnRejected records a real conn-cap rejection: multiplicatively halves the
// effective ceiling (floored at minCeiling) and resets the grow-probe
// window. Never surfaces as a per-segment retry -- callers invoke this once,
// at the pool level, when a dial/auth attempt is classified as a conn-cap
// rejection; ordinary per-segment fetch retries stay unaware of it.
func (c *ConnCeiling) OnRejected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.current / 2
	if next < c.minCeiling {
		next = c.minCeiling
	}
	c.current = next
	c.lastChange = c.now()
}
