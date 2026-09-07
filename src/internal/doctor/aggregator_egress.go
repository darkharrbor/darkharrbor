package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/topology"
)

// RX-0.6. RX-0.3 documents shared-tier reachability as a CLIENT constraint and
// RX-0.4 fixed the mirror-image case for DarkHarrbor's own playback URLs.
// Neither covers the case universal proxying creates: once the aggregator
// proxies EVERY service, DarkHarrbor becomes the sole fetcher of aggregator and
// shared-tier playback URLs, and an address that is perfectly reachable from
// every viewing device can be unreachable from inside the container network.
//
// Live 2026-08-21: a shared-tier base URL set to the host's tailnet address,
// published to the tailnet interface only, was unreachable from `arr-net`
// because Docker's DNAT rule for that port carries `! -i <bridge>`. Every
// shared-tier stream returned HTTP 502 at a flat 10.004s, NO request reached
// the shared tier, and DarkHarrbor logged NOTHING. A sibling container on the
// same bridge was unaffected and looked healthy, so one service working proved
// nothing about another. Diagnosis required reading Docker's nat table by hand.
//
// This check exists so that never requires hand-diagnosis again. It reports
// UNREACHABLE distinctly from unhealthy and NAMES the address it tried, because
// the whole difficulty was that the address looked correct from every vantage
// point the operator had.
func runAggregatorEgress(ctx context.Context, r *Report, cfg *config.Config) {
	active := cfg.Topology.Active
	if active != topology.T2 && active != topology.T3 {
		return
	}
	for _, backend := range cfg.HTTPStream.Backends {
		checkBackendEgress(ctx, r, backend)
	}
}

// checkBackendEgress fetches a backend's configured URL from DarkHarrbor's OWN
// network position. It deliberately does NOT require a 2xx: any HTTP status
// proves the socket path works, which is the property under test. A 404 from a
// reachable host is a PASS here; only transport failure is a FAIL.
func checkBackendEgress(ctx context.Context, r *Report, backend config.HTTPBackend) {
	name := "aggregator egress: " + backend.Name
	target := strings.TrimSpace(backend.URL)
	if target == "" {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodHead, target, nil)
	if err != nil {
		r.add(Check{Category: "deployment-topology", Name: name, Status: StatusFail,
			Detail: "configured URL is not usable: " + errClass(err)})
		return
	}
	client := httpstream.NewHTTPClient(httpstream.NewSecurityPolicy(false), httpstream.TrustBackend,
		httpstream.TransportMetadata, backend.RedirectOrigins...)
	resp, err := client.Do(req)
	if err != nil {
		r.add(Check{Category: "deployment-topology", Name: name, Status: StatusFail,
			Detail: fmt.Sprintf(
				"UNREACHABLE from DarkHarrbor's own network position at %s (%s). "+
					"This is NOT the same as the backend being down: it may be reachable from "+
					"viewing devices and still unreachable from here. A published port bound to a "+
					"single host interface is the common cause -- Docker's DNAT rule excludes the "+
					"container bridge, so a packet from inside the container network is never "+
					"rewritten and simply times out. Under universal proxying DarkHarrbor is the "+
					"ONLY fetcher of this URL, so a client-reachable address is not sufficient.",
				redactURL(target), transportCause(err))})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	r.add(Check{Category: "deployment-topology", Name: name, Status: StatusOK,
		Detail: fmt.Sprintf("reachable from DarkHarrbor at %s (HTTP %d; socket-path check only -- see the aggregator delivery check for observed playback health, since a reachable socket does not guarantee a resolving application)",
			redactURL(target), resp.StatusCode)})
}

// transportCause names WHY a fetch failed rather than collapsing every cause
// into one opaque string. The 2026-08-21 outage was indistinguishable from any
// other 502 precisely because this distinction was not made anywhere; it is the
// fourth instance of the RD-29 swallowed-underlying-error shape.
func transportCause(err error) string {
	if err == nil {
		return ""
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS: name does not resolve"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "TIMEOUT: no response before the deadline"
	}
	if errorChainContains(err, "certificate", "tls:") {
		return "TLS: handshake or certificate failure"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return "TIMEOUT: connection attempt timed out"
		}
		if opErr.Err != nil {
			message := strings.ToLower(opErr.Err.Error())
			if strings.Contains(message, "refused") {
				return "REFUSED: host reachable, nothing listening on that port"
			}
			if strings.Contains(message, "no route") {
				return "NO ROUTE: the address is not routable from this container"
			}
		}
		return "NETWORK: " + opErr.Op + " failed"
	}
	return "TRANSPORT: " + errClass(err)
}

func errorChainContains(err error, needles ...string) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		message := strings.ToLower(current.Error())
		for _, needle := range needles {
			if strings.Contains(message, needle) {
				return true
			}
		}
	}
	return false
}

// redactURL keeps scheme/host/port -- the diagnostic content -- and drops any
// path, query, or userinfo, which is where credentials live. A check that names
// the address it tried must not become a credential leak.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<unparseable-url>"
	}
	return u.Scheme + "://" + u.Host
}

// runProxyPostureVisibility surfaces WHICH LANES CAN COMMIT. A named
// proxied-service filter silently limits reactive commit to those services and
// looks entirely healthy; an empty filter is what universal commit requires.
// This is DarkHarrbor reporting what it can observe about the contract, not
// DarkHarrbor managing the aggregator, which RX-0.3 rules out.
func runProxyPostureVisibility(r *Report, cfg *config.Config) {
	if cfg.Topology.Active != topology.T3 {
		return
	}
	const name = "reactive: commit-capable lanes"
	if !cfg.Reactive.Enabled {
		r.add(Check{Category: "reactive", Name: name, Status: StatusSkip,
			Detail: "reactive capture is disabled (HARRBOR_REACTIVE_ENABLED=false); no lane can commit"})
		return
	}
	if cfg.Reactive.CommitMode == "off" {
		r.add(Check{Category: "reactive", Name: name, Status: StatusSkip,
			Detail: "capture is on but HARRBOR_REACTIVE_COMMIT_MODE=off; coverage is recorded and nothing is committed"})
		return
	}
	r.add(Check{Category: "reactive", Name: name, Status: StatusOK,
		Detail: "coverage is recorded ONLY for bytes DarkHarrbor delivers, so a lane can commit if and " +
			"only if the aggregator proxies it to DarkHarrbor. Verify by inspecting a real stream lookup " +
			"rather than the aggregator's saved configuration: environment forcing is applied at request " +
			"time, so the saved value can disagree with the effective one. If every returned stream URL " +
			"points at DarkHarrbor, every lane can commit; any that do not, cannot. Note that an " +
			"aggregator's OWN built-in usenet engine, and NzbDAV/AltMount outside the aggregator's " +
			"built-in proxy, are hardcoded to bypass external proxies and can NEVER commit."})
}
