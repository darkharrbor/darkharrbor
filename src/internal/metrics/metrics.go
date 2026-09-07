// Package metrics implements OBS-01's Prometheus text-exposition renderer.
//
// It deliberately owns no instrumentation of its own: every value in a
// Snapshot is read from an already-existing, already-bounded shared owner
// (store aggregates, rangecache's existing SnapshotMap, the existing
// origin/CDN score entry counts already exposed at /healthz) at scrape
// time. This keeps the metrics surface additive and prevents a second
// counting/observability vocabulary from growing alongside SF-01's own
// (WORKFLOW rule 3: no second failure vocabulary, cache, or observability
// owner).
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// SourceHealth is one hashed, bounded passive origin/CDN diagnostic.
type SourceHealth struct {
	Lane              string
	Source            string
	Health            string
	RetryStage        string
	Score             int64
	FirstByteMillis   int64
	TokenLifetimeSecs int64
	ExpiryCount       uint64
	Observations      uint64
}

// GovernorDenial is one fixed lane/priority/reason cancellation count.
type GovernorDenial struct {
	Lane     string
	Priority string
	Reason   string
	Count    uint64
}

// Snapshot is the bounded set of point-in-time values OBS-01 exposes at
// GET /metrics. Every field is optional/zero-value-safe: a nil map or zero
// count renders as an absent or zero series rather than an error, since a
// scrape must never fail because one shared owner had nothing to report.
type Snapshot struct {
	// UptimeSeconds is the process uptime.
	UptimeSeconds int64
	// ItemsByState is the current item count per ItemState string value
	// (e.g. "ready", "failed", "resolving", "accepted", "removed").
	ItemsByState map[string]int64
	// NNTPZeroFillTotal is NS-1.3's cumulative final-rung-exhaustion count,
	// summed across all items.
	NNTPZeroFillTotal int64
	// ErrorJournalEntriesTotal is OBS-01's own persisted error-journal ring
	// total across all items.
	ErrorJournalEntriesTotal int64
	// ErrorJournalByClass is the same total broken out by outcome.Class
	// string value (e.g. "permanent_here", "transient_here",
	// "account_level", "unknown").
	ErrorJournalByClass map[string]int64
	// RangeCacheEntries mirrors the existing /healthz "rangecache" snapshot
	// numeric fields (already bounded, already secret-free by construction
	// per rangecache.SnapshotMap's own contract).
	RangeCacheEntries map[string]int64
	// HTTPOriginScoreEntries mirrors the existing /healthz
	// "http_origin_score_entries" bounded aggregate count.
	HTTPOriginScoreEntries int64
	// TorrentCDNScoreEntries mirrors the existing /healthz
	// "torrent_cdn_score_entries" bounded aggregate count.
	TorrentCDNScoreEntries int64
	// SourceHealth contains at most the two existing scorers' fixed 512-entry
	// caps. Source values are short digests; no URL or upstream identity.
	SourceHealth []SourceHealth
	// GovernorDenials uses fixed lane, priority, and reason vocabularies.
	GovernorDenials []GovernorDenial
	// NNTPWarmOpen is NS-5.1's current warm-pool connection count per
	// configured usenet provider name, mirrored from /healthz
	// "nntp_warm_pool".
	NNTPWarmOpen map[string]int64
	// NNTPWarmTarget is NS-5.1's configured warm-pool target per provider.
	NNTPWarmTarget map[string]int64
	// NNTPColdAcquiresTotal is NS-5.1's cumulative cold (fresh-dial)
	// Acquire count per provider's demand pool.
	NNTPColdAcquiresTotal map[string]int64
	// NNTPWarmAcquiresTotal is NS-5.1's cumulative warm (reused-connection)
	// Acquire count per provider's demand pool.
	NNTPWarmAcquiresTotal map[string]int64
	ReactiveCommits       map[string]map[string]int64
	ReactivePending       map[string]int64
	ReactivePendingAge    map[string]int64
	ReactiveIdentity      map[string]int64
	// PlaybackRepresentations is RX-8.2's count of observed playback
	// representations by FIXED class. Bounded cardinality; no title,
	// identity, URL, service name or per-item label.
	PlaybackRepresentations map[string]int64
	// AggregatorDeliveryAgeSeconds is the age of the most recent
	// aggregator-proxied delivery, valid only when AggregatorDeliveryKnown.
	AggregatorDeliveryAgeSeconds int64
	// AggregatorDeliveryKnown makes the ABSENCE case visible without
	// database access: 0 means no aggregator-proxied delivery has ever been
	// observed, which RX-8.1 alone cannot distinguish from "no playback".
	AggregatorDeliveryKnown bool
	ReactiveUndoTotal       int64
	ReactivePromotionBytes  int64
	ReactiveHeadroomBytes   int64
	ReactiveHeadroomKnown   bool
}

const namespace = "darkharrbor"

