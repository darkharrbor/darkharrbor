package premiumize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

const (
	defaultBaseURL        = "https://www.premiumize.me/api"
	defaultRequestTimeout = 30 * time.Second
	defaultUserAgent      = "DarkHarrbor/1.0"
	maxResponseBytes      = 8 << 20
	defaultRateHold       = 60 * time.Second
)

type HTTPClient struct {
	baseURL   string
	apiKey    string
	http      *http.Client
	policy    provider.Policy
	userAgent string
	now       func() time.Time
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
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		policy: policy,
		now:    time.Now,
	}
}

var _ Client = (*HTTPClient)(nil)

func (c *HTTPClient) GetAccount(ctx context.Context) (*AccountInfo, error) {
	body, err := c.do(ctx, provider.OpQuery, http.MethodGet, "/account/info", nil)
	if err != nil {
		return nil, err
	}
	return parseAccount(body)
}

func (c *HTTPClient) CheckCache(ctx context.Context, source string) (bool, error) {
	body, err := c.do(ctx, provider.OpQuery, http.MethodPost, "/cache/check", url.Values{"items[]": {source}})
	if err != nil {
		return false, err
	}
	return parseCache(body)
}

func (c *HTTPClient) CreateTransfer(ctx context.Context, source string) (*Transfer, error) {
	body, err := c.do(ctx, provider.OpCreate, http.MethodPost, "/transfer/create", url.Values{"src": {source}})
	if err != nil {
		return nil, err
	}
	return parseCreate(body)
}

func (c *HTTPClient) listTransfers(ctx context.Context) ([]Transfer, error) {
	body, err := c.do(ctx, provider.OpQuery, http.MethodGet, "/transfer/list", nil)
	if err != nil {
		return nil, err
	}
	return parseTransfers(body)
}

func (c *HTTPClient) GetTransfer(ctx context.Context, id string) (*Transfer, error) {
	if !validID(id) {
		return nil, fmt.Errorf("premiumize: invalid transfer id")
	}
	transfers, err := c.listTransfers(ctx)
	if err != nil {
		return nil, err
	}
	var found *Transfer
	for i := range transfers {
		if transfers[i].ID == id {
			if found != nil {
				return nil, fmt.Errorf("premiumize: transfer id is ambiguous")
			}
			found = &transfers[i]
		}
	}
	if found == nil {
		return nil, ErrTransferNotFound
	}
	return found, nil
}

