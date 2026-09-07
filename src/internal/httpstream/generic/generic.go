// Package generic implements the "generic" HTTP-stream protocol handler
// (HTTP-STREAM-MASTER-PLAN.md HR2.5): typed, operator-declared source
// descriptors rather than a searched backend API. Each descriptor states
// its own kind explicitly (progressive, hls, m3u, metalink, object, webdav,
// fixed) — there is no URL-sniffing classifier here (WORKFLOW rule 3: no
// second classifier); the operator's typed contract IS the classification.
//
// A superseded "generic arbitrary JSON mapping DSL" design was rejected;
// this package implements the typed-contract replacement instead (see
// STREAMING-CONSOLIDATED-MASTER-PLAN.md §6.2).
package generic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/securefile"
)

// ── Bounds (DG-03/DG-07) ─────────────────────────────────────────────────────

const (
	MaxDescriptorFileBytes = 2 << 20 // 2 MiB
	maxDescriptors         = 5000
	maxM3UBytes            = 1 << 20
	maxM3ULines            = 4000
	maxM3UEntries          = 500
	maxMetalinkBytes       = 1 << 20
	maxMetalinkFiles       = 50
	maxMetalinkURLsPerFile = 20
	maxWebDAVBytes         = 2 << 20
	maxWebDAVMembers       = 2000
)

// ── Descriptor model ─────────────────────────────────────────────────────────

type Kind string

const (
	KindProgressive Kind = "progressive"
	KindHLS         Kind = "hls"
	KindM3U         Kind = "m3u"
	KindMetalink    Kind = "metalink"
	KindObject      Kind = "object"
	KindWebDAV      Kind = "webdav"
	KindFixed       Kind = "fixed"
)

var allowedKinds = map[Kind]bool{
	KindProgressive: true, KindHLS: true, KindM3U: true, KindMetalink: true,
	KindObject: true, KindWebDAV: true, KindFixed: true,
}

// Descriptor is one operator-declared typed source (HR2.5 fixed contract).
// Unlike ia/omss/stremio, there is no live search API: the operator commits
// this list directly, so no result is ever a title-similarity guess unless
// Title is the only identity supplied.
type Descriptor struct {
	ID string // computed, stable, never persisted URL material

	SourceID    string
	Kind        Kind
	Title       string
	Year        int
	Season      int
	Episode     int
	IMDBID      string
	TVDBID      string
	TMDBID      string
	URL         string
	Name        string
	Size        int64
	ContentType string
	// Quality is an operator-asserted label (e.g. "1080p"); empty means
	// unranked. Never inferred (D17: no result beats an identity guess).
	Quality string
}

type rawDescriptor struct {
	SourceID    string `json:"source_id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Year        int    `json:"year"`
	Season      int    `json:"season"`
	Episode     int    `json:"episode"`
	IMDBID      string `json:"imdb_id"`
	TVDBID      string `json:"tvdb_id"`
	TMDBID      string `json:"tmdb_id"`
	URL         string `json:"url"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	Quality     string `json:"quality"`
}

var rawDescriptorFields = map[string]struct{}{
	"source_id": {}, "kind": {}, "title": {}, "year": {}, "season": {}, "episode": {},
	"imdb_id": {}, "tvdb_id": {}, "tmdb_id": {}, "url": {}, "name": {},
	"size": {}, "content_type": {}, "quality": {},
}

var sourceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// LoadDescriptors reads and strictly validates the bounded typed-descriptor
// file. Any single malformed/oversized/ambiguous entry fails the whole load
// closed (D5: a broken config never boots half-configured) rather than
// silently dropping entries.
func LoadDescriptors(path string) ([]Descriptor, error) {
	body, err := securefile.Read(path, MaxDescriptorFileBytes)
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "generic descriptors file unreadable")
	}
	return ValidateDescriptors(body)
}

