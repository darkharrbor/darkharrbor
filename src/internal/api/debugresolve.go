// debugresolve.go implements OBS-02: a read-only "explain this item"
// ladder dry-run debug endpoint. It walks the same candidate/lease/proof
// concepts the real resolve/fetch paths use, but never calls a real
// Attempt, never contacts a provider, and never mutates item or account
// state -- every value comes from an already-existing, already-bounded
// shared owner (accountgov.Governor's non-blocking Capacity/InUse/Waiting
// readers, HR1.5's continuity ledger wiring flag, SF-04's already-persisted
// MediaFacts, and store.GetTorrentMeta), exactly OBS-01's "no new hot-path
// instrumentation" pattern applied to a dry-run explain view instead of a
// metrics snapshot.
//
// Disclosed scope limitation: the NNTP lane has no accountgov.Governor (its
// admission bound is the per-provider connection-pool ceiling instead, per
// HR1.4/NS-6.1's own established precedent) and no existing shared owner
// exposes that ceiling read-only outside the provider's own internals, so
// NNTP lease availability is reported as "not applicable" rather than
// guessed or fabricated. This is deliberately item-level only (no per-file
// drill-down) to keep the row's scope to one coherent read model.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// debugCandidate is one lane's named acquisition candidate: a provider or
// backend the real resolve path could try, reported without confirming or
// contacting it.
type debugCandidate struct {
	Name string `json:"name"`
	Role string `json:"role"` // "primary" or "alternate"
}

// debugLease reports one governed operation class's current, live,
// non-blocking capacity snapshot -- never an acquisition attempt.
type debugLease struct {
	Op          string `json:"op"`
	Capacity    int    `json:"capacity"`
	InUse       int    `json:"in_use"`
	Waiting     int    `json:"waiting"`
	HasHeadroom bool   `json:"has_headroom"`
}

// handleDebugResolve renders OBS-02's ladder dry-run explain view for one
// item. Read-only; never mutates item, account, or provider state; moves
// no media bytes. A missing/unknown item_id abstains with 400/404 rather
// than guessing.
func (s *Server) handleDebugResolve(w http.ResponseWriter, r *http.Request) {
	itemID := strings.TrimSpace(r.PathValue("item_id"))
	if itemID == "" {
		http.Error(w, "item_id is required", http.StatusBadRequest)
		return
	}
	if s.store == nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()
	item, err := s.store.GetItemByID(ctx, itemID)
	if err != nil || item == nil {
		if item2, err2 := s.store.GetItemByPublicID(ctx, itemID); err2 == nil && item2 != nil {
			item = item2
		} else {
			http.Error(w, "item not found", http.StatusNotFound)
			return
		}
	}

	body := map[string]any{
		"item_id":   item.ID,
		"public_id": item.PublicID,
		"lane":      string(item.SourceType),
		"state":     string(item.State),
		"cached":    item.Cached,
	}
	body["retry_stage"] = s.debugRetryStage(ctx, item.ID)

	switch item.SourceType {
	case store.SourceTypeTorrent:
		body["candidates"] = s.debugTorrentCandidates(item)
		body["proof"] = s.debugTorrentProof(ctx, item)
		body["provider_expiry"] = debugProviderExpiry(item)
		if s.torrentGov != nil {
			lease := debugLeaseFor(s.torrentGov, TorrentCDNGovOp, accountgov.PriorityPlayback)
			body["lease"] = lease
			body["predicted_rung"] = predictedRung(lease, "primary-provider", "alt-provider-failover")
		} else {
			body["lease"] = nil
			body["predicted_rung"] = "primary-provider (no governor configured)"
		}
	case store.SourceTypeNZB:
		body["candidates"] = s.debugNNTPCandidates(item)
		body["proof"] = s.debugMediaTruthProof(item)
		body["lease"] = "not applicable: NNTP lane admission is a per-provider connection-pool ceiling, not an accountgov.Governor lease (see HR1.4/NS-6.1)"
		body["predicted_rung"] = "provider-order (pool-ceiling admission, not lease-predicted)"
	case store.SourceTypeHTTP:
		body["candidates"] = s.debugHTTPCandidates()
		body["proof"] = s.debugHTTPProof()
		if s.httpGov != nil {
			lease := debugLeaseFor(s.httpGov, HTTPSourceGovOp, accountgov.PriorityPlayback)
			body["lease"] = lease
			body["predicted_rung"] = predictedRung(lease, "resolved-backend", "re-resolve")
		} else {
			body["lease"] = nil
			body["predicted_rung"] = "resolved-backend (no governor configured)"
		}
	default:
		body["candidates"] = []debugCandidate{}
		body["proof"] = nil
		body["lease"] = nil
		body["predicted_rung"] = "unknown lane"
	}

	writeJSON(w, http.StatusOK, body)
}

