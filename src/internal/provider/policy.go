package provider

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Op classifies provider API operations for per-class rate/quota policy.
type Op string

const (
	// OpCreate covers task-creation endpoints (createtorrent /
	// createusenetdownload and equivalents). It remains the legacy/generic
	// create class; providers with cache-aware create budgets should prefer
	// OpCreateCached or OpCreateUncached below.
	OpCreate Op = "create"
	// OpCreateCached covers task creation when the provider was instructed to
	// add only if the release is already cached. For TorBox this skips the
	// uncached 60/hour bucket and uses only create-edge + global budgets.
	OpCreateCached Op = "create_cached"
	// OpCreateUncached covers task creation that may consume an uncached slot.
	// For TorBox this includes create-edge + 60/hour uncached + global budgets.
	OpCreateUncached Op = "create_uncached"
	// OpEdge covers the create-edge burst class (TorBox: 10/min on create
	// endpoints, layered under the 60/hour create budget).
	OpEdge Op = "edge"
	// OpQuery covers status/list/cache queries (checkcached, mylist,
	// requestdl-class pacing).
	OpQuery Op = "query"
	// OpControl covers control actions (delete/stop/etc).
	OpControl Op = "control"
)

// LimitInfo describes one configured client-side limit for introspection.
type LimitInfo struct {
	Op       Op            `json:"op"`
	Capacity int           `json:"capacity"`
	Window   time.Duration `json:"window"`
	Kind     string        `json:"kind"`
}

// Snapshot is a point-in-time view of a Policy for logs/metrics. It carries
// the configured client-side limits and plan pre-check values; it makes no
// claim about server-side state (the upstream service — and in the harness,
// the faithful fake — remains the authority on its own limits).
type Snapshot struct {
	Provider     string           `json:"provider"`
	Limits       []LimitInfo      `json:"limits,omitempty"`
	Holds        map[Op]time.Time `json:"holds,omitempty"`
	PlanSlots    int              `json:"plan_slots,omitempty"`
	PlanMaxBytes int64            `json:"plan_max_bytes,omitempty"`
	Connections  int              `json:"connections,omitempty"`
}

// Policy is a per-provider rate/quota policy (plan §3.1.3). It is the
// call-pacing concern; the store-backed uncached-budget Governor
// (internal/governor) is a separate item-budget concern and is unchanged.
type Policy interface {
	// Acquire blocks until the policy admits one operation of class op:
	// first any Retry-After hold for the class, then the configured
	// client-side limiters. Context cancellation aborts the wait.
	Acquire(ctx context.Context, op Op) error
	// OnResponse feeds an upstream response back into the policy. A 429
	// with a positive Retry-After installs a hold-until deadline for the
	// op class; other statuses are ignored.
	OnResponse(op Op, status int, retryAfter time.Duration)
	// Limits returns an introspection snapshot.
	Limits() Snapshot
}

// BasicPolicy is the standard Policy implementation: per-op Waiter chains
// (TokenBucket mechanism, relocated from torbox/limiter.go unchanged in
// semantics) plus a Retry-After-aware hold-until deadline per op class.
type BasicPolicy struct {
	name         string
	planSlots    int
	planMaxBytes int64

	mu      sync.Mutex
	holds   map[Op]time.Time
	waiters map[Op][]Waiter
	info    []LimitInfo
	owned   []*TokenBucket

	// now/sleep are injectable for tests; defaults are real time.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func NewBasicPolicy(name string) *BasicPolicy {
	return &BasicPolicy{
		name:    name,
		holds:   map[Op]time.Time{},
		waiters: map[Op][]Waiter{},
		now:     time.Now,
		sleep:   sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// AddLimit attaches a Waiter to an op class, recording its description.
// If the waiter is a *TokenBucket created for this policy, also register it
// via Own so Stop() can reap it.
func (p *BasicPolicy) AddLimit(op Op, w Waiter, info LimitInfo) *BasicPolicy {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waiters[op] = append(p.waiters[op], w)
	info.Op = op
	p.info = append(p.info, info)
	return p
}

// Own registers a TokenBucket the policy owns and will Stop(), without
// attaching it to any op chain (attach separately via AddLimit).
func (p *BasicPolicy) Own(b *TokenBucket) *BasicPolicy {
	p.mu.Lock()
	p.owned = append(p.owned, b)
	p.mu.Unlock()
	return p
}

// SetPlan records plan pre-check values (0 = unenforced client-side; the
// upstream service's own enforcement remains the authority).
func (p *BasicPolicy) SetPlan(slots int, maxBytes int64) *BasicPolicy {
	p.planSlots = slots
	p.planMaxBytes = maxBytes
	return p
}

// PlanPreCheck applies the configured plan slot/size pre-checks before a
// submission. Values come from configuration only — no invented numerics;
// zero values skip the check. activeSlots < 0 means "unknown" and skips the
// slot check.
func (p *BasicPolicy) PlanPreCheck(activeSlots int, sizeBytes int64) error {
	if p.planSlots > 0 && activeSlots >= 0 && activeSlots >= p.planSlots {
		return fmt.Errorf("%s policy: plan slot limit reached (%d/%d active)", p.name, activeSlots, p.planSlots)
	}
	if p.planMaxBytes > 0 && sizeBytes > 0 && sizeBytes > p.planMaxBytes {
		return fmt.Errorf("%s policy: size %d exceeds plan max %d bytes", p.name, sizeBytes, p.planMaxBytes)
	}
	return nil
}

// Stop reaps owned token buckets.
func (p *BasicPolicy) Stop() {
	p.mu.Lock()
	owned := p.owned
	p.mu.Unlock()
	for _, b := range owned {
		b.Stop()
	}
}

func (p *BasicPolicy) Acquire(ctx context.Context, op Op) error {
	for {
		p.mu.Lock()
		hold := p.holds[op]
		now := p.now()
		p.mu.Unlock()
		if !hold.After(now) {
			break
		}
		if err := p.sleep(ctx, hold.Sub(now)); err != nil {
			return fmt.Errorf("%s policy: hold wait (%s): %w", p.name, op, err)
		}
	}
	p.mu.Lock()
	chain := append([]Waiter(nil), p.waiters[op]...)
	p.mu.Unlock()
	for _, w := range chain {
		if w == nil {
			continue
		}
		if err := w.Wait(ctx); err != nil {
			return fmt.Errorf("%s policy: limiter wait (%s): %w", p.name, op, err)
		}
	}
	return nil
}

func (p *BasicPolicy) OnResponse(op Op, status int, retryAfter time.Duration) {
	if status != 429 || retryAfter <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	until := p.now().Add(retryAfter)
	if until.After(p.holds[op]) {
		p.holds[op] = until
	}
}

func (p *BasicPolicy) Limits() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	holds := map[Op]time.Time{}
	now := p.now()
	for op, t := range p.holds {
		if t.After(now) {
			holds[op] = t
		}
	}
	return Snapshot{
		Provider:     p.name,
		Limits:       append([]LimitInfo(nil), p.info...),
		Holds:        holds,
		PlanSlots:    p.planSlots,
		PlanMaxBytes: p.planMaxBytes,
	}
}
