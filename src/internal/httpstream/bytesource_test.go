package httpstream

// HR1.1 deterministic gates. The adapter is exercised through a REAL
// NewHTTPClient built on the shared D11 SecurityPolicy (resolver/dialer
// injected so a public-looking hostname reaches an httptest origin), so
// SSRF policy, the capped redirect chain, and the stream transport budget
// are all in the loop rather than stubbed away.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/bytesource/bytesourcetest"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

type bsClock struct {
	mu sync.Mutex
	t  time.Time
}

func newBSClock() *bsClock { return &bsClock{t: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)} }

func (c *bsClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *bsClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// bsOrigin is a configurable HTTP origin. Every knob models one failure the
// HR1.1 row names: short read, mutation, Range-ignoring, expiry, 4xx/5xx.
type bsOrigin struct {
	mu sync.Mutex

	data        []byte
	etag        string
	lastMod     string
	contentType string

	ignoreRange bool // answer 200 with the whole body, ignoring Range
	forceStatus int  // if >0, always answer this status
	failFirstN  int  // answer 503 for the first N requests
	truncateTo  int  // cap body bytes per response (short read); -1 means 0 bytes
	mutateAfter int  // after this many requests, switch etag/lastMod
	mutatedETag string
	mutatedLast string
	shrinkAfter int // after this many requests, report a smaller total

	requests    int
	lastRange   string
	lastHeaders http.Header

	srv *httptest.Server
}

func (o *bsOrigin) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.requests
}

func (o *bsOrigin) seenHeaders() http.Header {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastHeaders.Clone()
}

func (o *bsOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.requests++
	n := o.requests
	o.lastRange = r.Header.Get("Range")
	o.lastHeaders = r.Header.Clone()
	data := o.data
	etag, lastMod := o.etag, o.lastMod
	if o.mutateAfter > 0 && n > o.mutateAfter {
		if o.mutatedETag != "" {
			etag = o.mutatedETag
		}
		if o.mutatedLast != "" {
			lastMod = o.mutatedLast
		}
	}
	total := int64(len(data))
	if o.shrinkAfter > 0 && n > o.shrinkAfter {
		total = int64(len(data)) - 1
	}
	ct := o.contentType
	if ct == "" {
		ct = "video/mp4"
	}
	ignoreRange, forceStatus, failFirstN, truncateTo := o.ignoreRange, o.forceStatus, o.failFirstN, o.truncateTo
	o.mu.Unlock()

	if forceStatus > 0 {
		if forceStatus == http.StatusRequestedRangeNotSatisfiable {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		}
		w.WriteHeader(forceStatus)
		return
	}
	if failFirstN > 0 && n <= failFirstN {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	if lastMod != "" {
		w.Header().Set("Last-Modified", lastMod)
	}
	w.Header().Set("Content-Type", ct)

	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" || ignoreRange {
		body := data
		if truncateTo != 0 {
			body = clampBody(body, truncateTo)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	start, end, ok := parseTestRange(rangeHdr)
	if !ok || start >= int64(len(data)) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	chunk := data[start : end+1]
	// Content-Range always advertises the full requested span; a truncated
	// body is exactly the short-read case the adapter must complete.
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	if truncateTo != 0 {
		chunk = clampBody(chunk, truncateTo)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(chunk)
}

func clampBody(b []byte, cap int) []byte {
	if cap < 0 {
		return nil
	}
	if cap < len(b) {
		return b[:cap]
	}
	return b
}

func parseTestRange(h string) (int64, int64, bool) {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(spec[:dash], 10, 64)
	end, err2 := strconv.ParseInt(spec[dash+1:], 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return 0, 0, false
	}
	return start, end, true
}

// ---------------------------------------------------------------------------
// policy-backed client harness
// ---------------------------------------------------------------------------

const bsHost = "origin.invalid"

type bsResolver struct{ ips map[string][]net.IP }

func (r *bsResolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	if ips, ok := r.ips[host]; ok {
		return ips, nil
	}
	return nil, fmt.Errorf("no such host")
}

// bsDialer routes a validated public IP back to the real httptest listener.
type bsDialer struct{ routes map[string]string }

func (d *bsDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	target, ok := d.routes[host]
	if !ok {
		return nil, fmt.Errorf("no route")
	}
	var dl net.Dialer
	return dl.DialContext(ctx, network, target)
}

// startOrigin brings up an origin and returns it plus a policy-backed client
// that can reach it via the public-looking hostname bsHost.
func startOrigin(t *testing.T, o *bsOrigin) (*bsOrigin, *http.Client) {
	t.Helper()
	o.srv = httptest.NewServer(o)
	t.Cleanup(o.srv.Close)
	policy := &SecurityPolicy{
		MaxRedirects: DefaultMaxRedirects,
		resolver:     &bsResolver{ips: map[string][]net.IP{bsHost: {net.ParseIP("93.184.216.34")}}},
		dialer:       &bsDialer{routes: map[string]string{"93.184.216.34": listenAddr(o.srv)}},
	}
	client := NewHTTPClient(policy, TrustSource, TransportStream)
	t.Cleanup(client.CloseIdleConnections)
	return o, client
}

func listenAddr(s *httptest.Server) string {
	return strings.TrimPrefix(s.URL, "http://")
}

func originURL(path string) string { return "http://" + bsHost + path }

// staticResolver counts calls and hands back fixed coordinates.
type staticResolver struct {
	mu      sync.Mutex
	calls   int
	forced  int
	url     string
	headers map[string]string
	expires func() time.Time
}

func (s *staticResolver) Resolve(ctx context.Context, forced bool) (ResolvedFile, error) {
	s.mu.Lock()
	s.calls++
	if forced {
		s.forced++
	}
	s.mu.Unlock()
	rf := ResolvedFile{
		Selector:       "sel-1",
		Name:           "fixture.mp4",
		URL:            s.url,
		RequestHeaders: s.headers,
		SupportsRange:  true,
	}
	if s.expires != nil {
		rf.ExpiresAt = s.expires()
	}
	return rf, nil
}

func (s *staticResolver) count() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.forced
}

func mustSource(t *testing.T, cfg ByteSourceConfig) bytesource.ByteSource {
	t.Helper()
	bs, err := NewByteSource(cfg)
	if err != nil {
		t.Fatalf("NewByteSource: %v", err)
	}
	return bs
}

func fixtureBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*7 + i/251) % 251)
	}
	return b
}

