package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

func TestHR74HealthAndMetricsExposeSafeTelemetry(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	scores := httpstream.NewOriginScorer(func() time.Time { return now })
	scores.Observe("opaque-source", httpstream.OriginObservation{
		FirstByte: 25 * time.Millisecond, RangeSuccess: true, RetryStage: "current",
	})
	gov := accountgov.New("test")
	gov.SetCapacity(HTTPSourceGovOp, 1)
	held, err := gov.Acquire(context.Background(), HTTPSourceGovOp, accountgov.PriorityPlayback, "held")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, _ = gov.Acquire(ctx, HTTPSourceGovOp, accountgov.PriorityRecovery, "waiter")
	held.Release()

	s := &Server{startedAt: now.Add(-time.Minute), nowFn: func() time.Time { return now }, httpOriginScores: scores, httpGov: gov}

	health := httptest.NewRecorder()
	s.handleHealth(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(health.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	sources, ok := body["http_origin_sources"].([]any)
	if !ok || len(sources) != 1 {
		t.Fatalf("health source telemetry = %#v", body["http_origin_sources"])
	}
	if _, ok := body["governor_denials"]; !ok {
		t.Fatal("health missing governor denial telemetry")
	}

	metrics := httptest.NewRecorder()
	s.handleMetrics(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{"darkharrbor_source_health_score", "darkharrbor_governor_denials_total"} {
		if !strings.Contains(metrics.Body.String(), want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
	if strings.Contains(health.Body.String(), "opaque-source") || strings.Contains(metrics.Body.String(), "opaque-source") {
		t.Fatal("raw source identity escaped telemetry")
	}
}
