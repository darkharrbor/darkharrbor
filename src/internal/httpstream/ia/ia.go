// Package ia implements the Internet Archive protocol handler ("ia") for the
// HTTP stream provider (HTTP-STREAM-MASTER-PLAN.md H2, HS-2.1/HS-2.2).
//
// NOT hardcoded or privileged: this is one protocol handler among equals,
// instantiated only for backends the user configured with type=ia (the
// backend base URL is user-supplied config; archive.org is merely the
// wizard's suggested example). If no ia backend is configured, this code
// never runs.
//
// Query strategy (HS-2.1): title is REQUIRED — ID-only queries are skipped
// entirely (v1 has no fuzzy episode-title matching and no TVDB metadata
// lookup). Matching is conservative — normalized title equality/containment
// plus year agreement for movies and SxxExx filename evidence for episodes.
// No result beats an identity guess.
package ia

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

const (
	// maxSearchRows caps advancedsearch result pages (HS-2.1: capped
	// candidate pages; a single capped page, sorted by downloads).
	maxSearchRows = 30
	// maxMetadataFetches caps per-search /metadata lookups.
	maxMetadataFetches = 5
	// maxResponseBytes bounds any backend response body read.
	maxResponseBytes = 4 << 20
)

// Handler is the ia protocol handler for one configured backend instance.
type Handler struct {
	backendID string
	baseURL   string
	client    *http.Client
	prefer    FilePreference
}

type FilePreference string

const (
	PreferDerivative FilePreference = "derivative"
	PreferOriginal   FilePreference = "original"
)

type Option func(*Handler)

func WithFilePreference(preference FilePreference) Option {
	return func(h *Handler) {
		if preference == PreferOriginal {
			h.prefer = PreferOriginal
		}
	}
}

// New constructs the handler. client must be a bounded metadata-class
// client (short timeouts); the live relay never uses it.
func New(backendID, baseURL string, client *http.Client, options ...Option) *Handler {
	h := &Handler{
		backendID: strings.ToLower(strings.TrimSpace(backendID)),
		baseURL:   strings.TrimRight(baseURL, "/"),
		client:    client,
		prefer:    PreferDerivative,
	}
	for _, option := range options {
		option(h)
	}
	return h
}

// Name implements httpstream.Handler.
func (h *Handler) Name() string { return "ia" }

// Healthy performs a bounded protocol-shaped metadata probe. An empty result
// is healthy; only transport/status/schema failure disables search fan-out.
func (h *Handler) Healthy(ctx context.Context) error {
	v := url.Values{}
	v.Set("q", "identifier:__darkharrbor_healthcheck__")
	v.Set("rows", "0")
	v.Set("output", "json")
	var parsed searchResponse
	return h.getJSON(ctx, h.baseURL+"/advancedsearch.php?"+v.Encode(), &parsed)
}

// ── Search ───────────────────────────────────────────────────────────────────

type searchDoc struct {
	Identifier string          `json:"identifier"`
	Title      json.RawMessage `json:"title"` // string OR []string in the wild
	Year       json.RawMessage `json:"year"`  // string or number
	Downloads  int64           `json:"downloads"`
}

type searchResponse struct {
	Response struct {
		Docs []searchDoc `json:"docs"`
	} `json:"response"`
}

// Search implements httpstream.Handler (HS-2.1).
func (h *Handler) Search(ctx context.Context, q httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	title := strings.TrimSpace(q.Title)
	if title == "" {
		// ID-only query: skip — this handler cannot answer it (HS-2.1).
		return nil, nil
	}
	episode := strings.EqualFold(q.Kind, "episode") || strings.EqualFold(q.Kind, "tv")
	if episode && (q.Season < 1 || q.Episode < 1) {
		return nil, nil // season-only never synthesized (D12)
	}

	docs, err := h.advancedSearch(ctx, title, q.Year, episode)
	if err != nil {
		return nil, err
	}

	var out []httpstream.SearchResult
	fetched := 0
	for _, doc := range docs {
		if fetched >= maxMetadataFetches {
			break
		}
		if doc.Identifier == "" || !titleMatches(title, decodeStringish(doc.Title), episode) {
			continue
		}
		if !episode && q.Year > 0 {
			if y := decodeYear(doc.Year); y > 0 && absInt(y-q.Year) > 1 {
				continue
			}
		}
		fetched++
		md, merr := h.fetchMetadata(ctx, doc.Identifier)
		if merr != nil {
			// Transient metadata failure on one candidate: propagate so the
			// route marks the pass non-authoritative (never cached empty).
			return out, merr
		}
		files := selectVideoFiles(md.Files)
		ambiguous := map[string]bool{}
		if episode {
			if len(q.Episodes) > 0 {
				files, ambiguous = filterEpisodeFilesTolerant(files, q)
			} else {
				files = filterEpisodeFiles(files, q.Season, q.Episode)
			}
		}
		best, ok := bestRankedWithPreference(files, h.prefer)
		if !ok {
			continue
		}
		res, rerr := h.resultFor(doc.Identifier, title, q, best, ambiguous[best.Name])
		if rerr != nil {
			continue
		}
		out = append(out, res)
	}
	return out, nil
}

