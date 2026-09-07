// Package omss implements the OMSS v1.1 protocol handler ("omss") for the
// HTTP stream provider (HTTP-STREAM-MASTER-PLAN.md H3, HS-3.5, D7/A22).
//
// Strict conformance to the OMSS v1.1 specification
// (github.com/omss-spec/omss-spec, spec/v1.1/omss-v1.1.md), not the earlier
// design draft's IMDb-passthrough claim:
//
//   - TMDB numeric IDs only (§2.3: "All media MUST have a TMDB ID"). No
//     IMDb-based lookup exists in the OMSS protocol itself; DH supplies a
//     TMDB ID from the arr query directly (Radarr) or via the shared HS-3.6
//     IDMapper (Sonarr tvdb -> tmdb). A query this handler cannot map is
//     skipped, never treated as a search failure (A23).
//   - `platform=native` is requested so sources carry the `headers` object
//     required to reach the real upstream URL (§6.2.1); DH is a byte-range
//     proxy, not a browser, so the web/CORS-safe `platform=web` shape is
//     never used.
//   - `streamable==true` progressive (`mp4`,`mkv`) and HLS sources are
//     accepted. DASH remains unsupported until it has a playback owner;
//     non-streamable sources are download links per §6.2, never playable.
//   - The `filter` query parameter (§5.4, a MUST-support backend
//     capability) is pushed down first; a `400 INVALID_PARAMETER` rejection
//     falls back to an unfiltered request with the identical client-side
//     filter applied afterward (G3).
//   - `/v1` health `status`, `spec`, and `media` fields are honored: a 200
//     with `status:"down"` is unhealthy (never authoritative), and an
//     explicit (non-wildcard) `media` list is used as a cheap authoritative
//     negative before ever calling the sources endpoint (GPT audit §2.5).
//   - Refresh (§4.5) is best-effort only, per the audit's resolution of the
//     spec's own internal ambiguity between the endpoint's prose ("refresh
//     a response by its id") and its own example (which posts a source id):
//     DH sends the top-level response id, ignores every refresh outcome
//     (400/404 included), and always performs a plain re-GET afterward —
//     refresh can never fail playback (GPT audit §2.3).
//   - Error classification follows §7.3: `NO_SOURCES_AVAILABLE` and the
//     other 404 codes mean "no result", never a backend outage; 429/5xx/
//     401/403 are transient and bounded-retried by the caller; malformed
//     JSON or a missing response id is a permanent per-attempt failure.
package omss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

const (
	// maxResponseBytes bounds any backend response body read.
	maxResponseBytes = 4 << 20
	// healthCacheTTL bounds how often the /v1 health/media map is re-fetched
	// per handler instance; it is a cheap-negative optimization, never a
	// correctness dependency (a stale/failed health fetch always falls
	// through to a direct sources request).
	healthCacheTTL = 5 * time.Minute
	// sourceFilter is the D7/§5.4 filter pushdown for streamable sources
	// with a wired owner. Pushed server-side first; falls back to client-side
	// filtering (identical predicate, see sourceKind) on a
	// 400 INVALID_PARAMETER rejection (G3).
	sourceFilter = "streamable==true;type=in=(mp4,mkv,hls)"
)

// errFilterRejected signals that the backend rejected the pushed-down
// filter expression (400 INVALID_PARAMETER) and the request must be retried
// without it.
var errFilterRejected = errors.New("omss: filter expression rejected")

// Handler is the omss protocol handler for one configured backend instance.
type Handler struct {
	backendID string
	baseURL   string
	client    *http.Client
	mapper    *httpstream.IDMapper // tvdb->tmdb mapping; nil-safe if unset

	mu       sync.Mutex
	health   *healthDoc
	healthAt time.Time
}

