// Package prowlarr is a minimal client for Prowlarr's search API, used by Dark
// Harrbor's cache-aware selection mode (the meta-indexer). DH fans an arr search
// to the user's Prowlarr indexers, then cache-prefilters the candidates.
package prowlarr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

// Release is the subset of Prowlarr's ReleaseResource that DH needs.
type Release struct {
	Title       string `json:"title"`
	GUID        string `json:"guid"`
	InfoHash    string `json:"infoHash"`
	DownloadURL string `json:"downloadUrl"`
	MagnetURL   string `json:"magnetUrl"`
	IndexerID   int    `json:"indexerId"`
	Indexer     string `json:"indexer"`
	Protocol    string `json:"protocol"` // "torrent" | "usenet"
	Size        int64  `json:"size"`
	Seeders     *int   `json:"seeders"`
	Categories  []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"categories"`
	PublishDate string `json:"publishDate"`
}

// IsUsenet reports whether the release is a usenet (NZB) release.
func (r Release) IsUsenet() bool { return strings.EqualFold(r.Protocol, "usenet") }

// SearchParams carries one search request to Prowlarr.
type SearchParams struct {
	Query      string
	Type       string // "search" | "tvsearch" | "movie"
	Categories []int
	IndexerIDs []int
	Limit      int
	// TV-specific filters forwarded to indexers via Prowlarr.
	Season int    // 0 = not specified
	Ep     int    // 0 = not specified (season pack)
	TvdbID string // optional; passed as tvdbid when non-empty
	ImdbID string // optional; passed as imdbid when non-empty
}

// Client talks to one Prowlarr instance.
type Client struct {
	baseURL   string
	apiKey    string
	userAgent string
	http      *http.Client
	log       *slog.Logger

	mu          sync.Mutex
	idxCache    []indexer
	idxCachedAt time.Time
}

type indexer struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Enable   bool   `json:"enable"`
	Protocol string `json:"protocol"`
	Fields   []struct {
		Name  string      `json:"name"`
		Value interface{} `json:"value"`
	} `json:"fields"`
}

// New constructs a Prowlarr client. timeout bounds each HTTP call.
func New(baseURL, apiKey, userAgent string, timeout time.Duration, log *slog.Logger) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:    strings.TrimSpace(apiKey),
		userAgent: userAgent,
		http:      &http.Client{Timeout: timeout},
		log:       log,
	}
}

func (c *Client) do(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// Search fans a query to Prowlarr's aggregated search API and returns releases.
// A non-2xx is surfaced as an error so the caller can degrade (empty feed).
func (c *Client) Search(ctx context.Context, p SearchParams) ([]Release, error) {
	q := url.Values{}
	q.Set("query", p.Query)
	if p.Type != "" {
		q.Set("type", p.Type)
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	for _, id := range p.IndexerIDs {
		q.Add("indexerIds", strconv.Itoa(id))
	}
	for _, cat := range p.Categories {
		q.Add("categories", strconv.Itoa(cat))
	}
	if p.Season > 0 {
		q.Set("season", strconv.Itoa(p.Season))
	}
	if p.Ep > 0 {
		q.Set("ep", strconv.Itoa(p.Ep))
	}
	if p.TvdbID != "" {
		q.Set("tvdbid", p.TvdbID)
	}
	if p.ImdbID != "" {
		q.Set("imdbid", p.ImdbID)
	}
	body, status, err := c.do(ctx, "/api/v1/search?"+q.Encode())
	if err != nil {
		return nil, outcome.Transient(fmt.Errorf("prowlarr search: %w", err))
	}
	if status < 200 || status >= 300 {
		// Prowlarr returns 400 with {"message":...} when all indexers are down.
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Message != "" {
			return nil, classifiedHTTPError(status, fmt.Errorf("prowlarr search status %d: %s", status, e.Message))
		}
		return nil, classifiedHTTPError(status, fmt.Errorf("prowlarr search status %d", status))
	}
	var releases []Release
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, outcome.Transient(fmt.Errorf("prowlarr decode: %w", err))
	}
	return releases, nil
}

// FanoutIndexerIDs resolves which indexer IDs to query. When allowlist is
// non-empty it is returned verbatim (the user is responsible for excluding DH).
// Otherwise all ENABLED indexers are returned EXCEPT DH's own — identified by a
// baseUrl field whose host matches selfBaseURL — to prevent recursion
// (DH -> Prowlarr -> DH). The indexer list is cached for five minutes.
func (c *Client) FanoutIndexerIDs(ctx context.Context, selfBaseURL string, allowlist []int) ([]int, error) {
	if len(allowlist) > 0 {
		return allowlist, nil
	}
	idxs, err := c.indexers(ctx)
	if err != nil {
		return nil, outcome.Transient(err)
	}
	selfHost := hostOf(selfBaseURL)
	out := make([]int, 0, len(idxs))
	for _, ix := range idxs {
		if !ix.Enable {
			continue
		}
		if selfHost != "" && ix.matchesHost(selfHost) {
			continue // loop-safety: skip DH's own indexer
		}
		out = append(out, ix.ID)
	}
	return out, nil
}

func classifiedHTTPError(status int, err error) error {
	if status == http.StatusTooManyRequests {
		return outcome.AccountLevel(err)
	}
	return outcome.Transient(err)
}

func (c *Client) indexers(ctx context.Context) ([]indexer, error) {
	c.mu.Lock()
	if c.idxCache != nil && time.Since(c.idxCachedAt) < 5*time.Minute {
		cached := c.idxCache
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	body, status, err := c.do(ctx, "/api/v1/indexer")
	if err != nil {
		return nil, fmt.Errorf("prowlarr indexers: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("prowlarr indexers status %d", status)
	}
	var idxs []indexer
	if err := json.Unmarshal(body, &idxs); err != nil {
		return nil, fmt.Errorf("prowlarr indexers decode: %w", err)
	}
	c.mu.Lock()
	c.idxCache = idxs
	c.idxCachedAt = time.Now()
	c.mu.Unlock()
	return idxs, nil
}

func (ix indexer) matchesHost(host string) bool {
	for _, f := range ix.Fields {
		if !strings.EqualFold(f.Name, "baseUrl") {
			continue
		}
		if sv, ok := f.Value.(string); ok && hostOf(sv) == host {
			return true
		}
	}
	return false
}

func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(raw)
	}
	return strings.ToLower(u.Host)
}
