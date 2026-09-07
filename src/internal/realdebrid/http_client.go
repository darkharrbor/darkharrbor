package realdebrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

const (
	defaultBaseURL        = "https://api.real-debrid.com/rest/1.0"
	defaultRequestTimeout = 30 * time.Second
	defaultUserAgent      = "DarkHarrbor/1.0"
	maxResponseBytes      = 8 << 20 // 8MB cap on response bodies

	// errCodeInfringing is RD's error_code for DMCA-blocked hashes.
	// Returned by addMagnet/addTorrent when the hash is on their blocklist.
	// Permanent — never retryable; the arr must blocklist and re-grab.
	errCodeInfringing = 35
)

// ErrInfringingFile is returned by AddMagnet when RD rejects a hash as
// DMCA-blocked (error_code 35, "infringing_file"). It is not retryable.
type ErrInfringingFile struct{ Hash string }

func (e *ErrInfringingFile) Error() string {
	return fmt.Sprintf("rd: infringing_file: hash %s is DMCA-blocked on Real-Debrid", e.Hash)
}

// IsInfringingFile reports whether err is an ErrInfringingFile.
func IsInfringingFile(err error) bool {
	var e *ErrInfringingFile
	return errors.As(err, &e)
}

// HTTPClient is the concrete Real-Debrid API client. It satisfies Client.
type HTTPClient struct {
	baseURL   string
	apiToken  string
	userAgent string
	http      *http.Client
	policy    provider.Policy
}

// NewHTTPClient constructs an HTTPClient. policy must not be nil.
func NewHTTPClient(baseURL, apiToken, userAgent string, timeout time.Duration, policy provider.Policy) *HTTPClient {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return &HTTPClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiToken:  apiToken,
		userAgent: userAgent,
		http:      &http.Client{Timeout: timeout},
		policy:    policy,
	}
}

var _ Client = (*HTTPClient)(nil)

// GetUser fetches /user for plan/capability discovery.
func (c *HTTPClient) GetUser(ctx context.Context) (*AccountInfo, error) {
	body, err := c.do(ctx, provider.OpQuery, http.MethodGet, "/user", nil, "")
	if err != nil {
		return nil, err
	}
	return parseUserResponse(body)
}

// InstantAvailability was RD's cache oracle. It is permanently disabled
// (returns error_code 37 "disabled_endpoint"). Returns empty without making
// a network call. CheckCached in the adapter always returns false for RD.
func (c *HTTPClient) InstantAvailability(_ context.Context, _ []string) (map[string][]CachedGroup, error) {
	return nil, nil
}

// AddMagnet submits a magnet URI. Returns the RD item ID.
func (c *HTTPClient) AddMagnet(ctx context.Context, magnet string) (string, error) {
	form := url.Values{}
	form.Set("magnet", magnet)
	body, err := c.do(ctx, provider.OpCreate, http.MethodPost, "/torrents/addMagnet",
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return "", err
	}
	return parseAddMagnetResponse(body)
}

