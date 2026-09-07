package config

import (
	"os"
	"strings"
)

//
// Env surface:
//
//	HARRBOR_PROVIDERS         comma-separated debrid provider names
//	                          (no default; unset selects none)
//	HARRBOR_USENET_PROVIDERS  comma-separated usenet provider names
//	                          (no default; unset selects none)
//
// Per-provider configuration uses HARRBOR_PROVIDER_<NAME>_* keys resolved
// through Config.Lookup (sealed secrets first, then env); the default
// providers also keep reading their legacy credential keys (HARRBOR_TORBOX_*,
// HARRBOR_NNTP_*), but credentials alone do not select a provider.

// ProvidersConfig is the parsed provider selection.
type ProvidersConfig struct {
	// Providers is the ordered debrid provider list.
	Providers []string
	// ProvidersExplicit is true when HARRBOR_PROVIDERS was set (even empty —
	// an explicitly empty selection is a refuse-start condition).
	ProvidersExplicit bool
	// UsenetProviders is the ordered usenet provider list.
	UsenetProviders []string
	// UsenetExplicit is true when HARRBOR_USENET_PROVIDERS was set.
	UsenetExplicit bool
}

func applyProviderSelection(cfg *Config) {
	cfg.Providers.Providers, cfg.Providers.ProvidersExplicit =
		parseProviderList(cfg, "HARRBOR_PROVIDERS")
	cfg.Providers.UsenetProviders, cfg.Providers.UsenetExplicit =
		parseProviderList(cfg, "HARRBOR_USENET_PROVIDERS")
}

func parseProviderList(cfg *Config, envKey string) (names []string, explicit bool) {
	raw, set := providerListValue(cfg, envKey)
	if !set {
		return nil, false
	}
	if strings.EqualFold(strings.TrimSpace(raw), "none") {
		return nil, false
	}
	for _, part := range strings.Split(raw, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			names = append(names, p)
		}
	}
	return names, true
}

func providerListValue(cfg *Config, key string) (string, bool) {
	if cfg != nil {
		if v := strings.TrimSpace(cfg.tuning[key]); v != "" {
			return v, true
		}
		if cfg.Secrets != nil {
			if v, ok := cfg.Secrets.values[key]; ok {
				return strings.TrimSpace(v.Value()), true
			}
		}
	}
	raw, ok := os.LookupEnv(key)
	return raw, ok
}