// ---------------------------------------------------------------------------
// contract conformance and short reads
// ---------------------------------------------------------------------------

// TestByteSourceConformance runs the shared TS-1.1 conformance battery, so
// the HTTP adapter is validated by the same contract as the NNTP-segment and
// debrid-CDN adapters rather than by ad-hoc assertions.
func TestByteSourceConformance(t *testing.T) {
	data := fixtureBytes(64 << 10)
	o, client := startOrigin(t, &bsOrigin{data: data, etag: `"v1"`})
	_ = o
	bytesourcetest.RunConformance(t, func() (bytesource.ByteSource, []byte) {
		res := &staticResolver{url: originURL("/media.mp4")}
		bs := mustSource(t, ByteSourceConfig{
			Key: "http|item-1|file-1", Size: int64(len(data)),
			RangeVerified: true, Client: client, Resolve: res.Resolve,
		})
		return bs, data
	})
}

func TestByteSourceCapsAndKey(t *testing.T) {
	data := fixtureBytes(4096)
	_, client := startOrigin(t, &bsOrigin{data: data})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|item-1|file-1", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	caps := bs.Caps()
	if !caps.RangeSupport || !caps.ExactSize {
		t.Fatalf("expected range support and exact size, got %+v", caps)
	}
	if caps.TailCost != bytesource.TailCostCheap {
		t.Fatalf("expected cheap tail cost, got %v", caps.TailCost)
	}
	if caps.Alignment != 0 {
		t.Fatalf("expected byte-granular alignment, got %d", caps.Alignment)
	}
	if bs.Key() != "http|item-1|file-1" || bs.Size() != int64(len(data)) {
		t.Fatalf("key/size mismatch: %q %d", bs.Key(), bs.Size())
	}
}

// TestByteSourceShortReadCompletes proves a truncated body is completed by
// re-requesting only the remainder, never returned as a silent short read.
func TestByteSourceShortReadCompletes(t *testing.T) {
	data := fixtureBytes(8192)
	o, client := startOrigin(t, &bsOrigin{data: data, etag: `"v1"`, truncateTo: 1000})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|short", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	p := make([]byte, 4096)
	n, err := bs.ReadAt(context.Background(), p, 100)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(p) {
		t.Fatalf("short return: n=%d want %d", n, len(p))
	}
	if string(p) != string(data[100:100+4096]) {
		t.Fatal("assembled bytes differ from the origin content")
	}
	if o.count() < 2 {
		t.Fatalf("expected multiple requests to complete the range, got %d", o.count())
	}
}

