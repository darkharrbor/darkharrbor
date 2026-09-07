package config

import (
	"reflect"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/util"
)

func TestApplyProviderSelectionReadsSealedUsenetProviders(t *testing.T) {
	cfg := defaultConfig()
	cfg.applySecrets(&SecretStore{values: map[string]util.Redacted{
		"HARRBOR_USENET_PROVIDERS": util.Redacted("torbox,newshosting"),
	}})

	applyProviderSelection(&cfg)

	want := []string{"torbox", "newshosting"}
	if !reflect.DeepEqual(cfg.Providers.UsenetProviders, want) {
		t.Fatalf("UsenetProviders = %v, want %v", cfg.Providers.UsenetProviders, want)
	}
	if !cfg.Providers.UsenetExplicit {
		t.Fatal("UsenetExplicit = false, want true")
	}
}

func TestApplyProviderSelectionAllowsExplicitNone(t *testing.T) {
	cfg := defaultConfig()
	cfg.applySecrets(&SecretStore{values: map[string]util.Redacted{
		"HARRBOR_PROVIDERS": util.Redacted("none"),
	}})

	applyProviderSelection(&cfg)

	if len(cfg.Providers.Providers) != 0 || cfg.Providers.ProvidersExplicit {
		t.Fatalf("providers = %v explicit=%v, want intentional empty selection", cfg.Providers.Providers, cfg.Providers.ProvidersExplicit)
	}
}

func TestApplyProviderSelectionHasNoImplicitProvider(t *testing.T) {
	cfg := defaultConfig()

	applyProviderSelection(&cfg)

	if len(cfg.Providers.Providers) != 0 || cfg.Providers.ProvidersExplicit {
		t.Fatalf("debrid providers = %v explicit=%v, want unselected", cfg.Providers.Providers, cfg.Providers.ProvidersExplicit)
	}
	if len(cfg.Providers.UsenetProviders) != 0 || cfg.Providers.UsenetExplicit {
		t.Fatalf("usenet providers = %v explicit=%v, want unselected", cfg.Providers.UsenetProviders, cfg.Providers.UsenetExplicit)
	}
}
