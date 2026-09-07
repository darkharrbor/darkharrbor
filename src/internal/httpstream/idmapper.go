package httpstream

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
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

const (
	defaultTMDBBaseURL = "https://api.themoviedb.org"
	defaultNegativeTTL = 24 * time.Hour
	// titleCacheTTL bounds how long a resolved TMDB display title is reused.
	// Titles are effectively immutable, so this is purely a call-rate guard.
	titleCacheTTL        = 24 * time.Hour
	maxTMDBResponseBytes = 1 << 20
)

var (
	ErrMappingUnavailable = errors.New("identifier mapping unavailable")
	ErrMappingAmbiguous   = errors.New("identifier mapping ambiguous")
)

// IdentifierMapping is the normalized output required by OMSS (TMDB) and
// Stremio (IMDb). Empty fields mean the affected handler must be skipped.
type IdentifierMapping struct {
	TMDBID string
	IMDBID string
}

type IDMappingStore interface {
	GetHTTPIDMapping(ctx context.Context, namespace, sourceID string) (*store.HTTPIDMapping, error)
	PutHTTPIDMapping(ctx context.Context, m store.HTTPIDMapping) error
}

type IDMapper struct {
	client      *http.Client
	store       IDMappingStore
	apiKey      string
	baseURL     string
	negativeTTL time.Duration
	now         func() time.Time

	titleMu    sync.Mutex
	titleCache map[string]titleEntry
}

// titleEntry is one cached TMDB display title. A negative result (empty
// title) is cached too, so a repeatedly-unresolvable id cannot turn every
// search into an upstream call.
type titleEntry struct {
	title string
	year  int
	at    time.Time
}

