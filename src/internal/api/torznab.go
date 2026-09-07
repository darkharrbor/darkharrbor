package api

//
// Add Dark Harrbor as a Generic Torznab indexer in Prowlarr:
//   URL:      http://darkharrbor:8381/torznab
//   API Key:  (leave blank — internal arr-net, no auth needed)
//   Priority: 1 (highest) — cache-first before other indexers.
//
// How it works:
//   1. Prowlarr sends a tvsearch/movie request with imdbid or tvdbid.
//   2. We query TorBox's search API: GET /torrents/{id_type}:{id}
//      This returns all known torrents for that title (up to ~900).
//   3. We filter by season/episode from title_parsed_data.
//   4. We batch the candidate hashes through TorBox checkcached.
//   5. Only cached hashes are returned as Torznab results.
//
// The cached_only=true query param on the TorBox search API is currently
// broken server-side (500). We do client-side filtering via checkcached
// instead — same net result, more reliable.
//
// When TorBox search returns no results (title not indexed), we return an
// empty feed. Sonarr falls back to other indexers gracefully.

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

// nowUTC returns the server clock (injectable for deterministic tests).
func (s *Server) nowUTC() time.Time {
	if s.nowFn != nil {
		return s.nowFn().UTC()
	}
	return time.Now().UTC()
}

type torznabCacheEntry struct {
	items   []torznabItem
	expires time.Time
}

// Register /torznab and /torznab/ routes. Called from Router().
// /torznab/torrent-only/... and /torznab/usenet-only/... are handled by the
// same catch-all (protocolOnlyFromPath inspects the path) — see
// protocolOnlyFromPath for why the split exists.
func (s *Server) registerTorznab(mux *http.ServeMux) {
	mux.HandleFunc("/torznab", s.handleTorznab)
	mux.HandleFunc("/torznab/", s.handleTorznab)
	mux.HandleFunc("/getnzb/", s.handleGetNZB)
}

func (s *Server) handleTorznab(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	t := strings.ToLower(strings.TrimSpace(q.Get("t")))
	protoOnly := protocolOnlyFromPath(r.URL.Path)
	switch t {
	case "caps":
		s.torznabCaps(w)
	case "movie":
		s.torznabSearch(w, r, "movie", protoOnly)
	case "tvsearch":
		s.torznabSearch(w, r, "tv", protoOnly)
	case "search":
		// Generic text search: t does not say movie vs tv, so derive it from
		// the requested categories (Radarr sends 2xxx, Sonarr 5xxx).
		s.torznabSearch(w, r, searchTypeFromCats(q.Get("cat")), protoOnly)
	default:
		s.torznabCaps(w)
	}
}

// protocolOnlyFromPath returns "torrent", "usenet", or "" (both — the
// original combined-feed behavior) based on a path segment in the request.
//
// Sonarr/Radarr's Torznab and Newznab indexer types are PROTOCOL-LOCKED:
// every item parsed from a given indexer registration is force-tagged with
// that indexer's configured protocol, regardless of the item's actual
// enclosure type in the feed XML (confirmed live 2026-07-01 — see
// HANDOFF.md). A single combined torrent+NZB feed registered once therefore
// silently mislabels roughly half its items, and registering it TWICE (once
// per protocol) causes the arr to see the same releases duplicated under
// both protocols and de-dupe down to whichever indexer entry it evaluates
// first — usually losing the NZB lane entirely.
//
// The fix: DH exposes the SAME search/caps logic under two additional path
// prefixes, /torznab/torrent-only and /torznab/usenet-only (plus the
// original unfiltered /torznab, kept for back-compat / Prowlarr's own
// meta-indexer fan-out, which does not have this problem). Register DH
// TWICE in each arr — a Torznab indexer pointed at .../torznab/torrent-only
// and a Newznab indexer pointed at .../torznab/usenet-only — so each
// registration's feed contains only the protocol it claims to be.
func protocolOnlyFromPath(path string) string {
	switch {
	case strings.Contains(path, "/torznab/torrent-only"):
		return "torrent"
	case strings.Contains(path, "/torznab/usenet-only"):
		return "usenet"
	default:
		return ""
	}
}

