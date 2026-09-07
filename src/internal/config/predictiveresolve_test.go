package config

import (
	"strings"
	"testing"
)

// TestHTTPStreamPredictiveResolveDefaultAndOverride covers HR3.2's config
// surface: predictive midstream re-resolve is enabled by default with a
// 5s floor / 60s cap lead, both overridable independently.
func TestHTTPStreamPredictiveResolveDefaultAndOverride(t *testing.T) {
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HTTPStream.PredictiveResolveEnabled {
		t.Fatal("PredictiveResolveEnabled default = false, want true")
	}
	if cfg.HTTPStream.PredictiveResolveMinLeadMS != 5000 {
		t.Fatalf("PredictiveResolveMinLeadMS default = %d, want 5000", cfg.HTTPStream.PredictiveResolveMinLeadMS)
	}
	if cfg.HTTPStream.PredictiveResolveMaxLeadMS != 60000 {
		t.Fatalf("PredictiveResolveMaxLeadMS default = %d, want 60000", cfg.HTTPStream.PredictiveResolveMaxLeadMS)
	}

	t.Setenv("HARRBOR_HTTP_PREDICTIVE_RESOLVE_ENABLED", "false")
	t.Setenv("HARRBOR_HTTP_PREDICTIVE_RESOLVE_MIN_LEAD_MS", "10000")
	t.Setenv("HARRBOR_HTTP_PREDICTIVE_RESOLVE_MAX_LEAD_MS", "30000")
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.HTTPStream.PredictiveResolveEnabled {
		t.Fatal("PredictiveResolveEnabled override did not take effect")
	}
	if cfg2.HTTPStream.PredictiveResolveMinLeadMS != 10000 {
		t.Fatalf("PredictiveResolveMinLeadMS override = %d, want 10000", cfg2.HTTPStream.PredictiveResolveMinLeadMS)
	}
	if cfg2.HTTPStream.PredictiveResolveMaxLeadMS != 30000 {
		t.Fatalf("PredictiveResolveMaxLeadMS override = %d, want 30000", cfg2.HTTPStream.PredictiveResolveMaxLeadMS)
	}
}

func TestHTTPStreamPredictiveResolveRejectsNegativeOrInvertedLeads(t *testing.T) {
	base := func() {
		t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
		t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
		t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
		t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))
	}

	t.Run("negative min", func(t *testing.T) {
		base()
		t.Setenv("HARRBOR_HTTP_PREDICTIVE_RESOLVE_MIN_LEAD_MS", "-1")
		if _, err := Load(); err == nil {
			t.Fatal("expected Load to reject a negative min lead")
		}
	})
	t.Run("negative max", func(t *testing.T) {
		base()
		t.Setenv("HARRBOR_HTTP_PREDICTIVE_RESOLVE_MAX_LEAD_MS", "-1")
		if _, err := Load(); err == nil {
			t.Fatal("expected Load to reject a negative max lead")
		}
	})
	t.Run("max below min", func(t *testing.T) {
		base()
		t.Setenv("HARRBOR_HTTP_PREDICTIVE_RESOLVE_MIN_LEAD_MS", "10000")
		t.Setenv("HARRBOR_HTTP_PREDICTIVE_RESOLVE_MAX_LEAD_MS", "5000")
		if _, err := Load(); err == nil {
			t.Fatal("expected Load to reject max lead below min lead")
		}
	})
}
