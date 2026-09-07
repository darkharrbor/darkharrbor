package httpstream

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

var forbiddenTransientHeaders = map[string]struct{}{
	"Connection":          {},
	"Content-Length":      {},
	"Host":                {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Range":               {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// ValidateResolvedFile validates and canonicalizes the transient HS-3.1
// representation before it is used for preflight or playback. It deliberately
// does not persist or log any URL, header, expiry, or refresh-context value.
func ValidateResolvedFile(in ResolvedFile, now time.Time) (ResolvedFile, error) {
	in.Selector = strings.TrimSpace(in.Selector)
	if in.Selector == "" {
		return ResolvedFile{}, NewError(ClassUpstreamMalformed, "resolved file has no stable selector")
	}
	if strings.TrimSpace(in.URL) == "" {
		return ResolvedFile{}, NewError(ClassNoSource, "resolved file has no source URL")
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return ResolvedFile{}, NewError(ClassUpstreamMalformed, "resolved file has invalid source URL")
	}
	if in.Size < 0 {
		return ResolvedFile{}, NewError(ClassUpstreamMalformed, "resolved file has negative size")
	}
	if !in.ExpiresAt.IsZero() && !now.IsZero() && !in.ExpiresAt.After(now) {
		return ResolvedFile{}, NewError(ClassBackendUnavailable, "resolved file is already expired")
	}

	in.RequestHeaders, err = validateTransientHeaders(in.RequestHeaders)
	if err != nil {
		return ResolvedFile{}, err
	}
	in.ResponseHeaders, err = validateTransientHeaders(in.ResponseHeaders)
	if err != nil {
		return ResolvedFile{}, err
	}
	// HR2.7: a malformed/oversized digest is dropped, never fatal to an
	// otherwise-valid representation -- supplementary evidence never gates
	// playback (D13-style: this is proof-graph bookkeeping, not a core
	// resolve field like Selector/URL above).
	in.Digests = filterValidDigests(in.Digests)
	return in, nil
}

const maxResolvedFileDigests = 4

func filterValidDigests(in []SourceDigest) []SourceDigest {
	if len(in) == 0 {
		return nil
	}
	out := make([]SourceDigest, 0, len(in))
	for _, d := range in {
		if len(out) >= maxResolvedFileDigests {
			break
		}
		if ValidSourceDigest(d) {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ValidateResolvedFiles validates a handler result set before selector matching.
func ValidateResolvedFiles(files []ResolvedFile, now time.Time) ([]ResolvedFile, error) {
	out := make([]ResolvedFile, 0, len(files))
	for _, file := range files {
		validated, err := ValidateResolvedFile(file, now)
		if err != nil {
			return nil, err
		}
		out = append(out, validated)
	}
	return out, nil
}

func validateTransientHeaders(in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for rawName, rawValue := range in {
		name := http.CanonicalHeaderKey(strings.TrimSpace(rawName))
		if name == "" || strings.ContainsAny(rawName, "\r\n:") {
			return nil, NewError(ClassUpstreamMalformed, "resolved file contains invalid header name")
		}
		if _, denied := forbiddenTransientHeaders[name]; denied {
			return nil, NewError(ClassUpstreamMalformed, "resolved file contains forbidden transport header")
		}
		value := strings.TrimSpace(rawValue)
		if strings.ContainsAny(value, "\r\n") {
			return nil, NewError(ClassUpstreamMalformed, "resolved file contains invalid header value")
		}
		out[name] = value
	}
	return out, nil
}
