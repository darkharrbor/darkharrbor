package api

// repair.go — TS-4.1: torrent-lane self-repair hierarchy (frozen plan T5).
//
// On a genuine broken/removed torrent detection (today: a terminal
// stream-time chunk-fetch exhaustion after TS-2.2's rung 4 alternate-
// provider candidates are also exhausted -- see streamViaCache's Stream()
// error path), repair is attempted before falling back to blacklist +
// re-search, per the frozen T5 decision:
//
//   (a) re-add the same infohash to the SAME provider the item is
//       currently bound to (zurg-proven: reinsertion usually re-caches);
//   (b) on failure, walk every OTHER configured provider lane, in
//       providerOrder, and re-add there;
//   (c) on failure of every lane, blacklist the identity (the existing
//       failed_hashes store shared with the NNTP lane's
//       handleAllProvidersDead) and mark the item StateFailed so the
//       arr's own qBit-compatible queue polling blocklists + re-searches
//       it -- exactly the "existing plumbing" the frozen T5 decision
//       names -- plus an optional immediate arr refresh, matching every
//       other state-transition call site's existing convention.
//
// Scope boundary vs TS-2.2 (internal/api/streamaltprovider.go): that row's
// altProviderCandidate confirm/resolve closures are ephemeral, per-fetch,
// and never mutate the persisted *store.Item -- item.Provider is
// deliberately left untouched there (see its own header comment). This
// file is the one place item.Provider is permanently rebound on a real
// repair (step b), and the one place a torrent identity is ever
// permanently blacklisted for repair exhaustion (step c). Every repair
// attempt reuses the existing per-lane submission path
// (ProviderLane.Gate.Check + ProviderLane.Prov.Submit) -- the same
// primitives cmd/darkharrbor's submitViaLane already uses for a fresh
// grab -- rather than a second governor, ladder, or submission code path.
// The existing failed_hashes blacklist (store.BlacklistImmediately) is
// reused rather than a second blacklist store.
//
// Bounded, cooldown-gated, in-memory only (this row carries no DG-02
// migration -- see its master-plan gate list): at most one repair
// attempt runs per infohash at a time, and a completed attempt (success
// or terminal failure) opens a cooldown window before the SAME infohash
// may be repaired again, so a readahead storm of concurrent chunk-fetch
// failures for one broken item can never trigger a pile of concurrent or
// repeated repair/blacklist attempts. Per-identity attempt counters are
// exposed via repairState.AttemptCount for observability/WORKLOG use.
import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// ArrRefreshNotifier is satisfied by cmd/darkharrbor's readyNotifier.
// Nil-safe: an unset notifier simply skips the immediate-refresh
// convenience — the arr's own periodic queue poll still catches the
// persisted StateFailed and blocklists/re-searches on its own schedule.
type ArrRefreshNotifier interface {
	Notify()
}

// SetArrRefreshNotifier wires the optional immediate arr-refresh
// convenience used after a repair chain's terminal blacklist step.
// Mirrors SetNNTPExperience's post-construction setter convention.
func (s *Server) SetArrRefreshNotifier(rn ArrRefreshNotifier) {
	s.arrRefresh = rn
}

// repairState is this row's own new, in-memory, per-identity bookkeeping.
// No schema/migration is introduced (TS-4.1 names no DG-02 gate): this
// survives only for the process lifetime, matching the NS-1.1 negative-
// cache precedent for per-process-only accounting.
type repairState struct {
	mu       sync.Mutex
	inFlight map[string]bool      // infohash -> a repair goroutine is running now
	cooldown map[string]time.Time // infohash -> earliest time a new repair may start
	attempts map[string]int       // infohash -> lifetime repair-attempt counter (observability only)
}

func newRepairState() *repairState {
	return &repairState{
		inFlight: make(map[string]bool),
		cooldown: make(map[string]time.Time),
		attempts: make(map[string]int),
	}
}

// tryClaim reports whether a new repair attempt may start now for hash,
// atomically claiming it (inFlight=true) if so. Refuses when a repair is
// already running for this identity, or the cooldown from the last
// attempt has not yet elapsed.
func (rs *repairState) tryClaim(hash string, now time.Time) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.inFlight[hash] {
		return false
	}
	if until, ok := rs.cooldown[hash]; ok && now.Before(until) {
		return false
	}
	rs.inFlight[hash] = true
	rs.attempts[hash]++
	return true
}

// release ends the in-flight claim and opens a fresh cooldown window.
func (rs *repairState) release(hash string, now time.Time, cooldown time.Duration) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	delete(rs.inFlight, hash)
	if cooldown > 0 {
		rs.cooldown[hash] = now.Add(cooldown)
	}
}

