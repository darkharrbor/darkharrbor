package httpstream

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
)

func testKey() ResolveKey {
	return ResolveKey{
		Version:   ResolveKeyVersion,
		BackendID: "MyBackend",
		Handler:   "OMSS",
		Kind:      "Episode",
		IDs:       map[string]string{"TMDB": "12345", "imdb": "TT0123456"},
		Season:    2,
		Episode:   7,
		Selector:  "src-uuid-1",
	}
}

// ── HS-1.5: canonical key + digest ───────────────────────────────────────────

func TestCanonicalDeterministicAndNormalized(t *testing.T) {
	a := testKey()
	b := ResolveKey{ // equivalent, different casing/whitespace/map insertion order
		Version:   ResolveKeyVersion,
		BackendID: " mybackend ",
		Handler:   "omss",
		Kind:      "episode",
		IDs:       map[string]string{"imdb": "tt0123456", "tmdb": " 12345 "},
		Season:    2,
		Episode:   7,
		Selector:  "src-uuid-1",
	}
	ca, err := a.Canonical()
	if err != nil {
		t.Fatalf("canonical a: %v", err)
	}
	cb, err := b.Canonical()
	if err != nil {
		t.Fatalf("canonical b: %v", err)
	}
	if ca != cb {
		t.Fatalf("canonical not deterministic:\n%s\n%s", ca, cb)
	}
	if strings.Contains(ca, "MyBackend") || strings.Contains(ca, "OMSS") {
		t.Fatalf("canonical not normalized: %s", ca)
	}
	// Repeat marshal 50x to shake map iteration order.
	for i := 0; i < 50; i++ {
		c, _ := a.Canonical()
		if c != ca {
			t.Fatal("canonical unstable across marshals")
		}
	}
}

