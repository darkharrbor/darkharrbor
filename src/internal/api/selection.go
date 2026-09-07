package api

import (
	"context"
	"crypto/md5" // #nosec G501 -- TorBox usenet checkcached keys on MD5(NZB); not a security primitive
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/outcome"
	"github.com/darkharrbor/darkharrbor/internal/prowlarr"
	"github.com/darkharrbor/darkharrbor/internal/searchbudget"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/suppress"
	"github.com/darkharrbor/darkharrbor/internal/util"
)

// selectionSearch implements cache-aware SELECTION mode: DH fans the arr's
// search to the user's Prowlarr indexers (excluding DH itself), then returns
// only the releases that are ELIGIBLE under the user's preference. Torrents are
// cache-checked when uncached torrents are disallowed or truthfully deranked; NZBs are
// cache-checked (fetch+MD5+usenet checkcached) only when NNTP is disabled and
// TorBox-NZB is enabled by preference and provider capability — otherwise NNTP
// can stream any NZB, so no check is needed. The arr never sees an ineligible
// release, so it cannot grab one.
type selectionSearchResult struct {
	items   []torznabItem
	outcome searchbudget.Result
}

func (s *Server) selectionSearch(r *http.Request, q url.Values, searchType string) selectionSearchResult {
	ctx := r.Context()
	text := strings.TrimSpace(q.Get("q"))
	season := parseIntParam(q.Get("season"))
	ep := parseIntParam(q.Get("ep"))
	tvdbID := strings.TrimSpace(q.Get("tvdbid"))
	imdbID := strings.TrimSpace(q.Get("imdbid"))
	kind := "series"
	if searchType == "movie" {
		kind = "movie"
	}
	identity := s.providerIdentityForQuery(ctx, kind, text,
		q.Get("tvdbid"), q.Get("tmdbid"), q.Get("imdbid"))
	identity = scopeProviderIdentity(identity, season, ep)
	budgetKey, budgetable := searchbudget.CanonicalKey(identity, season, ep)
	var releaseBudget func()
	if budgetable && s.searchBudget != nil {
		allowed, release, budgetErr := s.searchBudget.Begin(ctx, budgetKey)
		if budgetErr != nil {
			s.log.Warn("selection: search budget admission failed", "error", httpstream.Sanitize(budgetErr))
			return selectionSearchResult{outcome: searchbudget.ResultTransient}
		} else if !allowed {
			release()
			return selectionSearchResult{outcome: searchbudget.ResultFiltered}
		}
		releaseBudget = release
		defer releaseBudget()
	}
	finish := func(result searchbudget.Result, items []torznabItem) selectionSearchResult {
		if budgetable && s.searchBudget != nil {
			if err := s.searchBudget.Record(ctx, budgetKey, result); err != nil {
				s.log.Warn("selection: search budget update failed", "error", httpstream.Sanitize(err))
			}
		}
		return selectionSearchResult{items: items, outcome: result}
	}

	ptype := "search"
	cats := []int{5000}
	if searchType == "movie" {
		ptype = "movie"
		cats = []int{2000}
	} else if season > 0 {
		// Use tvsearch when season is specified — indexers apply season/ep
		// filtering so results are season-specific rather than whole-series.
		ptype = "tvsearch"
	}

	indexerIDs, err := s.prowlarr.FanoutIndexerIDs(ctx, s.cfg.Server.BaseURL, s.cfg.Prowlarr.IndexerIDs)
	if err != nil {
		s.log.Warn("selection: resolve fan-out indexers failed", "error", httpstream.Sanitize(err))
		return finish(searchResultForError(err), nil)
	}
	if len(indexerIDs) == 0 {
		s.log.Debug("selection: no fan-out indexers besides DH")
		return finish(searchbudget.ResultFiltered, nil)
	}

	releases, err := s.prowlarr.Search(ctx, prowlarr.SearchParams{
		Query:      text,
		Type:       ptype,
		Categories: cats,
		IndexerIDs: indexerIDs,
		Limit:      200,
		Season:     season,
		Ep:         ep,
		TvdbID:     tvdbID,
		ImdbID:     imdbID,
	})
	if err != nil {
		s.log.Warn("selection: prowlarr search failed", "error", httpstream.Sanitize(err))
		return finish(searchResultForError(err), nil)
	}
	if len(releases) == 0 {
		return finish(searchbudget.ResultNoResult, nil)
	}
	if ptype == "tvsearch" {
		releases = filterSeriesTitleMatches(releases, text)
		if len(releases) == 0 {
			return finish(searchbudget.ResultFiltered, nil)
		}
	}

	var torrents, nzbs []prowlarr.Release
	for _, rel := range releases {
		if rel.IsUsenet() {
			nzbs = append(nzbs, rel)
		} else {
			torrents = append(torrents, rel)
		}
	}

	category := "5040"
	if searchType == "movie" {
		category = "2040"
	}
	// Fallback stamp for releases without a parseable indexer publish date.
	// Taken once per (cached) search so ordering stays stable across the
	// arr's paged fetches within the feed cache TTL (HR0.3 RSS-window rule).
	pubDate := s.nowUTC().Format(time.RFC1123Z)

	reqSeason := parseIntParam(q.Get("season"))

	items := make([]torznabItem, 0, len(releases))
	items = append(items, s.eligibleTorrents(r, torrents, category, pubDate, reqSeason)...)
	eligibleNZBs := s.eligibleNZBs(r, nzbs, category, pubDate, reqSeason)

	// COR-12: NZB tvsearch fallback. Some indexers (e.g. NZBgeek) ignore
	// season/ep params entirely and return recency-sorted results for any
	// tvsearch query, causing dedupSeasonFilterNZBs to discard everything.
	// When a tvsearch with season+ep yields zero eligible NZBs, retry with a
	// plain text search using "Title SxxExx" so indexers that honour text
	// queries but not structured TV params still surface the right release.
	if len(eligibleNZBs) == 0 && ptype == "tvsearch" && season > 0 && ep > 0 && text != "" {
		textQuery := fmt.Sprintf("%s S%02dE%02d", text, season, ep)
		fallbackReleases, ferr := s.prowlarr.Search(ctx, prowlarr.SearchParams{
			Query:      textQuery,
			Type:       "search",
			Categories: cats,
			IndexerIDs: indexerIDs,
			Limit:      200,
		})
		if ferr == nil && len(fallbackReleases) > 0 {
			fallbackReleases = filterSeriesTitleMatches(fallbackReleases, text)
			var fallbackNZBs []prowlarr.Release
			for _, rel := range fallbackReleases {
				if rel.IsUsenet() {
					fallbackNZBs = append(fallbackNZBs, rel)
				}
			}
			if len(fallbackNZBs) > 0 {
				eligibleNZBs = s.eligibleNZBs(r, fallbackNZBs, category, pubDate, reqSeason)
				s.log.Info("selection: NZB tvsearch fallback used plain-text query",
					"query", textQuery, "nzb_candidates", len(fallbackNZBs),
					"eligible", len(eligibleNZBs),
				)
			}
		}
	}
	items = append(items, eligibleNZBs...)
	for i := range items {
		items[i].ProviderIdentity = identity
	}

	s.log.Info("selection: search complete",
		"query", text, "type", searchType,
		"candidates", len(releases), "eligible", len(items),
	)
	if len(items) == 0 {
		return finish(searchbudget.ResultFiltered, nil)
	}
	return finish(searchbudget.ResultFound, items)
}