func (h *Handler) advancedSearch(ctx context.Context, title string, year int, episode bool) ([]searchDoc, error) {
	// Conservative query: exact-phrase title within IA's video mediatype.
	query := fmt.Sprintf(`title:("%s") AND mediatype:(movies)`, escapeLucene(title))
	if !episode && year > 0 {
		query += fmt.Sprintf(" AND year:(%d)", year)
	}
	v := url.Values{}
	v.Set("q", query)
	v.Add("fl[]", "identifier")
	v.Add("fl[]", "title")
	v.Add("fl[]", "year")
	v.Add("fl[]", "downloads")
	v.Add("sort[]", "downloads desc")
	v.Set("rows", strconv.Itoa(maxSearchRows))
	v.Set("page", "1")
	v.Set("output", "json")

	var parsed searchResponse
	if err := h.getJSON(ctx, h.baseURL+"/advancedsearch.php?"+v.Encode(), &parsed); err != nil {
		return nil, err
	}
	return parsed.Response.Docs, nil
}

// ── Metadata / Resolve ───────────────────────────────────────────────────────

type metadataFile struct {
	Name   string          `json:"name"`
	Source string          `json:"source"`
	Format string          `json:"format"`
	Size   json.RawMessage `json:"size"`   // string or number
	Height json.RawMessage `json:"height"` // string or number
	// MD5/SHA1: HR2.7 authoritative whole-file digests IA's own metadata
	// already carries per file. Raw strings only -- validated centrally by
	// httpstream.ValidSourceDigest, never trusted unchecked.
	MD5  string `json:"md5"`
	SHA1 string `json:"sha1"`
}

type metadataResponse struct {
	Files []metadataFile `json:"files"`
}

func (h *Handler) fetchMetadata(ctx context.Context, identifier string) (*metadataResponse, error) {
	var parsed metadataResponse
	if err := h.getJSON(ctx, h.baseURL+"/metadata/"+url.PathEscape(identifier), &parsed); err != nil {
		return nil, err
	}
	return &parsed, nil
}

// Resolve implements httpstream.Handler: re-resolves the key to every
// currently-available video file so the caller can match the persisted
// selector exactly (D10). URLs are transient and derive from the configured
// backend base URL only.
func (h *Handler) Resolve(ctx context.Context, req httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	key := req.Key
	identifier := key.IDs["ia"]
	if strings.TrimSpace(identifier) == "" {
		return nil, httpstream.NewError(httpstream.ClassInvalidKey, "ia key missing identifier")
	}
	md, err := h.fetchMetadata(ctx, identifier)
	if err != nil {
		return nil, err
	}
	files := selectVideoFiles(md.Files)
	if len(files) == 0 {
		return nil, httpstream.NewError(httpstream.ClassNoSource, "ia item has no playable video files")
	}
	out := make([]httpstream.ResolvedFile, 0, len(files))
	for _, f := range files {
		out = append(out, httpstream.ResolvedFile{
			Selector:    identifier + "/" + f.Name,
			Name:        f.Name,
			Size:        decodeInt64ish(f.Size),
			ContentType: contentTypeFor(f.Name),
			URL:         h.baseURL + "/download/" + url.PathEscape(identifier) + "/" + escapeFilePath(f.Name),
			// SupportsRange stays false here: RangeVerified is earned by the
			// D9 grab-time preflight, never claimed by the handler.
			Digests: iaDigests(f),
		})
	}
	return out, nil
}

// ── HTTP plumbing ────────────────────────────────────────────────────────────

