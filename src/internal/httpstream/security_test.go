package httpstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
)

func TestValidateURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr ErrClass
	}{
		{"valid https", "https://example.com/path", ""},
		{"valid http", "http://example.com", ""},
		{"empty", "", ClassUpstreamMalformed},
		{"bad scheme ftp", "ftp://example.com", ClassUpstreamMalformed},
		{"bad scheme file", "file:///etc/passwd", ClassUpstreamMalformed},
		{"no host", "https:///path", ClassUpstreamMalformed},
		{"userinfo", "https://user:pass@example.com", ClassAddressDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateURL(tc.url)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error class %s, got nil", tc.wantErr)
			}
			if ClassOf(err) != tc.wantErr {
				t.Fatalf("expected class %s, got %s (%v)", tc.wantErr, ClassOf(err), err)
			}
		})
	}
}

func TestDisallowedIP(t *testing.T) {
	p := NewSecurityPolicy(false)
	deny := []string{
		"127.0.0.1",       // loopback
		"::1",             // loopback v6
		"0.0.0.0",         // unspecified
		"169.254.169.254", // cloud metadata
		"169.254.1.1",     // link-local
		"224.0.0.1",       // multicast
		"10.0.0.5",        // private (denied by default)
		"192.168.1.1",     // private
		"172.16.0.1",      // private
		"fd00::1",         // ULA (Go's IsPrivate covers fc00::/7)
	}
	for _, ip := range deny {
		if !p.disallowedIP(net.ParseIP(ip)) {
			t.Errorf("expected %s to be disallowed by default", ip)
		}
	}
	allow := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34",
	}
	for _, ip := range allow {
		if p.disallowedIP(net.ParseIP(ip)) {
			t.Errorf("expected %s to be allowed", ip)
		}
	}
}

func TestDisallowedIP_PrivateOptIn(t *testing.T) {
	p := NewSecurityPolicy(true)
	// Metadata/loopback/link-local/multicast stay denied even with opt-in.
	stillDenied := []string{"127.0.0.1", "169.254.169.254", "224.0.0.1", "0.0.0.0"}
	for _, ip := range stillDenied {
		if !p.disallowedIP(net.ParseIP(ip)) {
			t.Errorf("expected %s to remain disallowed even with AllowPrivateSourceCIDRs", ip)
		}
	}
	// Private ranges now permitted.
	nowAllowed := []string{"10.0.0.5", "192.168.1.1", "172.16.0.1"}
	for _, ip := range nowAllowed {
		if p.disallowedIP(net.ParseIP(ip)) {
			t.Errorf("expected %s to be allowed with AllowPrivateSourceCIDRs", ip)
		}
	}
}

// fakeResolver/fakeDialer let the DialContext path be tested without real
// network access.
type fakeResolver struct {
	ips map[string][]net.IP
	err error
}

func (f *fakeResolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ips[host], nil
}

type fakeDialer struct {
	dialed []string
	fail   map[string]bool
}

func (f *fakeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	f.dialed = append(f.dialed, address)
	if f.fail[address] {
		return nil, errors.New("dial failed")
	}
	return &net.TCPConn{}, nil
}

func TestDialContext_TrustSource_DeniesLoopback(t *testing.T) {
	res := &fakeResolver{ips: map[string][]net.IP{"evil.example": {net.ParseIP("127.0.0.1")}}}
	dial := &fakeDialer{}
	p := &SecurityPolicy{resolver: res, dialer: dial}
	dc := p.DialContext(TrustSource)
	_, err := dc(context.Background(), "tcp", "evil.example:80")
	if err == nil {
		t.Fatal("expected dial to be denied")
	}
	if ClassOf(err) != ClassAddressDenied {
		t.Fatalf("expected ClassAddressDenied, got %s (%v)", ClassOf(err), err)
	}
	if len(dial.dialed) != 0 {
		t.Fatalf("dialer should never have been called for a fully-denied address set, got %v", dial.dialed)
	}
}