func searchResultForError(err error) searchbudget.Result {
	if outcome.Classify(err) == outcome.ClassAccountLevel {
		return searchbudget.ResultAccount
	}
	return searchbudget.ResultTransient
}

const uncachedTitleToken = "DH-UNCACHED"

// eligibleTorrents filters torrent candidates. When uncached torrents are
// disallowed, only hashes known cached at a configured provider survive.
// TS-5.2 combines fresh bounded cache-oracle answers with unexpired own-traffic
// observations for providers without an oracle. In derank mode authoritative
// misses survive with the stable DH-UNCACHED token; an unknown cache state
// never receives a misleading token.
// Multi-season pack titles are split into synthetic per-season releases using the
// same synthInfoHash + rewriteSeasonTitle machinery as the TorBox search path, so
// Sonarr can import each season individually regardless of which indexer sourced
// the pack. reqSeason 0 means no season filter (e.g. a plain search).
func (s *Server) eligibleTorrents(r *http.Request, torrents []prowlarr.Release, category, pubDate string, reqSeason int) []torznabItem {
	if !s.cfg.TorrentsEnabled() || len(torrents) == 0 {
		return nil
	}
	type tc struct {
		rel  prowlarr.Release
		hash string
	}
	cands := make([]tc, 0, len(torrents))
	seen := make(map[string]bool, len(torrents))
	for _, rel := range torrents {
		h := strings.ToLower(strings.TrimSpace(rel.InfoHash))
		if h == "" {
			h = extractInfoHash(rel.MagnetURL)
		}
		if h == "" {
			continue // no hash: cannot cache-check or resolve a grab
		}
		if seen[h] {
			continue // dedup: same torrent returned by multiple Prowlarr indexers
		}
		seen[h] = true
		if blacklisted, identity, blErr := isTorrentBlacklisted(r.Context(), s.store, h); blErr != nil {
			s.log.Warn("selection: torrent blacklist check failed (non-fatal, proceeding)",
				"event", "torrent_blacklist_check_failed",
				"info_hash", h,
				"error", blErr,
			)
		} else if blacklisted {
			s.log.Info("selection: excluded blacklisted torrent",
				"event", "torrent_selection_excluded",
				"source_type", store.SourceTypeTorrent,
				"info_hash", identity,
				"submitted_hash", h,
			)
			continue
		}
		// Season filter: when a specific season was requested, drop releases that
		// clearly belong to a different single season and are not a multi-season
		// pack covering it. Titles with no parseable season token are kept.
		if reqSeason > 0 {
			if sn := extractSingleSeason(rel.Title); sn != 0 && sn != reqSeason &&
				!(isMultiSeasonTitle(rel.Title) && seasonCoversRequest(rel.Title, reqSeason)) {
				continue
			}
		}
		cands = append(cands, tc{rel, h})
	}

	var cachedSet, checkedSet map[string]bool
	if !s.cfg.UncachedTorrentAllowed() || s.cfg.UncachedTorrentDeranked() {
		hashes := make([]string, 0, len(cands))
		for _, c := range cands {
			hashes = append(hashes, c.hash)
		}
		cachedSet, checkedSet = s.searchTorrentAvailability(r, hashes)
	}

	items := make([]torznabItem, 0, len(cands))
	for _, c := range cands {
		if checkedSet != nil && !checkedSet[c.hash] {
			continue
		}
		isCached := cachedSet != nil && cachedSet[c.hash]
		if !s.cfg.UncachedTorrentAllowed() && !isCached {
			continue
		}
		if s.isReleaseSuppressed(r.Context(), c.rel.Title, c.rel.Size, suppress.LaneTorrent) {
			continue
		}
		magnet := c.rel.MagnetURL
		if magnet == "" {
			magnet = "magnet:?xt=urn:btih:" + c.hash
		}
		// Multi-season pack detection: if the title looks like a season pack
		// (S01-S05, "Complete Series", etc.) and a specific season was requested,
		// emit a synthetic per-season release so Sonarr can import it cleanly.
		// Uses synthInfoHash + rewriteSeasonTitle from torznab.go.
		pd := stablePubDate(c.rel.PublishDate, pubDate)
		if reqSeason > 0 && isMultiSeasonTitle(c.rel.Title) && seasonCoversRequest(c.rel.Title, reqSeason) {
			synthHash := synthInfoHash(c.hash, reqSeason)
			synthTitle := rewriteSeasonTitle(c.rel.Title, reqSeason)
			if s.cfg.UncachedTorrentDeranked() && !isCached {
				synthTitle = tagUncachedTitle(synthTitle)
			}
			synthMagnet := "magnet:?xt=urn:btih:" + synthHash
			items = append(items, torznabItem{
				Title: synthTitle, InfoHash: synthHash, Size: c.rel.Size,
				Magnet: synthMagnet, Category: category, PubDate: pd, Protocol: "torrent",
			})
			sr := store.SyntheticRelease{
				SynthHash:    synthHash,
				RealInfohash: c.hash,
				Magnet:       magnet,
				Season:       reqSeason,
			}
			if err := s.store.UpsertSyntheticRelease(r.Context(), sr); err != nil {
				s.log.Warn("selection: persist synthetic mapping failed",
					"synth_hash", synthHash, "error", err)
			}
			continue
		}
		title := c.rel.Title
		if s.cfg.UncachedTorrentDeranked() && !isCached {
			title = tagUncachedTitle(title)
		}
		items = append(items, torznabItem{
			Title: title, InfoHash: c.hash, Size: c.rel.Size,
			Magnet: magnet, Category: category, PubDate: pd, Protocol: "torrent",
		})
	}
	return items
}

