package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

func TestAggregatorEgressUsesBackendRedirectAuthority(t *testing.T) {
	target := newStatusServer(t, http.StatusNoContent)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	denied := &Report{}
	checkBackendEgress(context.Background(), denied, config.HTTPBackend{Name: "aggregator", URL: source.URL})
	if len(denied.Checks) != 1 || denied.Checks[0].Status != StatusFail {
		t.Fatalf("doctor followed an unapproved backend redirect: %+v", denied.Checks)
	}

	approved := &Report{}
	checkBackendEgress(context.Background(), approved, config.HTTPBackend{
		Name: "aggregator", URL: source.URL, RedirectOrigins: []string{target},
	})
	if len(approved.Checks) != 1 || approved.Checks[0].Status != StatusOK {
		t.Fatalf("doctor rejected approved backend redirect: %+v", approved.Checks)
	}
}
