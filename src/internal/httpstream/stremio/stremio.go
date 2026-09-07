// Package stremio implements the Stremio addon protocol handler ("stremio")
// for the HTTP stream provider (HTTP-STREAM-MASTER-PLAN.md H3, HS-3.7, D8/A24).
//
// Detection (manifest identity/version + a compatible stream resource) is
// already owned by internal/httpstream/detector.go (HS-3.4); this package
// only implements the actual Search/Resolve client for a backend already
// classified as "stremio".
//
//   - IMDb-id based, per the open Stremio addon protocol
//     (github.com/Stremio/stremio-addon-sdk/blob/master/docs/protocol.md):
//     GET {base}/stream/movie/{imdb}.json and
//     GET {base}/stream/series/{imdb}:{season}:{episode}.json.
//   - Radarr supplies an imdbid directly when configured for it; Sonarr
//     supplies only tvdbid, so episode queries route through the shared
//     HS-3.6 IDMapper (tvdb -> tmdb -> external imdb id). A query neither
//     path can answer is a silent per-handler skip (A23), never a search
//     failure — movies with no imdbid available today have no synthesized
//     TMDB->IMDb path either, by the same "never guess" rule.
//   - Absolute HTTP(S) `url` variants remain supported; `infoHash` variants
//     are dispatched into DH's existing torrent lane (HR2.4). `externalUrl`
//     and `ytId` remain skipped, and obviously HLS-shaped URLs are skipped in v1 (H6
//     boundary — DASH/HLS playback is out of scope, and the D9 grab-time
//     preflight enforces this again centrally regardless).
//   - `behaviorHints.notWebReady` is NOT treated as a rejection signal
//     (A24): it commonly just accompanies `proxyHeaders` on sources that
//     need special request headers to be fetched at all, which is exactly
//     what DH's byte-range relay does. `behaviorHints.proxyHeaders.request`
//     and `.response` are carried on the transient ResolvedFile only
//     (D11) — never persisted, never logged.
//   - Stremio stream objects carry no protocol-guaranteed stable ID, so the
//     D10 URL-free resolve-key Selector is built from the same-object,
//     non-URL metadata Stremio does define for exactly this purpose:
//     `behaviorHints.filename` first, falling back to `name`+`title` text,
//     disambiguated by `bingeGroup` and `videoSize` when present. A stream
//     with no such metadata at all yields no stable selector and is
//     dropped — no silent ordinal/positional pick (D10, matches IA/OMSS's
//     "no result beats an identity guess" posture).
//   - No refresh endpoint exists in the Stremio protocol: a forced refresh
//     (HS-3.8) is simply a plain re-fetch of the stream list, identical to
//     a normal resolve.
package stremio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

// maxResponseBytes bounds any backend response body read.
const maxResponseBytes = 4 << 20

// Handler is the stremio protocol handler for one configured backend instance.
type Handler struct {
	backendID string
	baseURL   string
	client    *http.Client
	mapper    *httpstream.IDMapper // tvdb->tmdb->imdb mapping; nil-safe if unset
	now       func() time.Time     // injectable clock (standing convention)

	searchCacheMu sync.Mutex
	searchCache   map[string]searchCacheEntry
}

// searchResultCacheTTL bounds the polite-revalidation cache for backend
// search results (HR2.1 master-row requirement): a burst of near-simultaneous
// Sonarr/Radarr searches for the same title/episode across multiple Arr
// instances shares one backend fetch instead of multiplying load on a small
// self-hosted backend. Deliberately short — this is call-rate protection for
// a burst window, not a staleness tolerance for grab/playback (Resolve never
// consults this cache).
const searchResultCacheTTL = 15 * time.Second

// searchCacheEntry is one cached fetchStreams result, keyed by exact media
// identity (imdbID + season/episode). A nil streams slice (authoritative
// "no streams") is cached too — the point is skipping repeat backend calls
// for any settled outcome, not only successful non-empty ones.
type searchCacheEntry struct {
	streams []streamObject
	at      time.Time
}