func TestDialContext_TrustSource_AllowsPublicIP(t *testing.T) {
	res := &fakeResolver{ips: map[string][]net.IP{"good.example": {net.ParseIP("93.184.216.34")}}}
	dial := &fakeDialer{}
	p := &SecurityPolicy{resolver: res, dialer: dial}
	dc := p.DialContext(TrustSource)
	if _, err := dc(context.Background(), "tcp", "good.example:443"); err != nil {
		t.Fatalf("expected dial to succeed, got %v", err)
	}
	if len(dial.dialed) != 1 || dial.dialed[0] != "93.184.216.34:443" {
		t.Fatalf("expected dial to the resolved literal IP, got %v", dial.dialed)
	}
}

func TestDialContext_TrustSource_MixedAddresses_DialsOnlyPermitted(t *testing.T) {
	// DNS returning both a denied and a permitted address: must dial only
	// the permitted one, never the loopback (defense against a backend
	// mixing a decoy public record with a rebinding target).
	res := &fakeResolver{ips: map[string][]net.IP{
		"mixed.example": {net.ParseIP("127.0.0.1"), net.ParseIP("93.184.216.34")},
	}}
	dial := &fakeDialer{}
	p := &SecurityPolicy{resolver: res, dialer: dial}
	dc := p.DialContext(TrustSource)
	if _, err := dc(context.Background(), "tcp", "mixed.example:443"); err != nil {
		t.Fatalf("expected dial to succeed via the permitted address, got %v", err)
	}
	for _, addr := range dial.dialed {
		if addr == "127.0.0.1:443" {
			t.Fatalf("dialer was called with the denied loopback address: %v", dial.dialed)
		}
	}
}

func TestDialContext_TrustBackend_SkipsIPPolicy(t *testing.T) {
	// A configured backend origin (e.g. explicit LAN URL) must dial without
	// resolver/IP-policy involvement — the operator already trusted it.
	dial := &fakeDialer{}
	p := &SecurityPolicy{dialer: dial}
	dc := p.DialContext(TrustBackend)
	if _, err := dc(context.Background(), "tcp", "192.168.1.50:8080"); err != nil {
		t.Fatalf("expected TrustBackend dial to succeed without IP policing, got %v", err)
	}
	if len(dial.dialed) != 1 || dial.dialed[0] != "192.168.1.50:8080" {
		t.Fatalf("expected direct dial to the configured address, got %v", dial.dialed)
	}
}

func TestCheckRedirect_CapsAndValidates(t *testing.T) {
	p := NewSecurityPolicy(false)
	base, _ := http.NewRequest(http.MethodGet, "https://example.com/next", nil)

	// Under the cap, valid scheme: allowed.
	var via []*http.Request
	for i := 0; i < DefaultMaxRedirects-1; i++ {
		via = append(via, base)
	}
	if err := p.CheckRedirect(base, via); err != nil {
		t.Fatalf("expected redirect under cap to be allowed, got %v", err)
	}

	// At the cap: denied.
	via = append(via, base)
	if err := p.CheckRedirect(base, via); err == nil {
		t.Fatal("expected redirect at the cap to be denied")
	}

	// Bad scheme on the redirect target: denied regardless of count.
	bad, _ := http.NewRequest(http.MethodGet, "ftp://example.com/x", nil)
	if err := p.CheckRedirect(bad, nil); err == nil {
		t.Fatal("expected redirect to a non-http(s) scheme to be denied")
	}
}

func TestCheckRedirectRejectsHTTPSDowngrade(t *testing.T) {
	p := NewSecurityPolicy(false)
	initial, _ := http.NewRequest(http.MethodGet, "https://media.example/file", nil)
	downgrade, _ := http.NewRequest(http.MethodGet, "http://media.example/file", nil)
	if err := p.CheckRedirect(downgrade, []*http.Request{initial}); ClassOf(err) != ClassAddressDenied {
		t.Fatalf("expected HTTPS downgrade denial, got %v", err)
	}
}

