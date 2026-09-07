// Package httpstream implements the HTTP stream provider core (H1):
// canonical resolve keys, signed grab envelopes, URL-free persisted file
// DTOs, protocol handler registry, range math, and sanitized error classes.
//
// Hard invariants (HTTP-STREAM-MASTER-PLAN.md §0):
//   - No source/CDN URL is ever persisted: not in the DB, not in .strm
//     files, not in logs/metrics/errors. Resolved URLs are transient,
//     in-memory values bounded by expiry (D10/A12, HS-1.9).
//   - .strm files are permanent library entries containing only the signed
//     DH /stream URL; the items row + strm_blobs are the permanent playback
//     identity (COR-13).
package httpstream

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// ResolveKeyVersion is the current canonical resolve key schema version.
// Bump when the canonicalization rules or field set change (D10).
const ResolveKeyVersion = 1

// digestDomain domain-separates the synthetic infohash digest from real
// torrent infohashes: a real SHA1 of torrent metadata can never collide
// with sha1(digestDomain || canonical-key) by construction (D10/A26).
const digestDomain = "darkharrbor/http-resolve-key/v1\x00"

// ResolveKey is the versioned, typed, URL-free identity of one grabbed HTTP
// stream representation (D10). It contains everything needed to re-resolve
// the real source at play time and nothing that could leak a source URL.
type ResolveKey struct {
	// Version is the schema version (ResolveKeyVersion).
	Version int `json:"v"`
	// BackendID is the immutable persisted backend identity — the normalized
	// unique backend name (D5). Never a URL.
	BackendID string `json:"backend_id"`
	// Handler is the protocol handler name: ia | omss | stremio | generic.
	Handler string `json:"handler"`
	// Kind is the media kind: movie | episode.
	Kind string `json:"kind"`
	// IDs maps a lowercase ID namespace (tmdb, imdb, tvdb, ia, ...) to the
	// normalized ID value in that namespace.
	IDs map[string]string `json:"ids"`
	// Season/Episode are set for Kind == "episode" (D12: no season-only keys).
	Season  int `json:"season,omitempty"`
	Episode int `json:"episode,omitempty"`
	// Selector is the URL-free representation selector: a handler-stable
	// source ID where the protocol provides one, otherwise a deterministic
	// metadata predicate (quality/filename). Missing, ambiguous, or changed
	// representations are rejected at resolve time — never silently
	// downgraded (D10).
	Selector string `json:"selector,omitempty"`
}

// KnownHandlers is the closed set of protocol handler names for v1.
var KnownHandlers = map[string]bool{
	"ia":      true,
	"omss":    true,
	"stremio": true,
	"generic": true,
}

// Validate rejects structurally invalid keys before signing or persistence.
func (k ResolveKey) Validate() error {
	if k.Version != ResolveKeyVersion {
		return NewError(ClassInvalidKey, fmt.Sprintf("unsupported resolve key version %d", k.Version))
	}
	if strings.TrimSpace(k.BackendID) == "" {
		return NewError(ClassInvalidKey, "empty backend_id")
	}
	if !KnownHandlers[strings.ToLower(strings.TrimSpace(k.Handler))] {
		return NewError(ClassInvalidKey, "unknown handler")
	}
	switch strings.ToLower(strings.TrimSpace(k.Kind)) {
	case "movie":
	case "episode":
		if k.Season <= 0 || k.Episode <= 0 {
			// D12: season-only (or malformed) episode keys are rejected;
			// DH never synthesizes fake season packs.
			return NewError(ClassInvalidKey, "episode key requires season and episode >= 1")
		}
	default:
		return NewError(ClassInvalidKey, "unknown media kind")
	}
	if len(k.IDs) == 0 {
		return NewError(ClassInvalidKey, "no media IDs")
	}
	for ns, id := range k.IDs {
		if strings.TrimSpace(ns) == "" || strings.TrimSpace(id) == "" {
			return NewError(ClassInvalidKey, "empty ID namespace or value")
		}
	}
	return nil
}

// normalized returns a deterministic normalized copy: trimmed, casing rules
// applied, ID namespaces lowercased. Normalization runs before hashing so
// equivalent keys always produce identical digests (D10).
func (k ResolveKey) normalized() ResolveKey {
	out := ResolveKey{
		Version:   k.Version,
		BackendID: strings.ToLower(strings.TrimSpace(k.BackendID)),
		Handler:   strings.ToLower(strings.TrimSpace(k.Handler)),
		Kind:      strings.ToLower(strings.TrimSpace(k.Kind)),
		Season:    k.Season,
		Episode:   k.Episode,
		Selector:  strings.TrimSpace(k.Selector),
	}
	if len(k.IDs) > 0 {
		out.IDs = make(map[string]string, len(k.IDs))
		for ns, id := range k.IDs {
			ns = strings.ToLower(strings.TrimSpace(ns))
			id = strings.TrimSpace(id)
			// Case-insensitive namespaces store lowercase values; IA
			// identifiers are case-sensitive and are kept verbatim.
			if ns != "ia" {
				id = strings.ToLower(id)
			}
			out.IDs[ns] = id
		}
	}
	return out
}

// Canonical returns the deterministic canonical JSON encoding of the
// normalized key. encoding/json sorts map keys and emits struct fields in
// declaration order, so the output is stable for equivalent inputs.
func (k ResolveKey) Canonical() (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(k.normalized())
	if err != nil {
		return "", fmt.Errorf("canonicalize resolve key: %w", err)
	}
	return string(b), nil
}

// Digest returns the domain-separated 40-hex synthetic infohash for this key.
// It is qBit/arr-compatible (40 lowercase hex chars) but can never collide
// with a real torrent infohash (D10).
func (k ResolveKey) Digest() (string, error) {
	canon, err := k.Canonical()
	if err != nil {
		return "", err
	}
	return DigestOfCanonical(canon), nil
}

// DigestOfCanonical hashes an already-canonical key string.
func DigestOfCanonical(canonical string) string {
	sum := sha1.Sum([]byte(digestDomain + canonical))
	return hex.EncodeToString(sum[:])
}

// ParseResolveKey decodes and validates a canonical (or equivalent) resolve
// key JSON string as persisted in items.resolve_key.
func ParseResolveKey(s string) (ResolveKey, error) {
	var k ResolveKey
	if err := json.Unmarshal([]byte(s), &k); err != nil {
		return ResolveKey{}, NewError(ClassInvalidKey, "resolve key is not valid JSON")
	}
	if err := k.Validate(); err != nil {
		return ResolveKey{}, err
	}
	return k, nil
}