func TestDigestDomainSeparatedAnd40Hex(t *testing.T) {
	k := testKey()
	d, err := k.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if len(d) != 40 {
		t.Fatalf("digest not 40 hex: %q", d)
	}
	if _, err := hex.DecodeString(d); err != nil {
		t.Fatalf("digest not hex: %q", d)
	}
	canon, _ := k.Canonical()
	raw := sha1.Sum([]byte(canon))
	if hex.EncodeToString(raw[:]) == d {
		t.Fatal("digest is NOT domain-separated from sha1(canonical)")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []func(*ResolveKey){
		func(k *ResolveKey) { k.Version = 99 },
		func(k *ResolveKey) { k.BackendID = " " },
		func(k *ResolveKey) { k.Handler = "scraper" },
		func(k *ResolveKey) { k.Kind = "series" },
		func(k *ResolveKey) { k.IDs = nil },
		func(k *ResolveKey) { k.Episode = 0 }, // season-only episode key (D12)
		func(k *ResolveKey) { k.IDs = map[string]string{"tmdb": " "} },
	}
	for i, mut := range cases {
		k := testKey()
		mut(&k)
		if err := k.Validate(); err == nil {
			t.Fatalf("case %d: expected validation failure", i)
		}
	}
	movie := testKey()
	movie.Kind = "movie"
	movie.Season, movie.Episode = 0, 0
	if err := movie.Validate(); err != nil {
		t.Fatalf("movie key should validate: %v", err)
	}
}

func TestParseResolveKeyRoundTrip(t *testing.T) {
	canon, err := testKey().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := ParseResolveKey(canon)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	canon2, err := k2.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if canon != canon2 {
		t.Fatalf("round-trip changed canonical:\n%s\n%s", canon, canon2)
	}
	if _, err := ParseResolveKey("{not json"); err == nil {
		t.Fatal("expected parse failure")
	}
}

// ── HS-1.3: grab envelope ────────────────────────────────────────────────────

func TestGrabTokenRoundTrip(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	env := GrabEnvelope{Key: testKey(), Title: "Show.S02E07.1080p.WEBDL.DH-HTTP"}
	tok, err := EncodeGrabToken(secret, env)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeGrabToken(secret, tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantDigest, _ := testKey().Digest()
	if got.Hash != wantDigest {
		t.Fatalf("hash mismatch: %s vs %s", got.Hash, wantDigest)
	}
	if got.Title != env.Title {
		t.Fatalf("title mismatch")
	}
}

func TestGrabTokenRoundTripsProviderIdentity(t *testing.T) {
	secret := "01234567890123456789012345678901"
	env := GrabEnvelope{
		Key: testKey(), Title: "Show.S01E02",
		ProviderIdentity: &mediaidentity.ProviderIdentity{
			Kind: "series", IDs: mediaidentity.ProviderIDs{TVDB: "100"},
			Episodes: []mediaidentity.EpisodeProviderIDs{{Season: 1, Episode: 2, TVDB: "200"}},
		},
	}
	token, err := EncodeGrabToken(secret, env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeGrabToken(secret, token)
	if err != nil || got.ProviderIdentity == nil || got.ProviderIdentity.IDs.TVDB != "100" ||
		len(got.ProviderIdentity.Episodes) != 1 {
		t.Fatalf("provider identity round-trip = (%+v, %v)", got.ProviderIdentity, err)
	}
}

func TestGrabTokenRejections(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	tok, err := EncodeGrabToken(secret, GrabEnvelope{Key: testKey(), Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeGrabToken("wrong-secret-wrong-secret-wrong!", tok); err == nil {
		t.Fatal("wrong secret accepted")
	}
	// Tamper with payload byte.
	b := []byte(tok)
	if b[3] == 'A' {
		b[3] = 'B'
	} else {
		b[3] = 'A'
	}
	if _, err := DecodeGrabToken(secret, string(b)); err == nil {
		t.Fatal("tampered token accepted")
	}
	if _, err := DecodeGrabToken(secret, strings.Repeat("a", MaxGrabTokenBytes+1)); err == nil {
		t.Fatal("oversized token accepted")
	}
	if _, err := DecodeGrabToken(secret, "no-dot-token"); err == nil {
		t.Fatal("malformed token accepted")
	}
	if _, err := EncodeGrabToken("", GrabEnvelope{Key: testKey()}); err == nil {
		t.Fatal("empty secret accepted at encode")
	}
}

// ── HS-1.5/HS-1.7: file IDs + strict lookup ──────────────────────────────────

func TestFileIDAndStrictLookup(t *testing.T) {
	id, err := NewFileID()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidFileID(id) {
		t.Fatalf("generated id invalid: %q", id)
	}
	if strings.ContainsAny(id, "/\\ .?&") {
		t.Fatalf("file id not path-safe: %q", id)
	}
	files := []PersistedFile{
		{FileID: "hf000000000001", Selector: "archive:zip:a", Name: "one.mkv", Size: 10, ArchiveSize: 20},
		{FileID: "hf000000000002", Selector: "b", Name: "two.mkv", Size: 20},
	}
	if f, ok := LookupFile(files, "hf000000000002"); !ok || f.Name != "two.mkv" {
		t.Fatal("strict lookup by id failed")
	}
	// Positional values must never resolve (D10).
	if _, ok := LookupFile(files, "0"); ok {
		t.Fatal("positional index resolved — strict lookup violated")
	}
	if _, ok := LookupFile(files, "hfdeadbeef0000"); ok {
		t.Fatal("unknown id resolved")
	}
	s, err := MarshalFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseFiles(s)
	if err != nil || len(back) != 2 || back[0].ArchiveSize != 20 || back[1].Size != 20 {
		t.Fatalf("file list round trip failed: %v %+v", err, back)
	}
}

// ── HS-1.9: persistence redaction ────────────────────────────────────────────

func TestPersistedFileHasNoURLBearingFields(t *testing.T) {
	tt := reflect.TypeOf(PersistedFile{})
	for i := 0; i < tt.NumField(); i++ {
		name := strings.ToLower(tt.Field(i).Name)
		if strings.Contains(name, "url") || strings.Contains(name, "header") {
			t.Fatalf("PersistedFile gained a URL/header-bearing field: %s", tt.Field(i).Name)
		}
	}
}

func TestResolvedFileMarshalDropsTransientFields(t *testing.T) {
	rf := ResolvedFile{
		Selector:       "s",
		Name:           "n.mkv",
		URL:            "https://cdn.example.invalid/secret/path?token=abc",
		RequestHeaders: map[string]string{"Authorization": "Bearer xyz"},
		ExpiresAt:      time.Now(),
	}
	b, err := json.Marshal(rf)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "cdn.example") || strings.Contains(s, "Bearer") || strings.Contains(s, "http") {
		t.Fatalf("transient fields leaked into marshal: %s", s)
	}
}

func TestSanitizeScrubsURLs(t *testing.T) {
	in := `Get "https://user:pass@cdn.host.example/path/file.mp4?sig=abc123": dial tcp: timeout`
	out := ScrubURLs(in)
	if strings.Contains(out, "cdn.host.example") || strings.Contains(out, "sig=abc123") {
		t.Fatalf("URL survived scrub: %s", out)
	}
	if !strings.Contains(out, "[url-redacted]") {
		t.Fatalf("no redaction marker: %s", out)
	}
	e := NewError(ClassBackendUnavailable, "fetch http://10.0.0.5:9000/v1 failed")
	if strings.Contains(e.Error(), "10.0.0.5") {
		t.Fatalf("NewError did not scrub detail: %s", e.Error())
	}
	if ClassOf(e) != ClassBackendUnavailable {
		t.Fatal("ClassOf failed")
	}
}

// ── HS-1.7 / D16: range + status math ────────────────────────────────────────

func TestParseRange(t *testing.T) {
	const size = 1000
	cases := []struct {
		hdr   string
		start int64
		end   int64
		full  bool // expect 200-full
		unsat bool // expect 416
	}{
		{"", 0, 0, true, false},
		{"bytes=0-499", 0, 499, false, false},
		{"bytes=500-", 500, 999, false, false},
		{"bytes=-200", 800, 999, false, false},
		{"bytes=0-1999", 0, 999, false, false},      // end clamped
		{"bytes=0-0", 0, 0, false, false},           // D9 preflight shape
		{"bytes=0-99,200-299", 0, 99, false, false}, // multipart → first only (G5)
		{"bytes=1000-", 0, 0, false, true},          // start == size → 416
		{"bytes=abc-def", 0, 0, false, true},
		{"bytes=", 0, 0, false, true},
		{"bytes=-0", 0, 0, false, true},
		{"items=0-1", 0, 0, true, false}, // unknown unit ignored
	}
	for _, c := range cases {
		rng, ok := ParseRange(c.hdr, size)
		if c.unsat {
			if ok {
				t.Fatalf("%q: expected unsatisfiable", c.hdr)
			}
			continue
		}
		if !ok {
			t.Fatalf("%q: unexpected 416", c.hdr)
		}
		if c.full {
			if rng != nil {
				t.Fatalf("%q: expected full 200, got %+v", c.hdr, rng)
			}
			continue
		}
		if rng == nil || rng.Start != c.start || rng.End != c.end {
			t.Fatalf("%q: got %+v want %d-%d", c.hdr, rng, c.start, c.end)
		}
	}
	if UnsatisfiedContentRange(1000) != "bytes */1000" {
		t.Fatal("416 content-range wrong")
	}
	if ContentRange(ByteRange{0, 499}, 1000) != "bytes 0-499/1000" {
		t.Fatal("206 content-range wrong")
	}
	if UpstreamRangeHeader(ByteRange{5, 9}) != "bytes=5-9" {
		t.Fatal("upstream range header wrong")
	}
}

func TestValidateUpstream206(t *testing.T) {
	want := ByteRange{Start: 100, End: 199}
	if total, err := ValidateUpstream206("bytes 100-199/1000", want); err != nil || total != 1000 {
		t.Fatalf("coherent 206 rejected: %v %d", err, total)
	}
	if _, err := ValidateUpstream206("bytes 0-199/1000", want); err == nil {
		t.Fatal("wrong start accepted")
	}
	if _, err := ValidateUpstream206("bytes 100-199/150", want); err == nil {
		t.Fatal("incoherent total accepted")
	}
	if _, err := ValidateUpstream206("garbage", want); err == nil {
		t.Fatal("malformed accepted")
	}
	if total, err := ValidateUpstream206("bytes 100-199/*", want); err != nil || total != 0 {
		t.Fatalf("*-total should be tolerated: %v", err)
	}
	if ClassOf(func() error { _, e := ValidateUpstream206("x", want); return e }()) != ClassUpstreamMalformed {
		t.Fatal("malformed 206 not classified upstream_malformed")
	}
}

// ── HS-1.10 / D17: presentation ──────────────────────────────────────────────

func TestQualityMappingAndTitleToken(t *testing.T) {
	for in, want := range map[string]string{
		"4K": "2160p", "8K": "2160p", "QHD": "1440p", "FHD": "1080p", "HD": "720p", "SD": "480p",
	} {
		got, ok := QualityToRes(in)
		if !ok || got != want {
			t.Fatalf("%s → %s (ok=%v), want %s", in, got, ok, want)
		}
	}
	if _, ok := QualityToRes("Auto"); ok {
		t.Fatal("Auto must be skipped, never guessed (D17)")
	}
	if _, ok := QualityToRes("weird"); ok {
		t.Fatal("unknown quality must not map")
	}
	tagged := TagTitle("Show.S01E01.1080p.WEB")
	if !strings.Contains(tagged, HTTPTitleToken) {
		t.Fatal("title not tagged")
	}
	if TagTitle(tagged) != tagged {
		t.Fatal("double-tagging")
	}
	if SizeEstimate("episode", "1080p") <= 0 || SizeEstimate("movie", "2160p") <= SizeEstimate("episode", "2160p") {
		t.Fatal("size estimate table incoherent")
	}
}

// ── D10: selector matching ───────────────────────────────────────────────────

func TestMatchSelector(t *testing.T) {
	files := []ResolvedFile{{Selector: "a"}, {Selector: "b"}, {Selector: "b"}}
	if f, err := MatchSelector(files, "a"); err != nil || f.Selector != "a" {
		t.Fatalf("exact match failed: %v", err)
	}
	if _, err := MatchSelector(files, "missing"); ClassOf(err) != ClassRepresentationLost {
		t.Fatalf("missing selector not rejected: %v", err)
	}
	if _, err := MatchSelector(files, "b"); ClassOf(err) != ClassRepresentationLost {
		t.Fatalf("ambiguous selector not rejected: %v", err)
	}
}