// searchTypeFromCats returns "movie" when any Newznab movie category
// (2000-2999) is requested, otherwise "tv".
func searchTypeFromCats(cat string) string {
	for _, c := range strings.Split(cat, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(c))
		if err != nil {
			continue
		}
		if n >= 2000 && n < 3000 {
			return "movie"
		}
	}
	return "tv"
}

// ─── Capabilities ─────────────────────────────────────────────────────────────

func (s *Server) torznabCaps(w http.ResponseWriter) {
	type attrSearch struct {
		Available       string `xml:"available,attr"`
		SupportedParams string `xml:"supportedParams,attr"`
	}
	type capsSearching struct {
		Search      attrSearch `xml:"search"`
		TVSearch    attrSearch `xml:"tv-search"`
		MovieSearch attrSearch `xml:"movie-search"`
	}
	type capsCategory struct {
		ID   string `xml:"id,attr"`
		Name string `xml:"name,attr"`
	}
	type capsCategories struct {
		Category []capsCategory `xml:"category"`
	}
	type capsServer struct {
		Version string `xml:"version,attr"`
		Title   string `xml:"title,attr"`
		URL     string `xml:"url,attr"`
	}
	type caps struct {
		XMLName    xml.Name       `xml:"caps"`
		Server     capsServer     `xml:"server"`
		Searching  capsSearching  `xml:"searching"`
		Categories capsCategories `xml:"categories"`
	}

	out := caps{
		Server: capsServer{
			Version: "1.1",
			Title:   "Dark Harrbor – TorBox Cache",
			URL:     s.cfg.Server.BaseURL + "/torznab",
		},
		Searching: capsSearching{
			Search: attrSearch{Available: "yes", SupportedParams: "q"},
			// DH does not resolve media IDs to titles, so it advertises only
			// text search; the arrs then send the series/movie title as q=,
			// which DH fans to Prowlarr's (text-search) indexers.
			//
			// Do NOT advertise tvdbid/imdbid here without an ID→title
			// resolution layer. When an arr sees ID caps it sends ID-ONLY
			// queries (no q); DH forwards them to Prowlarr, whose text-only
			// indexers (EZTV, RuTracker, zilean, Bitmagnet) degrade to a
			// newest-releases junk dump. Found live 2026-07-08: the caps
			// change from d13e22d was masked by the arrs' in-memory caps
			// cache — every gate ran on stale q-based caps — and surfaced
			// only when sonarr-classic restarted: season searches returned
			// 699 junk candidates, 0 downloads, and the multi-season pack
			// synthesis never fired.
			TVSearch:    attrSearch{Available: "yes", SupportedParams: "q,season,ep"},
			MovieSearch: attrSearch{Available: "yes", SupportedParams: "q"},
		},
		Categories: capsCategories{
			Category: []capsCategory{
				{ID: "5000", Name: "TV"},
				{ID: "5040", Name: "TV/HD"},
				{ID: "5045", Name: "TV/UHD"},
				{ID: "2000", Name: "Movies"},
				{ID: "2040", Name: "Movies/HD"},
				{ID: "2045", Name: "Movies/UHD"},
			},
		},
	}
	writeTorznabXML(w, &out)
}

// ─── Search ───────────────────────────────────────────────────────────────────

