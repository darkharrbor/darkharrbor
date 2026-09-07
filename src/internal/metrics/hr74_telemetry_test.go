package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestHR74TelemetryRenderUsesOnlySafeBoundedLabels(t *testing.T) {
	var out bytes.Buffer
	err := Render(&out, Snapshot{
		SourceHealth: []SourceHealth{{
			Lane: "http", Source: "0123456789abcdef", Health: "healthy",
			RetryStage: "current", Score: 12, FirstByteMillis: 50,
			TokenLifetimeSecs: 300, ExpiryCount: 1, Observations: 2,
		}},
		GovernorDenials: []GovernorDenial{{
			Lane: "torrent", Priority: "playback", Reason: "deadline", Count: 1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"darkharrbor_source_health_score",
		`source="0123456789abcdef"`,
		`retry_stage="current"`,
		"darkharrbor_governor_denials_total",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q", want)
		}
	}
}