// ValidateDescriptors parses and validates one bounded catalog before callers
// publish it. It never includes descriptor URLs in returned errors.
func ValidateDescriptors(body []byte) ([]Descriptor, error) {
	if len(body) == 0 || len(body) > MaxDescriptorFileBytes {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "generic descriptors file is empty or exceeds bound")
	}
	raws, err := decodeRawDescriptors(body)
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "generic descriptors file is not a valid JSON array")
	}
	if len(raws) > maxDescriptors {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "generic descriptors file exceeds entry bound")
	}

	seen := map[string]bool{}
	seenSourceIDs := map[string]bool{}
	out := make([]Descriptor, 0, len(raws))
	for i, r := range raws {
		d, err := normalizeDescriptor(r)
		if err != nil {
			return nil, fmt.Errorf("descriptor[%d]: %w", i, err)
		}
		if seen[d.ID] {
			return nil, fmt.Errorf("descriptor[%d]: duplicate descriptor identity", i)
		}
		if d.SourceID != "" && seenSourceIDs[d.SourceID] {
			return nil, fmt.Errorf("descriptor[%d]: duplicate source_id", i)
		}
		seen[d.ID] = true
		if d.SourceID != "" {
			seenSourceIDs[d.SourceID] = true
		}
		out = append(out, d)
	}
	return out, nil
}

func decodeRawDescriptors(body []byte) ([]rawDescriptor, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var entries []json.RawMessage
	if err := dec.Decode(&entries); err != nil || entries == nil {
		return nil, fmt.Errorf("catalog must be a JSON array")
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("catalog has trailing JSON")
	}

	out := make([]rawDescriptor, 0, len(entries))
	for _, entry := range entries {
		fields := json.NewDecoder(bytes.NewReader(entry))
		opening, err := fields.Token()
		if err != nil || opening != json.Delim('{') {
			return nil, fmt.Errorf("descriptor must be an object")
		}
		seen := make(map[string]struct{}, len(rawDescriptorFields))
		for fields.More() {
			token, err := fields.Token()
			name, ok := token.(string)
			if err != nil || !ok {
				return nil, fmt.Errorf("descriptor field is invalid")
			}
			if _, allowed := rawDescriptorFields[name]; !allowed {
				return nil, fmt.Errorf("descriptor field is unknown")
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, fmt.Errorf("descriptor field is duplicated")
			}
			seen[name] = struct{}{}
			var value json.RawMessage
			if err := fields.Decode(&value); err != nil {
				return nil, fmt.Errorf("descriptor field value is invalid")
			}
		}
		if closing, err := fields.Token(); err != nil || closing != json.Delim('}') {
			return nil, fmt.Errorf("descriptor object is invalid")
		}
		var raw rawDescriptor
		if err := json.Unmarshal(entry, &raw); err != nil {
			return nil, fmt.Errorf("descriptor value types are invalid")
		}
		out = append(out, raw)
	}
	return out, nil
}

func normalizeDescriptor(r rawDescriptor) (Descriptor, error) {
	sourceID := strings.ToLower(strings.TrimSpace(r.SourceID))
	if sourceID != "" && !sourceIDPattern.MatchString(sourceID) {
		return Descriptor{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "invalid source_id")
	}
	kind := Kind(strings.ToLower(strings.TrimSpace(r.Kind)))
	if !allowedKinds[kind] {
		return Descriptor{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "unknown descriptor kind")
	}
	u, err := httpstream.ValidateURL(r.URL)
	if err != nil {
		return Descriptor{}, err
	}
	title := strings.TrimSpace(r.Title)
	imdb := strings.TrimSpace(r.IMDBID)
	tvdb := strings.TrimSpace(r.TVDBID)
	tmdb := strings.TrimSpace(r.TMDBID)
	if title == "" && imdb == "" && tvdb == "" && tmdb == "" {
		return Descriptor{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "descriptor has no title or ID")
	}
	if r.Season < 0 || r.Episode < 0 {
		return Descriptor{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "negative season/episode")
	}
	if (r.Season > 0) != (r.Episode > 0) {
		// D12: no season-only (or malformed) episode descriptors.
		return Descriptor{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "descriptor requires both season and episode or neither")
	}
	if r.Size < 0 {
		return Descriptor{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "negative size")
	}
	if kind == KindFixed && r.Size == 0 {
		return Descriptor{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "fixed descriptor requires a positive size")
	}
	quality := strings.TrimSpace(r.Quality)
	if quality != "" {
		if _, ok := httpstream.QualityToRes(quality); !ok {
			return Descriptor{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "unrecognized quality label")
		}
	}

	d := Descriptor{
		SourceID: sourceID, Kind: kind, Title: title, Year: r.Year, Season: r.Season, Episode: r.Episode,
		IMDBID: imdb, TVDBID: tvdb, TMDBID: tmdb,
		URL: u.String(), Name: strings.TrimSpace(r.Name), Size: r.Size,
		ContentType: strings.TrimSpace(r.ContentType), Quality: quality,
	}
	d.ID = descriptorID(d)
	return d, nil
}