func (s *Server) torznabSearch(w http.ResponseWriter, r *http.Request, searchType string, protoOnly string) {
	q := r.URL.Query()
	offset, limit := torznabWindow(q)

	// Arr RSS-window sync (HR0.3): a search with no query text, no media IDs,
	// and no season/episode selector is the arr's periodic RSS window fetch —
	// and the arr's indexer test-search, which has the same shape. DH is a
	// fulfillment/selection proxy, not a discovery indexer — it has no
	// recent-releases corpus of its own — so the window is answered with one
	// deterministic connectivity-sentinel item. Live finding 2026-07-17:
	// Sonarr blocks indexer CREATION with a severity-error validation
	// ("no results in the configured categories") even under forceSave=true,
	// so a fully empty window feed makes DH indexers impossible to register
	// via the API. The sentinel's fixed 2015 pubDate predates any lastSync,
	// so the arr's RSS window is fully covered on page one with zero coverage
	// warnings, and its unparseable title never matches media. No Prowlarr
	// fan-out or per-interval eligibility load is generated.
	if isRSSWindowQuery(q) {
		s.writeTorznabFeed(r.Context(), w, windowSentinelItems(protoOnly, searchType, offset), offset, 1)
		return
	}

	cacheKey := protoOnly + "|" + torznabCacheKey(searchType, q)
	if cachedItems, ok := s.torznabCacheGet(cacheKey); ok {
		page, total := pageItems(cachedItems, offset, limit)
		s.writeTorznabFeed(r.Context(), w, page, offset, total)
		return
	}

	var items []torznabItem
	if s.cfg.SelectionMode() && s.prowlarr != nil {
		// Cache-aware SELECTION: fan to the user's Prowlarr indexers and return
		// only releases eligible under the preference (see selection.go).
		items = s.selectionSearch(r, q, searchType).items
	}
	// Otherwise FULFILLMENT-ONLY: DH is not a discovery indexer; return an empty
	// feed so the arr uses its other indexers. (The legacy TorBox search-api path
	// below is SUPERSEDED — that endpoint is access-gated; functions retained
	// pending removal.)

	items = filterByProtocol(items, protoOnly)
	sortItemsByPubDate(items)

	s.torznabCacheSet(cacheKey, items)
	page, total := pageItems(items, offset, limit)
	s.writeTorznabFeed(r.Context(), w, page, offset, total)
}

// isRSSWindowQuery reports whether the request is a parameterless window
// fetch (arr RSS sync / test search) rather than a targeted search.
func isRSSWindowQuery(q url.Values) bool {
	for _, k := range []string{"q", "imdbid", "tvdbid", "tmdbid", "rid", "season", "ep"} {
		if strings.TrimSpace(q.Get(k)) != "" {
			return false
		}
	}
	return true
}

// torznabWindow parses offset/limit with newznab defaults. Both DH feeds
// honor offset paging over the (cache-stable, pubDate-sorted) result set so
// arr paging is deterministic within the feed cache TTL.
func torznabWindow(q url.Values) (offset, limit int) {
	offset = atoiClamp(q.Get("offset"), 0, 0, 1<<20)
	limit = atoiClamp(q.Get("limit"), torznabDefaultLimit, 1, torznabMaxLimit)
	return offset, limit
}

const (
	torznabDefaultLimit = 100
	torznabMaxLimit     = 1000
)