// TestByteSourceZeroProgressHitsAttemptBudget proves an origin that never
// delivers bytes is bounded rather than looping forever.
func TestByteSourceZeroProgressHitsAttemptBudget(t *testing.T) {
	data := fixtureBytes(4096)
	o, client := startOrigin(t, &bsOrigin{data: data, truncateTo: -1})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|stall", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	n, err := bs.ReadAt(context.Background(), make([]byte, 64), 0)
	if err == nil {
		t.Fatal("expected an attempt-budget error")
	}
	if n != 0 {
		t.Fatalf("expected no bytes, got %d", n)
	}
	if ClassOf(err) != ClassBackendUnavailable {
		t.Fatalf("expected backend_unavailable, got %v", ClassOf(err))
	}
	if o.count() > byteSourceMaxAttempts {
		t.Fatalf("attempt budget exceeded: %d requests", o.count())
	}
}

// ---------------------------------------------------------------------------
// mutation (LC-05: never splice across representations)
// ---------------------------------------------------------------------------

func TestByteSourceMutationETagChange(t *testing.T) {
	data := fixtureBytes(8192)
	o, client := startOrigin(t, &bsOrigin{
		data: data, etag: `"v1"`, mutateAfter: 1, mutatedETag: `"v2"`,
	})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|mut-etag", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	if _, err := bs.ReadAt(context.Background(), make([]byte, 128), 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	n, err := bs.ReadAt(context.Background(), make([]byte, 128), 4096)
	if !errors.Is(err, ErrSourceMutated) {
		t.Fatalf("expected ErrSourceMutated, got %v", err)
	}
	if n != 0 {
		t.Fatalf("mutation must yield no bytes, got %d", n)
	}
	if ClassOf(err) != ClassUpstreamMalformed {
		t.Fatalf("expected upstream_malformed class, got %v", ClassOf(err))
	}
	// Terminal: exactly one extra request, never a refresh retry.
	if o.count() != 2 {
		t.Fatalf("mutation must not be retried, got %d requests", o.count())
	}
}

func TestByteSourceMutationTotalDisagreesWithVerifiedSize(t *testing.T) {
	data := fixtureBytes(8192)
	o, client := startOrigin(t, &bsOrigin{data: data, etag: `"v1"`})
	res := &staticResolver{url: originURL("/media.mp4")}
	// Verified size is authoritative; the origin now reports a different one.
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|mut-size", Size: int64(len(data)) + 10,
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	_, err := bs.ReadAt(context.Background(), make([]byte, 16), 0)
	if !errors.Is(err, ErrSourceMutated) {
		t.Fatalf("expected ErrSourceMutated, got %v", err)
	}
	if o.count() != 1 {
		t.Fatalf("expected a single terminal request, got %d", o.count())
	}
}

func TestByteSourceMutationTotalChangesMidLifetime(t *testing.T) {
	data := fixtureBytes(8192)
	_, client := startOrigin(t, &bsOrigin{data: data, etag: `"v1"`, shrinkAfter: 1})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|mut-shrink", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 1024); !errors.Is(err, ErrSourceMutated) {
		t.Fatalf("expected ErrSourceMutated on total change, got %v", err)
	}
}

// TestByteSourceWeakETagIsNotMutation: a weak validator does not guarantee
// octet equality, so it must never be treated as a continuity signal in
// either direction.
func TestByteSourceWeakETagIsNotMutation(t *testing.T) {
	data := fixtureBytes(8192)
	_, client := startOrigin(t, &bsOrigin{
		data: data, etag: `W/"v1"`, mutateAfter: 1, mutatedETag: `W/"v2"`,
	})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|weak-etag", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 1024); err != nil {
		t.Fatalf("weak etag change must not be mutation, got %v", err)
	}
}

func TestByteSourceLastModifiedChangeIsMutation(t *testing.T) {
	data := fixtureBytes(8192)
	_, client := startOrigin(t, &bsOrigin{
		data:        data,
		lastMod:     "Mon, 01 Jun 2026 00:00:00 GMT",
		mutateAfter: 1, mutatedLast: "Tue, 02 Jun 2026 00:00:00 GMT",
	})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|mut-lastmod", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 1024); !errors.Is(err, ErrSourceMutated) {
		t.Fatalf("expected ErrSourceMutated on last-modified change, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// expiry, forced refresh, and upstream status handling (DG-05)
// ---------------------------------------------------------------------------

func TestByteSourceExpiryTriggersReResolve(t *testing.T) {
	data := fixtureBytes(4096)
	_, client := startOrigin(t, &bsOrigin{data: data})
	clk := newBSClock()
	res := &staticResolver{
		url:     originURL("/media.mp4"),
		expires: func() time.Time { return clk.Now().Add(2 * time.Minute) },
	}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|expiry", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve, Now: clk.Now,
	})
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if calls, _ := res.count(); calls != 1 {
		t.Fatalf("expected one resolve, got %d", calls)
	}
	// Well inside the window: coordinates are reused, no re-resolve.
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 128); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if calls, _ := res.count(); calls != 1 {
		t.Fatalf("expected no re-resolve inside the window, got %d", calls)
	}
	// Past the skew-adjusted expiry: re-resolve, unforced.
	clk.Advance(2 * time.Minute)
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 256); err != nil {
		t.Fatalf("third read: %v", err)
	}
	calls, forced := res.count()
	if calls != 2 {
		t.Fatalf("expected a re-resolve after expiry, got %d calls", calls)
	}
	if forced != 0 {
		t.Fatalf("expiry re-resolve must not be a forced refresh, got %d", forced)
	}
}