// Render writes snap in Prometheus text exposition format (version 0.0.4)
// to w. Every metric name is a fixed, hardcoded string -- no user- or
// database-derived text ever becomes a metric name, and every label value
// is either a fixed lane/class vocabulary string (outcome.Class,
// store.ItemState) or omitted, so DG-04 (no upstream URL/secret in
// metrics) holds by construction: nothing free-form is ever rendered.
func Render(w io.Writer, snap Snapshot) error {
	var b strings.Builder

	writeGauge(&b, "uptime_seconds", "Process uptime in seconds.", nil, float64(snap.UptimeSeconds))

	writeHeader(&b, "items_total", "gauge", "Current item count by state.")
	for _, state := range sortedKeys(snap.ItemsByState) {
		writeSample(&b, "items_total", map[string]string{"state": state}, float64(snap.ItemsByState[state]))
	}

	writeGauge(&b, "nntp_zero_fill_events_total", "NS-1.3 cumulative NNTP final-rung-exhaustion events across all items.", nil, float64(snap.NNTPZeroFillTotal))

	writeGauge(&b, "error_journal_entries_total", "OBS-01 persisted per-item SF-01 outcome-event journal entries, summed across all items.", nil, float64(snap.ErrorJournalEntriesTotal))
	writeHeader(&b, "error_journal_entries_by_class_total", "gauge", "OBS-01 persisted error-journal entries by outcome class.")
	for _, class := range sortedKeys(snap.ErrorJournalByClass) {
		writeSample(&b, "error_journal_entries_by_class_total", map[string]string{"class": class}, float64(snap.ErrorJournalByClass[class]))
	}

	writeHeader(&b, "rangecache", "gauge", "Existing rangecache snapshot fields, mirrored from /healthz.")
	for _, key := range sortedKeys(snap.RangeCacheEntries) {
		writeSample(&b, "rangecache", map[string]string{"field": key}, float64(snap.RangeCacheEntries[key]))
	}

	writeGauge(&b, "http_origin_score_entries", "Bounded HR3.3 passive origin-score entry count, mirrored from /healthz.", nil, float64(snap.HTTPOriginScoreEntries))
	writeGauge(&b, "torrent_cdn_score_entries", "Bounded TS-2.3 passive CDN-score entry count, mirrored from /healthz.", nil, float64(snap.TorrentCDNScoreEntries))

	writeHeader(&b, "source_health_score", "gauge", "HR7.4 bounded passive health score by lane and hashed source.")
	writeHeader(&b, "source_first_byte_milliseconds", "gauge", "HR7.4 per-source first-byte EWMA.")
	writeHeader(&b, "source_token_lifetime_seconds", "gauge", "HR7.4 per-source observed token-lifetime EWMA.")
	writeHeader(&b, "source_expiry_observations_total", "counter", "HR7.4 per-source expiry observations.")
	writeHeader(&b, "source_observations_total", "counter", "HR7.4 per-source passive observations.")
	for _, source := range snap.SourceHealth {
		labels := map[string]string{"lane": source.Lane, "source": source.Source, "health": source.Health, "retry_stage": source.RetryStage}
		writeSample(&b, "source_health_score", labels, float64(source.Score))
		writeSample(&b, "source_first_byte_milliseconds", labels, float64(source.FirstByteMillis))
		writeSample(&b, "source_token_lifetime_seconds", labels, float64(source.TokenLifetimeSecs))
		writeSample(&b, "source_expiry_observations_total", labels, float64(source.ExpiryCount))
		writeSample(&b, "source_observations_total", labels, float64(source.Observations))
	}
	writeHeader(&b, "governor_denials_total", "counter", "HR7.4 canceled or deadline-expired governed acquisitions.")
	for _, denial := range snap.GovernorDenials {
		writeSample(&b, "governor_denials_total", map[string]string{"lane": denial.Lane, "priority": denial.Priority, "reason": denial.Reason}, float64(denial.Count))
	}

	writeHeader(&b, "nntp_warm_pool_open", "gauge", "NS-5.1 current warm-pool connection count by provider.")
	for _, name := range sortedKeys(snap.NNTPWarmOpen) {
		writeSample(&b, "nntp_warm_pool_open", map[string]string{"provider": name}, float64(snap.NNTPWarmOpen[name]))
	}
	writeHeader(&b, "nntp_warm_pool_target", "gauge", "NS-5.1 configured warm-pool target by provider.")
	for _, name := range sortedKeys(snap.NNTPWarmTarget) {
		writeSample(&b, "nntp_warm_pool_target", map[string]string{"provider": name}, float64(snap.NNTPWarmTarget[name]))
	}
	writeHeader(&b, "nntp_cold_acquires_total", "gauge", "NS-5.1 cumulative fresh-dial Acquire count by provider demand pool.")
	for _, name := range sortedKeys(snap.NNTPColdAcquiresTotal) {
		writeSample(&b, "nntp_cold_acquires_total", map[string]string{"provider": name}, float64(snap.NNTPColdAcquiresTotal[name]))
	}
	writeHeader(&b, "nntp_warm_acquires_total", "gauge", "NS-5.1 cumulative reused-connection Acquire count by provider demand pool.")
	for _, name := range sortedKeys(snap.NNTPWarmAcquiresTotal) {
		writeSample(&b, "nntp_warm_acquires_total", map[string]string{"provider": name}, float64(snap.NNTPWarmAcquiresTotal[name]))
	}

	writeHeader(&b, "reactive_commits", "gauge", "RX-8.1 commits by explicit mode and fixed lookback period.")
	for _, period := range sortedKeys(snap.ReactiveCommits) {
		for _, mode := range sortedKeys(snap.ReactiveCommits[period]) {
			writeSample(&b, "reactive_commits", map[string]string{"mode": mode, "period": period}, float64(snap.ReactiveCommits[period][mode]))
		}
	}
	writeHeader(&b, "reactive_pending_depth", "gauge", "RX-8.1 pending reactive items by fixed reason.")
	writeHeader(&b, "reactive_pending_oldest_age_seconds", "gauge", "RX-8.1 oldest pending item age by fixed reason.")
	for _, reason := range sortedKeys(snap.ReactivePending) {
		writeSample(&b, "reactive_pending_depth", map[string]string{"reason": reason}, float64(snap.ReactivePending[reason]))
		writeSample(&b, "reactive_pending_oldest_age_seconds", map[string]string{"reason": reason}, float64(snap.ReactivePendingAge[reason]))
	}
	writeHeader(&b, "reactive_identity_outcomes_total", "counter", "RX-8.1 durable identity outcomes by fixed evidence class.")
	for _, outcome := range sortedKeys(snap.ReactiveIdentity) {
		writeSample(&b, "reactive_identity_outcomes_total", map[string]string{"outcome": outcome}, float64(snap.ReactiveIdentity[outcome]))
	}
	writeHeader(&b, "playback_representations", "gauge", "RX-8.2 observed playback representations by fixed class.")
	for _, class := range sortedKeys(snap.PlaybackRepresentations) {
		writeSample(&b, "playback_representations", map[string]string{"class": class}, float64(snap.PlaybackRepresentations[class]))
	}
	aggregatorKnown := 0.0
	if snap.AggregatorDeliveryKnown {
		aggregatorKnown = 1
	}
	writeGauge(&b, "aggregator_delivery_observed", "RX-8.2 whether any aggregator-proxied delivery has ever been observed.", nil, aggregatorKnown)
	writeGauge(&b, "aggregator_delivery_age_seconds", "RX-8.2 age of the most recent aggregator-proxied delivery; meaningful only when aggregator_delivery_observed is 1.", nil, float64(snap.AggregatorDeliveryAgeSeconds))
	writeCounter(&b, "reactive_undo_events_total", "RX-8.1 durable undo events.", nil, float64(snap.ReactiveUndoTotal))
	writeCounter(&b, "reactive_promotion_bytes_total", "RX-8.1 durable bytes written by explicit promotion.", nil, float64(snap.ReactivePromotionBytes))
	known := 0.0
	if snap.ReactiveHeadroomKnown {
		known = 1
	}
	writeGauge(&b, "reactive_promotion_headroom_available", "RX-8.1 whether RX-5.1 has supplied current free-space headroom.", nil, known)
	writeGauge(&b, "reactive_promotion_headroom_bytes", "RX-8.1 current free-space headroom above the configured RX-5.1 floor.", nil, float64(snap.ReactiveHeadroomBytes))

	_, err := io.WriteString(w, b.String())
	return err
}

