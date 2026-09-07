package alldebrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

const (
	defaultBaseURL        = "https://api.alldebrid.com"
	defaultRequestTimeout = 30 * time.Second
	defaultUserAgent      = "DarkHarrbor/1.0"
	maxResponseBytes      = 8 << 20
)

type HTTPClient struct {
	baseURL   string
	apiKey    string
	http      *http.Client
	policy    provider.Policy
	userAgent string
}

func NewHTTPClient(baseURL, apiKey, userAgent string, timeout time.Duration, policy provider.Policy) *HTTPClient {
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
		apiKey:    apiKey,
		userAgent: userAgent,
		http:      &http.Client{Timeout: timeout},
		policy:    policy,
	}
}

var _ Client = (*HTTPClient)(nil)

func (c *HTTPClient) GetUser(ctx context.Context) (*AccountInfo, error) {
	body, err := c.do(ctx, provider.OpQuery, "/v4/user", nil)
	if err != nil {
		return nil, err
	}
	return parseUser(body)
}

func (c *HTTPClient) UploadMagnet(ctx context.Context, magnet string) (*Upload, error) {
	form := url.Values{"magnets[]": {magnet}}
	body, err := c.do(ctx, provider.OpCreate, "/v4/magnet/upload", form)
	if err != nil {
		return nil, err
	}
	return parseUpload(body)
}

func (c *HTTPClient) GetMagnet(ctx context.Context, id string) (*Magnet, error) {
	body, err := c.do(ctx, provider.OpQuery, "/v4.1/magnet/status", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	magnets, err := parseMagnets(body)
	if err != nil {
		return nil, err
	}
	if len(magnets) != 1 || magnets[0].ID != id {
		return nil, fmt.Errorf("alldebrid: status response does not match requested magnet")
	}
	return &magnets[0], nil
}

func (c *HTTPClient) GetFiles(ctx context.Context, id string) ([]File, error) {
	body, err := c.do(ctx, provider.OpQuery, "/v4/magnet/files", url.Values{"id[]": {id}})
	if err != nil {
		return nil, err
	}
	return parseFiles(body, id)
}

func (c *HTTPClient) UnlockLink(ctx context.Context, link string) (string, error) {
	form := url.Values{"link": {link}}
	body, err := c.do(ctx, provider.OpQuery, "/v4/link/unlock", form)
	if err != nil {
		return "", err
	}
	direct, err := parseUnlock(body)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(direct)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("alldebrid: unlock returned invalid direct link")
	}
	return direct, nil
}

func (c *HTTPClient) DeleteMagnet(ctx context.Context, id string) error {
	_, err := c.do(ctx, provider.OpControl, "/v4/magnet/delete", url.Values{"id": {id}})
	return err
}

func (c *HTTPClient) ActiveCount(ctx context.Context) (int, error) {
	body, err := c.do(ctx, provider.OpQuery, "/v4.1/magnet/status", url.Values{"status": {"active"}})
	if err != nil {
		return 0, err
	}
	magnets, err := parseMagnets(body)
	if err != nil {
		return 0, err
	}
	active := 0
	for _, m := range magnets {
		if m.StatusCode >= 0 && m.StatusCode <= 3 {
			active++
		}
	}
	return active, nil
}

func (c *HTTPClient) do(ctx context.Context, op provider.Op, endpoint string, form url.Values) ([]byte, error) {
	if c.policy == nil {
		return nil, fmt.Errorf("alldebrid: missing request policy")
	}
	if err := c.policy.Acquire(ctx, op); err != nil {
		return nil, fmt.Errorf("alldebrid: rate limit: %w", err)
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("alldebrid: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", c.userAgent)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, provider.MarkRetryable(fmt.Errorf("alldebrid: request failed: %w", err))
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, provider.MarkRetryable(fmt.Errorf("alldebrid: read response: %w", err))
	}
	if len(payload) > maxResponseBytes {
		return nil, fmt.Errorf("alldebrid: response exceeds byte limit")
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if len(payload) == 0 {
			return nil, fmt.Errorf("alldebrid: empty response")
		}
		var env envelope
		if json.Unmarshal(payload, &env) == nil && env.Status == "error" {
			_, parseErr := dataFrom(payload)
			if isTransientAPIError(parseErr) {
				return nil, provider.MarkRetryable(parseErr)
			}
			return nil, parseErr
		}
		return payload, nil
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		c.policy.OnResponse(op, http.StatusTooManyRequests, retryAfter)
		return nil, provider.MarkRetryable(fmt.Errorf("alldebrid: temporarily rate limited"))
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("alldebrid: authentication rejected")
	default:
		if resp.StatusCode >= 500 {
			return nil, provider.MarkRetryable(fmt.Errorf("alldebrid: server unavailable"))
		}
		return nil, fmt.Errorf("alldebrid: request rejected with status %d", resp.StatusCode)
	}
}

func isTransientAPIError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case "MAINTENANCE", "MAGNET_TOO_MANY_ACTIVE", "MAGNET_UPLOAD_FAILED",
		"MAGNET_INTERNAL_ERROR", "LINK_HOST_UNAVAILABLE", "LINK_HOST_FULL",
		"LINK_TOO_MANY_DOWNLOADS", "LINK_TEMPORARY_UNAVAILABLE":
		return true
	default:
		return false
	}
}

func parseRetryAfter(raw string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 60 * time.Second
}
