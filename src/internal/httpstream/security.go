package httpstream

// Outbound security policy client (HS-3.3, D11/A11).
//
// D11 defines two trust classes for everything the HTTP-stream lane dials:
//
//   - TrustBackend — a configured backend origin (HARRBOR_HTTPBACKEND_*_URL).
//     The operator typed this URL; it may legitimately be a LAN address
//     (D11: "explicit private-CIDR trust setting for LAN sources" — for
//     backends this trust is implicit, since the operator already chose the
//     address). No IP-level policing; scheme/userinfo rules still apply.
//
//   - TrustSource — a URL returned BY a backend (an OMSS/Stremio source, an
//     IA CDN link). This is an untrusted network instruction: a compromised
//     or misconfigured backend could point DH's egress at loopback,
//     link-local, cloud metadata, or another private service. Denied by
//     default; AllowPrivateSourceCIDRs is the one documented opt-in widening
//     (RFC1918/ULA only — loopback/link-local/metadata/multicast stay denied
//     even then).
//
// This client owns dial-time address validation and redirect caps. It does
// NOT own header filtering (badRelayHeader in internal/api/httpstream.go)
// or credential separation (backend API keys vs transient source
// RequestHeaders never mix — enforced by call-site plumbing, not this
// file) — those stay where the initial request is built. This policy does own
// stripping copied headers when a redirect changes origin. It does not own
// transport pooling/timeouts (HS-3.10 wires this policy's DialContext into the
// metadata/probe/streaming transports).

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Trust classifies the origin of an outbound dial per D11.
type Trust int

const (
	// TrustBackend is a configured backend origin: user-typed, no IP policing.
	TrustBackend Trust = iota
	// TrustSource is a backend-returned source origin: untrusted, IP-policed.
	TrustSource
)

// ClassAddressDenied marks a dial rejected by the security policy — a
// permanent (non-transient) classification: retrying the same address never
// helps, unlike a genuine transient backend outage.
const ClassAddressDenied ErrClass = "address_denied"

// DefaultMaxRedirects is the shared redirect cap for both trust classes
// (D11: "capped + revalidated redirects").
const DefaultMaxRedirects = 5

// SecurityPolicy is DH's shared outbound trust boundary. One instance is
// reused across every backend API client, the D9 grab-time preflight, and
// the live relay (HS-3.3: "shared by all handlers incl. preflight + relay").
type SecurityPolicy struct {
	// AllowPrivateSourceCIDRs opts a deployment into permitting TrustSource
	// dials to RFC1918/ULA private ranges. Loopback, link-local,
	// unspecified, multicast, and the cloud metadata address are denied
	// unconditionally regardless of this setting (D11).
	AllowPrivateSourceCIDRs bool
	// MaxRedirects bounds redirect chains for both trust classes. Zero uses
	// DefaultMaxRedirects.
	MaxRedirects int

	// resolver is overridable in tests; nil uses net.DefaultResolver.
	resolver interface {
		LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
	}
	// dialer is overridable in tests; nil uses a standard net.Dialer.
	dialer interface {
		DialContext(ctx context.Context, network, address string) (net.Conn, error)
	}
}

// NewSecurityPolicy returns a policy with the documented-default posture:
// private source CIDRs denied, DefaultMaxRedirects.
func NewSecurityPolicy(allowPrivateSourceCIDRs bool) *SecurityPolicy {
	return &SecurityPolicy{AllowPrivateSourceCIDRs: allowPrivateSourceCIDRs, MaxRedirects: DefaultMaxRedirects}
}

func (p *SecurityPolicy) maxRedirects() int {
	if p == nil || p.MaxRedirects <= 0 {
		return DefaultMaxRedirects
	}
	return p.MaxRedirects
}

// ValidateURL enforces the scheme/userinfo rules shared by both trust
// classes: absolute http(s) only, host present, no embedded credentials
// (D11). Every code path that accepts a backend or backend-returned URL
// must call this before use.
func ValidateURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, NewError(ClassUpstreamMalformed, "empty URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, NewError(ClassUpstreamMalformed, "URL not parseable")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, NewError(ClassUpstreamMalformed, "URL scheme must be http or https")
	}
	if u.Host == "" {
		return nil, NewError(ClassUpstreamMalformed, "URL missing host")
	}
	if u.User != nil {
		return nil, NewError(ClassAddressDenied, "URL must not embed credentials")
	}
	return u, nil
}

// metadataIP is the cloud-metadata address used by AWS/GCP/Azure/OCI —
// explicitly denied even though it is technically link-local-adjacent, not
// covered by every net.IP predicate on every platform.
var metadataIP = net.IPv4(169, 254, 169, 254)