func NewIDMapper(client *http.Client, st IDMappingStore, apiKey string) *IDMapper {
	return &IDMapper{
		client: client, store: st, apiKey: strings.TrimSpace(apiKey),
		baseURL: defaultTMDBBaseURL, negativeTTL: defaultNegativeTTL,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// MapTVDB maps a TVDB series ID to exactly one TMDB TV result, then asks TMDB
// for the corresponding IMDb external ID. Authoritative empty find responses
// are negative-cached for a finite TTL; transient failures are never cached.
func (m *IDMapper) MapTVDB(ctx context.Context, tvdbID string) (IdentifierMapping, error) {
	tvdbID = strings.TrimSpace(tvdbID)
	if tvdbID == "" {
		return IdentifierMapping{}, ErrMappingUnavailable
	}
	if m == nil || m.client == nil || m.store == nil || m.apiKey == "" {
		return IdentifierMapping{}, ErrMappingUnavailable
	}
	cached, err := m.store.GetHTTPIDMapping(ctx, "tvdb", tvdbID)
	if err != nil {
		return IdentifierMapping{}, err
	}
	if cached != nil {
		if cached.AuthoritativeEmpty {
			if cached.ExpiresAt != nil && m.now().Before(*cached.ExpiresAt) {
				return IdentifierMapping{}, ErrMappingUnavailable
			}
		} else if cached.TMDBID != "" {
			return IdentifierMapping{TMDBID: cached.TMDBID, IMDBID: cached.IMDBID}, nil
		}
	}

	results, err := m.findTVDB(ctx, tvdbID)
	if err != nil {
		return IdentifierMapping{}, err
	}
	if len(results) == 0 {
		expires := m.now().Add(m.negativeTTL)
		if err := m.store.PutHTTPIDMapping(ctx, store.HTTPIDMapping{
			SourceNamespace: "tvdb", SourceID: tvdbID, AuthoritativeEmpty: true,
			Provenance: "tmdb.find.tvdb_id", ExpiresAt: &expires,
		}); err != nil {
			return IdentifierMapping{}, err
		}
		return IdentifierMapping{}, ErrMappingUnavailable
	}
	if len(results) != 1 || results[0].ID <= 0 {
		return IdentifierMapping{}, ErrMappingAmbiguous
	}
	tmdbID := strconv.Itoa(results[0].ID)
	imdbID, err := m.externalIMDbID(ctx, tmdbID)
	if err != nil {
		return IdentifierMapping{}, err
	}
	mapping := store.HTTPIDMapping{
		SourceNamespace: "tvdb", SourceID: tvdbID, TMDBID: tmdbID,
		IMDBID: imdbID, Provenance: "tmdb.find.tvdb_id+tv.external_ids",
	}
	if err := m.store.PutHTTPIDMapping(ctx, mapping); err != nil {
		return IdentifierMapping{}, err
	}
	return IdentifierMapping{TMDBID: tmdbID, IMDBID: imdbID}, nil
}

type tmdbFindResponse struct {
	TVResults []struct {
		ID int `json:"id"`
	} `json:"tv_results"`
}

func (m *IDMapper) findTVDB(ctx context.Context, tvdbID string) ([]struct {
	ID int `json:"id"`
}, error) {
	var out tmdbFindResponse
	endpoint := strings.TrimRight(m.baseURL, "/") + "/3/find/" + url.PathEscape(tvdbID)
	q := url.Values{"external_source": {"tvdb_id"}, "api_key": {m.apiKey}}
	if err := m.getJSON(ctx, endpoint+"?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out.TVResults, nil
}

type tmdbExternalIDs struct {
	IMDbID string `json:"imdb_id"`
}

func (m *IDMapper) externalIMDbID(ctx context.Context, tmdbID string) (string, error) {
	var out tmdbExternalIDs
	endpoint := strings.TrimRight(m.baseURL, "/") + "/3/tv/" + url.PathEscape(tmdbID) + "/external_ids"
	q := url.Values{"api_key": {m.apiKey}}
	if err := m.getJSON(ctx, endpoint+"?"+q.Encode(), &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.IMDbID), nil
}

// tmdbTitleDoc covers both the movie and TV shapes of the TMDB detail
// endpoint; only one pair of fields is ever populated for a given request.
type tmdbTitleDoc struct {
	Title        string `json:"title"`          // movie
	ReleaseDate  string `json:"release_date"`   // movie
	Name         string `json:"name"`           // tv
	FirstAirDate string `json:"first_air_date"` // tv
}

// TitleFor resolves a human display title and year for a TMDB id. It exists
// because ID-native backends (OMSS is TMDB-keyed by specification) return no
// title text of their own, while Sonarr/Radarr reject a release whose name
// they cannot parse into a known series/movie.
//
// It is strictly best-effort: any failure, missing key, or unknown id yields
// ("", 0) and the caller falls back to its own stub. A title is presentation
// metadata and must never fail or block a search.
func (m *IDMapper) TitleFor(ctx context.Context, tmdbID string, episode bool) (string, int) {
	tmdbID = strings.TrimSpace(tmdbID)
	if m == nil || m.client == nil || m.apiKey == "" || tmdbID == "" {
		return "", 0
	}
	kind := "movie"
	if episode {
		kind = "tv"
	}
	ck := kind + ":" + tmdbID

	m.titleMu.Lock()
	if e, ok := m.titleCache[ck]; ok && m.now().Sub(e.at) < titleCacheTTL {
		m.titleMu.Unlock()
		return e.title, e.year
	}
	m.titleMu.Unlock()

	var doc tmdbTitleDoc
	endpoint := strings.TrimRight(m.baseURL, "/") + "/3/" + kind + "/" + url.PathEscape(tmdbID)
	q := url.Values{"api_key": {m.apiKey}}
	title, year := "", 0
	if err := m.getJSON(ctx, endpoint+"?"+q.Encode(), &doc); err == nil {
		date := doc.ReleaseDate
		title = strings.TrimSpace(doc.Title)
		if episode {
			title, date = strings.TrimSpace(doc.Name), doc.FirstAirDate
		}
		if len(date) >= 4 {
			if y, cerr := strconv.Atoi(date[:4]); cerr == nil {
				year = y
			}
		}
	}

	m.titleMu.Lock()
	if m.titleCache == nil {
		m.titleCache = make(map[string]titleEntry)
	}
	m.titleCache[ck] = titleEntry{title: title, year: year, at: m.now()}
	m.titleMu.Unlock()
	return title, year
}

func (m *IDMapper) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("tmdb request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		// A url.Error includes the full request URL; the TMDB API key is a
		// query parameter, so never wrap or expose the raw outbound error.
		return errors.New("tmdb request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTMDBResponseBytes+1))
	if err != nil {
		return fmt.Errorf("tmdb response read: %w", err)
	}
	if len(body) > maxTMDBResponseBytes {
		return errors.New("tmdb response exceeds body limit")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		retry := strings.TrimSpace(resp.Header.Get("Retry-After"))
		if retry == "" {
			retry = "unspecified"
		}
		return fmt.Errorf("tmdb rate limited retry_after=%s", retry)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
		return fmt.Errorf("tmdb transient/auth status %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("tmdb status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("tmdb JSON: %w", err)
	}
	return nil
}