func (c *HTTPClient) GetFile(ctx context.Context, id string) (*File, error) {
	if !validID(id) {
		return nil, fmt.Errorf("premiumize: invalid file id")
	}
	body, err := c.do(ctx, provider.OpQuery, http.MethodGet, "/item/details", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	return parseItem(body, id)
}

func (c *HTTPClient) listFolder(ctx context.Context, id string) (*folderPage, error) {
	if !validID(id) {
		return nil, fmt.Errorf("premiumize: invalid folder id")
	}
	body, err := c.do(ctx, provider.OpQuery, http.MethodGet, "/folder/list", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	return parseFolder(body, id)
}

func (c *HTTPClient) FilesForTransfer(ctx context.Context, transfer *Transfer) ([]File, error) {
	if transfer == nil || !validID(transfer.ID) {
		return nil, fmt.Errorf("premiumize: invalid transfer")
	}
	if transfer.FileID != "" {
		file, err := c.GetFile(ctx, transfer.FileID)
		if err != nil {
			return nil, err
		}
		return []File{*file}, nil
	}
	if transfer.FolderID == "" {
		return nil, fmt.Errorf("premiumize: transfer has no file or folder")
	}

	type folderWork struct {
		id     string
		prefix string
		depth  int
	}
	queue := []folderWork{{id: transfer.FolderID}}
	seenFolders := map[string]struct{}{}
	seenFiles := map[string]struct{}{}
	files := make([]File, 0)
	for len(queue) > 0 {
		work := queue[0]
		queue = queue[1:]
		if work.depth > maxFolderDepth || len(seenFolders) >= maxFiles {
			return nil, fmt.Errorf("premiumize: folder tree exceeds bounds")
		}
		if _, exists := seenFolders[work.id]; exists {
			return nil, fmt.Errorf("premiumize: folder tree contains a cycle")
		}
		seenFolders[work.id] = struct{}{}

		page, err := c.listFolder(ctx, work.id)
		if err != nil {
			return nil, err
		}
		for _, entry := range page.Entries {
			relative := path.Join(work.prefix, entry.Name)
			if entry.Type == "folder" {
				queue = append(queue, folderWork{id: entry.ID, prefix: relative, depth: work.depth + 1})
				continue
			}
			if len(files) >= maxFiles {
				return nil, fmt.Errorf("premiumize: folder tree exceeds file limit")
			}
			if _, exists := seenFiles[entry.ID]; exists {
				return nil, fmt.Errorf("premiumize: duplicate file identity")
			}
			seenFiles[entry.ID] = struct{}{}
			files = append(files, File{
				ID: entry.ID, Name: entry.Name, RelativePath: relative, Size: entry.Size, link: entry.link,
			})
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("premiumize: transfer has no files")
	}
	return files, nil
}

func (c *HTTPClient) DeleteTransfer(ctx context.Context, id string) error {
	return c.delete(ctx, "/transfer/delete", id)
}

func (c *HTTPClient) DeleteItem(ctx context.Context, id string) error {
	return c.delete(ctx, "/item/delete", id)
}

func (c *HTTPClient) DeleteFolder(ctx context.Context, id string) error {
	return c.delete(ctx, "/folder/delete", id)
}

func (c *HTTPClient) delete(ctx context.Context, endpoint, id string) error {
	if !validID(id) {
		return fmt.Errorf("premiumize: invalid delete id")
	}
	body, err := c.do(ctx, provider.OpControl, http.MethodPost, endpoint, url.Values{"id": {id}})
	if isAPIErrorCode(err, "not_found") {
		return nil
	}
	if err != nil {
		return err
	}
	return parseDelete(body)
}

func (c *HTTPClient) do(ctx context.Context, op provider.Op, method, endpoint string, values url.Values) ([]byte, error) {
	if c.policy == nil {
		return nil, fmt.Errorf("premiumize: missing request policy")
	}
	if err := c.policy.Acquire(ctx, op); err != nil {
		return nil, fmt.Errorf("premiumize: rate limit: %w", err)
	}

	requestURL := c.baseURL + endpoint
	var body io.Reader
	if method == http.MethodGet && values != nil {
		requestURL += "?" + values.Encode()
	} else if values != nil {
		body = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, fmt.Errorf("premiumize: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, provider.MarkRetryable(fmt.Errorf("premiumize: transport failure"))
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, provider.MarkRetryable(fmt.Errorf("premiumize: response read failure"))
	}
	if len(payload) > maxResponseBytes {
		return nil, fmt.Errorf("premiumize: response exceeds byte limit")
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if len(payload) == 0 {
			return nil, fmt.Errorf("premiumize: empty response")
		}
		if apiErr := checkEnvelope(payload); apiErr != nil {
			if isAPIErrorCode(apiErr, "rate_limit_reached") {
				c.policy.OnResponse(op, http.StatusTooManyRequests, defaultRateHold)
			}
			if isTransientAPIError(apiErr) {
				return nil, provider.MarkRetryable(apiErr)
			}
			return nil, apiErr
		}
		return payload, nil
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		delay := parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
		c.policy.OnResponse(op, http.StatusTooManyRequests, delay)
		return nil, provider.MarkRetryable(fmt.Errorf("premiumize: temporarily rate limited"))
	case http.StatusInternalServerError:
		return nil, provider.MarkRetryable(fmt.Errorf("premiumize: server unavailable"))
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("premiumize: authentication rejected")
	default:
		if resp.StatusCode >= 500 {
			return nil, provider.MarkRetryable(fmt.Errorf("premiumize: server unavailable"))
		}
		return nil, fmt.Errorf("premiumize: request rejected with status %d", resp.StatusCode)
	}
}

func isTransientAPIError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case "link_generation_failed", "transient_error", "service_down", "service_limit_reached",
		"account_limit_reached", "rate_limit_reached", "semi_permanent_error", "unknown_error":
		return true
	default:
		return false
	}
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if delay := when.Sub(now); delay > 0 {
			return delay
		}
	}
	return defaultRateHold
}