// New constructs the handler. client must be a bounded metadata-class
// client (short timeouts, TrustBackend dial policy); the live relay never
// uses it — Resolve only ever returns transient URLs/headers for the
// caller's own TrustSource-policed relay to fetch. mapper may be nil
// (episode queries without a direct imdbid are then always skipped).
func New(backendID, baseURL string, client *http.Client, mapper *httpstream.IDMapper) *Handler {
	return &Handler{
		backendID: strings.ToLower(strings.TrimSpace(backendID)),
		baseURL:   strings.TrimRight(baseURL, "/"),
		client:    client,
		mapper:    mapper,
		now:       func() time.Time { return time.Now() },
	}
}

// Name implements httpstream.Handler.
func (h *Handler) Name() string { return "stremio" }

// Healthy revalidates the manifest identity/capability surface used by D8.
func (h *Handler) Healthy(ctx context.Context) error {
	typeName, err := httpstream.DetectBackend(ctx, h.client, h.baseURL)
	if err != nil {
		if errors.Is(err, httpstream.ErrDetectionSchemaMismatch) {
			return httpstream.NewError(httpstream.ClassUpstreamMalformed, "stremio manifest identity changed")
		}
		return httpstream.WrapError(httpstream.ClassBackendUnavailable, "stremio manifest health failed", err)
	}
	if typeName != "stremio" {
		return httpstream.NewError(httpstream.ClassUpstreamMalformed, "stremio manifest identity changed")
	}
	return nil
}

// IDNative reports that this handler answers exact-ID queries directly —
// Stremio has no free-text search; "search" is an exact IMDb-id stream
// fetch (design §Stremio Addon Client, same posture as omss.Handler).
func (h *Handler) IDNative() bool { return true }

// ── Wire types (Stremio addon stream response) ───────────────────────────────

type proxyHeaders struct {
	Request  map[string]string `json:"request,omitempty"`
	Response map[string]string `json:"response,omitempty"`
}

type behaviorHints struct {
	NotWebReady  bool            `json:"notWebReady,omitempty"`
	Filename     string          `json:"filename,omitempty"`
	VideoSize    json.RawMessage `json:"videoSize,omitempty"` // number or numeric string in the wild
	BingeGroup   string          `json:"bingeGroup,omitempty"`
	ProxyHeaders proxyHeaders    `json:"proxyHeaders,omitempty"`
}

// streamObject is the subset of the Stremio stream object DH understands.
// HR2.1 classifies every documented variant named by HR-D2 into a
// httpstream.StreamKind (see classifyStream below) instead of accepting only
// `url` and folding every other variant into one undifferentiated rejection
// (the pre-HR2.1 v1 posture, preserved exactly for progressive HTTP via
// acceptableStream). infoHash/nzbUrl/archive-URL fields are recognized so a
// dependent row (HR2.4 for infoHash, a future NNTP-lane consumer for nzbUrl,
// HR2.6 for archive URLs) can dispatch to its lane without adding a second
// classifier — externalUrl/ytId remain never-DH-fetchable and always
// classify unsupported.
type streamObject struct {
	URL           string        `json:"url,omitempty"`
	InfoHash      string        `json:"infoHash,omitempty"`
	ExternalURL   string        `json:"externalUrl,omitempty"`
	YtID          string        `json:"ytId,omitempty"`
	NZBURL        string        `json:"nzbUrl,omitempty"`
	RarURLs       []string      `json:"rarUrls,omitempty"`
	ZipURLs       []string      `json:"zipUrls,omitempty"`
	SevenZipURLs  []string      `json:"7zipUrls,omitempty"`
	TgzURLs       []string      `json:"tgzUrls,omitempty"`
	TarURLs       []string      `json:"tarUrls,omitempty"`
	Subtitles     []subtitleRef `json:"subtitles,omitempty"`
	Name          string        `json:"name,omitempty"`
	Title         string        `json:"title,omitempty"`
	Description   string        `json:"description,omitempty"` // some addons use this instead of title
	BehaviorHints behaviorHints `json:"behaviorHints,omitempty"`
}