func tagUncachedTitle(title string) string {
	title = strings.TrimSpace(title)
	if strings.Contains(strings.ToUpper(title), uncachedTitleToken) {
		return title
	}
	if title == "" {
		return uncachedTitleToken
	}
	return title + "." + uncachedTitleToken
}

// stablePubDate converts a Prowlarr release publishDate (RFC 3339) to the
// feed's RFC 1123Z form. Unparseable/absent dates use the search-stable
// fallback rather than a fresh per-request timestamp, so the arr's RSS-window
// ordering check never sees an item's date drift between fetches.
func stablePubDate(publishDate, fallback string) string {
	pd := strings.TrimSpace(publishDate)
	if pd == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, pd)
	if err != nil {
		return fallback
	}
	return t.UTC().Format(time.RFC1123Z)
}

// isMultiSeasonTitle reports whether a release title looks like a genuine
// multi-season pack. Explicit season ranges (S01-S05) and complete-series
// markers (COMPLETE SERIES / FULL SERIES) qualify. Plain "Sxx.COMPLETE"
// titles are per-season completeness markers and do NOT qualify — they
// must not be split into synthetic releases for unrelated seasons.
func isMultiSeasonTitle(title string) bool {
	if seasonRangeRe.MatchString(title) {
		return true
	}
	return completeSeriesRe.MatchString(title)
}

