// Package availability implements TS-5.1 (T6): pure, deterministic input
// normalization/validation for own-traffic per-provider hash availability
// observations. This package holds no state and does no I/O — persistence
// lives in internal/store (hash_availability.go), mirroring the
// internal/suppress (SF-03) split of "pure fingerprint/normalize logic
// here, storage there" rather than mixing the two.
//
// T6's own "measure, don't gossip" rule means every observation recorded
// through this package traces back to this exact deployment's own real
// provider traffic (submit-time CheckCached, stream-time capability
// probes/genuine failures) — never an imported or crowd-sourced signal.
// Consuming these observations for search-time eligibility decisions is
// TS-5.2's explicit scope, not this package's.
package availability

import "strings"

const (
	// maxProviderBytes and maxHashBytes bound untrusted-shaped input before
	// it ever reaches a SQL statement or is used as part of a cache key.
	// Real provider names ("torbox", "realdebrid") and real v1/v2 info
	// hashes (40 or 64 lowercase hex chars) are both far under these
	// bounds; anything beyond them is refused rather than truncated.
	maxProviderBytes = 64
	maxHashBytes     = 128
)

// Source identifies which real outcome produced an observation. Unlike
// outcome.Class (SF-01), which governs retry behavior, Source exists purely
// for observability/provenance — recording behavior does not vary by
// Source; only the persisted row's own field does.
type Source string

const (
	// SourceSubmit is a submit-time provider CheckCached result (cachegate),
	// the strongest signal: a direct, authoritative provider cache-oracle
	// answer for this exact hash.
	SourceSubmit Source = "submit"
	// SourceStream is a real stream-time observation: either a live
	// bytes=0-0 capability probe succeeding (cached=true) against an
	// already-Ready item, or the established TS-4.1 genuine (non-client-
	// cancelled) failure trigger firing (cached=false).
	SourceStream Source = "stream"
	// SourceSearch is a search-time provider CheckCached result: the same
	// direct, authoritative cache-oracle answer SourceSubmit records, taken
	// at the live search refresh rather than at submit. It is own traffic --
	// this deployment's own oracle call for this exact hash -- so persisting
	// it does not weaken the "measure, don't gossip" rule. It is a distinct
	// Source only for provenance: a search-time answer was never gated on an
	// actual grab, so observability can tell the two apart.
	SourceSearch Source = "search"
	// SourcePreflight is reserved for TS-3.1's grab-time byte-0 preflight,
	// which does not exist in this repository yet (TS-3.1 remains READY,
	// not DONE, and is not a TS-5.1 dependency). Declared now so Source's
	// set is stable once TS-3.1 lands and calls into this same store API;
	// nothing in this codebase emits it yet.
	SourcePreflight Source = "preflight"
)

// NormalizeProvider lowercases and trims a provider name, refusing empty or
// oversized input. Malformed input abstains (returns "", false) rather than
// guessing or truncating.
func NormalizeProvider(name string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || len(name) > maxProviderBytes {
		return "", false
	}
	return name, true
}

// isHexDigit reports whether b is one of 0-9/a-f (lowercase only — callers
// normalize case first).
func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
}

// NormalizeHash lowercases and trims an info hash, refusing empty, oversized,
// or non-hex input. This deliberately does not enforce an exact length (v1
// SHA-1 hashes are 40 hex chars, v2 SHA-256 hashes are 64) since both are
// real, valid identities already produced elsewhere in DarkHarrbor
// (torrentmeta, canonicalTorrentHash/torrentRepairIdentity) — this package
// trusts that upstream shape and only refuses input that could not possibly
// be a real hash (empty, oversized, or containing non-hex characters).
func NormalizeHash(hash string) (string, bool) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if hash == "" || len(hash) > maxHashBytes {
		return "", false
	}
	for i := 0; i < len(hash); i++ {
		if !isHexDigit(hash[i]) {
			return "", false
		}
	}
	return hash, true
}

// ValidSource reports whether s is one of the declared observation sources.
// An unrecognized Source is refused rather than persisted under an unknown
// label.
func ValidSource(s Source) bool {
	switch s {
	case SourceSubmit, SourceStream, SourceSearch, SourcePreflight:
		return true
	default:
		return false
	}
}