func TestCheckRedirectTrustBackendRejectsChangedOrigin(t *testing.T) {
	p := NewSecurityPolicy(false)
	initial, _ := http.NewRequest(http.MethodGet, "https://backend.example/api", nil)
	redirect, _ := http.NewRequest(http.MethodGet, "https://other.example/api", nil)
	if err := p.checkRedirect(TrustBackend, redirect, []*http.Request{initial}); ClassOf(err) != ClassAddressDenied {
		t.Fatalf("expected configured backend origin change denial, got %v", err)
	}
}

func TestCheckRedirectTrustBackendAllowsOnlyApprovedExactOrigin(t *testing.T) {
	p := NewSecurityPolicy(false)
	initial, _ := http.NewRequest(http.MethodGet, "https://backend.example/api?token=secret", nil)
	approved, _ := http.NewRequest(http.MethodGet, "https://metadata.example/result", nil)
	approved.Header.Set("Authorization", "Bearer secret")
	approved.Header.Set("Referer", initial.URL.String())
	approved.Header.Set("Range", "bytes=0-9")
	if err := p.checkRedirect(TrustBackend, approved, []*http.Request{initial}, "https://metadata.example"); err != nil {
		t.Fatalf("approved exact origin rejected: %v", err)
	}
	if approved.Header.Get("Authorization") != "" || approved.Header.Get("Referer") != "" {
		t.Fatal("approved cross-origin redirect retained sensitive headers")
	}
	if approved.Header.Get("Range") == "" {
		t.Fatal("approved cross-origin redirect dropped bounded transport header")
	}

	unapproved, _ := http.NewRequest(http.MethodGet, "https://other.example/result", nil)
	if err := p.checkRedirect(TrustBackend, unapproved, []*http.Request{initial}, "https://metadata.example"); ClassOf(err) != ClassAddressDenied {
		t.Fatalf("unapproved origin accepted: %v", err)
	} else if origin, ok := RedirectOrigin(err); !ok || origin != "https://other.example" {
		t.Fatalf("redirect approval signal = %q, %v", origin, ok)
	}

	downgrade, _ := http.NewRequest(http.MethodGet, "http://metadata.example/result", nil)
	if err := p.checkRedirect(TrustBackend, downgrade, []*http.Request{initial}, "http://metadata.example"); ClassOf(err) != ClassAddressDenied {
		t.Fatalf("approved HTTPS downgrade accepted: %v", err)
	}
}

func TestCheckRedirectTrustSourceStripsCrossOriginHeaders(t *testing.T) {
	p := NewSecurityPolicy(false)
	initial, _ := http.NewRequest(http.MethodGet, "https://source.example/file?token=secret", nil)
	redirect, _ := http.NewRequest(http.MethodGet, "https://cdn.example/file", nil)
	redirect.Header.Set("Authorization", "Bearer secret")
	redirect.Header.Set("Cookie", "session=secret")
	redirect.Header.Set("Referer", initial.URL.String())
	redirect.Header.Set("X-Auth", "secret")
	redirect.Header.Set("Accept", "video/*")
	redirect.Header.Set("Accept-Encoding", "identity")
	redirect.Header.Set("Range", "bytes=0-9")
	if err := p.CheckRedirect(redirect, []*http.Request{initial}); err != nil {
		t.Fatalf("expected safe source redirect to remain allowed, got %v", err)
	}
	for _, name := range []string{"Authorization", "Cookie", "Referer", "X-Auth"} {
		if redirect.Header.Get(name) != "" {
			t.Fatalf("cross-origin redirect retained %s", name)
		}
	}
	for _, name := range []string{"Accept", "Accept-Encoding", "Range"} {
		if redirect.Header.Get(name) == "" {
			t.Fatalf("cross-origin redirect dropped %s", name)
		}
	}
}

func TestSecurityPolicy_NilSafe(t *testing.T) {
	var p *SecurityPolicy
	// maxRedirects and disallowedIP must not panic on a nil receiver —
	// defensive default posture matches other nil-safe types in this
	// package (e.g. Registry).
	if p.maxRedirects() != DefaultMaxRedirects {
		t.Fatalf("expected default redirect cap on nil policy")
	}
	if p.disallowedIP(net.ParseIP("10.0.0.1")) == false {
		t.Fatalf("expected nil policy to deny private IPs by default")
	}
}