// descriptorID is a stable, content-derived, URL-free-in-persistence
// identity (the raw URL is hashed, never stored verbatim in the resolve
// key — only this digest is, matching HS-1.9's no-source-URL-persisted
// invariant at the key layer; the live URL itself stays in the descriptor
// file, not the DB).
func descriptorID(d Descriptor) string {
	h := sha256.New()
	if d.SourceID != "" {
		_, _ = fmt.Fprintf(h, "generic/v2\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s",
			d.SourceID, d.Kind, d.Title, d.Year, d.Season, d.Episode, d.IMDBID, d.TVDBID, d.TMDBID)
		return hex.EncodeToString(h.Sum(nil))[:32]
	}
	_, _ = fmt.Fprintf(h, "generic/v1\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s\x00%s\x00%s",
		d.Kind, d.URL, d.Title, d.Season, d.Episode, d.IMDBID, d.TVDBID, d.TMDBID)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func findByID(ds []Descriptor, id string) (Descriptor, bool) {
	for _, d := range ds {
		if d.ID == id {
			return d, true
		}
	}
	return Descriptor{}, false
}

// ── Identity / conservative title matching ───────────────────────────────────

func (d Descriptor) matches(q httpstream.StreamQuery, episode bool) bool {
	if episode {
		if d.Season != q.Season || d.Episode != q.Episode {
			return false
		}
	} else if d.Season != 0 || d.Episode != 0 {
		return false
	}
	if ok, overlap := d.matchesIdentity(q); overlap {
		return ok
	}
	return titleMatches(q.Title, d.Title)
}

// matchesIdentity applies D7/D8-style direct-ID matching: if either side
// supplies an ID namespace the other also has, agreement is required and
// title is never consulted (an ID mismatch is authoritative, never
// overridden by a coincidental title match).
func (d Descriptor) matchesIdentity(q httpstream.StreamQuery) (matched, hadOverlap bool) {
	pairs := [][2]string{
		{strings.TrimSpace(q.IMDBID), d.IMDBID},
		{strings.TrimSpace(q.TVDBID), d.TVDBID},
		{strings.TrimSpace(q.TMDBID), d.TMDBID},
	}
	sawOverlap := false
	for _, p := range pairs {
		if p[0] == "" || p[1] == "" {
			continue
		}
		sawOverlap = true
		if !strings.EqualFold(p[0], p[1]) {
			return false, true
		}
	}
	return sawOverlap, sawOverlap
}

var titleNormRe = regexp.MustCompile(`[^a-z0-9]+`)

func normalizeTitle(t string) string {
	return strings.TrimSpace(titleNormRe.ReplaceAllString(strings.ToLower(t), " "))
}

// titleMatches requires normalized equality only: fixed descriptors are a
// 1:1 operator catalog, not a fuzzy search surface (D17: no result beats an
// identity guess).
func titleMatches(query, candidate string) bool {
	qn, cn := normalizeTitle(query), normalizeTitle(candidate)
	return qn != "" && qn == cn
}

var nonPathSafe = regexp.MustCompile(`[^A-Za-z0-9]+`)

func pathSafeTitle(t string) string {
	return strings.Trim(nonPathSafe.ReplaceAllString(t, "."), ".")
}

func releaseName(d Descriptor, episode bool) string {
	base := pathSafeTitle(d.Title)
	switch {
	case episode:
		base += fmt.Sprintf(".S%02dE%02d", d.Season, d.Episode)
	case d.Year > 0:
		base += fmt.Sprintf(".%d", d.Year)
	}
	if d.Quality != "" {
		if res, ok := httpstream.QualityToRes(d.Quality); ok {
			base += "." + res
		}
	}
	return base + ".WEBDL"
}

// ── Handler ───────────────────────────────────────────────────────────────────

// Handler is the generic protocol handler for one configured backend
// instance. client is a bounded metadata-class client (TrustBackend policy)
// used for fetching operator-typed descriptor documents (m3u/metalink/
// webdav listings) and HEAD-probing progressive/object URLs — the same
// trust class ia/omss/stremio use for their own configured origins, since
// every descriptor URL is itself operator-authored config, not a value
// returned by an untrusted remote backend. The final resolved URL is still
// subject to the shared TrustSource dial policy at the live relay/preflight
// layer regardless (D11), unchanged by this handler.
type Handler struct {
	backendID       string
	descriptorsPath string
	client          *http.Client
}

func New(backendID, descriptorsPath string, client *http.Client) *Handler {
	return &Handler{
		backendID:       strings.ToLower(strings.TrimSpace(backendID)),
		descriptorsPath: descriptorsPath,
		client:          client,
	}
}

func (h *Handler) Name() string { return "generic" }

// Healthy reports whether the descriptor file currently loads and validates.
func (h *Handler) Healthy(ctx context.Context) error {
	_, err := LoadDescriptors(h.descriptorsPath)
	return err
}

// Search implements httpstream.Handler.
func (h *Handler) Search(ctx context.Context, q httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	descriptors, err := LoadDescriptors(h.descriptorsPath)
	if err != nil {
		return nil, err
	}
	episode := strings.EqualFold(q.Kind, "episode") || strings.EqualFold(q.Kind, "tv")
	if episode && (q.Season < 1 || q.Episode < 1) {
		return nil, nil // season-only never synthesized (D12)
	}

	var out []httpstream.SearchResult
	for _, d := range descriptors {
		if !d.matches(q, episode) {
			continue
		}
		cands, cerr := h.candidatesFor(ctx, d)
		if cerr != nil || len(cands) == 0 {
			continue // one descriptor's transient/abstain failure never fails the whole search
		}
		best := cands[0]
		matches := 0
		for _, candidate := range cands {
			if candidate.selector == best.selector {
				matches++
			}
		}
		if matches != 1 {
			continue // ambiguous identity is never advertised as a grabbable result
		}
		res, rerr := h.resultFor(d, best, episode)
		if rerr != nil {
			continue
		}
		out = append(out, res)
	}
	return out, nil
}

func (h *Handler) resultFor(d Descriptor, c candidate, episode bool) (httpstream.SearchResult, error) {
	ids := map[string]string{"generic": d.ID}
	if d.IMDBID != "" {
		ids["imdb"] = d.IMDBID
	}
	if d.TVDBID != "" {
		ids["tvdb"] = d.TVDBID
	}
	if d.TMDBID != "" {
		ids["tmdb"] = d.TMDBID
	}
	kind := "movie"
	if episode {
		kind = "episode"
	}
	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: h.backendID, Handler: "generic",
		Kind: kind, IDs: ids, Selector: c.selector,
	}
	if episode {
		key.Season, key.Episode = d.Season, d.Episode
	}
	if err := key.Validate(); err != nil {
		return httpstream.SearchResult{}, err
	}
	res := ""
	if d.Quality != "" {
		res, _ = httpstream.QualityToRes(d.Quality)
	}
	return httpstream.SearchResult{
		Title: releaseName(d, episode), Key: key, Quality: res, Size: c.size,
		Season: key.Season, Episode: key.Episode,
	}, nil
}

