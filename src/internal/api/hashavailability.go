package api

// hashavailability.go — TS-5.1 (T6): own-traffic per-provider hash
// availability observation writers for the two real outcome sites that
// exist in this package today:
//
//   - submit: submitItemDirect/submitItemDirectSingle's own cachegate.Check
//     result (a direct, authoritative provider cache-oracle answer for this
//     exact hash) — see recordSubmitAvailability, called from router.go.
//   - stream: streamViaCache's own live bytes=0-0 capability probe success
//     (cached=true) and the same genuine (non-client-cancelled) failure
//     condition TS-4.1's TriggerRepairAsync already established
//     (cached=false) — see recordStreamAvailability, called from
//     streamcdn.go.
//
// A third source, "preflight" (TS-3.1's grab-time byte-0 preflight), is
// declared in internal/availability.SourcePreflight but not wired here:
// TS-3.1 does not exist in this repository yet and is not a TS-5.1
// dependency. TS-3.1 will call RecordHashAvailability directly at its own
// outcome site when it lands, reusing this row's store API rather than a
// second one.
//
// Every write here is best-effort and non-blocking: a failure is logged at
// Warn and never affects submission, gating, or streaming behavior — this
// is purely an observational side channel feeding TS-5.2's future (not this
// row's) search-time consumption.
import (
	"context"

	"github.com/darkharrbor/darkharrbor/internal/availability"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// recordHashAvailability normalizes providerName/infoHash via
// internal/availability and, if both are well-formed, upserts one
// observation through the shared store owner. Malformed or missing input
// abstains silently (never guessed, per DarkHarrbor's fail-closed
// convention) — this is expected and unremarkable for e.g. an item whose
// hash is not yet known.
func (s *Server) recordHashAvailability(ctx context.Context, providerName, infoHash string, cached bool, source availability.Source) {
	provNorm, ok := availability.NormalizeProvider(providerName)
	if !ok {
		return
	}
	hashNorm, ok := availability.NormalizeHash(infoHash)
	if !ok {
		return
	}
	if !availability.ValidSource(source) {
		return
	}
	if err := s.store.RecordHashAvailability(ctx, provNorm, hashNorm, cached, string(source), s.cfg.AvailabilityTTL()); err != nil {
		s.log.Warn("availability: record observation failed (non-fatal)",
			"event", "hash_availability_record_failed",
			"provider", provNorm,
			"source", string(source),
			"error", err,
		)
	}
}

// recordSubmitAvailability is TS-5.1's "submit" writer: called right after
// a lane's cachegate.Check succeeds, with the exact provider name and
// cached verdict that check produced — the strongest signal available
// (direct provider cache-oracle answer), independent of whether the
// subsequent provider Submit call itself succeeds.
func (s *Server) recordSubmitAvailability(ctx context.Context, item *store.Item, providerName string, cached bool) {
	hash := torrentRepairIdentity(item)
	if hash == "" {
		return
	}
	s.recordHashAvailability(ctx, providerName, hash, cached, availability.SourceSubmit)
}

// recordStreamAvailability is TS-5.1's "stream" writer: called from
// streamcdn.go at the two real outcome points that already exist there —
// a live capability probe confirming the provider still serves this hash
// (cached=true), or the same genuine non-client-cancelled failure
// TS-4.1's TriggerRepairAsync already treats as "broken/removed"
// (cached=false).
func (s *Server) recordStreamAvailability(ctx context.Context, item *store.Item, cached bool) {
	if item == nil || item.Provider == nil {
		return
	}
	hash := torrentRepairIdentity(item)
	if hash == "" {
		return
	}
	s.recordHashAvailability(ctx, *item.Provider, hash, cached, availability.SourceStream)
}
