// Package httpstream (this file): HR2.7 shared whole-object source digest
// carrier plus the RFC 3230 "Digest" response-header parser. Both are
// protocol-neutral so every handler (ia, generic, future ones) and the
// live D9 preflight fetch can feed the same shape into the content proof
// graph without a second parsing/validation path.
package httpstream

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
)

const (
	maxDigestHeaderBytes   = 512
	maxDigestHeaderEntries = 8
)

// SourceDigest is a transient, provenance-free whole-object digest a
// protocol handler learned from source metadata (Internet Archive
// md5/sha1, a Metalink <hash> element, or a live Digest response header).
// It is never part of PersistedFile/FileList -- HR2.7 consumes it once, at
// resolve/preflight time, to feed the shared content proof graph, then
// drops it.
type SourceDigest struct {
	Algorithm string // "md5", "sha1", or "sha256"
	Hex       string // lowercase hex-encoded digest, exact length for Algorithm
}

// ValidSourceDigest reports whether d has a recognized algorithm and a
// correctly sized, well-formed lowercase hex digest. Malformed digests are
// dropped by callers -- never fabricated or coerced.
func ValidSourceDigest(d SourceDigest) bool {
	n := digestHexLen(d.Algorithm)
	if n == 0 || len(d.Hex) != n {
		return false
	}
	for _, c := range d.Hex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func digestHexLen(algorithm string) int {
	switch algorithm {
	case "md5":
		return 32
	case "sha1":
		return 40
	case "sha256":
		return 64
	default:
		return 0
	}
}

// ParseDigestHeader parses an RFC 3230 "Digest" response header (e.g.
// "sha-256=base64value, md5=base64value") into whole-object SourceDigest
// entries. This is untrusted upstream input: the header is length-bounded,
// entries are count-bounded, and one malformed entry is skipped rather than
// failing the whole header (DG-03: never panic, never fabricate).
func ParseDigestHeader(raw string) []SourceDigest {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxDigestHeaderBytes {
		return nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxDigestHeaderEntries {
		parts = parts[:maxDigestHeaderEntries]
	}
	var out []SourceDigest
	for _, part := range parts {
		eq := strings.IndexByte(part, '=')
		if eq <= 0 || eq == len(part)-1 {
			continue
		}
		alg := normalizeDigestAlgorithm(strings.ToLower(strings.TrimSpace(part[:eq])))
		valRaw := strings.TrimSpace(part[eq+1:])
		if alg == "" || valRaw == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(valRaw)
		if err != nil {
			continue
		}
		d := SourceDigest{Algorithm: alg, Hex: hex.EncodeToString(decoded)}
		if !ValidSourceDigest(d) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func normalizeDigestAlgorithm(a string) string {
	switch a {
	case "md5":
		return "md5"
	case "sha", "sha-1":
		return "sha1"
	case "sha-256":
		return "sha256"
	default:
		return ""
	}
}
