package wizard

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/httpstream/generic"
	"github.com/darkharrbor/darkharrbor/internal/httpstream/ia"
	"github.com/darkharrbor/darkharrbor/internal/httpstream/omss"
	"github.com/darkharrbor/darkharrbor/internal/httpstream/stremio"
)

const (
	maxConfigureBackends = 16
	configureDialTimeout = 5 * time.Second

	// defaultIABaseURL is the well-known public Internet Archive endpoint.
	// ia.go's own doc comment already claimed this was "the wizard's
	// suggested example" -- this constant is what makes that true. IA needs
	// no credentials, so defaulting it (rather than requiring the operator
	// to already know and type this exact URL) matches the published
	// README/CONFIGURATION description of IA as a zero-credential source.
	defaultIABaseURL = "https://archive.org"
)

type backendDialer func(context.Context, config.HTTPBackend) error

func configureHTTPBackends(p *configurePrompter, cfg *config.Config, values, secretUpdates map[string]string, dial backendDialer, required bool) error {
	if !required {
		edit, err := p.yesNo("Configure HTTP stream sources", false)
		if err != nil || !edit {
			return err
		}
	} else {
		fmt.Fprintln(p.out, "Configure the HTTP stream sources selected in the reviewed plan.")
	}
	allowPrivate, err := p.yesNo("Allow selected HTTP sources on private/container networks", cfg.HTTPStream.AllowPrivateSourceCIDRs)
	if err != nil {
		return err
	}
	values["HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES"] = fmt.Sprint(allowPrivate)
	currentNames := make([]string, 0, len(cfg.HTTPStream.Backends))
	currentByName := make(map[string]config.HTTPBackend, len(cfg.HTTPStream.Backends))
	for _, backend := range cfg.HTTPStream.Backends {
		currentNames = append(currentNames, backend.Name)
		currentByName[backend.Name] = backend
	}
	raw, err := p.text("HTTP sources (comma-separated names in search priority order; 'none' disables)", strings.Join(currentNames, ","))
	if err != nil {
		return err
	}
	names, err := parseConfigureBackends(raw)
	if err != nil {
		return err
	}
	clearBackendTuning(values)
	if len(names) == 0 {
		delete(values, "HARRBOR_HTTP_BACKENDS")
		values["HARRBOR_HTTP_STREAM_ENABLED"] = "false"
		return nil
	}
	values["HARRBOR_HTTP_BACKENDS"] = strings.Join(names, ",")
	values["HARRBOR_HTTP_STREAM_ENABLED"] = "true"

	backends := make([]config.HTTPBackend, 0, len(names))
	for _, name := range names {
		current := currentByName[name]
		prefix := "HARRBOR_HTTPBACKEND_" + configureBackendEnv(name) + "_"
		backendType, err := p.text(name+" type (ia|omss|stremio|generic)", current.Type)
		if err != nil {
			return err
		}
		backendType = strings.ToLower(strings.TrimSpace(backendType))
		if backendType == "" {
			return fmt.Errorf("%s type is required; detection is not guessed by the wizard", name)
		}
		values[prefix+"TYPE"] = backendType

		backendURL := ""
		if backendType != "generic" {
			if backendType == "ia" && current.URL == "" {
				fmt.Fprintln(p.out, "  ℹ Leave blank to use the default Internet Archive endpoint (https://archive.org)")
			}
			enteredURL, changed, err := p.secret(name+" base URL", current.URL != "")
			if err != nil {
				return err
			}
			backendURL = current.URL
			if changed {
				parsed, parseErr := url.Parse(enteredURL)
				if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
					return fmt.Errorf("%s URL must be an absolute http(s) URL", name)
				}
				backendURL = enteredURL
				secretUpdates[prefix+"URL"] = enteredURL
				delete(values, prefix+"URL")
			}
			if backendURL == "" && backendType == "ia" {
				backendURL = defaultIABaseURL
				secretUpdates[prefix+"URL"] = backendURL
				delete(values, prefix+"URL")
			}
			if backendURL == "" {
				return fmt.Errorf("%s URL is required", name)
			}
		}

		publicURL := current.PublicURL
		if backendType == "stremio" {
			publicURL, err = p.text(name+" public/client origin (blank uses base URL)", publicURL)
			if err != nil {
				return err
			}
			publicURL = strings.TrimSpace(publicURL)
			if publicURL != "" {
				values[prefix+"PUBLIC_URL"] = publicURL
			}
		}

		descriptor := current.DescriptorsFile
		if backendType == "generic" {
			fmt.Fprintln(p.out, "  generic sources use one private typed-descriptor file; no unused backend URL is requested")
			descriptor, err = p.text(name+" descriptor file", descriptor)
			if err != nil {
				return err
			}
			values[prefix+"DESCRIPTORS_FILE"] = descriptor
		}
		redirectOriginsRaw, err := p.text(name+" additional metadata redirect origins (usually blank; exact origins only)", strings.Join(current.RedirectOrigins, ","))
		if err != nil {
			return err
		}
		redirectOrigins, err := config.ParseHTTPBackendRedirectOrigins(redirectOriginsRaw)
		if err != nil {
			return fmt.Errorf("%s redirect origins: %w", name, err)
		}
		if len(redirectOrigins) > 0 {
			values[prefix+"REDIRECT_ORIGINS"] = strings.Join(redirectOrigins, ",")
		}
		backends = append(backends, config.HTTPBackend{
			Name: name, URL: backendURL, PublicURL: publicURL, Type: backendType, DescriptorsFile: descriptor,
			RedirectOrigins: redirectOrigins,
		})
	}
	if err := config.ValidateTuning(values); err != nil {
		return err
	}
	if dial == nil {
		dialCfg := *cfg
		dialCfg.HTTPStream.AllowPrivateSourceCIDRs = allowPrivate
		dial = func(ctx context.Context, backend config.HTTPBackend) error {
			return dialHTTPBackend(ctx, &dialCfg, backend)
		}
	}
	for i := range backends {
		backend := &backends[i]
		for {
			ctx, cancel := context.WithTimeout(context.Background(), configureDialTimeout)
			dialErr := dial(ctx, *backend)
			cancel()
			if dialErr == nil {
				break
			}
			origin, approvalRequired := httpstream.RedirectOrigin(dialErr)
			if !approvalRequired {
				return fmt.Errorf("source %s dial test failed: %s", backend.Name, httpstream.Sanitize(dialErr))
			}
			origins, err := config.ParseHTTPBackendRedirectOrigins(strings.Join(append(append([]string(nil), backend.RedirectOrigins...), origin), ","))
			if err != nil || len(origins) == len(backend.RedirectOrigins) {
				return fmt.Errorf("source %s redirect did not produce a new valid origin", backend.Name)
			}
			approved, err := p.yesNo(fmt.Sprintf("Allow %s metadata redirects to exact origin %s", backend.Name, origin), false)
			if err != nil {
				return err
			}
			if !approved {
				return fmt.Errorf("source %s exact-origin redirect was not approved", backend.Name)
			}
			backend.RedirectOrigins = origins
			prefix := "HARRBOR_HTTPBACKEND_" + configureBackendEnv(backend.Name) + "_"
			values[prefix+"REDIRECT_ORIGINS"] = strings.Join(origins, ",")
		}
		fmt.Fprintf(p.out, "  source %s dial test passed\n", backend.Name)
	}
	return nil
}

