package generic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

func writeDescriptors(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("protect descriptor directory: %v", err)
	}
	p := filepath.Join(dir, "descriptors.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write descriptors: %v", err)
	}
	return p
}

func testClient() *http.Client {
	return httpstream.NewHTTPClient(httpstream.NewSecurityPolicy(true), httpstream.TrustBackend, httpstream.TransportMetadata)
}

// ── LoadDescriptors ───────────────────────────────────────────────────────────

func TestLoadDescriptorsValid(t *testing.T) {
	p := writeDescriptors(t, `[
		{"kind":"fixed","title":"Some Movie","year":2020,"url":"https://example.com/a.mp4","size":123,"quality":"1080p"},
		{"kind":"webdav","tvdb_id":"1","season":1,"episode":2,"url":"https://example.com/dav/"}
	]`)
	ds, err := LoadDescriptors(p)
	if err != nil {
		t.Fatalf("LoadDescriptors: %v", err)
	}
	if len(ds) != 2 {
		t.Fatalf("expected 2 descriptors, got %d", len(ds))
	}
	if ds[0].ID == "" || ds[0].ID == ds[1].ID {
		t.Fatalf("descriptor IDs must be non-empty and unique: %q %q", ds[0].ID, ds[1].ID)
	}
}

func TestLoadDescriptorsRejectsUnknownKind(t *testing.T) {
	p := writeDescriptors(t, `[{"kind":"bogus","title":"X","url":"https://example.com/a.mp4"}]`)
	if _, err := LoadDescriptors(p); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestLoadDescriptorsRejectsBadURL(t *testing.T) {
	cases := []string{
		`[{"kind":"fixed","title":"X","url":"not-a-url"}]`,
		`[{"kind":"fixed","title":"X","url":"ftp://example.com/a.mp4"}]`,
		`[{"kind":"fixed","title":"X","url":"https://user:pass@example.com/a.mp4"}]`,
	}
	for _, body := range cases {
		p := writeDescriptors(t, body)
		if _, err := LoadDescriptors(p); err == nil {
			t.Fatalf("expected error for %s", body)
		}
	}
}

func TestLoadDescriptorsRejectsZeroSizeFixedSource(t *testing.T) {
	p := writeDescriptors(t, `[{"kind":"fixed","title":"X","url":"https://example.com/a.mp4"}]`)
	if _, err := LoadDescriptors(p); err == nil || !strings.Contains(err.Error(), "positive size") {
		t.Fatalf("zero-size fixed descriptor error = %v", err)
	}
}

func TestValidateDescriptorsRejectsAmbiguousJSON(t *testing.T) {
	cases := []string{
		`null`,
		`[{"kind":"fixed","title":"X","url":"https://example.com/a.mkv","extra":true}]`,
		`[{"kind":"fixed","title":"X","url":"https://example.com/a.mkv","URL":"https://example.com/b.mkv"}]`,
		`[{"kind":"fixed","title":"X","url":"https://example.com/a.mkv","url":"https://example.com/b.mkv"}]`,
	}
	for _, body := range cases {
		if _, err := ValidateDescriptors([]byte(body)); err == nil {
			t.Fatalf("ambiguous catalog was accepted: %s", body)
		}
	}
}

func TestValidateDescriptorsRejectsInvalidOrDuplicateSourceID(t *testing.T) {
	cases := []string{
		`[{"source_id":"bad id","kind":"fixed","title":"X","url":"https://example.com/a.mkv"}]`,
		`[{"source_id":"same","kind":"fixed","title":"A","url":"https://example.com/a.mkv"},{"source_id":"SAME","kind":"fixed","title":"B","url":"https://example.com/b.mkv"}]`,
	}
	for _, body := range cases {
		if _, err := ValidateDescriptors([]byte(body)); err == nil {
			t.Fatalf("invalid source_id catalog was accepted: %s", body)
		}
	}
}

func TestSourceIDBindsMediaIdentityButNotTransport(t *testing.T) {
	load := func(body string) Descriptor {
		t.Helper()
		descriptors, err := ValidateDescriptors([]byte(body))
		if err != nil || len(descriptors) != 1 {
			t.Fatalf("ValidateDescriptors: descriptors=%d err=%v", len(descriptors), err)
		}
		return descriptors[0]
	}
	first := load(`[{"source_id":"movie-primary","kind":"fixed","title":"Movie","imdb_id":"tt1234567","url":"https://one.example/a.mkv","size":1}]`)
	relocated := load(`[{"source_id":"movie-primary","kind":"fixed","title":"Movie","imdb_id":"tt1234567","url":"https://two.example/b.mkv?token=new","size":1}]`)
	differentMedia := load(`[{"source_id":"movie-primary","kind":"fixed","title":"Other Movie","imdb_id":"tt7654321","url":"https://two.example/b.mkv","size":1}]`)
	legacyRelocated := load(`[{"kind":"fixed","title":"Movie","imdb_id":"tt1234567","url":"https://two.example/b.mkv","size":1}]`)
	if first.ID != relocated.ID {
		t.Fatal("stable source identity changed with transport URL")
	}
	if first.ID == differentMedia.ID {
		t.Fatal("stable source identity ignored changed media identity")
	}
	if first.ID == legacyRelocated.ID {
		t.Fatal("legacy URL-derived identity was silently converted")
	}
}

func TestLoadDescriptorsRejectsNoIdentity(t *testing.T) {
	p := writeDescriptors(t, `[{"kind":"fixed","url":"https://example.com/a.mp4"}]`)
	if _, err := LoadDescriptors(p); err == nil {
		t.Fatal("expected error for missing title/ID")
	}
}

func TestLoadDescriptorsRejectsSeasonOnly(t *testing.T) {
	p := writeDescriptors(t, `[{"kind":"fixed","title":"X","season":1,"url":"https://example.com/a.mp4"}]`)
	if _, err := LoadDescriptors(p); err == nil {
		t.Fatal("expected error for season without episode")
	}
}

func TestLoadDescriptorsRejectsBadQuality(t *testing.T) {
	p := writeDescriptors(t, `[{"kind":"fixed","title":"X","url":"https://example.com/a.mp4","quality":"potato"}]`)
	if _, err := LoadDescriptors(p); err == nil {
		t.Fatal("expected error for unrecognized quality")
	}
}

func TestLoadDescriptorsRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "descriptors.json")
	big := strings.Repeat("a", MaxDescriptorFileBytes+10)
	if err := os.WriteFile(p, []byte("["+big+"]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDescriptors(p); err == nil {
		t.Fatal("expected error for oversized file")
	}
}