// AttemptCount returns the lifetime repair-attempt counter for hash (0 if
// never attempted). Exposed for WORKLOG/health-surface use.
func (rs *repairState) AttemptCount(hash string) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.attempts[hash]
}

// torrentRepairIdentity returns the canonical repair identity for item —
// the same RealInfoHash-then-InfoHash convention TS-0.3's resolver and
// TS-0.2's backfill already established — or "" when neither is known
// (never guessed).
func torrentRepairIdentity(item *store.Item) string {
	if item == nil {
		return ""
	}
	if h := strings.ToLower(strings.TrimSpace(item.Metadata.RealInfoHash)); h != "" {
		return h
	}
	if item.InfoHash != nil {
		if h := strings.ToLower(strings.TrimSpace(*item.InfoHash)); h != "" {
			return h
		}
	}
	return ""
}

// laneFor returns the configured ProviderLane whose provider name matches
// name, if any.
func (s *Server) laneFor(name string) (ProviderLane, bool) {
	for _, l := range s.lanes {
		if l.Prov != nil && l.Prov.Name() == name {
			return l, true
		}
	}
	return ProviderLane{}, false
}

// TriggerRepairAsync is the stream-time entry point (T5: "on broken/
// removed detection ... stream-time"). Cheap and non-blocking: the
// cooldown/in-flight check happens synchronously so a storm of concurrent
// chunk-fetch failures for the same item spawns at most one repair
// goroutine; every other call is a fast no-op map lookup. ctx is
// deliberately NOT the request context — the HTTP response for the failed
// fetch is already committed/closing by the time this fires — so repair
// runs on a bounded background context derived from the server's own
// shutdown context instead (DG-07: cancellable on shutdown, but never tied
// to a client that has already gone away).
func (s *Server) TriggerRepairAsync(item *store.Item) {
	// Always emit at Info, unconditionally, so whether this was ever even
	// invoked -- and why it did or didn't proceed -- is never ambiguous
	// from operational logs alone (HARRBOR_LOG_LEVEL=INFO is the normal
	// deployed level; Debug-only breadcrumbs here previously made a
	// no-op-vs-never-called distinction impossible to observe live).
	if item == nil {
		s.log.Info("repair: trigger call ignored (nil item)", "event", "repair_trigger_ignored")
		return
	}
	itemIDForLog := item.ID
	if item.SourceType != store.SourceTypeTorrent {
		s.log.Info("repair: trigger ignored (not torrent lane)",
			"event", "repair_trigger_ignored", "item_id", itemIDForLog, "source_type", item.SourceType)
		return
	}
	if !s.cfg.Governor.RepairEnabled {
		s.log.Info("repair: trigger ignored (disabled)",
			"event", "repair_trigger_ignored", "item_id", itemIDForLog)
		return
	}
	if s.repairState == nil {
		s.log.Info("repair: trigger ignored (no repair state configured)",
			"event", "repair_trigger_ignored", "item_id", itemIDForLog)
		return
	}
	hash := torrentRepairIdentity(item)
	if hash == "" {
		s.log.Info("repair: trigger ignored (no infohash known)",
			"event", "repair_trigger_ignored", "item_id", itemIDForLog)
		return
	}
	now := s.nowUTC()
	if !s.repairState.tryClaim(hash, now) {
		s.log.Info("repair: skipped (in-flight or cooldown)",
			"event", "repair_skipped",
			"item_id", item.ID,
			"info_hash", hash,
		)
		return
	}
	s.log.Info("repair: trigger claimed, starting chain",
		"event", "repair_trigger_claimed",
		"item_id", item.ID,
		"info_hash", hash,
	)
	cooldown := time.Duration(s.cfg.Governor.RepairCooldownMin) * time.Minute
	itemID := item.ID

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.repairState.release(hash, s.nowUTC(), cooldown)
		ctx, cancel := context.WithTimeout(s.shutdownCtx, 2*time.Minute)
		defer cancel()
		s.runRepairChain(ctx, itemID, hash)
	}()
}