func atoiClamp(s string, def, min, max int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// pageItems slices one page out of the full sorted result set.
func pageItems(items []torznabItem, offset, limit int) (page []torznabItem, total int) {
	total = len(items)
	if offset >= total {
		return nil, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return items[offset:end], total
}

// sortItemsByPubDate orders items newest-first with a deterministic
// tiebreak (GUID, then title) so repeated fetches and offset pages present a
// stable ordering to the arr's RSS-window logic.
func sortItemsByPubDate(items []torznabItem) {
	parse := func(v string) time.Time {
		t, err := time.Parse(time.RFC1123Z, v)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	sort.SliceStable(items, func(i, j int) bool {
		ti, tj := parse(items[i].PubDate), parse(items[j].PubDate)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		gi, gj := itemGUID(items[i]), itemGUID(items[j])
		if gi != gj {
			return gi < gj
		}
		return items[i].Title < items[j].Title
	})
}

// sentinelPubDate is a fixed instant far in the past: every arr RSS window
// [lastSync, now) trivially covers it, so window fetches complete on page
// one with no paging and no coverage warning, and the item never looks new.
const sentinelPubDate = "Thu, 01 Jan 2015 00:00:00 +0000"

// windowSentinelItems returns the single connectivity-sentinel release for a
// window fetch's first page (offset 0); later pages are empty with total=1.
// The sentinel proves the feed answers with a categorized, parseable ITEM —
// which the arr's save-time indexer validation requires — while remaining
// inert: the title cannot parse as any episode or movie, it is served only
// on window queries (never targeted searches, so it cannot be grabbed), and
// its enclosure points at a DH URL that would simply 404 if ever fetched.
func windowSentinelItems(protoOnly, searchType string, offset int) []torznabItem {
	if offset > 0 {
		return nil
	}
	category := "5000"
	if searchType == "movie" {
		category = "2000"
	}
	if protoOnly == "usenet" {
		return []torznabItem{{
			Title:    "DarkHarrbor.Feed.Sentinel.Usenet",
			Size:     1 << 20,
			Category: category,
			PubDate:  sentinelPubDate,
			Protocol: "usenet",
			NZBToken: "feed-sentinel",
		}}
	}
	return []torznabItem{{
		Title:    "DarkHarrbor.Feed.Sentinel.Torrent",
		InfoHash: "feedfacefeedfacefeedfacefeedface0000da4b",
		Size:     1 << 20,
		Category: category,
		PubDate:  sentinelPubDate,
		Protocol: "torrent",
	}}
}

func itemGUID(it torznabItem) string {
	if it.Protocol == "usenet" {
		return it.NZBToken
	}
	return it.InfoHash
}

// filterByProtocol restricts items to one protocol when the arr's indexer
// registration requires it (see protocolOnlyFromPath). "" returns items
// unchanged — the original combined-feed behavior.
func filterByProtocol(items []torznabItem, protoOnly string) []torznabItem {
	if protoOnly == "" {
		return items
	}
	out := make([]torznabItem, 0, len(items))
	for _, it := range items {
		isUsenet := it.Protocol == "usenet"
		if (protoOnly == "usenet") == isUsenet {
			out = append(out, it)
		}
	}
	return out
}

// ─── Torznab item + XML rendering ────────────────────────────────────────────

type torznabItem struct {
	Title    string
	InfoHash string
	Size     int64
	Magnet   string
	Category string
	PubDate  string
	// Protocol is "torrent" (default) or "usenet". Usenet items render an
	// application/x-nzb enclosure pointing at DH's /getnzb proxy.
	Protocol string
	// NZBToken is the /getnzb proxy token for usenet items.
	NZBToken string
	// TVDBID/IMDBID/TMDBID are echoed back exactly as the arr supplied them
	// on the incoming request (never invented/looked up here) so the arr can
	// match a release to its library item by ID when the rendered title text
	// alone is not reliably parseable back to a known series/movie —
	// http-stream-only; torrent/usenet items never set these (HR0.2 live-gate
	// finding, 2026-07-18).
	TVDBID string
	IMDBID string
	TMDBID string
	// ProviderIdentity is captured from this Arr query and carried only
	// through DH's bounded search-to-grab context (ID-02).
	ProviderIdentity *store.ProviderIdentity
}

// torznabCacheKey builds a stable cache key from the search type and query
// parameters. searchType ("tv", "movie", "") must be included because the
// same title/IDs can be queried as both a TV search and a movie search (e.g.
// Sonarr and Radarr searching for the same show title), and the two result
// sets differ (different category filtering, season/ep vs no-ep). cat= is
// included for the same reason: a generic t=search with cat=5000 vs cat=2000
// produces different results via Prowlarr.
func torznabCacheKey(searchType string, q url.Values) string {
	return strings.Join([]string{
		searchType,
		q.Get("imdbid"), q.Get("tvdbid"), q.Get("tmdbid"),
		q.Get("season"), q.Get("ep"), q.Get("q"),
		q.Get("cat"),
	}, "|")
}

// torznabCacheGet returns cached items if the entry exists and has not expired.
func (s *Server) torznabCacheGet(key string) ([]torznabItem, bool) {
	s.torznabCacheMu.RLock()
	entry, ok := s.torznabCache[key]
	s.torznabCacheMu.RUnlock()
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.items, true
}

// torznabCacheMaxEntries is the hard cap on live cache entries. In practice
// the cache holds at most a handful of entries (a few arrs × a few queries ×
// two protocols), so this cap is never hit under normal load — it exists only
// to bound memory if something sends an unusual volume of distinct queries.
const torznabCacheMaxEntries = 200

// torznabCacheSet stores items under key with a 5-minute TTL.
// B-O8: if the map is at capacity, expired entries are evicted first; if it
// is still full, the entry with the earliest expiry is dropped.
func (s *Server) torznabCacheSet(key string, items []torznabItem) {
	s.torznabCacheMu.Lock()
	if len(s.torznabCache) >= torznabCacheMaxEntries {
		now := time.Now()
		for k, e := range s.torznabCache {
			if now.After(e.expires) {
				delete(s.torznabCache, k)
			}
		}
		if len(s.torznabCache) >= torznabCacheMaxEntries {
			// Evict the entry with the earliest (soonest-to-expire) deadline.
			var oldestKey string
			var oldestExp time.Time
			for k, e := range s.torznabCache {
				if oldestKey == "" || e.expires.Before(oldestExp) {
					oldestKey = k
					oldestExp = e.expires
				}
			}
			delete(s.torznabCache, oldestKey)
		}
	}
	s.torznabCache[key] = torznabCacheEntry{
		items:   items,
		expires: time.Now().Add(5 * time.Minute),
	}
	s.torznabCacheMu.Unlock()
}

func (s *Server) writeTorznabFeed(ctx context.Context, w http.ResponseWriter, items []torznabItem, offset, total int) {
	type tnAttr struct {
		XMLName xml.Name `xml:"torznab:attr"`
		Name    string   `xml:"name,attr"`
		Value   string   `xml:"value,attr"`
	}
	type tnEnclosure struct {
		XMLName xml.Name `xml:"enclosure"`
		URL     string   `xml:"url,attr"`
		Length  int64    `xml:"length,attr"`
		Type    string   `xml:"type,attr"`
	}
	type tnItem struct {
		XMLName   xml.Name `xml:"item"`
		Title     string   `xml:"title"`
		GUID      string   `xml:"guid"`
		PubDate   string   `xml:"pubDate"`
		Size      int64    `xml:"size"`
		Enclosure tnEnclosure
		Attrs     []tnAttr
	}
	type tnResponse struct {
		XMLName xml.Name `xml:"newznab:response"`
		Offset  int      `xml:"offset,attr"`
		Total   int      `xml:"total,attr"`
	}
	type tnChannel struct {
		XMLName  xml.Name `xml:"channel"`
		Title    string   `xml:"title"`
		Response tnResponse
		Items    []tnItem
	}
	type tnRSS struct {
		XMLName      xml.Name `xml:"rss"`
		Version      string   `xml:"version,attr"`
		XMLNS        string   `xml:"xmlns:torznab,attr"`
		XMLNSNewznab string   `xml:"xmlns:newznab,attr"`
		Channel      tnChannel
	}

	rss := tnRSS{
		Version:      "2.0",
		XMLNS:        "http://torznab.com/schemas/2015/feed",
		XMLNSNewznab: "http://www.newznab.com/DTD/2010/feeds/attributes/",
		Channel: tnChannel{
			Title:    "Dark Harrbor – TorBox Cache",
			Response: tnResponse{Offset: offset, Total: total},
		},
	}

	for _, it := range items {
		if it.Protocol == "usenet" {
			_ = s.persistGrabProviderIdentity(ctx, "nzb-token:"+it.NZBToken, it.ProviderIdentity)
			enclosureURL := strings.TrimRight(s.cfg.Server.BaseURL, "/") + "/getnzb/" + it.NZBToken
			attrs := []tnAttr{
				{Name: "category", Value: it.Category},
				{Name: "size", Value: strconv.FormatInt(it.Size, 10)},
			}
			if it.ProviderIdentity != nil {
				for _, value := range []struct{ name, id string }{
					{"tvdbid", it.ProviderIdentity.IDs.TVDB},
					{"tmdbid", it.ProviderIdentity.IDs.TMDB},
					{"imdbid", it.ProviderIdentity.IDs.IMDB},
				} {
					if value.id != "" {
						attrs = append(attrs, tnAttr{Name: value.name, Value: value.id})
					}
				}
			}
			rss.Channel.Items = append(rss.Channel.Items, tnItem{
				Title:   it.Title,
				GUID:    it.NZBToken,
				PubDate: it.PubDate,
				Size:    it.Size,
				Enclosure: tnEnclosure{
					URL:    enclosureURL,
					Length: it.Size,
					Type:   "application/x-nzb",
				},
				Attrs: attrs,
			})
			continue
		}
		magnetURL := it.Magnet
		if magnetURL == "" {
			magnetURL = "magnet:?xt=urn:btih:" + it.InfoHash
		}
		// Cache infohash -> full magnet so /download/ handler can redirect.
		s.magnetCacheSet(ctx, strings.ToLower(it.InfoHash), magnetURL, it.Title)
		_ = s.persistGrabProviderIdentity(ctx, "torrent:"+strings.ToLower(it.InfoHash), it.ProviderIdentity)
		// Serve a /download/ URL instead of raw magnet — Sonarr rejects
		// tracker-less magnets client-side before sending to qBit shim.
		downloadURL := s.cfg.Server.BaseURL + "/download/" + strings.ToLower(it.InfoHash)
		if identityID := providerIdentityContextID(it.ProviderIdentity); identityID != "" &&
			s.persistGrabProviderIdentity(ctx, "torrent-context:"+identityID, it.ProviderIdentity) {
			downloadURL += "?idc=" + identityID
		}
		attrs := []tnAttr{
			{Name: "category", Value: it.Category},
			{Name: "size", Value: strconv.FormatInt(it.Size, 10)},
			{Name: "infohash", Value: it.InfoHash},
			{Name: "seeders", Value: "1"},
			{Name: "peers", Value: "1"},
			{Name: "downloadvolumefactor", Value: "0"},
			{Name: "uploadvolumefactor", Value: "1"},
		}
		if it.ProviderIdentity != nil {
			for _, value := range []struct{ name, id string }{
				{"tvdbid", it.ProviderIdentity.IDs.TVDB},
				{"tmdbid", it.ProviderIdentity.IDs.TMDB},
				{"imdbid", it.ProviderIdentity.IDs.IMDB},
			} {
				if value.id != "" {
					attrs = append(attrs, tnAttr{Name: value.name, Value: value.id})
				}
			}
		}
		rss.Channel.Items = append(rss.Channel.Items, tnItem{
			Title:   it.Title,
			GUID:    it.InfoHash,
			PubDate: it.PubDate,
			Size:    it.Size,
			Enclosure: tnEnclosure{
				URL:    downloadURL,
				Length: it.Size,
				Type:   "application/x-bittorrent",
			},
			Attrs: attrs,
		})
	}

	writeTorznabXML(w, &rss)
}

func (s *Server) persistGrabProviderIdentity(ctx context.Context, key string, identity *store.ProviderIdentity) bool {
	if s.store == nil {
		return false
	}
	if identity == nil {
		keyKind, _, _ := strings.Cut(key, ":")
		s.log.Debug("provider identity context: nothing to persist for this render (upstream lookup found no identity)",
			"event", "identity_persist_nil", "key_kind", keyKind)
		return false
	}
	if err := s.store.UpsertGrabProviderIdentity(ctx, key, *identity); err != nil {
		s.log.Warn("provider identity context: durable write failed", "event", "identity_persist_write_failed", "error", err)
		return false
	}
	return true
}

func providerIdentityContextID(identity *store.ProviderIdentity) string {
	if identity == nil {
		return ""
	}
	body, err := json.Marshal(identity)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:12])
}

func writeTorznabXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(v)
}