func TestLoadDescriptorsMissingFile(t *testing.T) {
	if _, err := LoadDescriptors("/nonexistent/path/descriptors.json"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadDescriptorsRejectsUnsafeFileBoundary(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "descriptors.json")
	body := []byte(`[{"kind":"fixed","title":"X","url":"https://example.com/x.mkv"}]`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDescriptors(path); err == nil {
		t.Fatal("group/world-readable descriptor file was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "descriptors-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDescriptors(link); err == nil {
		t.Fatal("symlinked descriptor file was accepted")
	}
}

// ── matches ───────────────────────────────────────────────────────────────────

func TestMatchesIdentityOverridesTitle(t *testing.T) {
	d := Descriptor{Title: "Totally Different Title", IMDBID: "tt1111111"}
	q := httpstream.StreamQuery{Title: "Some Movie", IMDBID: "tt1111111"}
	if !d.matches(q, false) {
		t.Fatal("expected ID match to match despite different titles")
	}
	q2 := httpstream.StreamQuery{Title: "Some Movie", IMDBID: "tt2222222"}
	if d.matches(q2, false) {
		t.Fatal("expected ID mismatch to reject even if title happened to match")
	}
}

func TestMatchesTitleFallback(t *testing.T) {
	d := Descriptor{Title: "The Great Movie"}
	if !d.matches(httpstream.StreamQuery{Title: "the   great movie!!"}, false) {
		t.Fatal("expected normalized title equality to match")
	}
	if d.matches(httpstream.StreamQuery{Title: "The Great Movie Part 2"}, false) {
		t.Fatal("expected containment NOT to match (conservative, fixed-catalog semantics)")
	}
}

func TestMatchesEpisodeRequiresExact(t *testing.T) {
	d := Descriptor{Title: "Show", Season: 1, Episode: 2}
	if !d.matches(httpstream.StreamQuery{Title: "Show", Season: 1, Episode: 2}, true) {
		t.Fatal("expected exact season/episode match")
	}
	if d.matches(httpstream.StreamQuery{Title: "Show", Season: 1, Episode: 3}, true) {
		t.Fatal("expected episode mismatch to reject")
	}
}

// ── parseM3U ──────────────────────────────────────────────────────────────────

func TestParseM3U(t *testing.T) {
	body := "#EXTM3U\n#EXTINF:-1,Episode One\nhttps://example.com/e1.mp4\n#EXTINF:-1,Episode Two\n/relative/e2.mp4\n"
	base, _ := parseTestURL("https://example.com/playlist.m3u8")
	entries := parseM3U([]byte(body), base)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].name != "Episode One" || entries[0].url != "https://example.com/e1.mp4" {
		t.Fatalf("unexpected entry 0: %+v", entries[0])
	}
	if entries[1].url != "https://example.com/relative/e2.mp4" {
		t.Fatalf("expected relative URL resolved against base: %+v", entries[1])
	}
}

func TestParseM3UBoundsEntries(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	for i := 0; i < maxM3UEntries+50; i++ {
		sb.WriteString("https://example.com/e.mp4\n")
	}
	entries := parseM3U([]byte(sb.String()), nil)
	if len(entries) > maxM3UEntries {
		t.Fatalf("expected entries bounded at %d, got %d", maxM3UEntries, len(entries))
	}
}

func TestParseM3UNeverPanics(t *testing.T) {
	inputs := []string{"", "\x00\x00\x00", "#EXTM3U", "not a playlist at all", "https://[::1"}
	for _, in := range inputs {
		_ = parseM3U([]byte(in), nil)
	}
}

// ── parseMetalink ─────────────────────────────────────────────────────────────

func TestParseMetalink(t *testing.T) {
	xmlDoc := `<?xml version="1.0"?><metalink xmlns="urn:ietf:params:xml:ns:metalink">
	<file name="movie.mkv"><size>1000</size>
		<url priority="2">https://mirror.example.com/movie.mkv</url>
		<url priority="1">https://primary.example.com/movie.mkv</url>
	</file></metalink>`
	files, err := parseMetalink([]byte(xmlDoc))
	if err != nil {
		t.Fatalf("parseMetalink: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].bestURL != "https://primary.example.com/movie.mkv" {
		t.Fatalf("expected priority-1 URL to win, got %q", files[0].bestURL)
	}
	if files[0].size != 1000 {
		t.Fatalf("expected size 1000, got %d", files[0].size)
	}
}

// HR2.7: RFC 5854 <hash type="..."> elements become authoritative
// whole-file SourceDigest evidence.
func TestParseMetalinkExtractsDigests(t *testing.T) {
	xmlDoc := `<?xml version="1.0"?><metalink xmlns="urn:ietf:params:xml:ns:metalink">
	<file name="movie.mkv"><size>1000</size>
		<url priority="1">https://primary.example.com/movie.mkv</url>
		<hash type="sha-256">e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855</hash>
		<hash type="md5">d41d8cd98f00b204e9800998ecf8427e</hash>
		<hash type="crc32">deadbeef</hash>
	</file>
	<file name="nodigest.mkv"><size>500</size>
		<url priority="1">https://primary.example.com/nodigest.mkv</url>
	</file>
	</metalink>`
	files, err := parseMetalink([]byte(xmlDoc))
	if err != nil {
		t.Fatalf("parseMetalink: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if len(files[0].digests) != 2 {
		t.Fatalf("expected 2 recognized digests (sha256+md5, crc32 dropped), got %+v", files[0].digests)
	}
	seen := map[string]string{}
	for _, d := range files[0].digests {
		if !httpstream.ValidSourceDigest(d) {
			t.Fatalf("invalid digest: %+v", d)
		}
		seen[d.Algorithm] = d.Hex
	}
	if seen["sha256"] != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("sha256 = %q", seen["sha256"])
	}
	if seen["md5"] != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("md5 = %q", seen["md5"])
	}
	if len(files[1].digests) != 0 {
		t.Fatalf("file with no <hash> elements must yield zero digests, got %+v", files[1].digests)
	}
}

func TestParseMetalinkRejectsMalformedXML(t *testing.T) {
	if _, err := parseMetalink([]byte("<not-xml")); err == nil {
		t.Fatal("expected error for malformed XML")
	}
}

func TestParseMetalinkNeverPanics(t *testing.T) {
	inputs := []string{"", "<metalink>", "<metalink><file/></metalink>", strings.Repeat("<a>", 5000)}
	for _, in := range inputs {
		_, _ = parseMetalink([]byte(in))
	}
}

// ── parseWebDAVMultistatus ───────────────────────────────────────────────────

func TestParseWebDAVMultistatus(t *testing.T) {
	xmlDoc := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
	<D:response><D:href>/dav/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop></D:propstat></D:response>
	<D:response><D:href>/dav/movie.mkv?version=2</D:href><D:propstat><D:prop><D:getcontentlength>555</D:getcontentlength><D:resourcetype/></D:prop></D:propstat></D:response>
	<D:response><D:href>/dav/readme.txt</D:href><D:propstat><D:prop><D:getcontentlength>10</D:getcontentlength><D:resourcetype/></D:prop></D:propstat></D:response>
	</D:multistatus>`
	base, _ := parseTestURL("https://example.com/dav/")
	members, err := parseWebDAVMultistatus([]byte(xmlDoc), base)
	if err != nil {
		t.Fatalf("parseWebDAVMultistatus: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("expected 1 video member (collection and non-video skipped), got %d: %+v", len(members), members)
	}
	if members[0].size != 555 || members[0].name != "movie.mkv" || !strings.HasSuffix(members[0].url, "/dav/movie.mkv?version=2") {
		t.Fatalf("unexpected member: %+v", members[0])
	}
}

func TestParseWebDAVMultistatusConfinesMembersToCollection(t *testing.T) {
	xmlDoc := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
	<D:response><D:href>inside.mkv</D:href><D:propstat><D:prop><D:getcontentlength>111</D:getcontentlength></D:prop></D:propstat></D:response>
	<D:response><D:href>../outside.mkv</D:href><D:propstat><D:prop><D:getcontentlength>222</D:getcontentlength></D:prop></D:propstat></D:response>
	<D:response><D:href>/dav-sibling/outside.mkv</D:href><D:propstat><D:prop><D:getcontentlength>333</D:getcontentlength></D:prop></D:propstat></D:response>
	<D:response><D:href>https://other.example/dav/outside.mkv</D:href><D:propstat><D:prop><D:getcontentlength>444</D:getcontentlength></D:prop></D:propstat></D:response>
	</D:multistatus>`
	base, _ := parseTestURL("https://example.com/dav")
	members, err := parseWebDAVMultistatus([]byte(xmlDoc), base)
	if err != nil {
		t.Fatalf("parseWebDAVMultistatus: %v", err)
	}
	if len(members) != 1 || members[0].name != "inside.mkv" || members[0].size != 111 {
		t.Fatalf("collection escape was accepted: %+v", members)
	}
}

func TestParseWebDAVNeverPanics(t *testing.T) {
	inputs := []string{"", "<multistatus>", "not xml", strings.Repeat("<response>", 3000)}
	for _, in := range inputs {
		_, _ = parseWebDAVMultistatus([]byte(in), nil)
	}
}

// ── candidatesFor / kind dispatch ────────────────────────────────────────────

func TestCandidatesFixedNoNetworkCall(t *testing.T) {
	d := Descriptor{ID: "abc", Kind: KindFixed, URL: "https://example.com/a.mp4", Size: 42}
	h := New("backend", "unused", nil) // nil client: a network call here would panic/error
	cands, err := h.candidatesFor(context.Background(), d)
	if err != nil {
		t.Fatalf("candidatesFor fixed: %v", err)
	}
	if len(cands) != 1 || cands[0].size != 42 {
		t.Fatalf("unexpected fixed candidates: %+v", cands)
	}
}

func TestCandidatesHLSAbstainsOnDASH(t *testing.T) {
	d := Descriptor{ID: "abc", Kind: KindHLS, URL: "https://example.com/manifest.mpd"}
	h := New("backend", "unused", nil)
	if _, err := h.candidatesFor(context.Background(), d); err == nil {
		t.Fatal("expected DASH descriptor to abstain with an error")
	}
}

func TestCandidatesHLSAcceptsM3U8(t *testing.T) {
	d := Descriptor{ID: "abc", Kind: KindHLS, URL: "https://example.com/master.m3u8"}
	h := New("backend", "unused", nil)
	cands, err := h.candidatesFor(context.Background(), d)
	if err != nil {
		t.Fatalf("candidatesFor hls: %v", err)
	}
	if len(cands) != 1 || !strings.HasSuffix(cands[0].name, ".m3u8") || cands[0].contentType != "application/vnd.apple.mpegurl" {
		t.Fatalf("unexpected hls candidate: %+v", cands)
	}
}

func TestCandidatesWebDAVUsesBoundedCredentialFreeCollectionRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.Header.Get("Depth") != "1" {
			t.Errorf("unexpected WebDAV request: method=%q depth=%q", r.Method, r.Header.Get("Depth"))
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("generic WebDAV request carried credentials")
		}
		w.WriteHeader(207)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
		<D:response><D:href>/dav/inside.mkv</D:href><D:propstat><D:prop><D:getcontentlength>777</D:getcontentlength></D:prop></D:propstat></D:response>
		<D:response><D:href>/outside.mkv</D:href><D:propstat><D:prop><D:getcontentlength>888</D:getcontentlength></D:prop></D:propstat></D:response>
		</D:multistatus>`))
	}))
	defer srv.Close()

	h := New("backend", "unused", testClient())
	cands, err := h.candidatesWebDAV(context.Background(), Descriptor{ID: "abc", Kind: KindWebDAV, URL: srv.URL + "/dav/"})
	if err != nil {
		t.Fatalf("candidatesWebDAV: %v", err)
	}
	if len(cands) != 1 || cands[0].name != "inside.mkv" || cands[0].size != 777 {
		t.Fatalf("unexpected WebDAV candidates: %+v", cands)
	}
}

func TestSearchAndResolveM3U(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:-1,Feature\n" + "/movie.mp4\n"))
	}))
	defer srv.Close()

	p := writeDescriptors(t, `[{"kind":"m3u","title":"Fixture Movie","year":2021,"url":"`+srv.URL+`/playlist.m3u8"}]`)
	h := New("backend", p, testClient())

	results, err := h.Search(context.Background(), httpstream.StreamQuery{Title: "Fixture Movie"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 search result, got %d", len(results))
	}
	if !strings.Contains(results[0].Title, "Fixture.Movie") {
		t.Fatalf("unexpected release title: %q", results[0].Title)
	}

	files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: results[0].Key})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(files) != 1 || !strings.HasSuffix(files[0].URL, "/movie.mp4") {
		t.Fatalf("unexpected resolved files: %+v", files)
	}
	if _, err := httpstream.MatchSelector(files, results[0].Key.Selector); err != nil {
		t.Fatalf("MatchSelector: %v", err)
	}
}

func TestEnumeratedSelectorsSurviveListingReorder(t *testing.T) {
	var listing atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			w.WriteHeader(207)
		}
		_, _ = w.Write([]byte(listing.Load().(string)))
	}))
	defer srv.Close()

	tests := []struct {
		name   string
		kind   string
		first  string
		second string
	}{
		{
			name: "m3u", kind: "m3u",
			first:  "#EXTM3U\n#EXTINF:-1,A\n/a.mkv\n#EXTINF:-1,B\n/b.mkv\n",
			second: "#EXTM3U\n#EXTINF:-1,B\n/b.mkv\n#EXTINF:-1,A\n/a.mkv\n",
		},
		{
			name: "metalink", kind: "metalink",
			first:  `<metalink><file name="a.mkv"><size>10</size><url>` + srv.URL + `/a.mkv</url></file><file name="b.mkv"><size>20</size><url>` + srv.URL + `/b.mkv</url></file></metalink>`,
			second: `<metalink><file name="b.mkv"><size>20</size><url>` + srv.URL + `/b.mkv</url></file><file name="a.mkv"><size>10</size><url>` + srv.URL + `/a.mkv</url></file></metalink>`,
		},
		{
			name: "webdav", kind: "webdav",
			first:  `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/a.mkv</D:href><D:propstat><D:prop><D:getcontentlength>10</D:getcontentlength></D:prop></D:propstat></D:response><D:response><D:href>/dav/b.mkv</D:href><D:propstat><D:prop><D:getcontentlength>20</D:getcontentlength></D:prop></D:propstat></D:response></D:multistatus>`,
			second: `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/dav/b.mkv</D:href><D:propstat><D:prop><D:getcontentlength>20</D:getcontentlength></D:prop></D:propstat></D:response><D:response><D:href>/dav/a.mkv</D:href><D:propstat><D:prop><D:getcontentlength>10</D:getcontentlength></D:prop></D:propstat></D:response></D:multistatus>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listing.Store(tt.first)
			path := writeDescriptors(t, `[{"kind":"`+tt.kind+`","title":"Reorder Test","url":"`+srv.URL+`/dav/"}]`)
			h := New("backend", path, testClient())
			results, err := h.Search(context.Background(), httpstream.StreamQuery{Title: "Reorder Test"})
			if err != nil || len(results) != 1 {
				t.Fatalf("Search: results=%d err=%v", len(results), err)
			}
			if strings.ContainsAny(results[0].Key.Selector, "/?") {
				t.Fatalf("selector contains source URL material: %q", results[0].Key.Selector)
			}
			listing.Store(tt.second)
			files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: results[0].Key})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			matched, err := httpstream.MatchSelector(files, results[0].Key.Selector)
			if err != nil || !strings.HasSuffix(matched.URL, "/a.mkv") {
				t.Fatalf("reordered listing changed representation: file=%+v err=%v", matched, err)
			}
			legacy := results[0].Key.IDs["generic"] + "#0"
			if _, err := httpstream.MatchSelector(files, legacy); err == nil {
				t.Fatal("legacy positional selector was silently rebound")
			}
		})
	}
}