var (
	// seasonRangeRe matches an explicit season range like S01-S05.
	seasonRangeRe = regexp.MustCompile(`(?i)S\d{1,2}\s*[-]\s*S\d{1,2}`)

	// seasonRangeCaptureRe captures both season numbers from a range like S01-S05.
	// B-O7: hoisted from inline MustCompile inside seasonCoversRequest.
	seasonRangeCaptureRe = regexp.MustCompile(`(?i)S(\d{1,2})\s*[-]\s*S(\d{1,2})`)

	// completeSeriesRe matches "COMPLETE SERIES" or "FULL SERIES" only.
	// Bare "COMPLETE" without "series" is NOT a multi-season indicator.
	completeSeriesRe = regexp.MustCompile(`(?i)[._ -]*\b(complete[._ -]+series|full[._ -]+series)\b`)

	// singleSeasonRe captures the first Sxx token from a title.
	singleSeasonRe = regexp.MustCompile(`(?i)S(\d{1,2})`)
)

// extractSingleSeason returns the season number from the first Sxx token
// in the title, or 0 if none found.
func extractSingleSeason(title string) int {
	m := singleSeasonRe.FindStringSubmatch(title)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// seriesReleaseMatchesQuery rejects a season-bearing release when its title
// names a different series. Opaque titles abstain: aliases cannot be inferred
// safely here, while a parseable contradiction must not inherit the queried
// series' provider identity.
func seriesReleaseMatchesQuery(query, releaseTitle string) bool {
	query = normalizeSeriesSearchTitle(query)
	if query == "" {
		return true
	}
	seasonAt := singleSeasonRe.FindStringIndex(releaseTitle)
	if seasonAt == nil {
		return true
	}
	candidate := normalizeSeriesSearchTitle(releaseTitle[:seasonAt[0]])
	return candidate == "" || candidate == query
}

func normalizeSeriesSearchTitle(title string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

func filterSeriesTitleMatches(releases []prowlarr.Release, query string) []prowlarr.Release {
	out := make([]prowlarr.Release, 0, len(releases))
	for _, rel := range releases {
		if seriesReleaseMatchesQuery(query, rel.Title) {
			out = append(out, rel)
		}
	}
	return out
}

// seasonCoversRequest reports whether a title genuinely covers reqSeason.
// For explicit range titles, reqSeason must fall within the range. For
// complete-series titles (COMPLETE SERIES / FULL SERIES), any season qualifies.
// Returns false for titles that isMultiSeasonTitle would reject.
func seasonCoversRequest(title string, reqSeason int) bool {
	if seasonRangeRe.MatchString(title) {
		parts := seasonRangeCaptureRe.FindStringSubmatch(title)
		if len(parts) < 3 {
			return false
		}
		lo, _ := strconv.Atoi(parts[1])
		hi, _ := strconv.Atoi(parts[2])
		if lo > hi {
			lo, hi = hi, lo
		}
		return reqSeason >= lo && reqSeason <= hi
	}
	if completeSeriesRe.MatchString(title) {
		return true
	}
	return false
}

// dedupSeasonFilterNZBs removes duplicate NZB releases (by normalized title) and,
// when a specific season was requested, drops releases that clearly belong to a
// different single season and are not a multi-season pack covering it. NNTP can
// stream anything, so without this an S02 search returns every season's NZBs.
func dedupSeasonFilterNZBs(nzbs []prowlarr.Release, reqSeason int) []prowlarr.Release {
	if len(nzbs) == 0 {
		return nzbs
	}
	out := make([]prowlarr.Release, 0, len(nzbs))
	seen := make(map[string]bool, len(nzbs))
	for _, rel := range nzbs {
		key := normalizeReleaseTitle(rel.Title)
		if key != "" && seen[key] {
			continue
		}
		seen[key] = true
		if reqSeason > 0 {
			if sn := extractSingleSeason(rel.Title); sn != 0 && sn != reqSeason &&
				!(isMultiSeasonTitle(rel.Title) && seasonCoversRequest(rel.Title, reqSeason)) {
				continue
			}
		}
		out = append(out, rel)
	}
	return out
}

// normalizeReleaseTitle lowercases and collapses separators so the same release
// from different indexers compares equal for dedup purposes.
func normalizeReleaseTitle(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	t = strings.ReplaceAll(t, ".", " ")
	t = strings.ReplaceAll(t, "_", " ")
	return strings.Join(strings.Fields(t), " ")
}

// eligibleNZBs filters NZB candidates. If NNTP is enabled every NZB is servable
// (NNTP streams anything) so all pass with no checkcached. Only when NNTP is
// disabled AND TorBox-NZB is enabled by both preference and provider capability
// does DH cache-check (fetch the .nzb, MD5, usenet checkcached) — the costly path
// the user explicitly opted into.
func (s *Server) eligibleNZBs(r *http.Request, nzbs []prowlarr.Release, category, pubDate string, reqSeason int) []torznabItem {
	if !s.cfg.NZBEnabled() || len(nzbs) == 0 {
		return nil
	}
	torboxNZBUsable := s.cfg.NZBViaTorBox() && providerCaps(s.prov).UsenetIngest
	if !s.cfg.NZBViaNNTP() && !torboxNZBUsable {
		return nil
	}
	nzbs = dedupSeasonFilterNZBs(nzbs, reqSeason)
	cacheCheck := torboxNZBUsable && !s.cfg.NZBViaNNTP()
	if !cacheCheck {
		items := make([]torznabItem, 0, len(nzbs))
		for _, rel := range nzbs {
			if s.isReleaseSuppressed(r.Context(), rel.Title, rel.Size, suppress.LaneNZB) {
				continue
			}
			items = append(items, s.nzbItem(rel, category, pubDate))
		}
		return items
	}

	// Cached-only NZB: fetch + MD5 + usenet checkcached. Bounded; TorBox calls
	// are throttled by the shared rate-limiter (the governor of API cost).
	const maxCheck = 50
	type nc struct {
		rel  prowlarr.Release
		hash string
	}
	cands := make([]nc, 0, len(nzbs))
	for i, rel := range nzbs {
		if i >= maxCheck {
			break
		}
		h, err := s.fetchNZBMD5(r.Context(), rel.DownloadURL)
		if err != nil || h == "" {
			s.log.Debug("selection: nzb hash fetch failed (skip)", "error", httpstream.Sanitize(err))
			continue
		}
		cands = append(cands, nc{rel, h})
	}
	hashes := make([]string, 0, len(cands))
	for _, c := range cands {
		hashes = append(hashes, c.hash)
	}
	cachedSet := s.batchCheckcachedUsenet(r, hashes)
	items := make([]torznabItem, 0, len(cands))
	for _, c := range cands {
		if !cachedSet[c.hash] {
			continue
		}
		if s.isReleaseSuppressed(r.Context(), c.rel.Title, c.rel.Size, suppress.LaneNZB) {
			continue
		}
		items = append(items, s.nzbItem(c.rel, category, pubDate))
	}
	return items
}

// nzbItem builds a feed item for an NZB and registers a proxy token so the arr
// downloads the .nzb from DH (which adds the Prowlarr API key) rather than from
// Prowlarr directly (the arr has no Prowlarr key in selection mode).
//
// NS-9.1: search-time retention annotation. When at least one configured
// usenet provider (NS-8.4's HARRBOR_PROVIDER_<NAME>_RETENTION_DAYS) has a
// known retention horizon, and this NZB's Prowlarr publish date is older
// than the deepest configured horizon, the title is tagged with the same
// stable DH-UNCACHED token TS-0.4 already registered an Arr custom format
// for -- reusing that existing derank machinery rather than adding a new
// one. No configured horizon, or an absent/unparseable publish date, always
// abstains (never guesses a release is too old).
func (s *Server) nzbItem(rel prowlarr.Release, category, pubDate string) torznabItem {
	token := util.SHA1Hex(rel.GUID + "|" + rel.DownloadURL)
	s.nzbProxySet(token, rel.DownloadURL)
	title := rel.Title
	if s.nzbExceedsDeepestRetention(rel.PublishDate) {
		title = tagUncachedTitle(title)
	}
	return torznabItem{
		Title: title, Size: rel.Size, Category: category,
		PubDate:  stablePubDate(rel.PublishDate, pubDate),
		Protocol: "usenet", NZBToken: token,
	}
}

// nzbExceedsDeepestRetention reports whether an NZB's Prowlarr publish date
// is older than the deepest currently configured provider retention
// horizon. Abstains (returns false) when no horizon is configured for any
// configured usenet provider, or when publishDate is empty/unparseable --
// mirrors stablePubDate's own RFC3339 parsing so the two never disagree on
// what counts as a valid date.
func (s *Server) nzbExceedsDeepestRetention(publishDate string) bool {
	deepest, ok := config.DeepestProviderRetentionDays(s.cfg, s.usenetOrder, s.log)
	if !ok {
		return false
	}
	pd := strings.TrimSpace(publishDate)
	if pd == "" {
		return false
	}
	posted, err := time.Parse(time.RFC3339, pd)
	if err != nil {
		return false
	}
	ageDays := int(s.nowUTC().Sub(posted).Hours() / 24)
	return ageDays > deepest
}

// batchCheckcachedUsenet checks a slice of NZB content MD5s against TorBox's
// usenet cache (50/call, format=object) and returns the cached set.
func (s *Server) batchCheckcachedUsenet(r *http.Request, hashes []string) map[string]bool {
	cached := make(map[string]bool)
	const batchSize = 50
	for i := 0; i < len(hashes); i += batchSize {
		end := i + batchSize
		if end > len(hashes) {
			end = len(hashes)
		}
		batch := hashes[i:end]
		reqURL := fmt.Sprintf("%s/api/usenet/checkcached?hash=%s&format=object",
			strings.TrimRight(s.cfg.TorBox.BaseURL, "/"),
			url.QueryEscape(strings.Join(batch, ",")),
		)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, reqURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+s.cfg.TorBox.APIToken)
		req.Header.Set("User-Agent", s.cfg.TorBox.UserAgent)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			s.log.Warn("selection: usenet checkcached batch failed", "error", httpstream.Sanitize(err))
			continue
		}
		var envelope struct {
			Success bool           `json:"success"`
			Data    map[string]any `json:"data"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&envelope)
		_ = resp.Body.Close()
		if decErr != nil {
			continue
		}
		if envelope.Success && envelope.Data != nil {
			for _, h := range batch {
				if _, ok := envelope.Data[h]; ok {
					cached[h] = true
				}
			}
		}
	}
	return cached
}

// fetchNZBMD5 fetches the .nzb bytes and returns their lowercase-hex MD5 — the
// key TorBox's usenet checkcached expects.
func (s *Server) fetchNZBMD5(ctx context.Context, dlURL string) (string, error) {
	body, err := s.fetchNZBBytes(ctx, dlURL)
	if err != nil {
		return "", err
	}
	if len(body) == 0 {
		return "", fmt.Errorf("empty nzb body")
	}
	sum := md5.Sum(body) // #nosec G401 -- MD5 mandated by the TorBox usenet checkcached API
	return hex.EncodeToString(sum[:]), nil
}

const maxNZBFetchBytes = 32 << 20

// fetchNZBBytes downloads a .nzb from an upstream URL, adding the Prowlarr API
// key when the URL targets the configured Prowlarr host. Body is capped at 32MB.
func (s *Server) fetchNZBBytes(ctx context.Context, dlURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, err
	}
	if sameSchemeAndHost(dlURL, s.cfg.Prowlarr.BaseURL) {
		req.Header.Set("X-Api-Key", s.cfg.Prowlarr.APIKey)
	}
	req.Header.Set("User-Agent", s.cfg.TorBox.UserAgent)
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("nzb fetch: too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("nzb fetch: unsupported redirect scheme")
			}
			req.Header.Del("X-Api-Key")
			if sameSchemeAndHost(req.URL.String(), s.cfg.Prowlarr.BaseURL) {
				req.Header.Set("X-Api-Key", s.cfg.Prowlarr.APIKey)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("nzb fetch status %d", resp.StatusCode)
	}
	if resp.ContentLength > maxNZBFetchBytes {
		return nil, fmt.Errorf("nzb fetch body exceeds %d bytes", maxNZBFetchBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNZBFetchBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxNZBFetchBytes {
		return nil, fmt.Errorf("nzb fetch body exceeds %d bytes", maxNZBFetchBytes)
	}
	return body, nil
}

// handleGetNZB serves GET /getnzb/{token}: looks up the upstream Prowlarr .nzb
// URL for the token and streams the NZB to the arr (with the Prowlarr key added
// server-side). This keeps the Prowlarr API key out of the arr.
func (s *Server) handleGetNZB(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/getnzb/")
	token = strings.TrimSpace(token)
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	dlURL, ok := s.nzbProxyGet(token)
	if !ok {
		http.Error(w, "unknown nzb token", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-nzb")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	body, err := s.fetchNZBBytes(r.Context(), dlURL)
	if err != nil {
		s.log.Warn("getnzb: upstream fetch failed", "error", err)
		http.Error(w, "upstream nzb fetch failed", http.StatusBadGateway)
		return
	}
	if identity, found, lookupErr := s.store.GetGrabProviderIdentity(r.Context(), "nzb-token:"+token); lookupErr == nil && found {
		_ = s.persistGrabProviderIdentity(r.Context(), nntp.ContentKey(body), identity)
	}
	_, _ = w.Write(body)
}

func (s *Server) nzbProxySet(token, dlURL string) {
	s.nzbProxyMu.Lock()
	if s.nzbProxyCache == nil {
		s.nzbProxyCache = make(map[string]string)
	}
	if _, exists := s.nzbProxyCache[token]; !exists {
		s.nzbProxyCacheOrder = append(s.nzbProxyCacheOrder, token)
	}
	s.nzbProxyCache[token] = dlURL
	trimInsertionCache(s.nzbProxyCache, &s.nzbProxyCacheOrder)
	s.nzbProxyMu.Unlock()
}

func (s *Server) nzbProxyGet(token string) (string, bool) {
	s.nzbProxyMu.RLock()
	v, ok := s.nzbProxyCache[token]
	s.nzbProxyMu.RUnlock()
	return v, ok
}