// TestByteSourceSingleForcedRefreshRecovers mirrors the shipped relay's
// exactly-one-refresh discipline.
func TestByteSourceSingleForcedRefreshRecovers(t *testing.T) {
	data := fixtureBytes(4096)
	o, client := startOrigin(t, &bsOrigin{data: data, failFirstN: 1})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|refresh", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	p := make([]byte, 256)
	if _, err := bs.ReadAt(context.Background(), p, 0); err != nil {
		t.Fatalf("expected recovery after one forced refresh, got %v", err)
	}
	if string(p) != string(data[:256]) {
		t.Fatal("recovered bytes differ from origin content")
	}
	calls, forced := res.count()
	if calls != 2 || forced != 1 {
		t.Fatalf("expected exactly one forced re-resolve, got calls=%d forced=%d", calls, forced)
	}
	if o.count() != 2 {
		t.Fatalf("expected two upstream requests, got %d", o.count())
	}
}

func TestByteSourcePermanentFailureStopsAfterOneRefresh(t *testing.T) {
	data := fixtureBytes(4096)
	o, client := startOrigin(t, &bsOrigin{data: data, forceStatus: http.StatusNotFound})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|dead", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	_, err := bs.ReadAt(context.Background(), make([]byte, 64), 0)
	if ClassOf(err) != ClassBackendUnavailable {
		t.Fatalf("expected backend_unavailable, got %v (%v)", ClassOf(err), err)
	}
	if o.count() != 2 {
		t.Fatalf("expected exactly two attempts, got %d", o.count())
	}
	if calls, forced := res.count(); calls != 2 || forced != 1 {
		t.Fatalf("expected one forced refresh, got calls=%d forced=%d", calls, forced)
	}
}

func TestByteSourceRateLimitedMapsToRateLimitClass(t *testing.T) {
	data := fixtureBytes(4096)
	o, client := startOrigin(t, &bsOrigin{data: data, forceStatus: http.StatusTooManyRequests})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|429", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	_, err := bs.ReadAt(context.Background(), make([]byte, 64), 0)
	if ClassOf(err) != ClassRateLimited {
		t.Fatalf("expected rate_limited, got %v (%v)", ClassOf(err), err)
	}
	if o.count() != 1 {
		t.Fatalf("account-level 429 must stop before re-resolve, got %d requests", o.count())
	}
	if calls, forced := res.count(); calls != 1 || forced != 0 {
		t.Fatalf("account-level 429 unexpectedly re-resolved: calls=%d forced=%d", calls, forced)
	}
}

// TestByteSource416IsTerminalMutation: DH only requests ranges inside the
// verified size, so an upstream 416 means the representation changed.
func TestByteSource416IsTerminalMutation(t *testing.T) {
	data := fixtureBytes(4096)
	o, client := startOrigin(t, &bsOrigin{data: data, forceStatus: http.StatusRequestedRangeNotSatisfiable})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|416", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	_, err := bs.ReadAt(context.Background(), make([]byte, 64), 0)
	if !errors.Is(err, ErrSourceMutated) {
		t.Fatalf("expected ErrSourceMutated, got %v", err)
	}
	if o.count() != 1 {
		t.Fatalf("416 must be terminal, got %d requests", o.count())
	}
}

func TestByteSourceMalformedContentRangeIsMalformed(t *testing.T) {
	data := fixtureBytes(4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 999-1000/4096")
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[:2])
	}))
	t.Cleanup(srv.Close)
	policy := &SecurityPolicy{
		MaxRedirects: DefaultMaxRedirects,
		resolver:     &bsResolver{ips: map[string][]net.IP{bsHost: {net.ParseIP("93.184.216.34")}}},
		dialer:       &bsDialer{routes: map[string]string{"93.184.216.34": listenAddr(srv)}},
	}
	client := NewHTTPClient(policy, TrustSource, TransportStream)
	t.Cleanup(client.CloseIdleConnections)
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|badrange", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	_, err := bs.ReadAt(context.Background(), make([]byte, 64), 0)
	if ClassOf(err) != ClassUpstreamMalformed {
		t.Fatalf("expected upstream_malformed, got %v (%v)", ClassOf(err), err)
	}
}