// Resolve implements httpstream.Handler: re-derives every currently
// available representation for the persisted descriptor so the caller can
// match Selector exactly (D10) — never trusts the search-time snapshot.
func (h *Handler) Resolve(ctx context.Context, req httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	id := req.Key.IDs["generic"]
	if strings.TrimSpace(id) == "" {
		return nil, httpstream.NewError(httpstream.ClassInvalidKey, "generic key missing descriptor id")
	}
	descriptors, err := LoadDescriptors(h.descriptorsPath)
	if err != nil {
		return nil, err
	}
	d, ok := findByID(descriptors, id)
	if !ok {
		return nil, httpstream.NewError(httpstream.ClassRepresentationLost, "descriptor no longer configured")
	}
	cands, err := h.candidatesFor(ctx, d)
	if err != nil {
		return nil, err
	}
	out := make([]httpstream.ResolvedFile, 0, len(cands))
	for _, c := range cands {
		out = append(out, httpstream.ResolvedFile{
			Selector: c.selector, Name: c.name, Size: c.size, ContentType: c.contentType, URL: c.url,
			Digests: c.digests,
		})
	}
	return out, nil
}

// candidate is one concrete playable representation discovered for a
// descriptor (a fixed/progressive/object/hls descriptor always yields
// exactly one; m3u/metalink/webdav can yield several).
type candidate struct {
	selector    string
	name        string
	size        int64
	contentType string
	url         string
	// digests carries HR2.7 authoritative whole-object digests -- currently
	// only Metalink's own <hash> elements populate this.
	digests []httpstream.SourceDigest
}

