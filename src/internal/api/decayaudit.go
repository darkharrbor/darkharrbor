// decayaudit.go — TS-4.3 (frozen plan T8b): "Rate-capped Ready-item
// remote-presence decay audit feeding repair."
//
// The torrent-lane mirror of NS-6.1/NS-6.2: a slow-cadence, rate-capped
// background pass samples already-Ready torrent items' remote presence at
// their bound provider and, on a conclusive "no longer cached" verdict,
// hands the item to TS-4.1's existing repair chain (TriggerRepairAsync) --
// exactly the frozen plan's "removal ⇒ TS-4.1" T8b decision. This row
// measures and triggers; it never re-adds, blacklists, or fails an item
// directly -- that remains TS-4.1's own owned hierarchy, reused verbatim.
//
// Remote-presence check: this row reuses each provider's own pre-submit
// cache-oracle (provider.Capabilities().CacheOracle, e.g. TorBox's
// checkcached) via the SAME Provider.CheckCached primitive TS-2.2's
// alternate-provider confirm (streamaltprovider.go) and cachegate's own
// pre-submit check already use -- no second presence-check mechanism. This
// is a genuine live query against the provider's real remote cache state
// (not a CDN byte fetch), so it never touches TorrentCDNGovOp/the streaming
// governor and needs no accountgov lease, mirroring TS-4.2/NS-5.x's own
// no-lease precedent for non-CDN provider-API calls. A provider with no
// cache oracle (e.g. Real-Debrid) abstains for that item -- never guessed
// -- exactly like TS-4.2's own per-provider abstain convention for an
// unknown retention assumption. An item with no recoverable infohash
// (torrentRepairIdentity, TS-4.1's own convention) also abstains.
//
// Rotation: this row lists no DG-02 (no migration, no persisted
// last-audited timestamp -- unlike NS-6.1's DB-backed NextHealthAuditItem),
// so full-library coverage is achieved by an in-memory id-keyset cursor
// (store.ListReadyTorrentItemsAfter) held for the process lifetime,
// wrapping to the beginning whenever a page comes back short of the
// per-tick bound -- the same "id > cursor, ascending, bounded" shape
// ListReadyHTTPItems/ListReadyNNTPItems already use elsewhere, applied here
// as a rotation instead of a one-shot list. Not persisted: a restart simply
// restarts the rotation from the beginning, which is harmless (eventual
// full coverage still holds; no item is ever skipped forever).
package api

import (
	"context"
	"sync"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

// decayAuditCandidatesPerTick bounds how many items one cycle inspects,
// mirroring TS-4.2's keepWarmCandidatesPerTick rate-cap shape so a large
// library can never turn the audit into a provider-API-call storm.
const decayAuditCandidatesPerTick = 5

// decayAuditState holds the in-memory rotation cursor across ticks. No
// persisted state, per this row's own no-DG-02 gate list.
type decayAuditState struct {
	mu     sync.Mutex
	cursor string
}

func newDecayAuditState() *decayAuditState {
	return &decayAuditState{}
}

// DecayAuditCycleResult summarizes one RunDecayAuditCycle pass for
// WORKLOG/health-surface use.
type DecayAuditCycleResult struct {
	Sampled   int      `json:"sampled"`
	Cached    int      `json:"cached"`
	Decayed   []string `json:"decayed,omitempty"`
	Abstained int      `json:"abstained"`
}

// RunDecayAuditCycle runs one bounded TS-4.3 pass: select up to
// decayAuditCandidatesPerTick Ready torrent items from the rotation
// cursor (wrapping to the start when the page comes back short), sample
// each one's remote presence via its bound provider's cache oracle, and
// trigger TS-4.1 repair on a conclusive miss. A no-op when the server or
// store is unavailable.
func (s *Server) RunDecayAuditCycle(ctx context.Context) DecayAuditCycleResult {
	var result DecayAuditCycleResult
	if s == nil || s.store == nil || s.decayAuditState == nil {
		return result
	}

	s.decayAuditState.mu.Lock()
	cursor := s.decayAuditState.cursor
	s.decayAuditState.mu.Unlock()

	items, err := s.store.ListReadyTorrentItemsAfter(ctx, cursor, decayAuditCandidatesPerTick)
	if err != nil {
		s.log.Warn("decayaudit: candidate selection failed", "error", err, "event", "decayaudit_select_failed")
		return result
	}
	if len(items) == 0 && cursor != "" {
		// Reached the end of the rotation: wrap and try once more from the
		// beginning so a tick right at the boundary is not wasted.
		s.decayAuditState.mu.Lock()
		s.decayAuditState.cursor = ""
		s.decayAuditState.mu.Unlock()
		items, err = s.store.ListReadyTorrentItemsAfter(ctx, "", decayAuditCandidatesPerTick)
		if err != nil {
			s.log.Warn("decayaudit: candidate selection failed after wrap", "error", err, "event", "decayaudit_select_failed")
			return result
		}
	}
	if len(items) == 0 {
		return result
	}

	lastID := cursor
	for _, item := range items {
		lastID = item.ID
		s.sampleDecayAuditItem(ctx, item, &result)
	}

	s.decayAuditState.mu.Lock()
	if len(items) < decayAuditCandidatesPerTick {
		// Short page: this tick reached (or passed) the end of the
		// library. Wrap the cursor now so the next tick restarts from the
		// beginning rather than idling on an empty page forever.
		s.decayAuditState.cursor = ""
	} else {
		s.decayAuditState.cursor = lastID
	}
	s.decayAuditState.mu.Unlock()

	// Always emit at Info, unconditionally, for the same reason TS-4.1's
	// own trigger logging was fixed from Debug to Info: a tick that found
	// nothing wrong (the overwhelmingly common case) must still be
	// observable at the normal deployed log level, or "did this ever even
	// run, and what did it find" becomes unanswerable from operational
	// logs alone.
	s.log.Info("decayaudit: cycle complete",
		"event", "decayaudit_cycle_complete",
		"sampled", result.Sampled,
		"cached", result.Cached,
		"decayed", len(result.Decayed),
		"abstained", result.Abstained,
	)

	return result
}

// sampleDecayAuditItem performs one bounded remote-presence check for item
// and, on a conclusive miss, hands it to TS-4.1's existing repair chain.
// Abstains (no action, result.Abstained++, no error) when no live
// presence check is possible -- an unknown provider capability, a failed
// call, or no recoverable infohash are none of them evidence of decay.
func (s *Server) sampleDecayAuditItem(ctx context.Context, item *store.Item, result *DecayAuditCycleResult) {
	if item == nil {
		return
	}
	llog := s.log.With("item_id", item.ID, "event_group", "ts4_3_decayaudit")

	prov := s.pickDebridProvider(item)
	if prov == nil || !prov.Capabilities().CacheOracle {
		result.Abstained++
		llog.Debug("decayaudit: abstaining (no cache oracle for bound provider)")
		return
	}
	if torrentRepairIdentity(item) == "" {
		result.Abstained++
		llog.Debug("decayaudit: abstaining (no infohash known)")
		return
	}

	res, err := prov.CheckCached(ctx, item)
	if err != nil {
		result.Abstained++
		llog.Debug("decayaudit: check-cached call failed, abstaining (not evidence of decay)", "error", err)
		return
	}
	result.Sampled++
	if res != nil && res.Cached {
		result.Cached++
		return
	}

	llog.Warn("decayaudit: item no longer cached at bound provider, triggering repair",
		"event", "decayaudit_decayed",
		"provider", prov.Name(),
	)
	result.Decayed = append(result.Decayed, item.ID)
	s.TriggerRepairAsync(item)
}