// subtitleRef is a Stremio subtitle hint some addons attach directly to a
// stream object (HR-D2 names `subtitles` as one of the recognized fixture
// kinds). It is parsed so classification/fixtures cover its presence, but
// delivery remains HR5.5's shared sidecar layer — this row fetches,
// proxies, and persists nothing from it.
type subtitleRef struct {
	URL  string `json:"url,omitempty"`
	Lang string `json:"lang,omitempty"`
	ID   string `json:"id,omitempty"`
}

type streamResponse struct {
	Streams []streamObject `json:"streams"`
}

// ── Search ───────────────────────────────────────────────────────────────────

// Search implements httpstream.Handler. Stremio has no search endpoint: an
// exact IMDb-id stream fetch IS the search. A query this backend cannot map
// to an imdb id returns (nil, nil) — a skip, not a failure (A23).
func (h *Handler) Search(ctx context.Context, q httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	episode := strings.EqualFold(q.Kind, "episode") || strings.EqualFold(q.Kind, "tv")
	if episode && q.Episode < 1 {
		return nil, nil // D12: season-only never synthesized (double-guard; the route already filters this).
	}

	imdbID, skip, err := h.resolveIMDBID(ctx, q, episode)
	if err != nil {
		return nil, err
	}
	if skip {
		return nil, nil
	}

	streams, err := h.cachedFetchStreams(ctx, imdbID, episode, q.Season, q.Episode)
	if err != nil {
		return nil, err
	}
	if len(streams) == 0 {
		return nil, nil
	}

	kind := "movie"
	if episode {
		kind = "episode"
	}
	var out []httpstream.SearchResult
	for _, s := range streams {
		streamKind := classifyStream(s)
		if streamKind == httpstream.KindInfoHash {
			hash := strings.TrimSpace(s.InfoHash)
			if hash == "" {
				continue
			}
			out = append(out, httpstream.SearchResult{
				Title:    releaseName(q, imdbID, episode, "", streamNameHint(s)),
				Size:     videoSizeOf(s),
				Season:   q.Season,
				Episode:  q.Episode,
				Protocol: "torrent",
				InfoHash: hash,
			})
			continue
		}
		if streamKind == httpstream.KindArchive {
			descriptor, ok := remoteArchiveDescriptor(s)
			if !ok {
				continue
			}
			key := httpstream.ResolveKey{
				Version:   httpstream.ResolveKeyVersion,
				BackendID: h.backendID,
				Handler:   "stremio",
				Kind:      kind,
				IDs:       map[string]string{"imdb": imdbID},
				Selector:  descriptor.Selector,
			}
			if episode {
				key.Season, key.Episode = q.Season, q.Episode
			}
			if err := key.Validate(); err != nil {
				continue
			}
			res, _ := httpstream.QualityToRes(qualityFromStream(s))
			out = append(out, httpstream.SearchResult{
				Title: releaseName(q, imdbID, episode, res, streamNameHint(s)), Key: key,
				Quality: res, Size: videoSizeOf(s), Season: key.Season, Episode: key.Episode,
			})
			continue
		}
		// HLS -> HR4.x and nzbUrl -> its NNTP-lane consumer; retain their
		// existing fail-closed abstention until their owners exist.
		if streamKind != httpstream.KindProgressiveHTTP {
			continue
		}
		sel := streamSelector(s)
		if sel == "" {
			continue // no stable non-URL identity on this stream — drop, never guess (D10).
		}
		key := httpstream.ResolveKey{
			Version:   httpstream.ResolveKeyVersion,
			BackendID: h.backendID,
			Handler:   "stremio",
			Kind:      kind,
			IDs:       map[string]string{"imdb": imdbID},
			Selector:  sel,
		}
		if episode {
			key.Season, key.Episode = q.Season, q.Episode
		}
		if err := key.Validate(); err != nil {
			continue
		}
		res, _ := httpstream.QualityToRes(qualityFromStream(s))
		out = append(out, httpstream.SearchResult{
			Title:    releaseName(q, imdbID, episode, res, streamNameHint(s)),
			Filename: strings.TrimSpace(s.BehaviorHints.Filename),
			Key:      key,
			Quality:  res,
			Size:     videoSizeOf(s),
			Season:   key.Season,
			Episode:  key.Episode,
		})
	}
	return out, nil
}