func (h *Handler) candidatesFor(ctx context.Context, d Descriptor) ([]candidate, error) {
	switch d.Kind {
	case KindFixed:
		return []candidate{{
			selector: d.ID, name: displayName(d), size: d.Size,
			contentType: contentTypeFor(d, d.URL), url: d.URL,
		}}, nil
	case KindProgressive, KindObject:
		return h.candidatesProgressive(ctx, d)
	case KindHLS:
		return h.candidatesHLS(d)
	case KindM3U:
		return h.candidatesM3U(ctx, d)
	case KindMetalink:
		return h.candidatesMetalink(ctx, d)
	case KindWebDAV:
		return h.candidatesWebDAV(ctx, d)
	default:
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "unknown descriptor kind")
	}
}

func displayName(d Descriptor) string {
	if d.Name != "" {
		return d.Name
	}
	if u, err := url.Parse(d.URL); err == nil {
		if base := path.Base(u.Path); base != "" && base != "." && base != "/" {
			return base
		}
	}
	return d.ID
}

func contentTypeFor(d Descriptor, rawURL string) string {
	if d.ContentType != "" {
		return d.ContentType
	}
	return guessContentType(rawURL)
}

func guessContentType(rawURL string) string {
	lower := strings.ToLower(rawURL)
	switch {
	case strings.HasSuffix(lower, ".mkv"):
		return "video/x-matroska"
	case strings.HasSuffix(lower, ".mp4"), strings.HasSuffix(lower, ".m4v"):
		return "video/mp4"
	case strings.HasSuffix(lower, ".webm"):
		return "video/webm"
	case strings.HasSuffix(lower, ".ts"), strings.HasSuffix(lower, ".m2ts"):
		return "video/mp2t"
	case strings.HasSuffix(lower, ".avi"):
		return "video/x-msvideo"
	case strings.HasSuffix(lower, ".m3u8"):
		return "application/vnd.apple.mpegurl"
	default:
		return ""
	}
}

// candidatesProgressive HEAD-probes the descriptor URL for a live size/type;
// any probe failure falls back to the operator-asserted values rather than
// failing closed (the D9 grab-time preflight is the real source of truth
// downstream regardless).
func (h *Handler) candidatesProgressive(ctx context.Context, d Descriptor) ([]candidate, error) {
	size, ct := d.Size, contentTypeFor(d, d.URL)
	if h.client != nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, d.URL, nil)
		if err == nil {
			if resp, herr := h.client.Do(req); herr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					if resp.ContentLength > 0 {
						size = resp.ContentLength
					}
					if rct := resp.Header.Get("Content-Type"); rct != "" {
						ct = rct
					}
				}
			}
		}
	}
	return []candidate{{selector: d.ID, name: displayName(d), size: size, contentType: ct, url: d.URL}}, nil
}

// isDASH reports whether a descriptor's declared adaptive-manifest URL is
// DASH rather than HLS. DASH has no lane owner (HR-B1/HR-B2 remain PINNED
// BETA, not active scope) — this abstains rather than fabricating playback.
func isDASH(d Descriptor) bool {
	lower := strings.ToLower(d.URL)
	if strings.HasSuffix(lower, ".mpd") {
		return true
	}
	return strings.Contains(strings.ToLower(d.ContentType), "dash+xml")
}

func (h *Handler) candidatesHLS(d Descriptor) ([]candidate, error) {
	if isDASH(d) {
		return nil, httpstream.NewError(httpstream.ClassNoSource, "DASH manifest has no lane owner")
	}
	name := displayName(d)
	if !strings.HasSuffix(strings.ToLower(name), ".m3u8") {
		name = strings.TrimSuffix(name, path.Ext(name)) + ".m3u8"
	}
	return []candidate{{
		selector: d.ID, name: name, size: d.Size,
		contentType: "application/vnd.apple.mpegurl", url: d.URL,
	}}, nil
}

func (h *Handler) fetchBounded(ctx context.Context, rawURL string, max int64) ([]byte, error) {
	if h.client == nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "generic handler has no client")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "request build failed")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "generic source unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, httpstream.NewRateLimitError(resp.Header.Get("Retry-After"))
	}
	if resp.StatusCode >= 500 {
		return nil, httpstream.Wrapf(httpstream.ClassBackendUnavailable, "generic source responded %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != 207 {
		return nil, httpstream.Wrapf(httpstream.ClassUpstreamMalformed, "generic source responded %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "generic source read failed")
	}
	if int64(len(body)) > max {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "generic source response exceeds bound")
	}
	return body, nil
}

