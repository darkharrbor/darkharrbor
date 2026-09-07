package api

// HR0.3 RSS-window semantics for the torrent-only and usenet-only feeds:
// parameterless window fetches get a deterministic single-sentinel feed
// (fixed 2015 pubDate, total=1, echoed offset) and never fan out — the
// sentinel exists because Sonarr blocks indexer creation on a zero-result
// test search (live 2026-07-17); targeted-search results
// carry stable per-release pubDates, are ordered newest-first with a
// deterministic tiebreak, and honor offset/limit paging with a standard
// newznab:response element. The feed output never contains DH's own signed
// tok= values or upstream URLs (DG-04).

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

func TestIsRSSWindowQuery(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"t=search&cat=5030,5040&offset=0&limit=100", true},
		{"t=search", true},
		{"t=tvsearch&cat=5000&extended=1", true},
		{"t=search&q=yellowstone", false},
		{"t=tvsearch&q=yellowstone&season=5&ep=14", false},
		{"t=tvsearch&season=5", false},
		{"t=tvsearch&ep=2", false},
		{"t=movie&imdbid=tt0111161", false},
		{"t=tvsearch&tvdbid=1234", false},
		{"t=search&tmdbid=42", false},
		{"t=tvsearch&rid=9999", false},
		{"t=search&q=%20%20", true}, // whitespace-only q is still a window fetch
	}
	for _, c := range cases {
		if got := isRSSWindowQuery(mustParseQuery(c.query)); got != c.want {
			t.Errorf("isRSSWindowQuery(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

func TestTorznabWindowDefaultsAndClamping(t *testing.T) {
	cases := []struct {
		query      string
		wantOffset int
		wantLimit  int
	}{
		{"", 0, torznabDefaultLimit},
		{"offset=100&limit=50", 100, 50},
		{"offset=-5&limit=0", 0, 1},
		{"offset=junk&limit=junk", 0, torznabDefaultLimit},
		{"limit=999999", 0, torznabMaxLimit},
	}
	for _, c := range cases {
		off, lim := torznabWindow(mustParseQuery(c.query))
		if off != c.wantOffset || lim != c.wantLimit {
			t.Errorf("torznabWindow(%q) = (%d,%d), want (%d,%d)", c.query, off, lim, c.wantOffset, c.wantLimit)
		}
	}
}

func TestPageItems(t *testing.T) {
	items := make([]torznabItem, 5)
	for i := range items {
		items[i].Title = string(rune('a' + i))
	}
	page, total := pageItems(items, 0, 2)
	if total != 5 || len(page) != 2 || page[0].Title != "a" {
		t.Fatalf("page0 = %v total=%d", page, total)
	}
	page, total = pageItems(items, 4, 2)
	if total != 5 || len(page) != 1 || page[0].Title != "e" {
		t.Fatalf("tail page = %v total=%d", page, total)
	}
	page, total = pageItems(items, 10, 2)
	if total != 5 || page != nil {
		t.Fatalf("past-end page = %v total=%d", page, total)
	}
}

func TestSortItemsByPubDateStable(t *testing.T) {
	d := func(offsetMin int) string {
		return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC).
			Add(time.Duration(offsetMin) * time.Minute).Format(time.RFC1123Z)
	}
	items := []torznabItem{
		{Title: "old", InfoHash: "cc", PubDate: d(-60)},
		{Title: "new", InfoHash: "aa", PubDate: d(0)},
		{Title: "tie-b", InfoHash: "b2", PubDate: d(-30)},
		{Title: "tie-a", InfoHash: "b1", PubDate: d(-30)},
		{Title: "unparseable", InfoHash: "zz", PubDate: "not-a-date"},
	}
	sortItemsByPubDate(items)
	got := make([]string, 0, len(items))
	for _, it := range items {
		got = append(got, it.Title)
	}
	want := "new,tie-a,tie-b,old,unparseable"
	if strings.Join(got, ",") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
	// Re-sorting must not change the order (stability / determinism).
	sortItemsByPubDate(items)
	got2 := make([]string, 0, len(items))
	for _, it := range items {
		got2 = append(got2, it.Title)
	}
	if strings.Join(got2, ",") != want {
		t.Fatalf("second sort changed order: %v", got2)
	}
}

func TestStablePubDate(t *testing.T) {
	fallback := "Wed, 01 Jul 2026 12:00:00 +0000"
	if got := stablePubDate("2026-06-15T08:30:00Z", fallback); got != "Mon, 15 Jun 2026 08:30:00 +0000" {
		t.Fatalf("parsed = %q", got)
	}
	if got := stablePubDate("", fallback); got != fallback {
		t.Fatalf("empty fallback = %q", got)
	}
	if got := stablePubDate("junk", fallback); got != fallback {
		t.Fatalf("junk fallback = %q", got)
	}
}

func rssTestServer() *Server {
	cfg := &config.Config{}
	cfg.Server.BaseURL = "http://darkharrbor:8381"
	return &Server{
		cfg:          cfg,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		torznabCache: make(map[string]torznabCacheEntry),
		magnetCache:  make(map[string]magnetEntry),
	}
}

func TestRSSWindowReturnsSentinelFeed(t *testing.T) {
	cases := []struct {
		path         string
		wantTitle    string
		wantEncType  string
		wantCategory string
	}{
		{"/torznab/torrent-only/api?t=search&cat=5030,5040&offset=0&limit=100",
			"DarkHarrbor.Feed.Sentinel.Torrent", "application/x-bittorrent", ">5000<"},
		{"/torznab/usenet-only/api?t=search&cat=5030,5040&offset=0&limit=100",
			"DarkHarrbor.Feed.Sentinel.Usenet", "application/x-nzb", ">5000<"},
		{"/torznab/torrent-only/api?t=search&cat=2000&offset=0",
			"DarkHarrbor.Feed.Sentinel.Torrent", "application/x-bittorrent", ">2000<"},
	}
	for _, c := range cases {
		s := rssTestServer()
		rec := httptest.NewRecorder()
		s.handleTorznab(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", c.path, rec.Code)
		}
		body := rec.Body.String()
		if strings.Count(body, "<item>") != 1 || !strings.Contains(body, c.wantTitle) {
			t.Fatalf("%s: want exactly the sentinel item, got: %s", c.path, body)
		}
		if !strings.Contains(body, c.wantEncType) {
			t.Fatalf("%s: wrong enclosure type: %s", c.path, body)
		}
		if !strings.Contains(body, `total="1"`) || !strings.Contains(body, `offset="0"`) {
			t.Fatalf("%s: missing response attrs: %s", c.path, body)
		}
		if !strings.Contains(body, sentinelPubDate) {
			t.Fatalf("%s: sentinel pubDate not fixed: %s", c.path, body)
		}
		if len(s.torznabCache) != 0 {
			t.Fatalf("%s: window fetch polluted the search cache", c.path)
		}
		if strings.Contains(body, "tok=") {
			t.Fatalf("%s: signed token leaked into feed", c.path)
		}
	}
	// Later window pages are empty but keep total=1 and echo the offset, so
	// arr paging terminates deterministically.
	s := rssTestServer()
	rec := httptest.NewRecorder()
	s.handleTorznab(rec, httptest.NewRequest(http.MethodGet,
		"/torznab/torrent-only/api?t=search&offset=40", nil))
	body := rec.Body.String()
	if strings.Contains(body, "<item>") || !strings.Contains(body, `offset="40"`) || !strings.Contains(body, `total="1"`) {
		t.Fatalf("window page 2 wrong: %s", body)
	}
}

func TestHTTPStreamWindowReturnsSentinelWhileDark(t *testing.T) {
	// The http-stream indexer must be registrable (and RSS-coverable) even
	// while the feature is disabled / no backend exists.
	s := rssTestServer()
	rec := httptest.NewRecorder()
	s.handleTorznabHTTPStream(rec, httptest.NewRequest(http.MethodGet,
		"/torznab/http-stream?t=search&cat=5030,5040&offset=0&limit=100", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Count(body, "<item>") != 1 || !strings.Contains(body, "DarkHarrbor.Feed.Sentinel.HTTP") {
		t.Fatalf("want exactly the http sentinel: %s", body)
	}
	if !strings.Contains(body, sentinelPubDate) {
		t.Fatalf("sentinel pubDate not fixed: %s", body)
	}
	if len(s.magnetCache) != 0 {
		t.Fatal("http-stream sentinel touched magnetCache")
	}
	// Targeted searches keep the authoritative-empty dark behavior.
	rec = httptest.NewRecorder()
	s.handleTorznabHTTPStream(rec, httptest.NewRequest(http.MethodGet,
		"/torznab/http-stream?t=tvsearch&q=yellowstone&season=5&ep=14", nil))
	if strings.Contains(rec.Body.String(), "<item>") {
		t.Fatalf("dark targeted search returned items: %s", rec.Body.String())
	}
}

func TestFeedPagingOverCachedSortedResults(t *testing.T) {
	s := rssTestServer()
	d := func(offsetMin int) string {
		return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC).
			Add(time.Duration(offsetMin) * time.Minute).Format(time.RFC1123Z)
	}
	items := []torznabItem{
		{Title: "r-newest", InfoHash: "aaaa", PubDate: d(0)},
		{Title: "r-mid", InfoHash: "bbbb", PubDate: d(-10)},
		{Title: "r-oldest", InfoHash: "cccc", PubDate: d(-20)},
	}
	sortItemsByPubDate(items)
	s.torznabCacheSet("torrent|"+torznabCacheKey("tv", mustParseQuery("t=search&q=yellowstone")), items)

	fetch := func(query string) string {
		rec := httptest.NewRecorder()
		s.handleTorznab(rec, httptest.NewRequest(http.MethodGet,
			"/torznab/torrent-only/api?"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		return rec.Body.String()
	}

	p1 := fetch("t=search&q=yellowstone&offset=0&limit=2")
	if !strings.Contains(p1, "r-newest") || !strings.Contains(p1, "r-mid") || strings.Contains(p1, "r-oldest") {
		t.Fatalf("page1 wrong contents: %s", p1)
	}
	if !strings.Contains(p1, `offset="0"`) || !strings.Contains(p1, `total="3"`) {
		t.Fatalf("page1 missing response attrs: %s", p1)
	}
	if strings.Index(p1, "r-newest") > strings.Index(p1, "r-mid") {
		t.Fatalf("page1 not newest-first: %s", p1)
	}

	p2 := fetch("t=search&q=yellowstone&offset=2&limit=2")
	if !strings.Contains(p2, "r-oldest") || strings.Contains(p2, "r-newest") || strings.Contains(p2, "r-mid") {
		t.Fatalf("page2 wrong contents: %s", p2)
	}
	if !strings.Contains(p2, `offset="2"`) || !strings.Contains(p2, `total="3"`) {
		t.Fatalf("page2 missing response attrs: %s", p2)
	}

	// Pages must be identical on refetch (stable ordering within cache TTL).
	if again := fetch("t=search&q=yellowstone&offset=0&limit=2"); again != p1 {
		t.Fatal("page1 not stable across fetches")
	}
}
