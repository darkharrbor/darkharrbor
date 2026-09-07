package torbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/provider"
)

// HTTPClient is the concrete TorBox HTTP client. It implements Client.
//
// Waiters (createLimiter/pollLimiter/dlLimiter) are replaced by a single
// provider.Policy. Every call acquires its op class before the request and
// feeds the response status plus any Retry-After header back into the
// policy, so 429 hold-until deadlines are honored per op class.
type HTTPClient struct {
	log        *slog.Logger
	baseURL    string
	apiToken   string
	userAgent  string
	httpClient *http.Client
	// noRedirectClient is used only for requestdl — we want the permalink,
	// not the CDN redirect target.
	noRedirectClient *http.Client
	policy           provider.Policy
	// B-O4: all-time API call + byte counters. Nil → metering disabled.
	metrics *Metrics
}

func NewHTTPClient(
	log *slog.Logger,
	baseURL, apiToken, userAgent string,
	timeout time.Duration,
	policy provider.Policy,
) *HTTPClient {
	return &HTTPClient{
		log:       log,
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiToken:  apiToken,
		userAgent: userAgent,
		metrics:   newMetrics(),
		httpClient: &http.Client{
			Timeout: timeout,
			// Force IPv4: the container has no IPv6 egress; Go's Happy Eyeballs
			// race (RFC 6555) with IPv6 unreachable can produce a corrupted
			// connection that TorBox rejects as BAD_TOKEN before the IPv4 fallback
			// completes cleanly. DualStack:false pins the dialer to tcp4.
			Transport: &http.Transport{
				// Force tcp4: container has no IPv6 egress; dialing IPv6-first
				// returns a corrupted connection that TorBox rejects as BAD_TOKEN.
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return (&net.Dialer{
						Timeout:   30 * time.Second,
						KeepAlive: 30 * time.Second,
					}).DialContext(ctx, "tcp4", addr)
				},
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
			// Preserve the Authorization header across redirects on the same host.
			// TorBox occasionally issues 302s for maintenance/CDN reasons; without
			// this, Go strips Authorization on redirect and we get an HTML response.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				// Copy Authorization only when the redirect target stays on the
				// original TorBox origin; Go intentionally strips credentials on
				// cross-origin redirects.
				if via[0].URL != nil && req.URL != nil &&
					strings.EqualFold(req.URL.Scheme, via[0].URL.Scheme) &&
					strings.EqualFold(req.URL.Host, via[0].URL.Host) {
					if auth := via[0].Header.Get("Authorization"); auth != "" {
						req.Header.Set("Authorization", auth) // #nosec G119 -- same-origin redirect only; credentials are not propagated cross-host.
					}
				}
				return nil
			},
		},
		noRedirectClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return (&net.Dialer{
						Timeout:   30 * time.Second,
						KeepAlive: 30 * time.Second,
					}).DialContext(ctx, "tcp4", addr)
				},
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		policy: policy,
	}
}

// acquire blocks on the policy for the op class. Nil policy is a no-op
// (unit-test convenience; production wiring always supplies one).
func (c *HTTPClient) acquire(ctx context.Context, op provider.Op) error {
	if c.policy == nil {
		return nil
	}
	if err := c.policy.Acquire(ctx, op); err != nil {
		return fmt.Errorf("rate limiter wait: %w", err)
	}
	return nil
}

// feedback reports a response to the policy (429 + Retry-After installs a
// per-op hold-until deadline; everything else is ignored by BasicPolicy).
func (c *HTTPClient) feedback(op provider.Op, status int, retryAfterHeader string) {
	if c.policy == nil {
		return
	}
	c.policy.OnResponse(op, status, parseRetryAfter(retryAfterHeader))
}

func retryAfterHeaderValue(h http.Header) string {
	if v := h.Get("Retry-After"); v != "" {
		return v
	}
	return h.Get("X-RateLimit-After")
}