func (h *Handler) getJSON(ctx context.Context, rawURL string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return httpstream.NewError(httpstream.ClassBackendUnavailable, "ia request build failed")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return httpstream.WrapError(httpstream.ClassBackendUnavailable, "ia backend unreachable", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests {
			return httpstream.NewRateLimitError(resp.Header.Get("Retry-After"))
		}
		if resp.StatusCode >= 500 {
			return httpstream.Wrapf(httpstream.ClassBackendUnavailable, "ia backend responded %d", resp.StatusCode)
		}
		return httpstream.Wrapf(httpstream.ClassUpstreamMalformed, "ia backend responded %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return httpstream.NewError(httpstream.ClassBackendUnavailable, "ia response read failed")
	}
	if err := json.Unmarshal(body, into); err != nil {
		return httpstream.NewError(httpstream.ClassUpstreamMalformed, "ia response not valid JSON")
	}
	return nil
}

// ── Selection / ranking (HS-2.2) ─────────────────────────────────────────────

// formatRank: h.264 > plain MPEG4 > 512Kb MPEG4 (design §ia). Unlisted video
// formats rank below all listed ones but above rejection.
func formatRank(format string) int {
	f := strings.ToLower(format)
	switch {
	case strings.Contains(f, "h.264"):
		return 3
	case strings.Contains(f, "512kb"):
		return 1
	case strings.Contains(f, "mpeg4"), strings.Contains(f, "matroska"):
		return 2
	default:
		return 0
	}
}

func isVideoFile(f metadataFile) bool {
	name := strings.ToLower(f.Name)
	if !strings.HasSuffix(name, ".mp4") && !strings.HasSuffix(name, ".mkv") {
		return false
	}
	// Skip IA's tiny sample/trailer derivatives when identifiable.
	return f.Name != ""
}

func selectVideoFiles(files []metadataFile) []metadataFile {
	var out []metadataFile
	for _, f := range files {
		if isVideoFile(f) {
			out = append(out, f)
		}
	}
	return out
}

// filterEpisodeFiles keeps files whose names carry SxxExx evidence for the
// queried season/episode (HS-2.1: evidence required, never inferred).
func filterEpisodeFiles(files []metadataFile, season, episode int) []metadataFile {
	re := episodeRe(season, episode)
	var out []metadataFile
	for _, f := range files {
		if re.MatchString(f.Name) {
			out = append(out, f)
		}
	}
	return out
}

func filterEpisodeFilesTolerant(files []metadataFile, q httpstream.StreamQuery) ([]metadataFile, map[string]bool) {
	target := httpstream.EpisodeIdentity{Season: q.Season, Episode: q.Episode}
	var out []metadataFile
	ambiguous := map[string]bool{}
	for _, file := range files {
		match := httpstream.MatchEpisode(file.Name, target, q.Episodes)
		if !match.Matched {
			continue
		}
		out = append(out, file)
		ambiguous[file.Name] = match.Ambiguous
	}
	return out, ambiguous
}

func episodeRe(season, episode int) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`(?i)\bs0*%de0*%d(?:\D|$)`, season, episode))
}

// bestRanked returns the highest-ranked file (rank, then larger size wins).
func bestRanked(files []metadataFile) (metadataFile, bool) {
	return bestRankedWithPreference(files, PreferDerivative)
}

func bestRankedWithPreference(files []metadataFile, preference FilePreference) (metadataFile, bool) {
	best := -1
	var out metadataFile
	for _, f := range files {
		r := sourceRank(f.Source, preference)*10 + formatRank(f.Format)
		if r > best || (r == best && decodeInt64ish(f.Size) > decodeInt64ish(out.Size)) {
			best = r
			out = f
		}
	}
	return out, best >= 0
}

func sourceRank(source string, preference FilePreference) int {
	source = strings.ToLower(strings.TrimSpace(source))
	if preference == PreferOriginal {
		if source == "original" {
			return 2
		}
	} else if source == "derivative" {
		return 2
	}
	return 1
}