func collectInitialHTTPConfiguration(out io.Writer, dial backendDialer) (map[string]string, map[string]string, error) {
	p := configurePrompter{reader: stdinReader, out: out}
	values := map[string]string{}
	secrets := map[string]string{}
	if err := configureHTTPBackends(&p, &config.Config{}, values, secrets, dial, true); err != nil {
		return nil, nil, err
	}
	if values["HARRBOR_HTTP_STREAM_ENABLED"] != "true" || strings.TrimSpace(values["HARRBOR_HTTP_BACKENDS"]) == "" {
		return nil, nil, fmt.Errorf("HTTP was selected but no HTTP source was configured")
	}
	return values, secrets, nil
}

func printInitialHTTPConfiguration(out io.Writer, values, secrets map[string]string) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fmt.Fprintln(out, "\n  HTTP settings to include in the first-run transaction:")
	for _, key := range keys {
		fmt.Fprintf(out, "    %s=%s\n", key, values[key])
	}
	secretKeys := make([]string, 0, len(secrets))
	for key := range secrets {
		secretKeys = append(secretKeys, key)
	}
	sort.Strings(secretKeys)
	for _, key := range secretKeys {
		fmt.Fprintf(out, "    %s=[hidden; will be sealed]\n", key)
	}
}

func persistInitialHTTPConfiguration(cfgDir string, staged map[string]string) error {
	if len(staged) == 0 {
		return nil
	}
	path := filepath.Join(cfgDir, filepath.Base(config.DefaultTuningPath))
	values, err := config.ReadTuning(path)
	if err != nil {
		return err
	}
	for key, value := range staged {
		values[key] = value
	}
	return config.WriteTuning(path, values)
}

func dialHTTPBackend(ctx context.Context, cfg *config.Config, backend config.HTTPBackend) error {
	policy := httpstream.NewSecurityPolicy(cfg.HTTPStream.AllowPrivateSourceCIDRs)
	client := httpstream.NewHTTPClient(policy, httpstream.TrustBackend, httpstream.TransportMetadata, backend.RedirectOrigins...)
	var health interface{ Healthy(context.Context) error }
	switch backend.Type {
	case "ia":
		health = ia.New(backend.Name, backend.URL, client)
	case "omss":
		aggregate := httpstream.NewHTTPClient(policy, httpstream.TrustBackend, httpstream.TransportAggregateMetadata, backend.RedirectOrigins...)
		health = omss.New(backend.Name, backend.URL, aggregate, nil)
	case "stremio":
		health = stremio.New(backend.Name, backend.URL, client, nil)
	case "generic":
		health = generic.New(backend.Name, backend.DescriptorsFile, client)
	default:
		return fmt.Errorf("unsupported backend type")
	}
	return health.Healthy(ctx)
}

func parseConfigureBackends(raw string) ([]string, error) {
	if strings.EqualFold(strings.TrimSpace(raw), "none") || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var names []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" || len(name) > 64 {
			return nil, fmt.Errorf("HTTP source name must contain 1 through 64 bytes")
		}
		for i, r := range name {
			if !(unicode.IsLower(r) || unicode.IsDigit(r) || (i > 0 && (r == '_' || r == '-'))) {
				return nil, fmt.Errorf("invalid HTTP source name")
			}
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate HTTP source name")
		}
		seen[name] = true
		names = append(names, name)
		if len(names) > maxConfigureBackends {
			return nil, fmt.Errorf("at most %d HTTP sources may be configured", maxConfigureBackends)
		}
	}
	return names, nil
}

func clearBackendTuning(values map[string]string) {
	for key := range values {
		if strings.HasPrefix(key, "HARRBOR_HTTPBACKEND_") {
			delete(values, key)
		}
	}
}

func configureBackendEnv(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToUpper(r)
		}
		return '_'
	}, name)
}