func (h *Handler) candidatesM3U(ctx context.Context, d Descriptor) ([]candidate, error) {
	body, err := h.fetchBounded(ctx, d.URL, maxM3UBytes)
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(d.URL)
	entries := parseM3U(body, base)
	if len(entries) == 0 {
		return nil, httpstream.NewError(httpstream.ClassNoSource, "m3u playlist has no usable entries")
	}
	out := make([]candidate, 0, len(entries))
	for _, e := range entries {
		out = append(out, candidate{
			selector: enumeratedSelector(d.ID, "m3u", m3uEntryIdentity(e)), name: entryName(e.name, e.url),
			size: 0, contentType: guessContentType(e.url), url: e.url,
		})
	}
	return out, nil
}

func m3uEntryIdentity(entry m3uEntry) string {
	if entry.name != "" {
		return "name:" + entry.name
	}
	u, err := url.Parse(entry.url)
	if err != nil {
		return entry.url
	}
	return "path:" + path.Clean(u.Path)
}

type m3uEntry struct{ name, url string }

// parseM3U extracts playable entries from an (extended) M3U/M3U8 playlist.
// Bounded by line and entry count; malformed/unresolvable lines are skipped,
// never fatal to the whole parse (fail-closed only means "no fabrication",
// not "one bad line aborts every other entry").
func parseM3U(body []byte, base *url.URL) []m3uEntry {
	var out []m3uEntry
	lastName := ""
	lines := strings.Split(string(body), "\n")
	if len(lines) > maxM3ULines {
		lines = lines[:maxM3ULines]
	}
	for _, raw := range lines {
		if len(out) >= maxM3UEntries {
			break
		}
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			if idx := strings.Index(line, ","); idx >= 0 && idx+1 < len(line) {
				lastName = strings.TrimSpace(line[idx+1:])
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		resolved := resolveURL(base, line)
		if resolved == "" {
			lastName = ""
			continue
		}
		out = append(out, m3uEntry{name: lastName, url: resolved})
		lastName = ""
	}
	return out
}

func resolveURL(base *url.URL, ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if base != nil && !u.IsAbs() {
		u = base.ResolveReference(u)
	}
	if _, verr := httpstream.ValidateURL(u.String()); verr != nil {
		return ""
	}
	return u.String()
}

func entryName(name, rawURL string) string {
	if name != "" {
		return name
	}
	if u, err := url.Parse(rawURL); err == nil {
		if base := path.Base(u.Path); base != "" && base != "." && base != "/" {
			return base
		}
	}
	return "entry"
}

// ── Metalink (RFC 5854) ───────────────────────────────────────────────────────

type metalinkDoc struct {
	XMLName xml.Name          `xml:"metalink"`
	Files   []metalinkFileXML `xml:"file"`
}

type metalinkFileXML struct {
	Name   string            `xml:"name,attr"`
	Size   int64             `xml:"size"`
	URLs   []metalinkURLXML  `xml:"url"`
	Hashes []metalinkHashXML `xml:"hash"`
}

// metalinkHashXML is one RFC 5854 <hash type="..."> element (md5/sha-1/
// sha-256 in the wild).
type metalinkHashXML struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type metalinkURLXML struct {
	Priority int    `xml:"priority,attr"`
	Value    string `xml:",chardata"`
}

func (h *Handler) candidatesMetalink(ctx context.Context, d Descriptor) ([]candidate, error) {
	body, err := h.fetchBounded(ctx, d.URL, maxMetalinkBytes)
	if err != nil {
		return nil, err
	}
	files, err := parseMetalink(body)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, httpstream.NewError(httpstream.ClassNoSource, "metalink document has no usable files")
	}
	out := make([]candidate, 0, len(files))
	for _, f := range files {
		out = append(out, candidate{
			selector: enumeratedSelector(d.ID, "metalink", metalinkFileIdentity(f)), name: metalinkDisplayName(f),
			size: f.size, contentType: guessContentType(f.bestURL), url: f.bestURL,
			digests: f.digests,
		})
	}
	return out, nil
}

func metalinkFileIdentity(file metalinkFile) string {
	for _, algorithm := range []string{"sha256", "sha1", "md5"} {
		for _, digest := range file.digests {
			if digest.Algorithm == algorithm {
				return algorithm + ":" + digest.Hex
			}
		}
	}
	if file.name != "" {
		return file.name + "\x00" + fmt.Sprint(file.size)
	}
	return file.bestURL
}