// ---------------------------------------------------------------------------
// DG-09: Range-ignoring origins are bounded, never a full download
// ---------------------------------------------------------------------------

func TestByteSourceRangeIgnoringOriginWithinSkipWindow(t *testing.T) {
	data := fixtureBytes(16 << 10)
	_, client := startOrigin(t, &bsOrigin{data: data, ignoreRange: true})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|norange", Size: int64(len(data)),
		RangeVerified: false, Client: client, Resolve: res.Resolve,
	})
	p := make([]byte, 256)
	if _, err := bs.ReadAt(context.Background(), p, 4096); err != nil {
		t.Fatalf("bounded skip should succeed: %v", err)
	}
	if string(p) != string(data[4096:4096+256]) {
		t.Fatal("discard-skip landed on the wrong offset")
	}
}

func TestByteSourceRangeIgnoringOriginBeyondSkipWindow(t *testing.T) {
	data := fixtureBytes(4096)
	// A large declared size with a Range-ignoring origin: the offset bound is
	// checked before any body is consumed, so no full download is attempted.
	_, client := startOrigin(t, &bsOrigin{data: data, ignoreRange: true})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|norange-far", Size: 32 << 20,
		RangeVerified: false, Client: client, Resolve: res.Resolve,
	})
	_, err := bs.ReadAt(context.Background(), make([]byte, 64), 16<<20)
	if ClassOf(err) != ClassNoSource {
		t.Fatalf("expected no_source beyond the skip window, got %v (%v)", ClassOf(err), err)
	}
}

func TestByteSourceNonMediaBodyIsMalformed(t *testing.T) {
	data := fixtureBytes(4096)
	_, client := startOrigin(t, &bsOrigin{data: data, ignoreRange: true, contentType: "text/html"})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|html", Size: int64(len(data)),
		RangeVerified: false, Client: client, Resolve: res.Resolve,
	})
	_, err := bs.ReadAt(context.Background(), make([]byte, 64), 0)
	if ClassOf(err) != ClassUpstreamMalformed {
		t.Fatalf("expected upstream_malformed, got %v (%v)", ClassOf(err), err)
	}
}

// ---------------------------------------------------------------------------
// DG-03: redirects, SSRF, header hygiene, cancellation
// ---------------------------------------------------------------------------

const bsHostB = "mirror.invalid"

func TestByteSourceFollowsRedirectInternally(t *testing.T) {
	data := fixtureBytes(8192)
	target := &bsOrigin{data: data, etag: `"v1"`}
	tsrv := httptest.NewServer(target)
	t.Cleanup(tsrv.Close)

	rsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+bsHostB+"/media.mp4", http.StatusFound)
	}))
	t.Cleanup(rsrv.Close)

	policy := &SecurityPolicy{
		MaxRedirects: DefaultMaxRedirects,
		resolver: &bsResolver{ips: map[string][]net.IP{
			bsHost:  {net.ParseIP("93.184.216.34")},
			bsHostB: {net.ParseIP("93.184.216.35")},
		}},
		dialer: &bsDialer{routes: map[string]string{
			"93.184.216.34": listenAddr(rsrv),
			"93.184.216.35": listenAddr(tsrv),
		}},
	}
	client := NewHTTPClient(policy, TrustSource, TransportStream)
	t.Cleanup(client.CloseIdleConnections)

	res := &staticResolver{
		url: originURL("/media.mp4?token=SIGNEDVALUE"),
		headers: map[string]string{
			"Authorization": "Bearer SECRET",
			"X-Auth":        "SECRET",
		},
	}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|redirect", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	p := make([]byte, 512)
	if _, err := bs.ReadAt(context.Background(), p, 1024); err != nil {
		t.Fatalf("redirect should be followed internally: %v", err)
	}
	if string(p) != string(data[1024:1024+512]) {
		t.Fatal("bytes after redirect differ from origin content")
	}
	if target.count() == 0 {
		t.Fatal("redirect target was never reached")
	}
	seen := target.seenHeaders()
	for _, name := range []string{"Authorization", "Referer", "X-Auth"} {
		if seen.Get(name) != "" {
			t.Fatalf("cross-origin redirect leaked %s", name)
		}
	}
	if seen.Get("Range") != "bytes=1024-1535" || seen.Get("Accept-Encoding") != "identity" {
		t.Fatalf("redirect lost bounded transport headers: %#v", seen)
	}
}

