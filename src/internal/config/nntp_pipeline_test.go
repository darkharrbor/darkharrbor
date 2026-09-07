package config

import (
	"strings"
	"testing"
)

func TestNNTPPipelineDepthConfig(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("HARRBOR_NNTP_PIPELINE_DEPTH", "")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.NNTP.PipelineDepth != 4 || len(cfg.StartupWarnings) != 0 {
			t.Fatalf("depth=%d warnings=%v, want 4 and none", cfg.NNTP.PipelineDepth, cfg.StartupWarnings)
		}
	})
	t.Run("serial rollback", func(t *testing.T) {
		t.Setenv("HARRBOR_NNTP_PIPELINE_DEPTH", "1")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.NNTP.PipelineDepth != 1 || len(cfg.StartupWarnings) != 0 {
			t.Fatalf("depth=%d warnings=%v, want 1 and none", cfg.NNTP.PipelineDepth, cfg.StartupWarnings)
		}
	})
	t.Run("invalid warns and defaults", func(t *testing.T) {
		t.Setenv("HARRBOR_NNTP_PIPELINE_DEPTH", "unknown")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.NNTP.PipelineDepth != 4 || len(cfg.StartupWarnings) != 1 ||
			!strings.Contains(cfg.StartupWarnings[0], "HARRBOR_NNTP_PIPELINE_DEPTH") {
			t.Fatalf("depth=%d warnings=%v, want 4 and one warning", cfg.NNTP.PipelineDepth, cfg.StartupWarnings)
		}
	})
	t.Run("oversize clamps", func(t *testing.T) {
		t.Setenv("HARRBOR_NNTP_PIPELINE_DEPTH", "100")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.NNTP.PipelineDepth != 32 || len(cfg.StartupWarnings) != 1 {
			t.Fatalf("depth=%d warnings=%v, want 32 and one warning", cfg.NNTP.PipelineDepth, cfg.StartupWarnings)
		}
	})
}
