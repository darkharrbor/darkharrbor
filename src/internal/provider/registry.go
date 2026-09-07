package provider

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Registry + config selection (plan §3.1.5). Factories are registered by
// cmd/darkharrbor wiring (keeping this package free of torbox/nntp imports);
// Build resolves the configured selections.
//
// Refuse-start rules (precise errors, gate scenario R-1):
//   - unknown provider name        -> error
//   - duplicate provider name      -> error
//   - explicitly empty selection   -> error
//   - factory returns a non-ErrNotConfigured error -> error
//
// Back-compat rule (gate scenario R-2): a selected provider whose factory
// reports ErrNotConfigured is skipped with a warning — this preserves the
// pre-registry boot behavior where an unset HARRBOR_NNTP_HOST simply left
// the NNTP path unconfigured. Legacy env vars keep working as the default
// providers' configuration.

// ErrNotConfigured is returned by a Factory when its provider's required
// configuration is absent; Build skips the provider with a warning.
var ErrNotConfigured = fmt.Errorf("provider not configured")

// LookupFunc resolves a configuration key (secrets-aware; sealed values win
// over plaintext environment per F-D).
type LookupFunc func(key string) (string, bool)

// BuildContext is handed to factories.
type BuildContext struct {
	// Name is the registry name the provider is being built under.
	Name string
	// Lookup resolves configuration keys (HARRBOR_PROVIDER_<NAME>_* and
	// legacy keys), secrets-aware.
	Lookup LookupFunc
	// Log is the process logger (may be nil).
	Log *slog.Logger
}

type Factory func(bc BuildContext) (Provider, error)

type UsenetFactory func(bc BuildContext) (UsenetProvider, error)

// Registry holds registered provider factories.
type Registry struct {
	providers map[string]Factory
	usenet    map[string]UsenetFactory
}

func NewRegistry() *Registry {
	return &Registry{providers: map[string]Factory{}, usenet: map[string]UsenetFactory{}}
}

func (r *Registry) Register(name string, f Factory) error {
	name = normalizeName(name)
	if name == "" {
		return fmt.Errorf("provider registry: empty provider name")
	}
	if _, dup := r.providers[name]; dup {
		return fmt.Errorf("provider registry: duplicate provider registration %q", name)
	}
	r.providers[name] = f
	return nil
}

func (r *Registry) RegisterUsenet(name string, f UsenetFactory) error {
	name = normalizeName(name)
	if name == "" {
		return fmt.Errorf("provider registry: empty usenet provider name")
	}
	if _, dup := r.usenet[name]; dup {
		return fmt.Errorf("provider registry: duplicate usenet provider registration %q", name)
	}
	r.usenet[name] = f
	return nil
}

// Selection is the configured provider choice for one boot.
type Selection struct {
	// Providers is the ordered debrid-provider list (default ["torbox"]).
	Providers []string
	// ProvidersExplicit is true when HARRBOR_PROVIDERS was set (even empty).
	ProvidersExplicit bool
	// UsenetProviders is the ordered usenet list (default ["newshosting"]).
	UsenetProviders []string
	// UsenetExplicit is true when HARRBOR_USENET_PROVIDERS was set.
	UsenetExplicit bool
	// Lookup resolves configuration keys for factories.
	Lookup LookupFunc
	// Log is the process logger (may be nil).
	Log *slog.Logger
}

// Set is the built provider collection.
type Set struct {
	Providers     map[string]Provider
	ProviderOrder []string
	Usenet        map[string]UsenetProvider
	UsenetOrder   []string
}

// DefaultProvider returns the first built provider, or nil.
func (s *Set) DefaultProvider() Provider {
	if s == nil || len(s.ProviderOrder) == 0 {
		return nil
	}
	return s.Providers[s.ProviderOrder[0]]
}

// Build validates the selection and constructs the providers.
func (r *Registry) Build(sel Selection) (*Set, error) {
	set := &Set{Providers: map[string]Provider{}, Usenet: map[string]UsenetProvider{}}

	names, err := validateNames("HARRBOR_PROVIDERS", sel.Providers, sel.ProvidersExplicit, keys(r.providers))
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		f, ok := r.providers[name]
		if !ok {
			return nil, fmt.Errorf("provider registry: unknown provider %q in HARRBOR_PROVIDERS (registered: %s)", name, strings.Join(keys(r.providers), ", "))
		}
		p, err := f(BuildContext{Name: name, Lookup: sel.Lookup, Log: sel.Log})
		if err != nil {
			if isNotConfigured(err) {
				warn(sel.Log, "provider registry: provider selected but not configured; skipping", "provider", name, "reason", err.Error())
				continue
			}
			return nil, fmt.Errorf("provider registry: build provider %q: %w", name, err)
		}
		set.Providers[name] = p
		set.ProviderOrder = append(set.ProviderOrder, name)
	}

	unames, err := validateNames("HARRBOR_USENET_PROVIDERS", sel.UsenetProviders, sel.UsenetExplicit, keys(r.usenet))
	if err != nil {
		return nil, err
	}
	for _, name := range unames {
		f, ok := r.usenet[name]
		if !ok {
			return nil, fmt.Errorf("provider registry: unknown usenet provider %q in HARRBOR_USENET_PROVIDERS (registered: %s)", name, strings.Join(keys(r.usenet), ", "))
		}
		p, err := f(BuildContext{Name: name, Lookup: sel.Lookup, Log: sel.Log})
		if err != nil {
			if isNotConfigured(err) {
				warn(sel.Log, "provider registry: usenet provider selected but not configured; skipping", "provider", name, "reason", err.Error())
				continue
			}
			return nil, fmt.Errorf("provider registry: build usenet provider %q: %w", name, err)
		}
		set.Usenet[name] = p
		set.UsenetOrder = append(set.UsenetOrder, name)
	}

	return set, nil
}

func validateNames(envKey string, names []string, explicit bool, registered []string) ([]string, error) {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, raw := range names {
		n := normalizeName(raw)
		if n == "" {
			continue
		}
		if seen[n] {
			return nil, fmt.Errorf("provider registry: duplicate provider %q in %s", n, envKey)
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		if explicit {
			return nil, fmt.Errorf("provider registry: %s is explicitly empty; at least one of [%s] is required", envKey, strings.Join(registered, ", "))
		}
		// Defaulted-empty selects nothing for this family (legal: e.g. no
		// usenet providers at all).
		return nil, nil
	}
	return out, nil
}

func normalizeName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func isNotConfigured(err error) bool {
	return err != nil && (err == ErrNotConfigured || strings.Contains(err.Error(), ErrNotConfigured.Error()))
}

func keys[M ~map[string]V, V any](m M) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func warn(log *slog.Logger, msg string, args ...any) {
	if log != nil {
		log.Warn(msg, args...)
	}
}