// resolveIMDBID returns the IMDb id to query. skip=true means this backend
// cannot answer the query at all — the caller must treat that as "no result
// from this handler", not an error. A transient mapping-lookup failure is
// returned as a bounded-retry-class error instead, so the caller does not
// cache an authoritative empty result over it (A23).
func (h *Handler) resolveIMDBID(ctx context.Context, q httpstream.StreamQuery, episode bool) (imdbID string, skip bool, err error) {
	if id := normalizeIMDB(q.IMDBID); id != "" {
		return id, false, nil
	}
	if !episode {
		// No direct imdbid from Radarr and no TMDB->IMDb reverse mapping is
		// in scope: never guess a movie's imdb id (D10/A23 "never universal
		// passthrough").
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
		return "", false, httpstream.Wrapf(httpstream.ClassBackendUnavailable, "imdb id mapping lookup failed")
	}
	id := normalizeIMDB(mapping.IMDBID)
	if id == "" {
		return "", true, nil // TMDB mapped but no external imdb id — explicit skip (A23).
	}
	return id, false, nil
}

func normalizeIMDB(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// ── Stream fetch ─────────────────────────────────────────────────────────────

func (h *Handler) streamURL(imdbID string, episode bool, season, ep int) string {
	if episode {
		return fmt.Sprintf("%s/stream/series/%s:%d:%d.json", h.baseURL, url.PathEscape(imdbID), season, ep)
	}
	return fmt.Sprintf("%s/stream/movie/%s.json", h.baseURL, url.PathEscape(imdbID))
}

// fetchStreams retrieves the stream list for one exact media identity. A
// nil,nil result means an authoritative "no streams"; a non-nil error is
// bounded-retry transient (ClassBackendUnavailable) or a permanent
// per-attempt failure (ClassUpstreamMalformed).
func (h *Handler) fetchStreams(ctx context.Context, imdbID string, episode bool, season, ep int) ([]streamObject, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.streamURL(imdbID, episode, season, ep), nil)
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "stremio request build failed")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, httpstream.WrapError(httpstream.ClassBackendUnavailable, "stremio backend request failed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "stremio response read failed")
	}
	if len(body) > maxResponseBytes {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "stremio response exceeds body limit")
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		var parsed streamResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "stremio response not valid JSON")
		}
		return parsed.Streams, nil // zero-length is an authoritative empty result, not an error.

	case resp.StatusCode == http.StatusNotFound:
		// Many addons 404 an unrecognized id rather than returning an empty
		// streams array — treat identically: "no result", never an outage.
		return nil, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, httpstream.NewRateLimitError(resp.Header.Get("Retry-After"))

	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, httpstream.Wrapf(httpstream.ClassBackendUnavailable, "stremio backend auth failure %d", resp.StatusCode)

	case resp.StatusCode >= 500:
		return nil, httpstream.Wrapf(httpstream.ClassBackendUnavailable, "stremio backend responded %d", resp.StatusCode)

	default:
		return nil, httpstream.Wrapf(httpstream.ClassUpstreamMalformed, "stremio backend responded %d", resp.StatusCode)
	}
}