func TestSearchRejectsAmbiguousEnumeratedSelector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:-1,Same identity\n/media.mkv?token=one\n#EXTINF:-1,Same identity\n/media.mkv?token=two\n"))
	}))
	defer srv.Close()

	path := writeDescriptors(t, `[{"kind":"m3u","title":"Ambiguous Test","url":"`+srv.URL+`/list.m3u"}]`)
	results, err := New("backend", path, testClient()).Search(context.Background(), httpstream.StreamQuery{Title: "Ambiguous Test"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("ambiguous selector was advertised: %+v", results)
	}
}

func TestStableSourceIDAllowsURLProviderFallback(t *testing.T) {
	path := writeDescriptors(t, `[{"source_id":"movie-primary","kind":"fixed","title":"Provider Fallback","url":"https://one.example/signed.mkv?token=old","size":1}]`)
	h := New("backend", path, testClient())
	results, err := h.Search(context.Background(), httpstream.StreamQuery{Title: "Provider Fallback"})
	if err != nil || len(results) != 1 {
		t.Fatalf("Search: results=%d err=%v", len(results), err)
	}
	updated := `[{"source_id":"movie-primary","kind":"fixed","title":"Provider Fallback","url":"https://two.example/replacement.mkv?token=new","size":1}]`
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: results[0].Key})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	matched, err := httpstream.MatchSelector(files, results[0].Key.Selector)
	if err != nil || matched.URL != "https://two.example/replacement.mkv?token=new" {
		t.Fatalf("stable source did not adopt replacement provider URL: file=%+v err=%v", matched, err)
	}
}

