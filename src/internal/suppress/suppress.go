// Package suppress implements SF-03: own-measurement failed-release
// suppression. DarkHarrbor fingerprints releases (normalized name + size +
// lane) that it has itself observed failing with a small set of
// deterministic, distinct error reasons, and filters that same fingerprint
// out of its own feeds for a bounded, configurable TTL.
//
// This is deliberately own-measurement only: DarkHarrbor never imports a
// crowd-sourced or shared blocklist, never grows a sharing surface, and
// suppression only ever reflects what this exact deployment has itself
// measured. It is also a different key space from the existing
// info-hash/content-key blacklist (internal/store's failed_hashes table,
// consulted via isTorrentBlacklisted / NZBBlacklistKey): that mechanism
// blacklists one exact source identity (one hash) permanently for a fixed
// duration; this package fingerprints on normalized name+size+lane instead,
// so the SAME effective release re-surfacing under a DIFFERENT hash from a
// different indexer (a re-post, a different tracker's copy of the same
// encode) is still caught, for an operator-configurable TTL rather than a
// fixed one. Neither mechanism replaces the other.
package suppress

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// Reason is one of SF-03's deterministic, distinct suppression triggers.
// Unlike outcome.Class (SF-01), which governs retry behavior, Reason exists
// purely for observability/counters — suppression behavior (the TTL) does
// not vary by reason; only the exposed counts are broken out by it.
type Reason string

const (
	// ReasonUnsupportedFormat is an archive/container format DarkHarrbor
	// cannot read at all (e.g. an unsupported ZIP compression method).
	ReasonUnsupportedFormat Reason = "unsupported_format"
	// ReasonEncryptedNoPassword is a password-protected archive DarkHarrbor
	// has no password for.
	ReasonEncryptedNoPassword Reason = "encrypted_no_password"
	// ReasonDeadPost is a confirmed-missing NNTP post (nntp.ErrDeadPost)
	// observed across every configured usenet provider.
	ReasonDeadPost Reason = "dead_post"
	// ReasonIdentityMismatch is reserved for ID-01 (grab-time identity
	// verification against the grabbing Arr's authoritative metadata),
	// which is not yet implemented. Declared now so Reason's set is stable
	// once ID-01 lands; nothing in this codebase emits it yet.
	ReasonIdentityMismatch Reason = "identity_mismatch"
)

// Lane identifies which of DarkHarrbor's acquisition lanes a fingerprinted
// release came from. The same title+size on two different lanes is a
// different fingerprint: a release that is an unsupported archive as an
// NZB says nothing about the same title showing up as a torrent.
type Lane string

const (
	LaneTorrent Lane = "torrent"
	LaneNZB     Lane = "nzb"
	LaneHTTP    Lane = "http"
)

// nonAlnum collapses everything but letters/digits so trivial punctuation,
// spacing, or case differences between indexers describing the same release
// still fold to the same fingerprint.
var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeName lowercases and collapses non-alphanumeric runs. This is
// deliberately coarse — full release-name parsing (group/quality/codec
// tokens) is ID-01/media-truth territory, not this row's scope.
func normalizeName(name string) string {
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// Fingerprint returns the stable suppression key for a release. size <= 0
// is treated as unknown and folded into the key as "0" — the fingerprint is
// still stable, just coarser (matches on name+lane alone) for sources that
// never report an exact size.
func Fingerprint(name string, size int64, lane Lane) string {
	if size < 0 {
		size = 0
	}
	raw := normalizeName(name) + "|" + strconv.FormatInt(size, 10) + "|" + string(lane)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16]) // 128 bits: ample for a hygiene key, keeps rows small
}
