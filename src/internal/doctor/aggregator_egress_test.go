package doctor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/topology"
)

// The whole point of RX-0.6 is that an UNREACHABLE upstream must be
// distinguishable from an unhealthy one, and that the check must NAME the
// address it tried -- the 2026-08-21 outage was invisible precisely because
// the address looked correct from every vantage point the operator had.
func TestAggregatorEgressUnreachableNamesAddress(t *testing.T) {
	r := &Report{}
	// 203.0.113.0/24 is TEST-NET-3 (RFC 5737): guaranteed non-routable, so
	// this exercises the real transport failure path without a live network.
	checkBackendEgress(context.Background(), r, config.HTTPBackend{
		Name: "aggregator", URL: "http://203.0.113.1:9/",
	})
	if len(r.Checks) != 1 {
		t.Fatalf("expected exactly one check, got %d", len(r.Checks))
	}
	c := r.Checks[0]
	if c.Status != StatusFail {
		t.Fatalf("unreachable backend must FAIL, got %v", c.Status)
	}
	if !strings.Contains(c.Detail, "203.0.113.1:9") {
		t.Fatalf("check must name the address it tried, got: %s", c.Detail)
	}
	if !strings.Contains(c.Detail, "UNREACHABLE") {
		t.Fatalf("check must say unreachable distinctly, got: %s", c.Detail)
	}
	// It must also explain that client-reachable is not sufficient, which is
	// the misconception that made the outage take hours.
	if !strings.Contains(c.Detail, "viewing devices") {
		t.Fatalf("check must distinguish client reachability, got: %s", c.Detail)
	}
}

// A reachable backend passes on ANY status: the property under test is the
// socket path, not the application response. A 404 from a reachable host is
// not a reachability problem.
func TestAggregatorEgressReachablePassesOnAnyStatus(t *testing.T) {
	srv := newStatusServer(t, 404)
	r := &Report{}
	checkBackendEgress(context.Background(), r, config.HTTPBackend{Name: "aggregator", URL: srv})
	if len(r.Checks) != 1 || r.Checks[0].Status != StatusOK {
		t.Fatalf("reachable backend must pass regardless of status, got %+v", r.Checks)
	}
	if !strings.Contains(r.Checks[0].Detail, "socket-path check only") ||
		!strings.Contains(r.Checks[0].Detail, "aggregator delivery") {
		t.Fatalf("reachable detail must point to the aggregator delivery check: %s", r.Checks[0].Detail)
	}
}

// The check names an address, so it must not become a credential leak.
func TestAggregatorEgressRedactsPathAndQuery(t *testing.T) {
	r := &Report{}
	checkBackendEgress(context.Background(), r, config.HTTPBackend{
		Name: "aggregator", URL: "http://203.0.113.1:9/proxy/stream?tok=SUPERSECRETTOKEN&api_key=LEAKME",
	})
	d := r.Checks[0].Detail
	for _, leak := range []string{"SUPERSECRETTOKEN", "LEAKME", "tok=", "api_key"} {
		if strings.Contains(d, leak) {
			t.Fatalf("egress detail leaks %q: %s", leak, d)
		}
	}
	if !strings.Contains(d, "203.0.113.1:9") {
		t.Fatalf("redaction removed the diagnostic host too: %s", d)
	}
}

// Naming the transport cause is the fix for the fourth RD-29 instance: a 502
// that could mean DNS, timeout, or refusal is not actionable.
func TestTransportCauseDistinguishesFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"dns", &net.DNSError{Err: "no such host", Name: "nope.invalid"}, "DNS"},
		{"deadline", context.DeadlineExceeded, "TIMEOUT"},
		{"refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, "REFUSED"},
		{"noroute", &net.OpError{Op: "dial", Err: errors.New("no route to host")}, "NO ROUTE"},
		{"tls", errors.New("tls: failed to verify certificate"), "TLS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := transportCause(tc.err)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("transportCause(%v) = %q, want it to contain %q", tc.err, got, tc.want)
			}
		})
	}
	if transportCause(nil) != "" {
		t.Fatalf("nil error must produce no cause")
	}
}

func TestRedactURLDropsCredentialBearingParts(t *testing.T) {
	got := redactURL("https://user:pass@host.example:8443/a/b?tok=x")
	if strings.Contains(got, "pass") || strings.Contains(got, "tok") || strings.Contains(got, "/a/b") {
		t.Fatalf("redactURL leaked: %s", got)
	}
	if !strings.Contains(got, "host.example:8443") {
		t.Fatalf("redactURL dropped the host: %s", got)
	}
	if redactURL("::::not a url") != "<unparseable-url>" {
		t.Fatalf("unparseable input must be reported as such")
	}
}

// Commit-capable-lane visibility must reflect the two-stage toggle honestly:
// capture off and commit-mode off are DIFFERENT states and neither commits.
func TestProxyPostureVisibilityReflectsToggles(t *testing.T) {
	mk := func(enabled bool, mode string) *config.Config {
		c := &config.Config{}
		c.Topology.Active = topology.T3
		c.Reactive.Enabled = enabled
		c.Reactive.CommitMode = mode
		return c
	}
	for _, tc := range []struct {
		name   string
		cfg    *config.Config
		status Status
		want   string
	}{
		{"capture off", mk(false, "auto"), StatusSkip, "HARRBOR_REACTIVE_ENABLED=false"},
		{"mode off", mk(true, "off"), StatusSkip, "COMMIT_MODE=off"},
		{"auto", mk(true, "auto"), StatusOK, "ONLY for bytes DarkHarrbor delivers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Report{}
			runProxyPostureVisibility(r, tc.cfg)
			if len(r.Checks) != 1 || r.Checks[0].Status != tc.status {
				t.Fatalf("got %+v", r.Checks)
			}
			if !strings.Contains(r.Checks[0].Detail, tc.want) {
				t.Fatalf("detail %q missing %q", r.Checks[0].Detail, tc.want)
			}
		})
	}
}

// A non-T3 topology must produce no lane check at all rather than a
// misleading one.
func TestProxyPostureVisibilitySkippedBelowT3(t *testing.T) {
	c := &config.Config{}
	c.Topology.Active = topology.T1
	c.Reactive.Enabled = true
	c.Reactive.CommitMode = "auto"
	r := &Report{}
	runProxyPostureVisibility(r, c)
	if len(r.Checks) != 0 {
		t.Fatalf("expected no checks below T3, got %+v", r.Checks)
	}
}

// newStatusServer returns the URL of a test server answering with the given
// status, so the reachable path is exercised without depending on the network.
func newStatusServer(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
