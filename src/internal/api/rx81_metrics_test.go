package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
)

func TestRX81MetricsHandlerExposesFixedVocabulary(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreForDebugResolve(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })
	if created, err := st.CreatePlaybackProposal(ctx, playbackcoverage.Proposal{
		RepresentationID: "http:representation", ItemID: "item", FileID: "file",
		ExtentKind: playbackcoverage.ExtentDeclaredBytes, Delivered: 1, Total: 2,
		Threshold: 0.5, CreatedAt: now.Add(-2 * time.Minute),
	}); err != nil || !created {
		t.Fatalf("create proposal: created=%v err=%v", created, err)
	}
	if _, _, err := st.BeginReactiveCommit(ctx, "http:representation", "item", "file", "supervised", now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkReactiveCommit(ctx, "http:representation", "identity_year_mismatch", now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	(&Server{store: st}).handleMetrics(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		`darkharrbor_reactive_commits{mode="auto",period="24h"} 0`,
		`darkharrbor_reactive_pending_depth{reason="positive_mismatch"} 1`,
		`darkharrbor_reactive_pending_oldest_age_seconds{reason="positive_mismatch"} 120`,
		`darkharrbor_reactive_identity_outcomes_total{outcome="positive_mismatch"} 1`,
		"darkharrbor_reactive_promotion_headroom_available 0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
	if strings.Contains(body, "identity_year_mismatch") {
		t.Fatal("raw pending reason escaped fixed metric vocabulary")
	}
}