type metalinkFile struct {
	name    string
	size    int64
	bestURL string
	digests []httpstream.SourceDigest
}

func metalinkDisplayName(f metalinkFile) string {
	if f.name != "" {
		return f.name
	}
	return entryName("", f.bestURL)
}

// parseMetalink extracts files and picks each one's most-preferred URL
// (lowest numeric priority wins per RFC 5854; unset/zero ranks last).
// Bounded by file and per-file URL count; a file with no valid URL is
// dropped, never fatal to sibling files.
func parseMetalink(body []byte) ([]metalinkFile, error) {
	var doc metalinkDoc
	dec := xml.NewDecoder(strings_NewReader(body))
	dec.Strict = true
	if err := dec.Decode(&doc); err != nil {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "metalink document is not valid XML")
	}
	files := doc.Files
	if len(files) > maxMetalinkFiles {
		files = files[:maxMetalinkFiles]
	}
	var out []metalinkFile
	for _, f := range files {
		urls := f.URLs
		if len(urls) > maxMetalinkURLsPerFile {
			urls = urls[:maxMetalinkURLsPerFile]
		}
		best := ""
		bestPriority := int(^uint(0) >> 1) // max int: unset priority ranks last
		for _, u := range urls {
			resolved := resolveURL(nil, strings.TrimSpace(u.Value))
			if resolved == "" {
				continue
			}
			p := u.Priority
			if p <= 0 {
				p = int(^uint(0) >> 1)
			}
			if best == "" || p < bestPriority {
				best, bestPriority = resolved, p
			}
		}
		if best == "" {
			continue
		}
		if f.Size < 0 {
			continue
		}
		out = append(out, metalinkFile{
			name: strings.TrimSpace(f.Name), size: f.Size, bestURL: best,
			digests: metalinkDigests(f.Hashes),
		})
	}
	return out, nil
}

// maxMetalinkHashesPerFile bounds how many <hash> elements one file
// contributes to the proof graph (DG-03/07: bounded, untrusted XML input).
const maxMetalinkHashesPerFile = 4

// metalinkDigests extracts HR2.7 authoritative whole-file digests from a
// Metalink file's <hash> elements. Unrecognized hash types are skipped, not
// fatal; centralized httpstream.ValidSourceDigest still checks length/hex
// well-formedness before anything reaches the proof graph.
func metalinkDigests(hashes []metalinkHashXML) []httpstream.SourceDigest {
	var out []httpstream.SourceDigest
	for _, hEl := range hashes {
		if len(out) >= maxMetalinkHashesPerFile {
			break
		}
		alg := normalizeMetalinkHashType(hEl.Type)
		val := strings.ToLower(strings.TrimSpace(hEl.Value))
		if alg == "" || val == "" {
			continue
		}
		out = append(out, httpstream.SourceDigest{Algorithm: alg, Hex: val})
	}
	return out
}

func normalizeMetalinkHashType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "md5":
		return "md5"
	case "sha-1", "sha1":
		return "sha1"
	case "sha-256", "sha256":
		return "sha256"
	default:
		return ""
	}
}

// strings_NewReader avoids importing "strings" and "bytes" both just for a
// Reader — kept local for parser clarity.
func strings_NewReader(b []byte) *xmlByteReader { return &xmlByteReader{b: b} }

type xmlByteReader struct {
	b   []byte
	pos int
}

func (r *xmlByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// ── WebDAV (RFC 4918 PROPFIND) ────────────────────────────────────────────────

type davMultistatus struct {
	XMLName   xml.Name      `xml:"multistatus"`
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string        `xml:"href"`
	Propstat []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Prop davProp `xml:"prop"`
}

type davProp struct {
	ContentLength int64           `xml:"getcontentlength"`
	ResourceType  davResourceType `xml:"resourcetype"`
}

type davResourceType struct {
	Collection *struct{} `xml:"collection"`
}

var videoExt = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".ts": true, ".m2ts": true,
	".webm": true, ".mov": true, ".m4v": true,
}

