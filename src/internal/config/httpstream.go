package config

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// HTTP stream backend configuration (HTTP-STREAM-MASTER-PLAN.md D5):
// backends are NAMED INSTANCES following the provider pattern.
//
// Env surface (sealed-secrets-first via Config.Lookup):
//
//	HARRBOR_HTTP_BACKENDS                    ordered comma-separated backend names
//	HARRBOR_HTTPBACKEND_<CANONICAL_NAME>_URL   base URL (required for every
//	                                           type except generic)
//	HARRBOR_HTTPBACKEND_<CANONICAL_NAME>_PUBLIC_URL
//	                                           optional client-facing origin
//	HARRBOR_HTTPBACKEND_<CANONICAL_NAME>_TYPE  protocol handler type
//	                                           (H2: "ia"; H3 adds omss/stremio +
//	                                           autodetection when unset; HR2.5
//	                                           adds "generic", never autodetected)
//	HARRBOR_HTTPBACKEND_<CANONICAL_NAME>_DESCRIPTORS_FILE
//	                                           absolute path to the typed
//	                                           descriptor list (required, and
//	                                           only meaningful, when TYPE=generic)
//	HARRBOR_HTTPBACKEND_<CANONICAL_NAME>_REDIRECT_ORIGINS
//	                                           optional comma-separated exact
//	                                           metadata redirect origins
//
// The normalized unique name (lowercase, trimmed) is the immutable persisted
// backend_id inside resolve keys — renaming a backend orphans its items'
// keys, exactly like renaming an NNTP provider. Priority = list order with
// stable-ID tie-break (D5).

// HTTPBackend is one configured HTTP stream backend instance.
type HTTPBackend struct {
	// Name is the normalized unique backend name (== persisted backend_id).
	Name string
	// URL is the backend base URL (scheme+host[+path], no trailing slash).
	URL string
	// PublicURL is the optional client-facing origin emitted by a backend
	// reached through a different internal URL. RX-2.3 uses it only for exact
	// owned-route origin validation.
	PublicURL string
	// Type is an explicit read-only protocol override. Empty means detect at
	// startup and use the persisted detector result (HS-3.4/D6).
	Type string
	// DescriptorsFile is the absolute path to the typed generic-source
	// descriptor list (HR2.5). Set only when Type == "generic"; the
	// operator-authored file itself is read live at Search/Resolve time,
	// never persisted or cached beyond the process.
	DescriptorsFile string
	// RedirectOrigins is the bounded set of additional exact origins that
	// this backend's metadata client may follow. Same-origin redirects need
	// no entry. Values contain no credentials, paths, queries, or fragments.
	RedirectOrigins []string
}

var backendNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

const maxHTTPBackendRedirectOrigins = 8

// ParseHTTPBackendRedirectOrigins validates and canonicalizes the public,
// non-secret per-backend redirect authority list.
func ParseHTTPBackendRedirectOrigins(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := make(map[string]bool)
	origins := make([]string, 0, maxHTTPBackendRedirectOrigins)
	for _, part := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(part)
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("redirect origin must be an absolute http(s) origin without userinfo, path, query, or fragment")
		}
		origin = strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
		if seen[origin] {
			continue
		}
		seen[origin] = true
		origins = append(origins, origin)
		if len(origins) > maxHTTPBackendRedirectOrigins {
			return nil, fmt.Errorf("at most %d redirect origins may be configured per HTTP backend", maxHTTPBackendRedirectOrigins)
		}
	}
	return origins, nil
}

// canonicalBackendEnv converts a normalized backend name to the env-segment
// form: uppercase, every non-alphanumeric rune → underscore (mirrors the
// HARRBOR_PROVIDER_<NAME>_* convention).
func canonicalBackendEnv(name string) string {
	up := strings.ToUpper(name)
	return strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, up)
}