// runRepairChain executes T5's bounded (a)->(b)->(c) walk for one repair
// attempt. It re-fetches the item fresh from the store rather than trusting
// the possibly-stale copy captured at trigger time, since an item may have
// moved on (already repaired, removed by the user, failed via an unrelated
// path) between the stream-time failure and this goroutine actually
// running.
func (s *Server) runRepairChain(ctx context.Context, itemID, hash string) {
	llog := s.log.With(
		"item_id", itemID,
		"info_hash", hash,
		"event_group", "ts4_1_repair",
	)

	fresh, err := s.store.GetItemByID(ctx, itemID)
	if err != nil || fresh == nil {
		llog.Warn("repair: item lookup failed or item gone (non-fatal, abstaining)", "error", err)
		return
	}
	if fresh.State != store.StateReady {
		// Already moved on (failed/removed/resolving again) — repairing a
		// non-Ready item risks racing whatever else is handling it.
		llog.Info("repair: item no longer Ready, abstaining", "state", fresh.State)
		return
	}

	primaryName := ""
	if fresh.Provider != nil {
		primaryName = *fresh.Provider
	}

	// Step (a): re-add to the SAME provider.
	if primaryName != "" {
		if lane, ok := s.laneFor(primaryName); ok {
			if s.repairViaLane(ctx, llog, lane, fresh) {
				llog.Warn("repair: healed via same-provider re-add",
					"event", "repair_healed_same_provider",
					"provider", primaryName)
				return
			}
		}
	}

	// Step (b): walk every OTHER configured lane, in providerOrder.
	for _, name := range s.providerOrder {
		if name == primaryName {
			continue
		}
		lane, ok := s.laneFor(name)
		if !ok {
			continue
		}
		if s.repairViaLane(ctx, llog, lane, fresh) {
			llog.Warn("repair: healed via alternate-provider re-add",
				"event", "repair_healed_alt_provider",
				"provider", name)
			return
		}
	}

	// Step (c): repair exhausted on every configured lane.
	s.blacklistAndFail(ctx, llog, fresh, hash)
}

// repairViaLane performs one bounded re-add attempt (T5 steps a/b) through
// lane's existing gate + provider submit path — the same primitives a
// fresh grab already uses (cmd/darkharrbor's submitViaLane), so no second
// submission code path is introduced. On success, persists the item's new
// binding (item.Provider, RemoteID/QueuedID) and returns it to
// StateResolving so the existing resolve cycle polls it to Ready/Failed
// exactly like a fresh grab.
func (s *Server) repairViaLane(ctx context.Context, llog *slog.Logger, lane ProviderLane, item *store.Item) bool {
	if lane.Gate == nil || lane.Prov == nil {
		return false
	}
	name := lane.Prov.Name()
	result, gateErr := lane.Gate.Check(ctx, item)
	if gateErr != nil {
		llog.Debug("repair: lane gate rejected", "provider", name, "error", gateErr)
		return false
	}
	resp, err := lane.Prov.Submit(ctx, item, provider.SubmitOptions{AddOnlyIfCached: result.Cached})
	if err != nil {
		llog.Debug("repair: lane submit failed", "provider", name, "error", err)
		return false
	}
	if resp == nil {
		return false
	}

	item.Provider = &name
	if resp.RemoteID != "" {
		item.RemoteID = &resp.RemoteID
	}
	if resp.QueuedID != "" {
		item.QueuedID = &resp.QueuedID
	}
	if resp.RemoteHash != "" {
		item.InfoHash = &resp.RemoteHash
	}

	if err := s.store.UpdateItemState(ctx, item, store.StateResolving, "ts4_1_repair: re-added to "+name); err != nil {
		llog.Error("repair: persist re-add failed", "provider", name, "error", err)
		return false
	}
	return true
}

// blacklistAndFail is T5 step (c): the identity is blacklisted (the
// existing failed_hashes store shared with the NNTP lane's
// handleAllProvidersDead in cmd/darkharrbor/main.go) and the item is
// marked StateFailed so the arr's own qBit-compatible queue polling
// blocklists and re-searches it. An optional immediate arr refresh is
// fired afterward, matching every other state-transition call site's
// existing convention (never a correctness dependency — an un-notified
// arr still catches up on its own next poll).
func (s *Server) blacklistAndFail(ctx context.Context, llog *slog.Logger, item *store.Item, hash string) {
	result, err := s.store.BlacklistImmediately(ctx, hash)
	if err != nil {
		llog.Error("repair: blacklist persist failed",
			"event", "item_failure_persist_failed",
			"error", err,
		)
	} else {
		llog.Warn("repair: identity blacklisted after repair exhaustion",
			"event", "source_blacklisted",
			"fail_count", result.FailCount,
			"blacklisted", result.Blacklisted,
			"blacklisted_until", result.BlacklistedUntil,
		)
	}

	const msg = "ts4_1_repair_exhausted: same-provider re-add and every alternate-provider re-add failed"
	if err := s.store.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
		llog.Error("repair: persist failed state",
			"event", "item_failure_persist_failed",
			"error", err,
		)
		return
	}
	llog.Warn("repair: item marked failed after repair exhaustion",
		"event", "item_state_transition",
		"previous_state", store.StateReady,
		"state", store.StateFailed,
	)
	if s.arrRefresh != nil {
		s.arrRefresh.Notify()
	}
}