func (h *Handler) candidatesWebDAV(ctx context.Context, d Descriptor) ([]candidate, error) {
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", d.URL, strings_NewReaderString(propfindBody))
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "propfind request build failed")
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	if h.client == nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "generic handler has no client")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "webdav source unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 207 && resp.StatusCode != http.StatusOK {
		return nil, httpstream.Wrapf(httpstream.ClassUpstreamMalformed, "webdav propfind responded %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWebDAVBytes+1))
	if err != nil {
		return nil, httpstream.NewError(httpstream.ClassBackendUnavailable, "webdav response read failed")
	}
	if int64(len(body)) > maxWebDAVBytes {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "webdav response exceeds bound")
	}
	base, _ := url.Parse(d.URL)
	members, perr := parseWebDAVMultistatus(body, base)
	if perr != nil {
		return nil, perr
	}
	if len(members) == 0 {
		return nil, httpstream.NewError(httpstream.ClassNoSource, "webdav collection has no video members")
	}
	out := make([]candidate, 0, len(members))
	for _, m := range members {
		out = append(out, candidate{
			selector: enumeratedSelector(d.ID, "webdav", sourcePathIdentity(m.url)), name: m.name,
			size: m.size, contentType: guessContentType(m.url), url: m.url,
		})
	}
	return out, nil
}

// enumeratedSelector pins a listed representation by opaque content identity,
// never by mutable list position and never by persisting source URL material.
func enumeratedSelector(descriptorID, kind, identity string) string {
	sum := sha256.Sum256([]byte("generic-selector/v1\x00" + kind + "\x00" + identity))
	return descriptorID + "#" + kind + "-" + hex.EncodeToString(sum[:16])
}

func sourcePathIdentity(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + path.Clean(u.Path)
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/><D:getcontentlength/></D:prop></D:propfind>`

type davMember struct {
	name string
	size int64
	url  string
}

// parseWebDAVMultistatus extracts non-collection, video-extension members.
// Bounded by member count; a member missing a resolvable href, escaping the
// declared collection, or whose extension is unrecognized is dropped, never
// fatal to siblings.
func parseWebDAVMultistatus(body []byte, base *url.URL) ([]davMember, error) {
	var doc davMultistatus
	dec := xml.NewDecoder(strings_NewReader(body))
	dec.Strict = true
	if err := dec.Decode(&doc); err != nil {
		return nil, httpstream.NewError(httpstream.ClassUpstreamMalformed, "webdav response is not valid XML")
	}
	var out []davMember
	for _, r := range doc.Responses {
		if len(out) >= maxWebDAVMembers {
			break
		}
		href := strings.TrimSpace(r.Href)
		if href == "" {
			continue
		}
		isCollection := false
		var size int64
		for _, ps := range r.Propstat {
			if ps.Prop.ResourceType.Collection != nil {
				isCollection = true
			}
			if ps.Prop.ContentLength > 0 {
				size = ps.Prop.ContentLength
			}
		}
		if isCollection {
			continue
		}
		resolved := resolveWebDAVMember(base, href)
		if resolved == nil {
			continue
		}
		ext := strings.ToLower(path.Ext(resolved.Path))
		if !videoExt[ext] {
			continue
		}
		out = append(out, davMember{name: path.Base(resolved.Path), size: size, url: resolved.String()})
	}
	return out, nil
}

// resolveWebDAVMember confines server-returned hrefs to the operator-declared
// collection. A WebDAV listing is data, not fresh authority to select another
// origin or another path on the configured backend.
func resolveWebDAVMember(base *url.URL, href string) *url.URL {
	if base == nil {
		return nil
	}
	collection := *base
	if collection.Path == "" {
		collection.Path = "/"
	}
	if !strings.HasSuffix(collection.Path, "/") {
		collection.Path += "/"
		collection.RawPath = ""
	}

	ref, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return nil
	}
	resolved := collection.ResolveReference(ref)
	if _, err := httpstream.ValidateURL(resolved.String()); err != nil {
		return nil
	}
	if !strings.EqualFold(collection.Scheme, resolved.Scheme) ||
		!strings.EqualFold(collection.Host, resolved.Host) {
		return nil
	}

	rootPath, err := url.PathUnescape(collection.EscapedPath())
	if err != nil {
		return nil
	}
	memberPath, err := url.PathUnescape(resolved.EscapedPath())
	if err != nil {
		return nil
	}
	rootPath = path.Clean(rootPath)
	if rootPath != "/" {
		rootPath += "/"
	}
	if !strings.HasPrefix(path.Clean(memberPath), rootPath) {
		return nil
	}
	return resolved
}

func strings_NewReaderString(s string) io.Reader { return strings.NewReader(s) }

var _ httpstream.Handler = (*Handler)(nil)