// cachedFetchStreams wraps fetchStreams with HR2.1's bounded polite-
// revalidation cache (searchResultCacheTTL): a burst of near-simultaneous
// searches for the same exact media identity across multiple Arr instances
// shares one backend fetch. Only Search uses this — Resolve always calls
// fetchStreams directly, since grab/playback needs the current live stream
// list, never a cached one.
func (h *Handler) cachedFetchStreams(ctx context.Context, imdbID string, episode bool, season, ep int) ([]streamObject, error) {
	key := searchCacheKey(imdbID, episode, season, ep)
	now := h.now()

	h.searchCacheMu.Lock()
	if e, ok := h.searchCache[key]; ok && now.Sub(e.at) < searchResultCacheTTL {
		h.searchCacheMu.Unlock()
		return e.streams, nil
	}
	h.searchCacheMu.Unlock()

	streams, err := h.fetchStreams(ctx, imdbID, episode, season, ep)
	if err != nil {
		// Transient/permanent failures are never cached (A23 posture: an
		// authoritative empty or successful result must not be masked by,
		// nor cached over, a backend error).
		return nil, err
	}

	h.searchCacheMu.Lock()
	if h.searchCache == nil {
		h.searchCache = make(map[string]searchCacheEntry)
	}
	h.searchCache[key] = searchCacheEntry{streams: streams, at: now}
	h.searchCacheMu.Unlock()
	return streams, nil
}

func searchCacheKey(imdbID string, episode bool, season, ep int) string {
	if !episode {
		return "movie:" + imdbID
	}
	return fmt.Sprintf("episode:%s:%d:%d", imdbID, season, ep)
}

// ── Classification / dispatch (HR2.1, HR-D2, D8, A24) ───────────────────────

var hlsLikeRe = regexp.MustCompile(`(?i)\.m3u8(\?|$)`)

// classifyStream applies HR2.1's dispatch classification to one Stremio
// stream object: it names the lane that must acquire the candidate instead
// of only ever answering "is this an acceptable progressive-HTTP stream".
// A stream object describes exactly one candidate resource, so when more
// than one documented field happens to be present a fixed precedence
// applies — most-specific/unambiguous acquisition signal wins:
// externalUrl/ytId (never DH-fetchable, always unsupported) >
// infoHash (torrent lane) > nzbUrl (NNTP lane) > archive URL sets
// (rarUrls/zipUrls/7zipUrls/tgzUrls/tarUrls) > url (progressive HTTP or,
// when HLS-shaped, the HLS lane). Progressive HTTP, info-hash, and archive
// candidates have wired consumers; the remaining kinds stay distinct rather
// than collapsing into one drop bucket, so their owners can dispatch them
// later without inventing a second classifier.
func classifyStream(s streamObject) httpstream.StreamKind {
	if strings.TrimSpace(s.ExternalURL) != "" || strings.TrimSpace(s.YtID) != "" {
		return httpstream.KindUnsupported
	}
	if strings.TrimSpace(s.InfoHash) != "" {
		return httpstream.KindInfoHash
	}
	if strings.TrimSpace(s.NZBURL) != "" {
		return httpstream.KindNZB
	}
	if hasArchiveURLs(s) {
		return httpstream.KindArchive
	}
	if isFetchableURL(s.URL) {
		if isHLSLike(s.URL) {
			return httpstream.KindHLS
		}
		return httpstream.KindProgressiveHTTP
	}
	return httpstream.KindUnsupported
}

// hasArchiveURLs reports whether any documented remote-archive URL set is
// present (HR-D2: rarUrls/zipUrls/7zipUrls/tgzUrls/tarUrls).
func hasArchiveURLs(s streamObject) bool {
	return len(s.RarURLs) > 0 || len(s.ZipURLs) > 0 || len(s.SevenZipURLs) > 0 || len(s.TgzURLs) > 0 || len(s.TarURLs) > 0
}

const maxRemoteArchiveParts = 16

