// metrics.go implements OBS-01: the Prometheus /metrics endpoint plus the
// per-item error-journal HTTP exposure ("via API/health"). Every value
// rendered is read from an already-existing, already-bounded shared owner
// at request time -- see internal/metrics's own package doc for why no new
// instrumentation is added here. The write side (appending journal
// entries) lives in cmd/darkharrbor/main.go, reusing NS-1.3's existing
// FinalRungReporter chokepoint rather than a new call site here.
package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/metrics"
)

// handleMetrics renders the current Prometheus snapshot. Bounded by
// construction: every underlying query is a fixed aggregate over the items
// table (GROUP BY / SUM / json_each), never a per-item scan in Go, so
// scrape cost stays flat as the library grows in the same way TS-0.5's
// consistency sweep and NS-6.1's audit selection already do.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap := metrics.Snapshot{
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
	}

	ctx := r.Context()
	if s.store != nil {
		if counts, err := s.store.ItemStateCounts(ctx); err == nil {
			snap.ItemsByState = make(map[string]int64, len(counts))
			for state, n := range counts {
				snap.ItemsByState[string(state)] = n
			}
		} else if s.log != nil {
			s.log.Warn("metrics: item state counts failed (non-fatal)", "error", err)
		}
		if total, err := s.store.NNTPZeroFillTotal(ctx); err == nil {
			snap.NNTPZeroFillTotal = total
		} else if s.log != nil {
			s.log.Warn("metrics: nntp zero-fill total failed (non-fatal)", "error", err)
		}
		if agg, err := s.store.ErrorJournalAggregateStats(ctx); err == nil {
			snap.ErrorJournalEntriesTotal = agg.Total
			snap.ErrorJournalByClass = agg.ByClass
		} else if s.log != nil {
			s.log.Warn("metrics: error journal aggregate failed (non-fatal)", "error", err)
		}
		if reactive, err := s.store.CurrentReactiveMetrics(ctx); err == nil {
			snap.ReactiveCommits = reactive.CommitsByPeriod
			snap.ReactivePending = reactive.PendingByReason
			snap.ReactivePendingAge = reactive.OldestByReason
			snap.ReactiveIdentity = reactive.Identity
			snap.ReactiveUndoTotal = reactive.UndoTotal
			snap.ReactivePromotionBytes = reactive.PromotionBytes
			snap.ReactiveHeadroomBytes = reactive.HeadroomBytes
			snap.ReactiveHeadroomKnown = reactive.HeadroomKnown
		} else if s.log != nil {
			s.log.Warn("metrics: reactive aggregate failed (non-fatal)", "error", err)
		}
		if pb, err := s.store.CurrentPlaybackRepresentationMetrics(ctx, time.Now()); err == nil {
			snap.PlaybackRepresentations = pb.ByClass
			snap.AggregatorDeliveryKnown = pb.AggregatorKnown
			snap.AggregatorDeliveryAgeSeconds = int64(pb.AggregatorAge.Seconds())
		} else if s.log != nil {
			s.log.Warn("metrics: playback representation aggregate failed (non-fatal)", "error", err)
		}
	}

	if s.streamCache != nil {
		snap.RangeCacheEntries = numericInt64Fields(s.streamCache.SnapshotMap())
	}
	if s.httpOriginScores != nil {
		snap.HTTPOriginScoreEntries = int64(s.httpOriginScores.Len())
		for _, source := range s.httpOriginScores.Snapshot() {
			snap.SourceHealth = append(snap.SourceHealth, metrics.SourceHealth{
				Lane: "http", Source: source.Source, Health: source.Health,
				RetryStage: source.RetryStage, Score: source.Score,
				FirstByteMillis:   source.FirstByteMillis,
				TokenLifetimeSecs: source.TokenLifetimeSecs,
				ExpiryCount:       source.ExpiryObservations,
				Observations:      source.Observations,
			})
		}
	}
	if s.torrentCDNScores != nil {
		snap.TorrentCDNScoreEntries = int64(s.torrentCDNScores.Len())
		for _, source := range s.torrentCDNScores.Snapshot() {
			snap.SourceHealth = append(snap.SourceHealth, metrics.SourceHealth{
				Lane: "torrent", Source: source.Source, Health: source.Health,
				RetryStage: source.RetryStage, Score: source.Score,
				FirstByteMillis:   source.FirstByteMillis,
				TokenLifetimeSecs: source.TokenLifetimeSecs,
				ExpiryCount:       source.ExpiryObservations,
				Observations:      source.Observations,
			})
		}
	}
	if s.torrentGov != nil {
		for _, denial := range s.torrentGov.Denials(TorrentCDNGovOp) {
			snap.GovernorDenials = append(snap.GovernorDenials, metrics.GovernorDenial{
				Lane: "torrent", Priority: denial.Priority, Reason: denial.Reason, Count: denial.Count,
			})
		}
	}
	if s.httpGov != nil {
		for _, denial := range s.httpGov.Denials(HTTPSourceGovOp) {
			snap.GovernorDenials = append(snap.GovernorDenials, metrics.GovernorDenial{
				Lane: "http", Priority: denial.Priority, Reason: denial.Reason, Count: denial.Count,
			})
		}
	}
	if warm := s.nntpWarmPoolSnapshot(); len(warm) > 0 {
		snap.NNTPWarmOpen = make(map[string]int64, len(warm))
		snap.NNTPWarmTarget = make(map[string]int64, len(warm))
		snap.NNTPColdAcquiresTotal = make(map[string]int64, len(warm))
		snap.NNTPWarmAcquiresTotal = make(map[string]int64, len(warm))
		for name, fields := range warm {
			snap.NNTPWarmOpen[name] = fields["open"]
			snap.NNTPWarmTarget[name] = fields["target"]
			snap.NNTPColdAcquiresTotal[name] = fields["cold"]
			snap.NNTPWarmAcquiresTotal[name] = fields["warm"]
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := metrics.Render(w, snap); err != nil && s.log != nil {
		s.log.Warn("metrics: render failed", "error", err)
	}
}

// numericInt64Fields extracts only the integer-shaped values from an
// already-secret-free snapshot map (rangecache.SnapshotMap's own contract
// guarantees no URL/secret ever appears there -- this just filters to the
// subset a gauge can render, dropping strings/bools/nested values rather
// than guessing a numeric coercion for them).
func numericInt64Fields(m map[string]any) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		switch n := v.(type) {
		case int64:
			out[k] = n
		case int:
			out[k] = int64(n)
		case float64:
			out[k] = int64(n)
		}
	}
	return out
}

// handleErrorJournal exposes OBS-01's bounded per-item persisted SF-01
// event ring for a single item, oldest-first. Read-only; never mutates
// state. A missing/unknown item_id path segment or nonexistent item
// abstains with 400/404 respectively rather than guessing.
func (s *Server) handleErrorJournal(w http.ResponseWriter, r *http.Request) {
	itemID := strings.TrimSpace(r.PathValue("item_id"))
	if itemID == "" {
		http.Error(w, "item_id is required", http.StatusBadRequest)
		return
	}
	if s.store == nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	entries, err := s.store.GetErrorJournal(r.Context(), itemID)
	if err != nil {
		http.Error(w, "item not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"item_id": itemID,
		"entries": entries,
	})
}