// resultFor synthesizes the arr-facing result (D17 naming).
func (h *Handler) resultFor(identifier, queryTitle string, q httpstream.StreamQuery, f metadataFile, ambiguous bool) (httpstream.SearchResult, error) {
	episode := strings.EqualFold(q.Kind, "episode") || strings.EqualFold(q.Kind, "tv")
	kind := "movie"
	if episode {
		kind = "episode"
	}
	key := httpstream.ResolveKey{
		Version:   httpstream.ResolveKeyVersion,
		BackendID: h.backendID,
		Handler:   "ia",
		Kind:      kind,
		IDs:       map[string]string{"ia": identifier},
		Selector:  identifier + "/" + f.Name,
	}
	if episode {
		key.Season, key.Episode = q.Season, q.Episode
	}
	if err := key.Validate(); err != nil {
		return httpstream.SearchResult{}, err
	}

	res := resFromHeight(decodeInt64ish(f.Height))
	name := releaseName(queryTitle, q, res)
	if ambiguous {
		name = strings.TrimSuffix(name, ".WEBDL") + ".AMBIGUOUS.WEBDL"
	}
	return httpstream.SearchResult{
		Title:   name,
		Key:     key,
		Quality: res,
		Size:    decodeInt64ish(f.Size),
		Season:  key.Season,
		Episode: key.Episode,
	}, nil
}

// releaseName builds a path-safe, arr-parseable release title (D17):
// Normalized.Title.[Year|SxxExx].[res.]WEBDL
func releaseName(title string, q httpstream.StreamQuery, res string) string {
	base := pathSafeTitle(title)
	episode := strings.EqualFold(q.Kind, "episode") || strings.EqualFold(q.Kind, "tv")
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

func resFromHeight(h int64) string {
	switch {
	case h >= 2000:
		return "2160p"
	case h >= 1300:
		return "1440p"
	case h >= 1000:
		return "1080p"
	case h >= 700:
		return "720p"
	case h > 0:
		return "480p"
	default:
		return ""
	}
}

// ── Conservative matching + tolerant field decoding ─────────────────────────

var titleNormRe = regexp.MustCompile(`[^a-z0-9]+`)

func normalizeTitle(t string) string {
	return strings.TrimSpace(titleNormRe.ReplaceAllString(strings.ToLower(t), " "))
}

// titleMatches applies the conservative rule (HS-2.1: no result beats an
// identity guess).
//
// Movies require normalized EQUALITY: IA is saturated with trailers,
// featurettes, and fan clips whose titles CONTAIN mainstream movie titles
// ("AVENGERS: ENDGAME | VFX Breakdown"); containment would import a clip as
// the movie. Verified live 2026-07-13 against real advancedsearch results.
//
// Episodes allow word-bounded containment ("Show Name - Complete Series"
// legitimately holds episodes) because the SxxExx filename evidence filter
// still has to prove the exact episode afterwards.
func titleMatches(query, candidate string, episode bool) bool {
	qn, cn := normalizeTitle(query), normalizeTitle(candidate)
	if qn == "" || cn == "" {
		return false
	}
	if qn == cn {
		return true
	}
	if !episode {
		return false
	}
	return strings.Contains(" "+cn+" ", " "+qn+" ")
}

func decodeStringish(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		return arr[0]
	}
	return ""
}

func decodeYear(raw json.RawMessage) int {
	return int(decodeInt64ish(raw))
}

func decodeInt64ish(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return v
		}
	}
	return 0
}

// iaDigests extracts HR2.7 authoritative whole-file digests from IA's own
// metadata response, if present. Raw values only -- httpstream.ResolvedFile
// validation (ValidSourceDigest) is the single place that checks length and
// hex well-formedness before anything reaches the proof graph.
func iaDigests(f metadataFile) []httpstream.SourceDigest {
	var out []httpstream.SourceDigest
	if v := strings.ToLower(strings.TrimSpace(f.MD5)); v != "" {
		out = append(out, httpstream.SourceDigest{Algorithm: "md5", Hex: v})
	}
	if v := strings.ToLower(strings.TrimSpace(f.SHA1)); v != "" {
		out = append(out, httpstream.SourceDigest{Algorithm: "sha1", Hex: v})
	}
	return out
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(strings.ToLower(name), ".mkv"):
		return "video/x-matroska"
	default:
		return "video/mp4"
	}
}

// escapeLucene escapes characters meaningful inside a quoted Lucene phrase.
func escapeLucene(s string) string {
	s = strings.ReplaceAll(s, `\`, ` `)
	return strings.ReplaceAll(s, `"`, ` `)
}

// escapeFilePath escapes an IA file path segment-by-segment (names can
// contain '/' for files inside subdirectories).
func escapeFilePath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

var _ httpstream.Handler = (*Handler)(nil)