func remoteArchiveDescriptor(s streamObject) (httpstream.RemoteArchiveDescriptor, bool) {
	type archiveSet struct {
		format httpstream.RemoteArchiveFormat
		urls   []string
	}
	sets := []archiveSet{
		{httpstream.RemoteArchiveRAR, s.RarURLs},
		{httpstream.RemoteArchiveZIP, s.ZipURLs},
		{httpstream.RemoteArchiveSevenZip, s.SevenZipURLs},
		{httpstream.RemoteArchiveTGZ, s.TgzURLs},
		{httpstream.RemoteArchiveTAR, s.TarURLs},
	}
	var selected archiveSet
	found := 0
	for _, set := range sets {
		if len(set.urls) > 0 {
			selected = set
			found++
		}
	}
	baseSelector := streamSelector(s)
	if found != 1 || baseSelector == "" || len(selected.urls) > maxRemoteArchiveParts {
		return httpstream.RemoteArchiveDescriptor{}, false
	}
	parts := make([]httpstream.ResolvedFile, 0, len(selected.urls))
	for i, raw := range selected.urls {
		raw = strings.TrimSpace(raw)
		if !isFetchableURL(raw) {
			return httpstream.RemoteArchiveDescriptor{}, false
		}
		parts = append(parts, httpstream.ResolvedFile{
			Selector:       fmt.Sprintf("archive-part:%d", i),
			URL:            raw,
			RequestHeaders: s.BehaviorHints.ProxyHeaders.Request,
		})
	}
	return httpstream.RemoteArchiveDescriptor{
		Selector:   "archive:" + string(selected.format) + ":" + baseSelector,
		Format:     selected.format,
		MemberHint: strings.TrimSpace(s.BehaviorHints.Filename),
		Parts:      parts,
	}, true
}

// isFetchableURL reports whether raw is an absolute http(s) URL DH's own
// egress could request directly — no userinfo, a host, and an http(s)
// scheme. This is the same acceptance test the pre-HR2.1 acceptableStream
// applied; it is now shared by both the progressive and HLS classification
// arms.
func isFetchableURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return false
	}
	return true
}

// isHLSLike reports whether an already-fetchable URL is obviously
// manifest-shaped (H6 boundary: DASH/HLS playback stays out of scope for
// the progressive lane; the D9 grab-time preflight enforces this again
// centrally regardless).
func isHLSLike(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return hlsLikeRe.MatchString(u.Path) || hlsLikeRe.MatchString(u.RawQuery)
}

// acceptableStream preserves the exact pre-HR2.1 v1 acceptance rule byte-
// for-byte: true only for the progressive-HTTP kind. infoHash, externalUrl,
// ytId, nzbUrl, archive-URL, and HLS-shaped variants are all rejected here
// exactly as before — they are simply now distinguishable from one another
// via classifyStream instead of collapsing into one boolean. Resolve()
// below still uses this directly, matching today's shipped Resolve
// behavior with zero change.
func acceptableStream(s streamObject) bool {
	return classifyStream(s) == httpstream.KindProgressiveHTTP
}

// streamSelector builds the D10 URL-free representation selector from
// same-object, non-URL metadata: behaviorHints.filename first (the
// protocol's own stable per-stream identity field), falling back to
// name+title text, disambiguated by bingeGroup and videoSize. Empty string
// means no stable identity exists on this stream — the caller must drop it.
func streamSelector(s streamObject) string {
	var parts []string
	if fn := strings.TrimSpace(s.BehaviorHints.Filename); fn != "" {
		parts = append(parts, "file:"+fn)
	} else {
		name := strings.TrimSpace(s.Name)
		title := strings.TrimSpace(s.Title)
		if title == "" {
			title = strings.TrimSpace(s.Description)
		}
		if name == "" && title == "" {
			return ""
		}
		parts = append(parts, "text:"+name+"|"+title)
	}
	if bg := strings.TrimSpace(s.BehaviorHints.BingeGroup); bg != "" {
		parts = append(parts, "bg:"+bg)
	}
	if size := videoSizeOf(s); size > 0 {
		parts = append(parts, fmt.Sprintf("sz:%d", size))
	}
	return strings.Join(parts, "#")
}

