// keepwarm.go — TS-4.2 (frozen plan T8a): "Re-add recently played items
// before provider expiry without preempting demand; Ready items expose a
// derived provider-expiry horizon."
//
// Debrid-provider CDN caches are not retained forever: TorBox's cache
// retention for content nobody re-touches is a disclosed ~30-day class
// (frozen TORRENT-STREAM-MASTER-PLAN.md T8 rationale: "30-day TorBox
// class"). DarkHarrbor has no live query for a provider's actual per-item
// expiry -- TS-0.2 already found TorBox's own exportdata metainfo endpoint
// returns a bare null for magnet-added items in this account/plan -- so the
// horizon this row exposes is a DERIVED estimate: item.CreatedAt plus an
// assumed, hardcoded per-provider retention constant. An item bound to a
// provider with no known assumption abstains entirely (never guessed):
// today that means only TorBox items are ever considered; Real-Debrid, with
// zero Ready items in this deployment and no disclosed expiry class, is
// left out rather than guessed at.
//
// This row shares TS-4.1's own no-DG-02 gate list (no migration, no new
// persisted field): the horizon is computed fresh on every read from
// item.CreatedAt, never persisted.
//
// Re-add reuses TS-4.1's own same-provider re-add primitive
// (Server.repairViaLane) verbatim -- no second submission code path, per
// WORKFLOW.md's "reuse the shared owner" rule. Keep-warm deliberately does
// NOT walk into TS-4.1's alternate-provider or blacklist steps on failure:
// an item that is not yet actually broken should never be blacklisted
// merely because one opportunistic, preventive re-add attempt failed. A
// failed keep-warm attempt is logged and abstained; the item is picked up
// again on a future eligible tick, or, if it later genuinely breaks,
// TS-4.1's own stream-time repair trigger handles it through the full
// hierarchy. Re-add here also never touches TorrentCDNGovOp/HTTPSourceGovOp
// (no CDN byte fetch is involved, only the provider's add-torrent API, the
// same ungoverned primitive TS-4.1's repair already uses) -- this is how
// this row avoids preempting live playback demand, per its own name.
package api

import (
	"context"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

// assumedProviderExpiryDays is the disclosed, hardcoded, per-provider
// content-cache retention assumption used to derive a provider-expiry
// horizon. Deliberately NOT provider-API-derived (no live oracle exists,
// per TS-0.2's own disclosed finding) and deliberately NOT a new exported
// per-provider config surface: the frozen plan names only one knob
// (HARRBOR_TORRENT_KEEPWARM_DAYS, the "recent play" window). An unlisted
// provider abstains (ProviderExpiryHorizon returns ok=false), never
// guessing a retention window nobody has confirmed.
var assumedProviderExpiryDays = map[string]int{
	// TorBox's cache retention for untouched content is a disclosed
	// ~30-day class (frozen plan T8 rationale: "30-day TorBox class").
	"torbox": 30,
}

// keepWarmLeadDays is the fixed, non-configurable safety buffer before the
// derived expiry horizon at which a re-add becomes due. Kept generous (5
// days inside a 30-day assumed TorBox horizon) since the janitor cadence,
// the per-tick candidate bound, and the underlying retention assumption
// itself are all approximations; re-adding a few days early is inexpensive
// (one provider API call) against the cost of guessing wrong and letting a
// favorite go cold. Not a separate config knob because the frozen plan
// names only one (HARRBOR_TORRENT_KEEPWARM_DAYS).
const keepWarmLeadDays = 5

// keepWarmCandidatesPerTick bounds how many candidates one cycle inspects,
// mirroring NS-6.1's rate-capped per-tick design so a large library can
// never turn keep-warm into a re-add storm.
const keepWarmCandidatesPerTick = 3

// ProviderExpiryHorizon derives item's estimated provider-side cache
// expiry from its original grab time plus the assumed per-provider
// retention window. ok is false when item is nil, its provider is unknown/
// unbound, that provider has no assumed retention window, or its CreatedAt
// is zero -- never guessed.
func ProviderExpiryHorizon(item *store.Item) (horizon time.Time, providerName string, days int, ok bool) {
	if item == nil || item.Provider == nil || *item.Provider == "" {
		return time.Time{}, "", 0, false
	}
	name := *item.Provider
	d, known := assumedProviderExpiryDays[name]
	if !known {
		return time.Time{}, name, 0, false
	}
	if item.CreatedAt.IsZero() {
		return time.Time{}, name, 0, false
	}
	return item.CreatedAt.AddDate(0, 0, d), name, d, true
}

// RunKeepWarmCycle runs one bounded TS-4.2 pass: select up to
// keepWarmCandidatesPerTick recently-played Ready torrent items (oldest
// grab first, a natural closest-to-expiry-first ordering under the fixed
// per-provider retention assumption), and same-provider re-add any whose
// derived expiry horizon falls within keepWarmLeadDays. A no-op when days
// <= 0 (config's documented "0 = off") or the store is unavailable.
func (s *Server) RunKeepWarmCycle(ctx context.Context, days int) {
	if days <= 0 || s.store == nil {
		return
	}
	now := s.nowUTC()
	playedSince := now.AddDate(0, 0, -days)
	candidates, err := s.store.ListKeepWarmCandidates(ctx, playedSince, keepWarmCandidatesPerTick)
	if err != nil {
		s.log.Warn("keepwarm: candidate selection failed", "error", err, "event", "keepwarm_select_failed")
		return
	}
	for _, item := range candidates {
		s.maybeKeepWarmReAdd(ctx, item, now)
	}
}

// maybeKeepWarmReAdd computes item's derived provider-expiry horizon and,
// if it falls within keepWarmLeadDays of now, attempts one same-provider
// re-add via the shared TS-4.1 repairViaLane primitive. Abstains (no
// action, no error) on an unknown provider assumption or an unconfigured
// provider lane.
func (s *Server) maybeKeepWarmReAdd(ctx context.Context, item *store.Item, now time.Time) {
	llog := s.log.With("item_id", item.ID, "event_group", "ts4_2_keepwarm")

	horizon, providerName, assumedDays, ok := ProviderExpiryHorizon(item)
	if !ok {
		llog.Debug("keepwarm: abstaining (no assumed provider retention known)", "provider", providerName)
		return
	}
	if horizon.Sub(now) > keepWarmLeadDays*24*time.Hour {
		// Not near expiry yet -- nothing to do this tick.
		return
	}
	if horizon.Before(now) {
		// Already past the derived horizon -- the estimate itself may be
		// stale (e.g. some other real event already re-cached it, which
		// this row doesn't track), but attempting the re-add regardless is
		// still the safe, cheap action: worst case it's a no-op reinsert.
		llog.Info("keepwarm: item already past derived horizon, attempting re-add anyway",
			"event", "keepwarm_past_horizon", "provider", providerName, "assumed_days", assumedDays)
	}

	lane, ok := s.laneFor(providerName)
	if !ok {
		llog.Debug("keepwarm: abstaining (provider lane not configured)", "provider", providerName)
		return
	}

	llog.Info("keepwarm: re-add due, attempting",
		"event", "keepwarm_readd_attempt",
		"provider", providerName,
		"assumed_expiry_days", assumedDays,
		"derived_horizon", horizon.Format(time.RFC3339),
	)

	if s.repairViaLane(ctx, llog, lane, item) {
		llog.Info("keepwarm: re-add succeeded", "event", "keepwarm_readd_succeeded", "provider", providerName)
		return
	}
	llog.Warn("keepwarm: re-add attempt failed, will retry on a future eligible tick",
		"event", "keepwarm_readd_failed", "provider", providerName)
}