// New constructs the handler. client must be a bounded metadata-class
// client (short timeouts, TrustBackend dial policy); the live relay never
// uses it. mapper may be nil (episode queries without a direct TMDB id are
// then always skipped, matching "no TMDB key configured" per design).
func New(backendID, baseURL string, client *http.Client, mapper *httpstream.IDMapper) *Handler {
	return &Handler{
		backendID: strings.ToLower(strings.TrimSpace(backendID)),
		baseURL:   strings.TrimRight(baseURL, "/"),
		client:    client,
		mapper:    mapper,
	}
}

// Name implements httpstream.Handler.
func (h *Handler) Name() string { return "omss" }

// IDNative reports that this handler answers exact-ID queries directly —
// OMSS has no search endpoint; "search" is an exact TMDB-id fetch (design
// §Handler: OMSS). The torznab caps layer (internal/api/httpstream.go)
// advertises tvdbid/tmdbid/imdbid search params only once a handler like
// this one is registered.
func (h *Handler) IDNative() bool { return true }

// ── Wire types (OMSS v1.1 §6, §7) ────────────────────────────────────────────

type providerRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// sourceObject is the §6.2 Source Object under platform=native.
type sourceObject struct {
	ID      string            `json:"id"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	// Streamable is a pointer so an ABSENT field is distinguishable from an
	// explicit false. v1.1 requires the field; a backend that omits it is
	// non-conforming, and DH infers streamability from a supported `type`
	// rather than silently dropping every source (see sourceKind).
	// An explicit false is still authoritative and always rejected.
	Streamable  *bool          `json:"streamable"`
	Type        string         `json:"type"`
	Quality     string         `json:"quality"`
	AudioTracks audioTrackList `json:"audioTracks,omitempty"`
	Provider    providerRef    `json:"provider"`
}

// audioTrackList tolerates both encodings of §6.2 `audioTracks` seen in the
// wild: the v1.1 string array (["en","fr"]) and the object array some
// backends emit ([{"language":"en","label":"English"}]). Parsing is
// fixed and typed — DH never exposes a configurable field-mapping surface
// (master plan §6.2 rejects a generic JSON mapping DSL). A shape that is
// neither yields an empty list rather than failing the whole response:
// audio track labels are advisory metadata and never gate playability.
type audioTrackList []string

func (a *audioTrackList) UnmarshalJSON(b []byte) error {
	*a = nil
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var asStrings []string
	if json.Unmarshal(b, &asStrings) == nil {
		*a = asStrings
		return nil
	}
	var asObjects []struct {
		Language string `json:"language"`
		Label    string `json:"label"`
	}
	if json.Unmarshal(b, &asObjects) == nil {
		out := make([]string, 0, len(asObjects))
		for _, o := range asObjects {
			if v := strings.TrimSpace(o.Language); v != "" {
				out = append(out, v)
				continue
			}
			if v := strings.TrimSpace(o.Label); v != "" {
				out = append(out, v)
			}
		}
		*a = out
		return nil
	}
	return nil
}

type diagnosticObject struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Source   string `json:"source"`
	Severity string `json:"severity"`
}

// sourcesResponse is the §6.1 Success Response shared by the movie and TV
// episode endpoints.
type sourcesResponse struct {
	ID string `json:"id"`
	// ResponseID is the non-conforming spelling emitted by some backends.
	// v1.1 §6.1 names this field `id`; `id` always wins when both are set.
	ResponseID  string             `json:"responseId,omitempty"`
	ExpiresAt   string             `json:"expiresAt,omitempty"`
	Sources     []sourceObject     `json:"sources"`
	Diagnostics []diagnosticObject `json:"diagnostics,omitempty"`
}

// responseID returns the top-level response identifier, preferring the
// conforming `id` spelling and falling back to `responseId`. An empty result
// means the response carried neither and is malformed.
func (r *sourcesResponse) responseID() string {
	if v := strings.TrimSpace(r.ID); v != "" {
		return v
	}
	return strings.TrimSpace(r.ResponseID)
}

// errorBody is the §7.1 error envelope.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	TraceID string `json:"traceId,omitempty"`
}

// healthDoc is the subset of the §4.1 health response DH consumes: identity
// (spec/version/status) and the media availability map. Endpoints/providers
// are not modeled — DH only ever calls the two fixed endpoint shapes the
// spec itself mandates.
type healthDoc struct {
	Spec      string          `json:"spec"`
	Version   string          `json:"version"`
	Status    string          `json:"status"`
	Endpoints json.RawMessage `json:"endpoints"`
	Media     struct {
		Movies json.RawMessage `json:"movies"` // "*" (wildcard) or []int
		TV     json.RawMessage `json:"tv"`     // "*" (wildcard) or []healthTVSeries
	} `json:"media"`
}

type healthTVSeries struct {
	ID      int `json:"id"`
	Seasons []struct {
		Season   int   `json:"season"`
		Episodes []int `json:"episodes"`
	} `json:"seasons"`
}

// ── Search ───────────────────────────────────────────────────────────────────

// Search implements httpstream.Handler. OMSS has no search endpoint: an
// exact TMDB-id fetch IS the search (design). A query this backend cannot
// map to a TMDB id returns (nil, nil) — a skip, not a failure (A23).
func (h *Handler) Search(ctx context.Context, q httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	episode := strings.EqualFold(q.Kind, "episode") || strings.EqualFold(q.Kind, "tv")
	if episode && q.Episode < 1 {
		return nil, nil // D12: season-only never synthesized (double-guard; the route already filters this).
	}

	tmdbID, skip, err := h.resolveTMDBID(ctx, q, episode)
	if err != nil {
		return nil, err
	}
	if skip {
		return nil, nil
	}

	if h.mediaUnavailable(ctx, tmdbID, episode, q.Season, q.Episode) {
		return nil, nil
	}

	resp, err := h.fetchSources(ctx, tmdbID, episode, q.Season, q.Episode)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}

	kind := "movie"
	if episode {
		kind = "episode"
	}
	// OMSS is TMDB-keyed by specification and carries no title text of its
	// own, so an ID-native search would otherwise render an unparseable
	// TMDB.<id> stub that Sonarr/Radarr reject with "Unable to parse
	// release". Recover a real display name from the shared TMDB mapper
	// when the arr query itself supplied none. Best-effort: on any failure
	// releaseName falls back to the stub exactly as before.
	dq := q
	if strings.TrimSpace(dq.Title) == "" && h.mapper != nil {
		if t, y := h.mapper.TitleFor(ctx, tmdbID, episode); t != "" {
			dq.Title = t
			if dq.Year == 0 {
				dq.Year = y
			}
		}
	}

	var out []httpstream.SearchResult
	selectors := assignSelectors(resp)
	for i, src := range resp.Sources {
		if sourceKind(src) == httpstream.KindUnsupported {
			continue
		}
		key := httpstream.ResolveKey{
			Version:   httpstream.ResolveKeyVersion,
			BackendID: h.backendID,
			Handler:   "omss",
			Kind:      kind,
			IDs:       map[string]string{"tmdb": tmdbID},
			Selector:  selectors[i],
		}
		if episode {
			key.Season, key.Episode = q.Season, q.Episode
		}
		if err := key.Validate(); err != nil {
			continue // malformed source id or similar — drop, never guess (D10).
		}
		res, _ := httpstream.QualityToRes(src.Quality)
		out = append(out, httpstream.SearchResult{
			Title:   releaseName(dq, tmdbID, episode, res),
			Key:     key,
			Quality: res,
			Season:  key.Season,
			Episode: key.Episode,
		})
	}
	return out, nil
}

// resolveTMDBID returns the TMDB id to query. skip=true means this backend
// cannot answer the query at all (no TMDB id available) — the caller must
// treat that as "no result from this handler", not an error. A transient
// mapping-lookup failure is returned as a bounded-retry-class error instead,
// so the caller does not cache an authoritative empty result over it (A23).
func (h *Handler) resolveTMDBID(ctx context.Context, q httpstream.StreamQuery, episode bool) (tmdbID string, skip bool, err error) {
	if id := strings.TrimSpace(q.TMDBID); id != "" {
		return id, false, nil
	}
	if !episode {
		// Radarr supplies tmdbid directly (design); OMSS has no
		// imdb/other-namespace movie lookup path.
		return "", true, nil
	}
	tvdb := strings.TrimSpace(q.TVDBID)
	if tvdb == "" || h.mapper == nil {
		return "", true, nil
	}
	mapping, merr := h.mapper.MapTVDB(ctx, tvdb)
	if merr != nil {
		if errors.Is(merr, httpstream.ErrMappingUnavailable) || errors.Is(merr, httpstream.ErrMappingAmbiguous) {
			return "", true, nil
		}
		return "", false, httpstream.Wrapf(httpstream.ClassBackendUnavailable, "tmdb id mapping lookup failed")
	}
	if mapping.TMDBID == "" {
		return "", true, nil
	}
	return mapping.TMDBID, false, nil
}

// ── Health / media-map cheap negative (GPT audit §2.5) ───────────────────────

func (h *Handler) fetchHealth(ctx context.Context) (*healthDoc, error) {
	h.mu.Lock()
	if h.health != nil && time.Since(h.healthAt) < healthCacheTTL {
		cached := h.health
		h.mu.Unlock()
		return cached, nil
	}
	h.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/v1", nil)
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "omss health request build failed")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, httpstream.WrapError(httpstream.ClassBackendUnavailable, "omss backend unreachable", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "omss health response read failed")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, httpstream.NewRateLimitError(resp.Header.Get("Retry-After"))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, httpstream.Wrapf(httpstream.ClassBackendUnavailable, "omss health responded %d", resp.StatusCode)
	}
	if len(body) > maxResponseBytes {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "omss health response exceeds body limit")
	}
	var doc healthDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "omss health response not valid JSON")
	}
	if doc.Spec != "omss" || strings.TrimSpace(doc.Version) == "" || len(doc.Endpoints) == 0 {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "omss health missing spec identity")
	}
	if doc.Status == "down" {
		// A 200 with status=down is unhealthy (GPT audit §2.5) — never
		// treated as authoritative, never cached.
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "omss backend reports down")
	}
	if doc.Status != "operational" && doc.Status != "degraded" {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "omss health has invalid status")
	}

	h.mu.Lock()
	h.health = &doc
	h.healthAt = time.Now()
	h.mu.Unlock()
	return &doc, nil
}

// Healthy implements the shared backend lifecycle probe.
func (h *Handler) Healthy(ctx context.Context) error {
	_, err := h.fetchHealth(ctx)
	return err
}

// mediaUnavailable reports whether the health media map authoritatively
// excludes this title: an explicit (non-wildcard) list that does not
// contain it. Any failure, wildcard, or decode ambiguity means "unknown"
// (false) — this is a cheap-negative optimization only and must never
// produce a false negative that hides a real source.
func (h *Handler) mediaUnavailable(ctx context.Context, tmdbID string, episode bool, season, ep int) bool {
	doc, err := h.fetchHealth(ctx)
	if err != nil || doc == nil {
		return false
	}
	id, err := strconv.Atoi(tmdbID)
	if err != nil {
		return false
	}
	if !episode {
		return movieUnavailable(doc.Media.Movies, id)
	}
	return tvUnavailable(doc.Media.TV, id, season, ep)
}

func movieUnavailable(raw json.RawMessage, id int) bool {
	if len(raw) == 0 {
		return false
	}
	var wildcard string
	if json.Unmarshal(raw, &wildcard) == nil {
		return false // "*" (or any other string shape): not an explicit list.
	}
	var ids []int
	if json.Unmarshal(raw, &ids) != nil {
		return false // undecodable (e.g. per-id wildcards): unknown, not a negative.
	}
	for _, v := range ids {
		if v == id {
			return false
		}
	}
	return true // explicit list, id absent — authoritative negative.
}

func tvUnavailable(raw json.RawMessage, id, season, ep int) bool {
	if len(raw) == 0 {
		return false
	}
	var wildcard string
	if json.Unmarshal(raw, &wildcard) == nil {
		return false
	}
	var series []healthTVSeries
	if json.Unmarshal(raw, &series) != nil {
		return false // per-series/season "*" wildcards inside the tv array: unknown.
	}
	for _, s := range series {
		if s.ID != id {
			continue
		}
		for _, se := range s.Seasons {
			if se.Season != season {
				continue
			}
			for _, e := range se.Episodes {
				if e == ep {
					return false
				}
			}
		}
		return true // series listed, but this season/episode absent.
	}
	return true // explicit list, series absent.
}

// ── Sources fetch (movie + TV episode endpoints, §4.2/§4.3) ─────────────────

// fetchSources retrieves sources for one exact media identity. A nil,nil
// result means an authoritative "no sources" (§7.3/§7.4.1 classification);
// a non-nil error is bounded-retry transient (ClassBackendUnavailable) or a
// permanent per-attempt failure (ClassUpstreamMalformed).
func (h *Handler) fetchSources(ctx context.Context, tmdbID string, episode bool, season, ep int) (*sourcesResponse, error) {
	resp, err := h.doFetchSources(ctx, tmdbID, episode, season, ep, true)
	if errors.Is(err, errFilterRejected) {
		resp, err = h.doFetchSources(ctx, tmdbID, episode, season, ep, false)
	}
	return resp, err
}

func (h *Handler) sourceURL(tmdbID string, episode bool, season, ep int, withFilter bool) string {
	var path string
	if episode {
		path = fmt.Sprintf("/v1/tv/%s/seasons/%d/episodes/%d", url.PathEscape(tmdbID), season, ep)
	} else {
		path = "/v1/movies/" + url.PathEscape(tmdbID)
	}
	v := url.Values{}
	v.Set("platform", "native") // §6.2: native carries the headers object DH needs to relay.
	if withFilter {
		v.Set("filter", sourceFilter)
	}
	return h.baseURL + path + "?" + v.Encode()
}

func (h *Handler) doFetchSources(ctx context.Context, tmdbID string, episode bool, season, ep int, withFilter bool) (*sourcesResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.sourceURL(tmdbID, episode, season, ep, withFilter), nil)
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "omss request build failed")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "omss backend unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "omss response read failed")
	}
	if len(body) > maxResponseBytes {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "omss response exceeds body limit")
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		var parsed sourcesResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "omss response not valid JSON")
		}
		if parsed.responseID() == "" {
			return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "omss response missing id")
		}
		return &parsed, nil

	case resp.StatusCode == http.StatusBadRequest:
		if withFilter && errorCode(body) == "INVALID_PARAMETER" {
			// The filter pushdown itself was rejected (§5.4.6) — never the
			// query's own fault. Signal the caller to retry unfiltered (G3).
			return nil, errFilterRejected
		}
		// INVALID_TMDB_ID / INVALID_SEASON / INVALID_EPISODE / MISSING_PARAMETER:
		// DH's own request shape was rejected — authoritative no-result,
		// never a retry candidate (§7.3).
		return nil, nil

	case resp.StatusCode == http.StatusNotFound:
		// NO_SOURCES_AVAILABLE / INVALID_TMDB_ID / PROVIDER_NOT_FOUND /
		// ENDPOINT_NOT_FOUND: "no result", never an outage (D7/§7.4.1).
		return nil, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, httpstream.NewRateLimitError(resp.Header.Get("Retry-After"))

	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, httpstream.Wrapf(httpstream.ClassBackendUnavailable, "omss backend auth failure %d", resp.StatusCode)

	case resp.StatusCode >= 500:
		return nil, httpstream.Wrapf(httpstream.ClassBackendUnavailable, "omss backend responded %d", resp.StatusCode)

	default:
		return nil, httpstream.Wrapf(httpstream.ClassUpstreamMalformed, "omss backend responded %d", resp.StatusCode)
	}
}

func errorCode(body []byte) string {
	var e errorBody
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	return strings.TrimSpace(e.Error.Code)
}

func sourceKind(src sourceObject) httpstream.StreamKind {
	if src.Streamable != nil && !*src.Streamable {
		return httpstream.KindUnsupported
	}
	// An omitted streamable field is a tolerated backend near-miss, not a
	// trust upgrade: the owning progressive/HLS preflight still verifies the
	// source before it can become Ready.
	switch strings.ToLower(strings.TrimSpace(src.Type)) {
	case "mp4", "mkv":
		return httpstream.KindProgressiveHTTP
	case "hls":
		return httpstream.KindHLS
	default:
		return httpstream.KindUnsupported
	}
}

// selectorSanitize reduces a synthesized selector component to a
// key-safe token. ResolveKey.Selector is persisted durably, so the charset
// is deliberately narrow and stable.
func selectorSanitize(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// assignSelectors returns one selector per source, positionally aligned with
// resp.Sources. Search and Resolve MUST derive selectors identically or a
// grabbed key will never match at playback time, so both call this single
// helper over the full unfiltered slice.
//
// A conforming backend supplies `source.id` and it is used verbatim. When it
// is absent, a selector is synthesized from provider/type/quality — values
// that stay stable across re-resolve and refresh. The source URL is
// deliberately NOT used: it carries expiry/token state and would churn the
// persisted key on every refresh. Residual limitation: if a backend's
// provider set changes between grab and playback, a synthesized selector can
// shift and the resolve falls back to the caller's no-match handling.
func assignSelectors(resp *sourcesResponse) []string {
	out := make([]string, len(resp.Sources))
	used := make(map[string]int, len(resp.Sources))
	for i, src := range resp.Sources {
		sel := strings.TrimSpace(src.ID)
		if sel == "" {
			parts := []string{
				selectorSanitize(src.Provider.ID),
				selectorSanitize(src.Type),
				selectorSanitize(src.Quality),
			}
			trimmed := parts[:0]
			for _, p := range parts {
				if p != "" {
					trimmed = append(trimmed, p)
				}
			}
			sel = strings.Join(trimmed, ".")
			if sel == "" {
				sel = "src"
			}
			sel = "syn." + sel
			used[sel]++
			if n := used[sel]; n > 1 {
				sel = sel + "." + strconv.Itoa(n)
			}
		}
		out[i] = sel
	}
	return out
}

// ── Resolve ──────────────────────────────────────────────────────────────────

// Resolve implements httpstream.Handler: re-fetches sources for the grabbed
// media identity and returns every currently-available supported progressive
// or HLS representation so the caller can match the persisted Selector exactly
// (D10). OMSS has no per-request TTL cache in DH today (HS-3.8/HS-3.9 land
// the coalescing/expiry layer); every call is a live fetch, matching the ia
// handler's pattern.
func (h *Handler) Resolve(ctx context.Context, req httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	key := req.Key
	tmdbID := strings.TrimSpace(key.IDs["tmdb"])
	if tmdbID == "" {
		return nil, httpstream.NewError(httpstream.ClassInvalidKey, "omss key missing tmdb id")
	}
	episode := strings.EqualFold(key.Kind, "episode")
	if episode && (key.Season < 1 || key.Episode < 1) {
		return nil, httpstream.NewError(httpstream.ClassInvalidKey, "omss episode key missing season/episode")
	}

	if req.Operation == httpstream.ResolveForceRefresh {
		h.bestEffortRefresh(ctx, req.RefreshContext)
	}

	resp, err := h.fetchSources(ctx, tmdbID, episode, key.Season, key.Episode)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, httpstream.NewError(httpstream.ClassNoSource, "omss backend has no sources for this media")
	}

	var expiresAt time.Time
	if s := strings.TrimSpace(resp.ExpiresAt); s != "" {
		if t, perr := time.Parse(time.RFC3339, s); perr == nil {
			expiresAt = t
		}
		// An unparseable expiresAt is treated as absent (HS-3.9 owns the
		// full expiry-hygiene contract; this handler never guesses).
	}

	out := make([]httpstream.ResolvedFile, 0, len(resp.Sources))
	selectors := assignSelectors(resp)
	for i, src := range resp.Sources {
		if sourceKind(src) == httpstream.KindUnsupported {
			continue
		}
		if strings.TrimSpace(selectors[i]) == "" || strings.TrimSpace(src.URL) == "" {
			continue
		}
		out = append(out, httpstream.ResolvedFile{
			Selector:       selectors[i],
			Name:           fileName(tmdbID, episode, key.Season, key.Episode, src.Type),
			ContentType:    contentTypeForSourceType(src.Type),
			URL:            src.URL,
			RequestHeaders: src.Headers,
			ExpiresAt:      expiresAt,
			RefreshContext: resp.responseID(),
		})
	}
	if len(out) == 0 {
		return nil, httpstream.NewError(httpstream.ClassNoSource, "omss backend returned no supported streamable sources")
	}
	return out, nil
}

// bestEffortRefresh invalidates a cached response ahead of a forced
// re-resolve (§4.5). The spec's endpoint description ("refresh a response by
// its id") and its own example 11.6 (which posts a source id) disagree on
// what "id" means (GPT audit §2.3); this sends the top-level response id per
// the endpoint description. Every outcome is swallowed — a failed or
// rejected refresh never blocks the plain re-GET that always follows it in
// Resolve.
func (h *Handler) bestEffortRefresh(ctx context.Context, responseID string) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, h.baseURL+"/v1/refresh/"+url.PathEscape(responseID), nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	_ = resp.Body.Close()
}

// ── Naming (D17 — OMSS supplies neither filename nor size) ───────────────────

func fileName(tmdbID string, episode bool, season, ep int, srcType string) string {
	ext := ".mp4"
	switch strings.ToLower(strings.TrimSpace(srcType)) {
	case "mkv":
		ext = ".mkv"
	case "hls":
		ext = ".m3u8"
	}
	if episode {
		return fmt.Sprintf("omss.tmdb%s.s%02de%02d%s", tmdbID, season, ep, ext)
	}
	return fmt.Sprintf("omss.tmdb%s%s", tmdbID, ext)
}

func contentTypeForSourceType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "mkv":
		return "video/x-matroska"
	case "hls":
		return "application/vnd.apple.mpegurl"
	default:
		return "video/mp4"
	}
}

// releaseName builds a path-safe, arr-parseable Torznab title. OMSS gives no
// title; an ID-native automatic search from the arr often carries no text
// query either (torznab.go caps advertise ID params once this handler is
// registered, and arrs send ID-only queries against ID caps). The arr has
// already matched this release to the correct item by TMDB/TVDB id before
// this text is parsed, so a TMDB-id-derived stub is a safe fallback — never
// an identity guess (D17 still forbids guessing quality/resolution, which
// this never does: QualityToRes's ok=false already skips `res` entirely).
func releaseName(q httpstream.StreamQuery, tmdbID string, episode bool, res string) string {
	base := pathSafeTitle(strings.TrimSpace(q.Title))
	if base == "" {
		base = "TMDB." + tmdbID
	}
	switch {
	case episode:
		base += fmt.Sprintf(".S%02dE%02d", q.Season, q.Episode)
	case q.Year > 0:
		base += fmt.Sprintf(".%d", q.Year)
	}
	if res != "" {
		base += "." + res
	}
	return base + ".WEBDL"
}

var nonPathSafe = regexp.MustCompile(`[^A-Za-z0-9]+`)

func pathSafeTitle(t string) string {
	return strings.Trim(nonPathSafe.ReplaceAllString(t, "."), ".")
}

var _ httpstream.Handler = (*Handler)(nil)