var qualityTokenRe = regexp.MustCompile(`(?i)\b(2160p|4320p|8k|4k|uhd|1440p|qhd|1080p|fhd|720p|hd|480p|sd)\b`)

// qualityFromStream extracts a QualityToRes-compatible token from the
// stream's own free-text fields. Stremio has no structured quality field;
// this never invents one when no token is present (QualityToRes's ok=false
// path already handles "no result beats a guess" — D17).
func qualityFromStream(s streamObject) string {
	text := s.Name + " " + s.Title + " " + s.Description + " " + s.BehaviorHints.Filename
	return qualityTokenRe.FindString(text)
}

func videoSizeOf(s streamObject) int64 {
	raw := s.BehaviorHints.VideoSize
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(str), 10, 64); err == nil {
			return v
		}
	}
	return 0
}

func contentTypeForURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "video/mp4"
	}
	if strings.HasSuffix(strings.ToLower(u.Path), ".mkv") {
		return "video/x-matroska"
	}
	return "video/mp4"
}

// ── Resolve ──────────────────────────────────────────────────────────────────

// Resolve implements httpstream.Handler: re-fetches the stream list for the
// grabbed media identity and returns every currently-acceptable url-type
// stream so the caller can match the persisted Selector exactly (D10).
// behaviorHints.proxyHeaders are carried onto the transient ResolvedFile
// only (D11) — never persisted, never logged. There is no Stremio refresh
// endpoint, so ResolveForceRefresh performs the identical live re-fetch as a
// normal resolve; a stale/expired proxy-header source is simply replaced by
// whatever the addon returns next.
func (h *Handler) Resolve(ctx context.Context, req httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	key := req.Key
	imdbID := strings.TrimSpace(key.IDs["imdb"])
	if imdbID == "" {
		return nil, httpstream.NewError(httpstream.ClassInvalidKey, "stremio key missing imdb id")
	}
	episode := strings.EqualFold(key.Kind, "episode")
	if episode && (key.Season < 1 || key.Episode < 1) {
		return nil, httpstream.NewError(httpstream.ClassInvalidKey, "stremio episode key missing season/episode")
	}

	streams, err := h.fetchStreams(ctx, imdbID, episode, key.Season, key.Episode)
	if err != nil {
		return nil, err
	}
	if len(streams) == 0 {
		return nil, httpstream.NewError(httpstream.ClassNoSource, "stremio backend has no streams for this media")
	}

	out := make([]httpstream.ResolvedFile, 0, len(streams))
	for _, s := range streams {
		if !acceptableStream(s) {
			continue
		}
		sel := streamSelector(s)
		if sel == "" {
			continue
		}
		rf := httpstream.ResolvedFile{
			Selector:    sel,
			Name:        fileNameFor(s, imdbID, episode, key.Season, key.Episode),
			Size:        videoSizeOf(s),
			ContentType: contentTypeForURL(s.URL),
			URL:         s.URL,
		}
		if len(s.BehaviorHints.ProxyHeaders.Request) > 0 {
			rf.RequestHeaders = s.BehaviorHints.ProxyHeaders.Request
		}
		if len(s.BehaviorHints.ProxyHeaders.Response) > 0 {
			rf.ResponseHeaders = s.BehaviorHints.ProxyHeaders.Response
		}
		out = append(out, rf)
	}
	if len(out) == 0 {
		return nil, httpstream.NewError(httpstream.ClassNoSource, "stremio backend returned no acceptable url streams")
	}
	return out, nil
}