func writeHeader(b *strings.Builder, name, kind, help string) {
	fmt.Fprintf(b, "# HELP %s_%s %s\n", namespace, name, help)
	fmt.Fprintf(b, "# TYPE %s_%s %s\n", namespace, name, kind)
}

func writeGauge(b *strings.Builder, name, help string, labels map[string]string, value float64) {
	writeHeader(b, name, "gauge", help)
	writeSample(b, name, labels, value)
}

func writeCounter(b *strings.Builder, name, help string, labels map[string]string, value float64) {
	writeHeader(b, name, "counter", help)
	writeSample(b, name, labels, value)
}

func writeSample(b *strings.Builder, name string, labels map[string]string, value float64) {
	fmt.Fprintf(b, "%s_%s%s %v\n", namespace, name, formatLabels(labels), value)
}

// formatLabels renders a fixed, sorted label set. Label values are always
// one of a small closed vocabulary (ItemState/outcome.Class/field names)
// supplied by this package's own callers, never raw user or DB text, so no
// escaping beyond the trivial cases below is needed -- but they are applied
// anyway as defense in depth, matching Prometheus's own exposition-format
// escaping rules for the three characters that are structurally significant
// in a label value.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := sortedKeys(labels)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := labels[k]
		v = strings.ReplaceAll(v, `\`, `\\`)
		v = strings.ReplaceAll(v, "\n", `\n`)
		v = strings.ReplaceAll(v, `"`, `\"`)
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