// SelectFiles tells RD which files to download. files = "all" or comma-separated IDs.
func (c *HTTPClient) SelectFiles(ctx context.Context, rdID string, files string) error {
	form := url.Values{}
	form.Set("files", files)
	_, err := c.do(ctx, provider.OpCreate, http.MethodPost,
		"/torrents/selectFiles/"+url.PathEscape(rdID),
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	return err
}

// GetTorrentInfo fetches current status for a submitted item.
func (c *HTTPClient) GetTorrentInfo(ctx context.Context, rdID string) (*TorrentInfo, error) {
	body, err := c.do(ctx, provider.OpQuery, http.MethodGet,
		"/torrents/info/"+url.PathEscape(rdID), nil, "")
	if err != nil {
		return nil, err
	}
	return parseTorrentInfo(body)
}

// UnrestrictLink converts an RD internal link to a direct CDN URL.
func (c *HTTPClient) UnrestrictLink(ctx context.Context, link string) (string, error) {
	form := url.Values{}
	form.Set("link", link)
	body, err := c.do(ctx, provider.OpQuery, http.MethodPost, "/unrestrict/link",
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return "", err
	}
	return parseUnrestrictResponse(body)
}

// DeleteTorrent removes a submitted item from the user's RD account.
func (c *HTTPClient) DeleteTorrent(ctx context.Context, rdID string) error {
	_, err := c.do(ctx, provider.OpControl, http.MethodDelete,
		"/torrents/delete/"+url.PathEscape(rdID), nil, "")
	return err
}

// do executes one RD API call: acquires rate-limit token, sends the request,
// handles 4xx/5xx, feeds 429 Retry-After back to the policy.
func (c *HTTPClient) do(ctx context.Context, op provider.Op, method, path string, body io.Reader, contentType string) ([]byte, error) {
	if err := c.policy.Acquire(ctx, op); err != nil {
		return nil, fmt.Errorf("rd: rate limit: %w", err)
	}

	rawURL := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("rd: build request %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("User-Agent", c.userAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, provider.MarkRetryable(fmt.Errorf("rd: %s %s: %w", method, path, err))
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, provider.MarkRetryable(fmt.Errorf("rd: read response %s %s: %w", method, path, err))
	}

	switch resp.StatusCode {
	case http.StatusNoContent: // 204: DELETE success
		return nil, nil
	case http.StatusOK, http.StatusCreated:
		// Check for RD application-level errors embedded in 200 responses.
		// RD returns some errors as HTTP 200 with a JSON error body.
		if len(respBody) > 0 && respBody[0] == '{' {
			var errResp struct {
				Error     string `json:"error"`
				ErrorCode int    `json:"error_code"`
			}
			if jsonErr := json.Unmarshal(respBody, &errResp); jsonErr == nil && errResp.Error != "" {
				if errResp.ErrorCode == errCodeInfringing {
					return nil, &ErrInfringingFile{}
				}
				return nil, fmt.Errorf("rd: api error %d: %s", errResp.ErrorCode, errResp.Error)
			}
		}
		return respBody, nil
	case http.StatusTooManyRequests:
		// Feed Retry-After back to the policy so future calls back off.
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		c.policy.OnResponse(op, http.StatusTooManyRequests, retryAfter)
		return nil, provider.MarkRetryable(fmt.Errorf("rd: 429 rate limited (retry after %v)", retryAfter))
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("rd: 401 unauthorized — check HARRBOR_RD_API_TOKEN")
	case http.StatusForbidden:
		return nil, fmt.Errorf("rd: 403 forbidden — account may lack premium access")
	case http.StatusNotFound:
		return nil, fmt.Errorf("rd: 404 not found: %s", path)
	case http.StatusUnavailableForLegalReasons: // 451: infringing_file at unrestrict/link time
		// The filter fires here for existing cached content whose filename
		// matches RD's keyword blocklist (May 2026). Same outcome as addMagnet
		// error_code 35 — permanent, non-retryable.
		return nil, &ErrInfringingFile{}
	default:
		if resp.StatusCode >= 500 {
			return nil, provider.MarkRetryable(fmt.Errorf("rd: %d server error: %s", resp.StatusCode, truncate(string(respBody), 200)))
		}
		return nil, fmt.Errorf("rd: %d error: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
}

// ListTorrents returns the user's torrent list (up to limit items).
func (c *HTTPClient) ListTorrents(ctx context.Context, limit int) ([]TorrentInfo, error) {
	if limit <= 0 {
		limit = 100
	}
	path := fmt.Sprintf("/torrents?limit=%d", limit)
	body, err := c.do(ctx, provider.OpQuery, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	var items []struct {
		ID       string   `json:"id"`
		Filename string   `json:"filename"`
		Hash     string   `json:"hash"`
		Status   string   `json:"status"`
		Progress float64  `json:"progress"`
		Links    []string `json:"links"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("rd: parse torrent list: %w", err)
	}
	result := make([]TorrentInfo, 0, len(items))
	for _, item := range items {
		result = append(result, TorrentInfo{
			ID:       item.ID,
			Filename: item.Filename,
			Hash:     item.Hash,
			Status:   item.Status,
			Progress: item.Progress,
			Links:    item.Links,
		})
	}
	return result, nil
}

// FindByHash finds an existing user torrent by infohash (case-insensitive).
// Returns (id, true, nil) if found as "downloaded"; ("", false, nil) otherwise.
func (c *HTTPClient) FindByHash(ctx context.Context, hash string) (string, bool, error) {
	items, err := c.ListTorrents(ctx, 2500)
	if err != nil {
		return "", false, err
	}
	for _, item := range items {
		if strings.EqualFold(item.Hash, hash) && strings.EqualFold(item.Status, "downloaded") {
			return item.ID, true, nil
		}
	}
	return "", false, nil
}

// ActiveCount returns the number of currently active (non-completed) torrents.
func (c *HTTPClient) ActiveCount(ctx context.Context) (int, error) {
	body, err := c.do(ctx, provider.OpQuery, http.MethodGet, "/torrents/activeCount", nil, "")
	if err != nil {
		return 0, err
	}
	var data struct {
		NB int `json:"nb"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return 0, fmt.Errorf("rd: parse activeCount: %w", err)
	}
	return data.NB, nil
}

// parseRetryAfter parses a Retry-After header value (seconds integer or
// HTTP-date). Falls back to 60s on parse failure.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 60 * time.Second
	}
	// Try integer seconds first.
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	// Try HTTP-date.
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 60 * time.Second
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