func parseIntParam(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

// synthInfoHash derives a deterministic, unique synthetic infohash for one
// (real infohash, season) pair. Stable 40-hex; the real torrent reference is
// kept separately.
func synthInfoHash(realHash string, season int) string {
	sum := sha1.Sum([]byte(strings.ToLower(realHash) + "|s" + strconv.Itoa(season)))
	return hex.EncodeToString(sum[:])
}

var (
	multiSeasonRangeRe = regexp.MustCompile(`(?i)S\d{1,2}\s*-\s*S\d{1,2}`)
	completePackRe     = regexp.MustCompile(`(?i)[._ -]*\b(complete[._ -]*series|complete|full[._ -]*series)\b`)
	dupSeparatorRe     = regexp.MustCompile(`[._ ]{2,}`)
	sxxTokenRe         = regexp.MustCompile(`(?i)S\d{1,2}(E\d{1,3})?`)
)

// rewriteSeasonTitle turns a multi-season / complete-series raw title into a
// truthful single-season title Sonarr will parse, preserving resolution/
// source/group tokens. The range token (S01-S05) becomes the single season;
// complete/full-series markers are removed; a season token is appended if none
// survives.
func rewriteSeasonTitle(rawTitle string, season int) string {
	se := fmt.Sprintf("S%02d", season)
	out := multiSeasonRangeRe.ReplaceAllString(rawTitle, se)
	out = completePackRe.ReplaceAllString(out, "")
	out = dupSeparatorRe.ReplaceAllString(out, ".")
	out = strings.Trim(out, "._ -")
	if !sxxTokenRe.MatchString(out) {
		out = out + "." + se
	}
	return out
}