func TestNamedM3USelectorAllowsMirrorFallback(t *testing.T) {
	first := m3uEntry{name: "Stable Movie", url: "https://one.example/path/movie.mkv?token=old"}
	second := m3uEntry{name: "Stable Movie", url: "https://two.example/other/replacement.mkv?token=new"}
	if m3uEntryIdentity(first) != m3uEntryIdentity(second) {
		t.Fatal("named M3U identity was pinned to transport location")
	}
	if m3uEntryIdentity(m3uEntry{url: first.url}) == m3uEntryIdentity(m3uEntry{url: second.url}) {
		t.Fatal("unnamed M3U entries with different paths were treated as the same media")
	}
}

func TestSearchSkipsSeasonOnlyQuery(t *testing.T) {
	p := writeDescriptors(t, `[{"kind":"fixed","title":"Show","season":1,"episode":1,"url":"https://example.com/a.mp4","size":1}]`)
	h := New("backend", p, testClient())
	results, err := h.Search(context.Background(), httpstream.StreamQuery{Kind: "episode", Title: "Show", Season: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results for season-only query, got %d", len(results))
	}
}

func TestResolveRejectsMissingDescriptorID(t *testing.T) {
	p := writeDescriptors(t, `[{"kind":"fixed","title":"X","url":"https://example.com/a.mp4"}]`)
	h := New("backend", p, testClient())
	key := httpstream.ResolveKey{Version: 1, BackendID: "backend", Handler: "generic", Kind: "movie", IDs: map[string]string{"generic": "does-not-exist"}}
	if _, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: key}); err == nil {
		t.Fatal("expected error for unknown descriptor id")
	}
}

func parseTestURL(raw string) (*url.URL, error) { return url.Parse(raw) }
