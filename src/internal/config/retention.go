package config

import (
	"log/slog"
	"strconv"
	"strings"
)

// MaxProviderRetentionDays is the upper bound accepted for any
// HARRBOR_PROVIDER_<NAME>_RETENTION_DAYS value; matches the tuning-file
// validator's own bound (see tuning.go) so both paths agree.
const MaxProviderRetentionDays = 36500

// ProviderRetentionDays reads only configured provider retention horizons
// for the given provider names. Missing, malformed, zero, or excessive
// values safely abstain (the provider is simply absent from the returned
// map) rather than guessing; the raw invalid value is never logged. This is
// the single shared owner for HARRBOR_PROVIDER_<NAME>_RETENTION_DAYS
// parsing -- reused by NNTP per-segment routing (NS-8.4) and by
// search-time retention annotation (NS-9.1).
func ProviderRetentionDays(cfg *Config, providerNames []string, log *slog.Logger) map[string]int {
	retentionDays := make(map[string]int, len(providerNames))
	for _, providerName := range providerNames {
		key := "HARRBOR_PROVIDER_" + strings.ToUpper(providerName) + "_RETENTION_DAYS"
		raw, ok := cfg.Lookup(key)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		days, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || days < 1 || days > MaxProviderRetentionDays {
			if log != nil {
				log.Warn("config: ignoring invalid retention horizon", "provider", providerName)
			}
			continue
		}
		retentionDays[providerName] = days
	}
	return retentionDays
}

// DeepestProviderRetentionDays returns the maximum configured retention
// horizon across the given provider names, and whether any horizon is
// configured at all. With zero configured horizons, callers must abstain
// from any retention-based decision rather than assuming an implicit
// default depth.
func DeepestProviderRetentionDays(cfg *Config, providerNames []string, log *slog.Logger) (days int, ok bool) {
	for _, d := range ProviderRetentionDays(cfg, providerNames, log) {
		if d > days {
			days = d
			ok = true
		}
	}
	return days, ok
}