// parseRetryAfter parses an RFC 9110 Retry-After header value: either a
// non-negative integer number of seconds or an HTTP-date.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// do executes one authenticated request for the given op class and decodes
// the TorBox envelope.
func (c *HTTPClient) do(ctx context.Context, op provider.Op, method, path string, body io.Reader, contentType string) (*apiEnvelope, error) {
	if err := c.acquire(ctx, op); err != nil {
		return nil, err
	}
	// B-O4: meter this call by op class.
	if c.metrics != nil {
		c.metrics.CallsTotal.Add(1)
		switch op {
		case provider.OpCreate, provider.OpCreateCached, provider.OpCreateUncached:
			c.metrics.CallsCreate.Add(1)
		case provider.OpQuery:
			c.metrics.CallsQuery.Add(1)
		case provider.OpControl:
			c.metrics.CallsControl.Add(1)
		}
	}
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		safeErr := httpstream.Sanitize(err)
		c.warn("torbox http request failed",
			"method", method,
			"path", sanitizeLoggedPath(path),
			"duration", time.Since(started).String(),
			"error", safeErr,
		)
		if isNetRetryable(err) {
			return nil, MarkRetryable(fmt.Errorf("torbox request failed: %s", safeErr))
		}
		return nil, fmt.Errorf("torbox request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	c.feedback(op, resp.StatusCode, retryAfterHeaderValue(resp.Header))

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	c.debug("torbox http response",
		"method", method,
		"path", sanitizeLoggedPath(path),
		"status", resp.StatusCode,
		"duration", time.Since(started).String(),
		"response_bytes", len(raw),
	)

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, MarkRetryable(fmt.Errorf("torbox status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("torbox status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var env apiEnvelope
	if len(bytes.TrimSpace(raw)) == 0 {
		return &env, nil
	}
	// Guard against HTML responses (Cloudflare challenges, maintenance pages,
	// redirect targets that strip the Authorization header). These are always
	// transient — treat as retryable so the submit/resolver loops back off
	// and retry rather than permanently failing the item.
	if len(raw) > 0 && raw[0] == '<' {
		return nil, MarkRetryable(fmt.Errorf("torbox returned non-JSON response (maintenance/cloudflare?): %s",
			string(bytes.TrimSpace(raw[:min(len(raw), 120)]))))
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, MarkRetryable(fmt.Errorf("decode response envelope: %w", err))
	}
	if !env.Success && env.Error != nil {
		return nil, fmt.Errorf("torbox api error: %v (%s)", env.Error, env.Detail)
	}
	return &env, nil
}

// CreateTorrentTask submits a torrent (magnet or .torrent file) to TorBox.
func (c *HTTPClient) CreateTorrentTask(ctx context.Context, req CreateTorrentTaskRequest) (*CreateTaskResponse, error) {
	// TorBox torrent create endpoint uses multipart when uploading a .torrent
	// file, or form-encoded when submitting a magnet.
	if len(req.TorrentFile) > 0 {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		name := req.TorrentFileName
		if name == "" {
			name = "upload.torrent"
		}
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			return nil, fmt.Errorf("createtorrent multipart: %w", err)
		}
		if _, err := fw.Write(req.TorrentFile); err != nil {
			return nil, fmt.Errorf("createtorrent multipart write: %w", err)
		}
		if req.Name != "" {
			_ = mw.WriteField("name", req.Name)
		}
		if req.AsQueued {
			_ = mw.WriteField("as_queued", "true")
		}
		if req.AddOnlyIfCached {
			_ = mw.WriteField("add_only_if_cached", "true")
		}
		if err := mw.Close(); err != nil {
			return nil, fmt.Errorf("createtorrent multipart close: %w", err)
		}
		op := createPolicyOp(req.PolicyOp, req.AddOnlyIfCached)
		env, err := c.do(ctx, op, http.MethodPost, "/api/torrents/createtorrent",
			&buf, mw.FormDataContentType())
		if err != nil {
			return nil, err
		}
		return parseCreateTask(env)
	}
	form := url.Values{}
	if req.Magnet != "" {
		form.Set("magnet", req.Magnet)
	}
	if req.Name != "" {
		form.Set("name", req.Name)
	}
	if req.AsQueued {
		form.Set("as_queued", "true")
	}
	if req.AddOnlyIfCached {
		form.Set("add_only_if_cached", "true")
	}
	op := createPolicyOp(req.PolicyOp, req.AddOnlyIfCached)
	env, err := c.do(ctx, op, http.MethodPost, "/api/torrents/createtorrent",
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return nil, err
	}
	return parseCreateTask(env)
}

// CreateUsenetTask submits an NZB to TorBox.
func (c *HTTPClient) CreateUsenetTask(ctx context.Context, req CreateUsenetTaskRequest) (*CreateTaskResponse, error) {
	form := url.Values{}
	if req.Link != "" {
		form.Set("link", req.Link)
	}
	if req.Name != "" {
		form.Set("name", req.Name)
	}
	if req.Password != "" {
		form.Set("password", req.Password)
	}
	if req.AsQueued {
		form.Set("as_queued", "true")
	}
	if req.AddOnlyIfCached {
		form.Set("add_only_if_cached", "true")
	}
	op := createPolicyOp(req.PolicyOp, req.AddOnlyIfCached)
	env, err := c.do(ctx, op, http.MethodPost, "/api/usenet/createusenetdownload",
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return nil, err
	}
	return parseCreateTask(env)
}

func createPolicyOp(op provider.Op, addOnlyIfCached bool) provider.Op {
	if op != "" {
		return op
	}
	if addOnlyIfCached {
		return provider.OpCreateCached
	}
	return provider.OpCreateUncached
}

// CheckCachedTorrent queries TorBox's checkcached endpoint for a torrent hash.
func (c *HTTPClient) CheckCachedTorrent(ctx context.Context, hash string) (*CheckCachedResult, error) {
	path := "/api/torrents/checkcached?hash=" + url.QueryEscape(hash) + "&format=object&list_files=true"
	env, err := c.do(ctx, provider.OpQuery, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	return parseCheckcachedResponse(env, hash)
}

// CheckCachedUsenet queries TorBox's usenet checkcached endpoint.
func (c *HTTPClient) CheckCachedUsenet(ctx context.Context, nzbHash string) (*CheckCachedResult, error) {
	path := "/api/usenet/checkcached?hash=" + url.QueryEscape(nzbHash) + "&format=object&list_files=true"
	env, err := c.do(ctx, provider.OpQuery, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	return parseCheckcachedResponse(env, nzbHash)
}

// GetQueuedStatus polls the as_queued status of a submitted item.
func (c *HTTPClient) GetQueuedStatus(ctx context.Context, sourceType string, queuedID string) (*TaskStatus, error) {
	path := fmt.Sprintf("/api/%s/mylist?id=%s&bypass_cache=true", endpointFamily(sourceType), url.QueryEscape(queuedID))
	env, err := c.do(ctx, provider.OpQuery, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	items, err := parseItemsEnvelope(env)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		return parseTaskStatus(sourceType, item), nil
	}
	return nil, fmt.Errorf("queued item %s not found", queuedID)
}

// GetTaskStatus polls the status of an active (non-queued) TorBox item.
func (c *HTTPClient) GetTaskStatus(ctx context.Context, sourceType string, remoteID string) (*TaskStatus, error) {
	path := fmt.Sprintf("/api/%s/mylist?id=%s&bypass_cache=true", endpointFamily(sourceType), url.QueryEscape(remoteID))
	env, err := c.do(ctx, provider.OpQuery, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	items, err := parseItemsEnvelope(env)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		return parseTaskStatus(sourceType, item), nil
	}
	return nil, fmt.Errorf("item %s not found", remoteID)
}

// RequestDownloadURL returns the requestdl permalink for a specific file in a
// TorBox item. The returned URL embeds the API token as a query parameter and
// has redirect=true so the client (Jellyfin) is sent directly to the CDN when
// it opens the .strm file. The URL is placed verbatim into the .strm — it has
// a 3-hour window to START streaming; Dark Harrbor is not in the byte path.
//
// sourceType must be "torrents" or "usenet". remoteID is the TorBox
// torrent_id / usenetdownload_id. fileID is the per-file id from mylist.
//
// No network call is made (the URL is deterministic from its parameters);
// the policy acquire on OpQuery is retained deliberately to preserve the
// pre-policy dlLimiter pacing of resolve-time URL construction.
func (c *HTTPClient) RequestDownloadURL(ctx context.Context, sourceType string, remoteID string, fileID string) (string, error) {
	if err := c.acquire(ctx, provider.OpQuery); err != nil {
		return "", err
	}

	family := endpointFamily(sourceType)
	// TorBox requestdl requires the API token as a query param (not just Bearer)
	// when used with redirect=true, so the redirect target carries auth.
	// remoteID param name differs by endpoint family.
	remoteIDParam := "torrent_id"
	if family == "usenet" {
		remoteIDParam = "usenet_id"
	}

	q := url.Values{}
	q.Set("token", c.apiToken)
	q.Set(remoteIDParam, remoteID)
	q.Set("file_id", fileID)
	q.Set("redirect", "true")

	rawURL := fmt.Sprintf("%s/api/%s/requestdl?%s", c.baseURL, family, q.Encode())

	// Use noRedirectClient — we want to store the requestdl permalink itself
	// in the .strm file, not follow the redirect to the ephemeral CDN URL.
	// The 302 Location header IS the permalink (TorBox returns the signed CDN
	// URL as the redirect target, but the requestdl URL itself is what we put
	// in the .strm so Jellyfin re-resolves it on playback).
	//
	// In practice: just return the constructed URL directly without making a
	// network call — the URL is deterministic from its parameters and the token.
	// This avoids an extra round-trip at resolve time and keeps the .strm URL
	// stable across calls. The Jellyfin client opens the URL when the user plays
	// the item; TorBox then issues the 302 to the CDN.
	_ = c.noRedirectClient // available if a preflight check is ever needed

	c.debug("requestdl url constructed",
		"source_type", family,
		"remote_id", remoteID,
		"file_id", fileID,
	)

	return rawURL, nil
}

// ControlTorrent sends a control action (e.g. "delete") to a TorBox torrent item.
// OpControl shares the global policy budget — deletes are rare but share the
// same rate limit pool.
// TorBox controltorrent requires a JSON body with "operation" field (confirmed live).
func (c *HTTPClient) ControlTorrent(ctx context.Context, remoteID, action string) error {
	body := fmt.Sprintf(`{"torrent_id":%s,"operation":%q}`, remoteID, action)
	env, err := c.do(ctx, provider.OpControl, http.MethodPost, "/api/torrents/controltorrent",
		strings.NewReader(body), "application/json")
	if err != nil {
		return fmt.Errorf("controltorrent %s %s: %w", action, remoteID, err)
	}
	_ = env
	return nil
}

// ControlUsenet sends a control action (e.g. "delete") to a TorBox usenet item.
// OpControl shares the global policy budget — deletes are rare but share the
// same rate limit pool.
// Uses JSON body matching TorBox API convention (field: "operation").
func (c *HTTPClient) ControlUsenet(ctx context.Context, remoteID, action string) error {
	body := fmt.Sprintf(`{"usenet_id":%s,"operation":%q}`, remoteID, action)
	env, err := c.do(ctx, provider.OpControl, http.MethodPost, "/api/usenet/controlusenetdownload",
		strings.NewReader(body), "application/json")
	if err != nil {
		return fmt.Errorf("controlusenet %s %s: %w", action, remoteID, err)
	}
	_ = env
	return nil
}

// ExportTorrentFile fetches the raw .torrent bytes TorBox holds for an
// already-submitted torrent via GET /api/torrents/exportdata?type=file
// (TS-0.2/T1). Unlike every other call in this file, the response is a
// binary file, not the JSON envelope c.do() decodes, so this talks to the
// endpoint directly while still honoring the same policy acquire/feedback
// pacing as every other query. TorBox documents this endpoint as
// incompatible with already-cached downloads; a non-2xx or non-bencode
// response is returned as an error and MUST be treated by the caller as
// best-effort absence, never as fatal (T1: "absence is non-fatal").
func (c *HTTPClient) ExportTorrentFile(ctx context.Context, remoteID string) ([]byte, error) {
	if err := c.acquire(ctx, provider.OpQuery); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("torrent_id", remoteID)
	q.Set("type", "file")
	path := "/api/torrents/exportdata?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Accept", "application/x-bittorrent, application/octet-stream, */*")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	if c.metrics != nil {
		c.metrics.CallsTotal.Add(1)
		c.metrics.CallsQuery.Add(1)
	}
	started := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		safeErr := httpstream.Sanitize(err)
		c.warn("torbox exportdata request failed",
			"remote_id", remoteID,
			"duration", time.Since(started).String(),
			"error", safeErr,
		)
		if isNetRetryable(err) {
			return nil, MarkRetryable(fmt.Errorf("torbox exportdata failed: %s", safeErr))
		}
		return nil, fmt.Errorf("torbox exportdata failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	c.feedback(provider.OpQuery, resp.StatusCode, retryAfterHeaderValue(resp.Header))

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, MarkRetryable(fmt.Errorf("torbox exportdata status %d", resp.StatusCode))
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("torbox exportdata status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read exportdata response: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("torbox exportdata: empty response")
	}
	if len(data) > maxTorrentFileBytes {
		return nil, fmt.Errorf("torbox exportdata: response exceeds %d bytes", maxTorrentFileBytes)
	}
	// TorBox occasionally answers a rejected export (maintenance,
	// Cloudflare challenge, or the documented cached-download rejection)
	// with a 200 status and an HTML/JSON body instead of a 4xx. A
	// well-formed .torrent always starts with 'd' (bencode dict); anything
	// else is absence, not a parse attempt.
	if data[0] != 'd' {
		return nil, fmt.Errorf("torbox exportdata: response is not a bencoded file (unexpected content)")
	}

	c.debug("torbox exportdata response",
		"remote_id", remoteID,
		"status", resp.StatusCode,
		"duration", time.Since(started).String(),
		"response_bytes", len(data),
	)
	return data, nil
}

// CountActiveSlots returns the live count of uncached TorBox torrents
// occupying an Allowed Active Slot. TorBox Usenet and already-cached torrent
// work are outside this account ceiling.
func (c *HTTPClient) CountActiveSlots(ctx context.Context) (int, error) {
	return c.countActiveForFamily(ctx, "torrents")
}

// countActiveForFamily lists every item in one mylist family and counts how
// many are in a slot-occupying state. An empty/absent list is zero active
// items, not an error — only transport/decode failures (already surfaced by
// c.do) propagate.
func (c *HTTPClient) countActiveForFamily(ctx context.Context, family string) (int, error) {
	path := fmt.Sprintf("/api/%s/mylist?bypass_cache=true", family)
	env, err := c.do(ctx, provider.OpQuery, http.MethodGet, path, nil, "")
	if err != nil {
		return 0, err
	}
	if env == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return 0, nil
	}
	items, err := parseItemsEnvelope(env)
	if err != nil {
		return 0, fmt.Errorf("parse active torrent list: %w", err)
	}
	n := 0
	for _, item := range items {
		if occupiesUncachedTorrentSlot(item) {
			n++
		}
	}
	return n, nil
}

// endpointFamily returns the TorBox API path segment for a source type.
func endpointFamily(sourceType string) string {
	if strings.EqualFold(sourceType, "nzb") || strings.EqualFold(sourceType, "usenet") {
		return "usenet"
	}
	return "torrents"
}

// Metrics returns the live metrics counter set for this client (B-O4).
// Never nil after NewHTTPClient.
func (c *HTTPClient) Metrics() *Metrics { return c.metrics }

func (c *HTTPClient) debug(msg string, args ...any) {
	if c.log != nil {
		c.log.Debug(msg, args...)
	}
}

func (c *HTTPClient) warn(msg string, args ...any) {
	if c.log != nil {
		c.log.Warn(msg, args...)
	}
}

func sanitizeLoggedPath(rawPath string) string {
	parsed, err := url.Parse(rawPath)
	if err != nil {
		return rawPath
	}
	query := parsed.Query()
	if _, ok := query["token"]; ok {
		query.Set("token", "[redacted]")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func isNetRetryable(err error) bool {
	if err == nil {
		return false
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}
