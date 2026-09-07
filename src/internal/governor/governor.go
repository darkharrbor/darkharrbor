package governor

import (
	"context"
	"fmt"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// Governor gates uncached submissions.
//
// R2 redesign: when a provider.SlotSource is wired (SetProvider, called once
// the provider is built in main.go), Allow gates on real TorBox headroom —
// AllowedActiveSlots minus live uncached torrent transfers — and honors
// cooldown_until only for Free-tier accounts, recovering continuously instead
// of the old rolling 20-adds/15-day counter. Paid-plan cooldown_until is
// advisory display noise and is re-stamped by account calls, so it must not
// block R2. Free-tier accounts additionally enforce TorBox's published 1/24h
// + 10/month time-windowed budgets on top of the slot check (the 1-active-slot
// piece of the Free contract falls out of the slot check itself, since Free's
// AllowedActiveSlots is 1).
//
// If no provider is wired, or the wired provider doesn't implement
// SlotSource, Allow falls back to the legacy rolling-window budget alone —
// this keeps non-TorBox / test wiring working without a live account.
type Governor struct {
	store *store.Store
	prov  provider.SlotSource

	dailyBudget   int
	dailyWindow   time.Duration
	monthlyBudget int
	monthlyWindow time.Duration
}

// New builds a Governor. uncachedBudget/windowDays are the legacy knobs
// (HARRBOR_GOVERNOR_*); they now serve two roles: (1) the Free-tier monthly
// budget check when a slot-aware provider is wired, and (2) the sole gating
// mechanism in legacy fallback mode (no provider wired).
func New(st *store.Store, uncachedBudget, windowDays int) *Governor {
	return &Governor{
		store:         st,
		dailyBudget:   1,
		dailyWindow:   24 * time.Hour,
		monthlyBudget: uncachedBudget,
		monthlyWindow: time.Duration(windowDays) * 24 * time.Hour,
	}
}

// SetProvider wires the live debrid provider for slot-based gating. Call
// once, after the provider is built (registry.Build in main.go). A provider
// that does not implement provider.SlotSource leaves the Governor in legacy
// rolling-window mode.
func (g *Governor) SetProvider(p provider.Provider) {
	if ss, ok := p.(provider.SlotSource); ok {
		g.prov = ss
		return
	}
	g.prov = nil
}

// Allow returns nil if an uncached add is within budget, or an error
// describing which check failed.
func (g *Governor) Allow(ctx context.Context) error {
	if g.prov == nil {
		return g.allowWithinWindow(ctx, g.monthlyBudget, g.monthlyWindow, "rolling")
	}

	status, err := g.prov.SlotStatus(ctx)
	if err != nil {
		return fmt.Errorf("governor: slot status: %w", err)
	}

	if status.FreeTier && !status.CooldownUntil.IsZero() && time.Now().UTC().Before(status.CooldownUntil) {
		return fmt.Errorf("governor: provider cooldown active until %s", status.CooldownUntil.Format(time.RFC3339))
	}

	headroom := status.AllowedActiveSlots - status.ActiveCount
	if headroom <= 0 {
		return fmt.Errorf("governor: no active slots available (%d/%d in use)", status.ActiveCount, status.AllowedActiveSlots)
	}

	if status.FreeTier {
		if err := g.allowWithinWindow(ctx, g.dailyBudget, g.dailyWindow, "daily"); err != nil {
			return err
		}
		if err := g.allowWithinWindow(ctx, g.monthlyBudget, g.monthlyWindow, "monthly"); err != nil {
			return err
		}
	}

	return nil
}

func (g *Governor) allowWithinWindow(ctx context.Context, budget int, window time.Duration, label string) error {
	count, err := g.store.CountUncachedSince(ctx, time.Now().UTC().Add(-window))
	if err != nil {
		return fmt.Errorf("governor: count uncached (%s): %w", label, err)
	}
	if count >= budget {
		return fmt.Errorf("governor: uncached %s budget exhausted (%d/%d in %v)", label, count, budget, window)
	}
	return nil
}

// Record records one uncached add event for the given item.
func (g *Governor) Record(ctx context.Context, itemID string) error {
	return g.store.RecordUncachedAdd(ctx, itemID)
}