// ResolveRemoteArchives implements httpstream.RemoteArchiveHandler. It
// normalizes the already-classified archive arm without surfacing it through
// the progressive Search/Resolve path. HR5.1 owns opening and streaming parts.
func (h *Handler) ResolveRemoteArchives(ctx context.Context, req httpstream.ResolveRequest) ([]httpstream.RemoteArchiveDescriptor, error) {
	key := req.Key
	imdbID := strings.TrimSpace(key.IDs["imdb"])
	if imdbID == "" {
		return nil, httpstream.NewError(httpstream.ClassInvalidKey, "stremio key missing imdb id")
	}
	episode := strings.EqualFold(key.Kind, "episode")
	if episode && (key.Season < 1 || key.Episode < 1) {
		return nil, httpstream.NewError(httpstream.ClassInvalidKey, "stremio episode key missing season/episode")
	}
	streams, err := h.fetchStreams(ctx, imdbID, episode, key.Season, key.Episode)
	if err != nil {
		return nil, err
	}
	out := make([]httpstream.RemoteArchiveDescriptor, 0, len(streams))
	for _, stream := range streams {
		if classifyStream(stream) != httpstream.KindArchive {
			continue
		}
		if descriptor, ok := remoteArchiveDescriptor(stream); ok {
			out = append(out, descriptor)
		}
	}
	if len(out) == 0 {
		return nil, httpstream.NewError(httpstream.ClassNoSource, "stremio backend returned no safe remote archive descriptors")
	}
	return out, nil
}

// ── Naming (D17 — Stremio streams carry no guaranteed filename/size) ────────

func fileNameFor(s streamObject, imdbID string, episode bool, season, ep int) string {
	if fn := strings.TrimSpace(s.BehaviorHints.Filename); fn != "" {
		return fn
	}
	ext := ".mp4"
	if strings.HasSuffix(strings.ToLower(s.URL), ".mkv") {
		ext = ".mkv"
	}
	if episode {
		return fmt.Sprintf("stremio.%s.s%02de%02d%s", imdbID, season, ep, ext)
	}
	return fmt.Sprintf("stremio.%s%s", imdbID, ext)
}

// streamNameHint extracts path-safe, human-readable release text directly
// from the stream object itself — behaviorHints.filename (extension
// stripped) first, since real addons commonly hand back a genuine
// scene-like filename there (already carries its own season/episode/
// quality/group tokens), falling back to name+title free text. Returns ""
// when the stream carries no usable text at all (D17 "never invent").
func streamNameHint(s streamObject) string {
	if fn := strings.TrimSpace(s.BehaviorHints.Filename); fn != "" {
		if ext := path.Ext(fn); ext != "" && len(ext) <= 5 {
			fn = strings.TrimSuffix(fn, ext)
		}
		if hint := pathSafeTitle(fn); hint != "" {
			return hint
		}
	}
	return pathSafeTitle(strings.TrimSpace(s.Name + " " + s.Title))
}

// releaseName builds a path-safe, arr-parseable Torznab title. q.Title (the
// arr's own free-text search term) wins when present. Absent that — the
// common case for an ID-only tvdbid/imdbid search, which carries no q= at
// all — the stream's own name/filename text (fallback) is used as-is: it
// already carries whatever season/episode/quality/group tokens the source
// addon supplied, so no synthesized suffix is appended on top of it. Only
// the last-resort "IMDB.<id>" placeholder (no reliable text from either the
// arr or the stream) gets DH's own synthesized SxxEyy/res/WEBDL suffix,
// exactly as before. Callers additionally rely on the response-side
// tvdbid/imdbid/tmdbid torznab:attr (api.renderHTTPStreamItems) so the arr
// can match by ID even when this text alone would not be parseable back to
// a known series/movie (HR0.2 live-gate finding, 2026-07-18).
func releaseName(q httpstream.StreamQuery, imdbID string, episode bool, res string, fallback string) string {
	if base := pathSafeTitle(strings.TrimSpace(q.Title)); base != "" {
		return withSynthesizedSuffix(base, q, episode, res)
	}
	if fallback != "" {
		return fallback
	}
	base := "IMDB." + strings.TrimPrefix(strings.ToUpper(imdbID), "TT")
	return withSynthesizedSuffix(base, q, episode, res)
}

func withSynthesizedSuffix(base string, q httpstream.StreamQuery, episode bool, res string) string {
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
var _ httpstream.RemoteArchiveHandler = (*Handler)(nil)