func (s *Server) debugRetryStage(ctx context.Context, itemID string) any {
	entries, err := s.store.GetErrorJournal(ctx, itemID)
	if err != nil || len(entries) == 0 {
		return nil
	}
	entry := entries[len(entries)-1]
	stage := "attempt"
	switch {
	case strings.Contains(entry.Rung, "reresolve"):
		stage = "reresolve"
	case strings.Contains(entry.Rung, "altprovider"):
		stage = "alternate"
	case strings.Contains(entry.Rung, "cross_lane"):
		stage = "cross_lane"
	case strings.Contains(entry.Rung, "final"):
		stage = "final"
	}
	class := entry.Class
	switch class {
	case "permanent_here", "transient_here", "account_level", "unknown":
	default:
		class = "unknown"
	}
	source := ""
	if entry.Provider != "" {
		sum := sha256.Sum256([]byte(entry.Provider))
		source = hex.EncodeToString(sum[:8])
	}
	return map[string]any{
		"stage": stage, "class": class, "source": source, "at": entry.At,
	}
}

func debugLeaseFor(gov *accountgov.Governor, op string, _ accountgov.Priority) debugLease {
	capacity := gov.Capacity(op)
	inUse := gov.InUse(op)
	waiting := gov.Waiting(op)
	return debugLease{
		Op:          op,
		Capacity:    capacity,
		InUse:       inUse,
		Waiting:     waiting,
		HasHeadroom: capacity <= 0 || inUse < capacity,
	}
}

// predictedRung reports the first rung name when the governed op currently
// has headroom, or the same rung name with a wait note otherwise -- a live,
// deterministic, non-blocking prediction, never a real attempt.
func predictedRung(lease debugLease, firstRung, laterRung string) string {
	if lease.HasHeadroom {
		return firstRung
	}
	return firstRung + " (no current lease headroom; would wait, next candidate " + laterRung + ")"
}

// debugTorrentCandidates lists configured provider names without confirming
// or contacting any of them (never calls confirmAltProvider).
func (s *Server) debugTorrentCandidates(item *store.Item) []debugCandidate {
	primaryName := ""
	if s.prov != nil {
		primaryName = s.prov.Name()
	}
	if item.Provider != nil && *item.Provider != "" {
		primaryName = *item.Provider
	}
	out := make([]debugCandidate, 0, len(s.providerOrder))
	for _, name := range s.providerOrder {
		role := "alternate"
		if name == primaryName {
			role = "primary"
		}
		out = append(out, debugCandidate{Name: name, Role: role})
	}
	return out
}

// debugProviderExpiry reports TS-4.2's derived provider-expiry horizon for
// item, read-only: never contacts a provider, never mutates state. "known"
// is false when the item's provider has no assumed retention window
// (assumedProviderExpiryDays in keepwarm.go) -- never guessed.
func debugProviderExpiry(item *store.Item) map[string]any {
	horizon, providerName, days, ok := ProviderExpiryHorizon(item)
	if !ok {
		return map[string]any{"known": false, "provider": providerName}
	}
	return map[string]any{
		"known":                  true,
		"provider":               providerName,
		"assumed_retention_days": days,
		"horizon":                horizon.Format(time.RFC3339),
		"keep_warm_lead_days":    keepWarmLeadDays,
	}
}

func (s *Server) debugTorrentProof(ctx context.Context, item *store.Item) map[string]any {
	if item.InfoHash == nil || *item.InfoHash == "" {
		return map[string]any{"native_proof_available": false, "reason": "no infohash"}
	}
	meta, ok, err := s.store.GetTorrentMeta(ctx, *item.InfoHash)
	if err != nil || !ok || meta == nil {
		return map[string]any{"native_proof_available": false}
	}
	return map[string]any{
		"native_proof_available": true,
		"meta_version":           meta.MetaVersion,
	}
}

func (s *Server) debugNNTPCandidates(item *store.Item) []debugCandidate {
	primaryName := ""
	if item.Provider != nil {
		primaryName = *item.Provider
	}
	out := make([]debugCandidate, 0, len(s.usenetOrder))
	for _, name := range s.usenetOrder {
		role := "alternate"
		if name == primaryName || (primaryName == "" && len(out) == 0) {
			role = "primary"
		}
		out = append(out, debugCandidate{Name: name, Role: role})
	}
	return out
}

func (s *Server) debugMediaTruthProof(item *store.Item) map[string]any {
	return map[string]any{
		"media_facts_available": item.Metadata.MediaFacts != nil,
	}
}

func (s *Server) debugHTTPCandidates() []debugCandidate {
	if s.httpHandlers == nil {
		return []debugCandidate{}
	}
	statuses := s.httpHandlers.Statuses()
	out := make([]debugCandidate, 0, len(statuses))
	for i, st := range statuses {
		role := "alternate"
		if i == 0 {
			role = "primary"
		}
		out = append(out, debugCandidate{Name: st.BackendID, Role: role})
	}
	return out
}

func (s *Server) debugHTTPProof() map[string]any {
	return map[string]any{
		"continuity_ledger_wired": s.httpContinuity != nil,
	}
}
