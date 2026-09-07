package api

import (
	"net/url"
	"testing"
)

func mustParseQuery(s string) url.Values {
	q, err := url.ParseQuery(s)
	if err != nil {
		panic(err)
	}
	return q
}

func TestProtocolOnlyFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/torznab", ""},
		{"/torznab/", ""},
		{"/torznab/api", ""},
		{"/torznab/torrent-only/api", "torrent"},
		{"/torznab/usenet-only/api", "usenet"},
		{"/torznab/usenet-only", "usenet"},
	}
	for _, c := range cases {
		if got := protocolOnlyFromPath(c.path); got != c.want {
			t.Errorf("protocolOnlyFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestFilterByProtocol(t *testing.T) {
	items := []torznabItem{
		{Title: "torrent-a"}, // Protocol == "" (torrent)
		{Title: "torrent-b"}, // Protocol == "" (torrent)
		{Title: "nzb-a", Protocol: "usenet"},
		{Title: "nzb-b", Protocol: "usenet"},
	}

	both := filterByProtocol(items, "")
	if len(both) != 4 {
		t.Errorf("filterByProtocol(\"\") len = %d, want 4 (unfiltered)", len(both))
	}

	torrentOnly := filterByProtocol(items, "torrent")
	if len(torrentOnly) != 2 {
		t.Fatalf("filterByProtocol(torrent) len = %d, want 2", len(torrentOnly))
	}
	for _, it := range torrentOnly {
		if it.Protocol == "usenet" {
			t.Errorf("filterByProtocol(torrent) leaked a usenet item: %s", it.Title)
		}
	}

	usenetOnly := filterByProtocol(items, "usenet")
	if len(usenetOnly) != 2 {
		t.Fatalf("filterByProtocol(usenet) len = %d, want 2", len(usenetOnly))
	}
	for _, it := range usenetOnly {
		if it.Protocol != "usenet" {
			t.Errorf("filterByProtocol(usenet) leaked a torrent item: %s", it.Title)
		}
	}
}

func TestTorznabCacheKeyDistinctBySearchType(t *testing.T) {
	// A-B3: movie and TV searches with identical query params must produce
	// different cache keys so Sonarr and Radarr don't share results.
	cases := []struct {
		name        string
		searchTypeA string
		searchTypeB string
		q           string
	}{
		{"tv vs movie same title", "tv", "movie", "q=Breaking+Bad"},
		{"tv vs empty", "tv", "", "q=Succession"},
		{"movie vs empty", "movie", "", "q=Inception&cat=2000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := mustParseQuery(tc.q)
			keyA := torznabCacheKey(tc.searchTypeA, q)
			keyB := torznabCacheKey(tc.searchTypeB, q)
			if keyA == keyB {
				t.Errorf("expected distinct keys for searchType %q vs %q, both got %q",
					tc.searchTypeA, tc.searchTypeB, keyA)
			}
		})
	}
}

func TestTorznabCacheKeyDistinctByCat(t *testing.T) {
	// A-B3: same searchType but different cat= must produce different keys.
	qTV := mustParseQuery("q=The+Office&cat=5000")
	qMovie := mustParseQuery("q=The+Office&cat=2000")
	keyTV := torznabCacheKey("", qTV)
	keyMovie := torznabCacheKey("", qMovie)
	if keyTV == keyMovie {
		t.Errorf("expected distinct keys for cat=5000 vs cat=2000, both got %q", keyTV)
	}
}

func TestTorznabCacheKeyStable(t *testing.T) {
	// Same inputs must always produce the same key.
	q := mustParseQuery("q=Breaking+Bad&season=3&ep=1&tvdbid=81189")
	k1 := torznabCacheKey("tv", q)
	k2 := torznabCacheKey("tv", q)
	if k1 != k2 {
		t.Errorf("cache key not stable: %q vs %q", k1, k2)
	}
}