func TestByteSourceRedirectLoopIsCappedAndSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+bsHost+"/loop?tok=SIGNEDVALUE", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	policy := &SecurityPolicy{
		MaxRedirects: DefaultMaxRedirects,
		resolver:     &bsResolver{ips: map[string][]net.IP{bsHost: {net.ParseIP("93.184.216.34")}}},
		dialer:       &bsDialer{routes: map[string]string{"93.184.216.34": listenAddr(srv)}},
	}
	client := NewHTTPClient(policy, TrustSource, TransportStream)
	t.Cleanup(client.CloseIdleConnections)
	res := &staticResolver{url: originURL("/loop")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|loop", Size: 4096,
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	_, err := bs.ReadAt(context.Background(), make([]byte, 64), 0)
	if err == nil {
		t.Fatal("expected the redirect cap to fail the read")
	}
	assertNoSecrets(t, err)
}

func TestByteSourceSSRFDeniedAddress(t *testing.T) {
	policy := &SecurityPolicy{
		MaxRedirects: DefaultMaxRedirects,
		resolver:     &bsResolver{ips: map[string][]net.IP{bsHost: {net.ParseIP("127.0.0.1")}}},
	}
	client := NewHTTPClient(policy, TrustSource, TransportStream)
	t.Cleanup(client.CloseIdleConnections)
	res := &staticResolver{url: originURL("/media.mp4?tok=SIGNEDVALUE")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|ssrf", Size: 4096,
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	_, err := bs.ReadAt(context.Background(), make([]byte, 64), 0)
	if ClassOf(err) != ClassBackendUnavailable {
		t.Fatalf("expected backend_unavailable for a denied address, got %v (%v)", ClassOf(err), err)
	}
	assertNoSecrets(t, err)
}

func TestByteSourceHeaderHygiene(t *testing.T) {
	data := fixtureBytes(4096)
	o, client := startOrigin(t, &bsOrigin{data: data})
	res := &staticResolver{
		url:     originURL("/media.mp4"),
		headers: map[string]string{"Authorization": "Bearer SECRET", "X-Trace": "abc"},
	}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|headers", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 128); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	seen := o.seenHeaders()
	if seen.Get("Authorization") != "Bearer SECRET" {
		t.Fatal("transient source auth header was not applied upstream")
	}
	if seen.Get("Range") != "bytes=128-191" {
		t.Fatalf("DH must own the upstream Range header, got %q", seen.Get("Range"))
	}
	if seen.Get("Accept-Encoding") != "identity" {
		t.Fatalf("expected identity encoding, got %q", seen.Get("Accept-Encoding"))
	}
}

func TestBadByteSourceHeaderFiltersTransportHeaders(t *testing.T) {
	for _, name := range []string{"range", "Host", "content-length", "Transfer-Encoding", "connection", "Accept-Encoding", "TE"} {
		if !badByteSourceHeader(name) {
			t.Fatalf("%q must be filtered", name)
		}
	}
	for _, name := range []string{"Authorization", "Referer", "User-Agent", "X-Trace"} {
		if badByteSourceHeader(name) {
			t.Fatalf("%q must be forwarded", name)
		}
	}
}

func TestByteSourceCancellation(t *testing.T) {
	data := fixtureBytes(4096)
	o, client := startOrigin(t, &bsOrigin{data: data})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|cancel", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := bs.ReadAt(ctx, make([]byte, 64), 0)
	if err == nil || n != 0 {
		t.Fatalf("cancelled read must fail with no bytes, got n=%d err=%v", n, err)
	}
	if o.count() != 0 {
		t.Fatalf("cancelled read must not contact the origin, got %d requests", o.count())
	}
}

func TestByteSourceCancellationMidFlight(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	policy := &SecurityPolicy{
		MaxRedirects: DefaultMaxRedirects,
		resolver:     &bsResolver{ips: map[string][]net.IP{bsHost: {net.ParseIP("93.184.216.34")}}},
		dialer:       &bsDialer{routes: map[string]string{"93.184.216.34": listenAddr(srv)}},
	}
	client := NewHTTPClient(policy, TrustSource, TransportStream)
	t.Cleanup(client.CloseIdleConnections)
	res := &staticResolver{url: originURL("/slow")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|slow", Size: 4096,
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := bs.ReadAt(ctx, make([]byte, 64), 0); err == nil {
		t.Fatal("expected a cancelled mid-flight read to fail")
	}
}

// ---------------------------------------------------------------------------
// DG-04 / LG-10: nothing leaks a URL, query, or credential
// ---------------------------------------------------------------------------

func assertNoSecrets(t *testing.T, err error) {
	t.Helper()
	for _, s := range []string{err.Error(), Sanitize(err)} {
		for _, needle := range []string{"tok=", "SIGNEDVALUE", "http://", "https://", bsHost, "Bearer", "SECRET"} {
			if strings.Contains(s, needle) {
				t.Fatalf("error surface leaked %q: %s", needle, s)
			}
		}
	}
}

func TestByteSourceErrorsCarryNoURLOrToken(t *testing.T) {
	data := fixtureBytes(4096)
	_, client := startOrigin(t, &bsOrigin{data: data, forceStatus: http.StatusInternalServerError})
	res := &staticResolver{
		url:     originURL("/media.mp4?tok=SIGNEDVALUE&sig=abc"),
		headers: map[string]string{"Authorization": "Bearer SECRET"},
	}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|redact", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	_, err := bs.ReadAt(context.Background(), make([]byte, 64), 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	assertNoSecrets(t, err)
}

func TestByteSourceRejectsURLBearingKey(t *testing.T) {
	client := &http.Client{}
	res := &staticResolver{url: originURL("/media.mp4")}
	bad := []string{
		"http://origin.invalid/media.mp4",
		"item?tok=SIGNEDVALUE",
		"item&x=1",
		"",
		"   ",
	}
	for _, key := range bad {
		if _, err := NewByteSource(ByteSourceConfig{
			Key: key, Size: 10, Client: client, Resolve: res.Resolve,
		}); err == nil {
			t.Fatalf("key %q must be rejected", key)
		}
	}
	if _, err := NewByteSource(ByteSourceConfig{
		Key: "http|item|file", Size: 0, Client: client, Resolve: res.Resolve,
	}); err == nil {
		t.Fatal("non-positive size must be rejected")
	}
	if _, err := NewByteSource(ByteSourceConfig{
		Key: "http|item|file", Size: 10, Resolve: res.Resolve,
	}); err == nil {
		t.Fatal("nil client must be rejected")
	}
	if _, err := NewByteSource(ByteSourceConfig{
		Key: "http|item|file", Size: 10, Client: client,
	}); err == nil {
		t.Fatal("nil resolver must be rejected")
	}
}

// ---------------------------------------------------------------------------
// DG-07: concurrency, cancellation isolation, no leaked goroutines
// ---------------------------------------------------------------------------

func TestByteSourceConcurrentReads(t *testing.T) {
	data := fixtureBytes(64 << 10)
	_, client := startOrigin(t, &bsOrigin{data: data, etag: `"v1"`})
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|concurrent", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i * 1024)
			p := make([]byte, 512)
			if _, err := bs.ReadAt(context.Background(), p, off); err != nil {
				errs <- err
				return
			}
			if string(p) != string(data[off:off+512]) {
				errs <- fmt.Errorf("content mismatch at %d", off)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read: %v", err)
	}
}

func TestByteSourceNoLeakedGoroutines(t *testing.T) {
	baseline := settledGoroutines()
	data := fixtureBytes(16 << 10)
	o := &bsOrigin{data: data, etag: `"v1"`}
	srv := httptest.NewServer(o)
	policy := &SecurityPolicy{
		MaxRedirects: DefaultMaxRedirects,
		resolver:     &bsResolver{ips: map[string][]net.IP{bsHost: {net.ParseIP("93.184.216.34")}}},
		dialer:       &bsDialer{routes: map[string]string{"93.184.216.34": listenAddr(srv)}},
	}
	client := NewHTTPClient(policy, TrustSource, TransportStream)
	res := &staticResolver{url: originURL("/media.mp4")}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|leak", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve,
	})
	for i := 0; i < 25; i++ {
		if _, err := bs.ReadAt(context.Background(), make([]byte, 256), int64(i*256)); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	// A cancelled read must not strand anything either.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = bs.ReadAt(ctx, make([]byte, 256), 0)

	client.CloseIdleConnections()
	srv.Close()
	if got := settledGoroutines(); got > baseline+2 {
		t.Fatalf("goroutine leak: baseline %d, after %d", baseline, got)
	}
}

func settledGoroutines() int {
	best := runtime.NumGoroutine()
	for i := 0; i < 40; i++ {
		runtime.Gosched()
		time.Sleep(25 * time.Millisecond)
		if n := runtime.NumGoroutine(); n < best {
			best = n
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// DG-03 fuzz: upstream response classification never fabricates bytes
// ---------------------------------------------------------------------------

// fuzzRT serves the truthful body for whatever range is requested while the
// fuzzer controls the status line and the validating headers, so a success
// return can be checked for byte-identity against the reference buffer.
type fuzzRT struct {
	data     []byte
	status   int
	cr       string
	etag     string
	lastMod  string
	truncate int
}

func (rt *fuzzRT) RoundTrip(req *http.Request) (*http.Response, error) {
	hdr := make(http.Header)
	hdr.Set("Content-Type", "video/mp4")
	if rt.etag != "" {
		hdr.Set("ETag", rt.etag)
	}
	if rt.lastMod != "" {
		hdr.Set("Last-Modified", rt.lastMod)
	}
	var body []byte
	start, end, ok := parseTestRange(req.Header.Get("Range"))
	if ok && start < int64(len(rt.data)) {
		if end >= int64(len(rt.data)) {
			end = int64(len(rt.data)) - 1
		}
		body = rt.data[start : end+1]
		cr := rt.cr
		if cr == "" {
			cr = fmt.Sprintf("bytes %d-%d/%d", start, end, len(rt.data))
		}
		hdr.Set("Content-Range", cr)
	} else if rt.cr != "" {
		hdr.Set("Content-Range", rt.cr)
	}
	if rt.truncate > 0 && rt.truncate < len(body) {
		body = body[:rt.truncate]
	}
	status := rt.status
	if status <= 0 || status > 599 {
		status = http.StatusPartialContent
	}
	return &http.Response{
		StatusCode:    status,
		Header:        hdr,
		Body:          io.NopCloser(strings.NewReader(string(body))),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

func FuzzByteSourceUpstream(f *testing.F) {
	f.Add(206, "", `"v1"`, "", 0, int64(0), 64)
	f.Add(206, "bytes 0-63/2048", `"v1"`, "", 0, int64(0), 64)
	f.Add(206, "bytes 0-63/9999", "", "", 0, int64(0), 64)
	f.Add(200, "", "", "", 0, int64(0), 32)
	f.Add(416, "bytes */2048", "", "", 0, int64(100), 16)
	f.Add(429, "", "", "", 0, int64(0), 16)
	f.Add(206, "bytes x-y/z", "", "", 0, int64(10), 16)
	f.Add(206, "", `W/"v1"`, "Mon, 01 Jun 2026 00:00:00 GMT", 7, int64(500), 128)

	data := fixtureBytes(2048)
	f.Fuzz(func(t *testing.T, status int, cr, etag, lastMod string, truncate int, off int64, plen int) {
		if plen <= 0 {
			plen = 1
		}
		if plen > 512 {
			plen = 512
		}
		if off < 0 {
			off = -off
		}
		off %= int64(len(data))
		if truncate < 0 {
			truncate = 0
		}
		client := &http.Client{Transport: &fuzzRT{
			data: data, status: status, cr: cr, etag: etag, lastMod: lastMod, truncate: truncate,
		}}
		res := &staticResolver{url: "http://origin.invalid/media.mp4"}
		bs, err := NewByteSource(ByteSourceConfig{
			Key: "http|fuzz", Size: int64(len(data)),
			RangeVerified: true, Client: client, Resolve: res.Resolve,
		})
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		p := make([]byte, plen)
		n, rerr := bs.ReadAt(context.Background(), p, off)
		if n < 0 || n > len(p) {
			t.Fatalf("impossible count %d", n)
		}
		// The one invariant that matters: a successful read is always the
		// exact requested bytes. No fabrication, no misplacement, no silent
		// short return.
		if rerr == nil {
			if n != len(p) {
				t.Fatalf("nil error with short count %d/%d", n, len(p))
			}
			want := data[off : off+int64(len(p))]
			if string(p) != string(want) {
				t.Fatalf("successful read returned wrong bytes at off=%d", off)
			}
		}
		// Any bytes reported must match the reference prefix, error or not.
		if n > 0 {
			end := off + int64(n)
			if end > int64(len(data)) {
				t.Fatalf("read past the reference buffer: off=%d n=%d", off, n)
			}
			if string(p[:n]) != string(data[off:end]) {
				t.Fatalf("fabricated bytes at off=%d n=%d", off, n)
			}
		}
		if rerr != nil {
			assertNoSecretsInString(t, Sanitize(rerr))
		}
	})
}

func assertNoSecretsInString(t *testing.T, s string) {
	t.Helper()
	for _, needle := range []string{"tok=", "http://", "https://", "origin.invalid"} {
		if strings.Contains(s, needle) {
			t.Fatalf("sanitized error leaked %q: %s", needle, s)
		}
	}
}