// disallowedIP reports whether ip is denied for TrustSource dials.
// Loopback/unspecified/multicast/link-local/metadata are denied
// unconditionally; private (RFC1918/ULA) is denied unless the policy opts
// in (D11).
func (p *SecurityPolicy) disallowedIP(ip net.IP) bool {
	switch {
	case ip.IsLoopback(),
		ip.IsUnspecified(),
		ip.IsMulticast(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.Equal(metadataIP):
		return true
	}
	if p != nil && p.AllowPrivateSourceCIDRs {
		return false
	}
	return ip.IsPrivate()
}

func (p *SecurityPolicy) lookupIP(ctx context.Context, host string) ([]net.IP, error) {
	if p != nil && p.resolver != nil {
		return p.resolver.LookupIP(ctx, "ip", host)
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

func (p *SecurityPolicy) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if p != nil && p.dialer != nil {
		return p.dialer.DialContext(ctx, network, address)
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, address)
}

// DialContext returns a dial function for the given trust class. It
// resolves the hostname itself and dials the validated literal IP directly
// — never re-resolving the original hostname string after validation, which
// would reopen a DNS-rebinding window between the check and the connect.
//
// TrustBackend dials the resolved address(es) without IP policing (D11: the
// operator explicitly configured this origin).
//
// TrustSource resolves, drops every disallowed IP, and dials only a
// permitted one; if every resolved address is denied, the dial fails with
// ClassAddressDenied.
func (p *SecurityPolicy) DialContext(trust Trust) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if trust == TrustBackend {
			return p.dial(ctx, network, addr)
		}
		ips, err := p.lookupIP(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("no addresses resolved")
		}
		var lastErr error
		sawDenied := false
		for _, ip := range ips {
			if p.disallowedIP(ip) {
				sawDenied = true
				continue
			}
			conn, derr := p.dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr == nil && sawDenied {
			return nil, NewError(ClassAddressDenied, "resolved address not permitted")
		}
		if lastErr == nil {
			lastErr = NewError(ClassAddressDenied, "no permitted address resolved")
		}
		return nil, lastErr
	}
}

// Transport returns a bare *http.Transport wired with this policy's
// DialContext for the given trust class. Callers (HS-3.10) set pooling and
// timeout fields — this function owns only the dial-time SSRF boundary.
func (p *SecurityPolicy) Transport(trust Trust) *http.Transport {
	return &http.Transport{DialContext: p.DialContext(trust)}
}

// CheckRedirect is the TrustSource-compatible redirect policy retained for
// direct callers. NewHTTPClient uses checkRedirect so configured backends keep
// their stricter exact-origin authority boundary.
func (p *SecurityPolicy) CheckRedirect(req *http.Request, via []*http.Request) error {
	return p.checkRedirect(TrustSource, req, via)
}

type redirectOriginRequired struct{ origin string }

func (*redirectOriginRequired) Error() string {
	return "backend redirect requires exact-origin approval"
}

// RedirectOrigin returns only the credential-free exact origin carried by an
// unapproved backend redirect. The complete redirect URL is never exposed.
func RedirectOrigin(err error) (string, bool) {
	var required *redirectOriginRequired
	if !errors.As(err, &required) || required.origin == "" {
		return "", false
	}
	return required.origin, true
}

// checkRedirect enforces the shared redirect cap and re-validates every hop.
// Configured backend authority widens only to an explicit exact-origin list.
// Source/CDN redirects may change origin after copied headers are reduced to DH-owned
// transport semantics; every new dial is still revalidated by TrustSource.
func (p *SecurityPolicy) checkRedirect(trust Trust, req *http.Request, via []*http.Request, allowedOrigins ...string) error {
	if len(via) >= p.maxRedirects() {
		return NewError(ClassUpstreamMalformed, "too many redirects")
	}
	if _, err := ValidateURL(req.URL.String()); err != nil {
		return err
	}
	if len(via) == 0 {
		return nil
	}
	previous := via[len(via)-1].URL
	if previous.Scheme == "https" && req.URL.Scheme == "http" {
		return NewError(ClassAddressDenied, "redirect must not downgrade HTTPS")
	}
	if sameOrigin(previous, req.URL) {
		return nil
	}
	stripCrossOriginRedirectHeaders(req.Header)
	if trust == TrustBackend {
		if sameOrigin(via[0].URL, req.URL) || originAllowed(req.URL, allowedOrigins) {
			return nil
		}
		origin := strings.ToLower(req.URL.Scheme) + "://" + strings.ToLower(req.URL.Host)
		return WrapError(ClassAddressDenied, "configured backend redirect changed to an unapproved origin",
			&redirectOriginRequired{origin: origin})
	}
	return nil
}

func originAllowed(target *url.URL, allowed []string) bool {
	for _, raw := range allowed {
		u, err := url.Parse(raw)
		if err == nil && u.User == nil && u.RawQuery == "" && u.Fragment == "" &&
			(u.Path == "" || u.Path == "/") && sameOrigin(u, target) {
			return true
		}
	}
	return false
}

func sameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func stripCrossOriginRedirectHeaders(header http.Header) {
	for name := range header {
		switch http.CanonicalHeaderKey(name) {
		case "Accept", "Accept-Encoding", "Range":
		default:
			header.Del(name)
		}
	}
}