// applyHTTPBackends parses the named-backend list. Config errors are fatal
// at startup (reject empty/dupe names, missing/invalid URL, unknown type)
// so a broken lane never boots half-configured (D5).
func applyHTTPBackends(cfg *Config) error {
	raw, set := providerListValue(cfg, "HARRBOR_HTTP_BACKENDS")
	if !set || strings.TrimSpace(raw) == "" {
		cfg.HTTPStream.Backends = nil
		return nil
	}
	seen := map[string]bool{}
	var backends []HTTPBackend
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if !backendNameRe.MatchString(name) {
			return fmt.Errorf("http backend name %q invalid (lowercase alphanumeric/_/- only)", name)
		}
		if seen[name] {
			return fmt.Errorf("http backend name %q duplicated after normalization", name)
		}
		seen[name] = true

		env := canonicalBackendEnv(name)
		btype, _ := cfg.Lookup("HARRBOR_HTTPBACKEND_" + env + "_TYPE")
		btype = strings.ToLower(strings.TrimSpace(btype))
		switch btype {
		case "", "ia", "omss", "stremio", "generic":
		default:
			return fmt.Errorf("http backend %q has unsupported type %q (supported: ia, omss, stremio, generic)", name, btype)
		}

		baseURL := ""
		if btype != "generic" {
			baseURL, _ = cfg.Lookup("HARRBOR_HTTPBACKEND_" + env + "_URL")
			baseURL = strings.TrimSpace(baseURL)
			if baseURL == "" {
				return fmt.Errorf("http backend %q missing HARRBOR_HTTPBACKEND_%s_URL", name, env)
			}
			u, err := url.Parse(baseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("http backend %q URL must be absolute http(s): %q", name, baseURL)
			}
			if u.User != nil {
				// D11: no URL userinfo anywhere in the lane.
				return fmt.Errorf("http backend %q URL must not embed credentials", name)
			}
		}
		publicURL, _ := cfg.Lookup("HARRBOR_HTTPBACKEND_" + env + "_PUBLIC_URL")
		publicURL = strings.TrimSpace(publicURL)
		if publicURL != "" {
			public, parseErr := url.Parse(publicURL)
			if parseErr != nil || (public.Scheme != "http" && public.Scheme != "https") || public.Host == "" ||
				public.User != nil || public.RawQuery != "" || public.Fragment != "" || (public.Path != "" && public.Path != "/") {
				return fmt.Errorf("http backend %q public URL must be an absolute http(s) origin without userinfo, path, query, or fragment", name)
			}
			publicURL = strings.TrimRight(publicURL, "/")
		}
		redirectOriginsRaw, _ := cfg.Lookup("HARRBOR_HTTPBACKEND_" + env + "_REDIRECT_ORIGINS")
		redirectOrigins, err := ParseHTTPBackendRedirectOrigins(redirectOriginsRaw)
		if err != nil {
			return fmt.Errorf("http backend %q redirect origins invalid: %w", name, err)
		}

		var descriptorsFile string
		if btype == "generic" {
			descriptorsFile, _ = cfg.Lookup("HARRBOR_HTTPBACKEND_" + env + "_DESCRIPTORS_FILE")
			descriptorsFile = strings.TrimSpace(descriptorsFile)
			if descriptorsFile == "" {
				return fmt.Errorf("http backend %q type generic requires HARRBOR_HTTPBACKEND_%s_DESCRIPTORS_FILE", name, env)
			}
			if !path.IsAbs(descriptorsFile) {
				return fmt.Errorf("http backend %q descriptors file must be an absolute path", name)
			}
		}

		backends = append(backends, HTTPBackend{
			Name:            name,
			URL:             strings.TrimRight(baseURL, "/"),
			PublicURL:       publicURL,
			Type:            btype,
			DescriptorsFile: descriptorsFile,
			RedirectOrigins: redirectOrigins,
		})
	}
	if len(backends) == 0 {
		return fmt.Errorf("HARRBOR_HTTP_BACKENDS set but contains no valid names")
	}
	cfg.HTTPStream.Backends = backends
	return nil
}
