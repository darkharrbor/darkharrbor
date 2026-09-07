package hls

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/hlssession"
)

func TestLiveMediaSequenceCoordinatesAndBounds(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:41\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n" +
		"#EXTINF:2,\nseg41.ts\n#EXTINF:2,\nseg42.ts\n"
	entries, err := ParseMedia(body)
	if err != nil {
		t.Fatalf("ParseMedia live: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries=%d, want 3", len(entries))
	}
	want := []int64{41, 41, 42}
	for i, entry := range entries {
		if !entry.HasMediaSequence || entry.MediaSequence != want[i] {
			t.Fatalf("entry %d sequence=(%t,%d), want (true,%d)", i, entry.HasMediaSequence, entry.MediaSequence, want[i])
		}
	}

	vod, err := ParseMedia(body + "#EXT-X-ENDLIST\n")
	if err != nil {
		t.Fatalf("ParseMedia VOD: %v", err)
	}
	for i, entry := range vod {
		if entry.HasMediaSequence {
			t.Fatalf("VOD entry %d unexpectedly has live sequence %+v", i, entry)
		}
	}

	for _, malformed := range []string{"-1", "not-a-number", "9223372036854775808"} {
		input := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:" + malformed + "\n#EXTINF:2,\nseg.ts\n"
		if _, err := ParseMedia(input); !errors.Is(err, ErrMediaSequence) {
			t.Fatalf("media sequence %q error=%v, want ErrMediaSequence", malformed, err)
		}
	}

	var oversized strings.Builder
	oversized.WriteString("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := 0; i <= MaxLivePlaylistEntries; i++ {
		oversized.WriteString("#EXTINF:1,\nseg.ts\n")
	}
	if _, err := ParseMedia(oversized.String()); !errors.Is(err, ErrLiveWindow) {
		t.Fatalf("oversized live window error=%v, want ErrLiveWindow", err)
	}
}

func TestVODMediaTimeCoordinatesFailClosed(t *testing.T) {
	entries, err := ParseMedia("#EXTM3U\n#EXTINF:4.25,first\na.ts\n#EXTINF:5.75,second\nb.ts\n#EXT-X-ENDLIST\n")
	if err != nil || len(entries) != 2 {
		t.Fatalf("ParseMedia VOD: entries=%+v err=%v", entries, err)
	}
	want := [][2]int64{{0, 4250 * int64(time.Millisecond)}, {4250 * int64(time.Millisecond), 10 * int64(time.Second)}}
	for i, entry := range entries {
		if !entry.HasMediaTime || entry.MediaStartNS != want[i][0] || entry.MediaEndNS != want[i][1] {
			t.Fatalf("entry %d media time=(%t,%d,%d), want (true,%d,%d)", i, entry.HasMediaTime, entry.MediaStartNS, entry.MediaEndNS, want[i][0], want[i][1])
		}
		if !entry.HasMediaDuration || entry.MediaDurationNS != 10*int64(time.Second) {
			t.Fatalf("entry %d VOD duration=(%t,%d), want (true,%d)", i, entry.HasMediaDuration, entry.MediaDurationNS, 10*int64(time.Second))
		}
	}

	live, err := ParseMedia("#EXTM3U\n#EXTINF:4,\na.ts\n")
	if err != nil || len(live) != 1 || live[0].HasMediaTime || live[0].HasMediaDuration {
		t.Fatalf("live playlist must abstain: entries=%+v err=%v", live, err)
	}
	malformed, err := ParseMedia("#EXTM3U\n#EXTINF:not-a-duration,\na.ts\n#EXTINF:4,\nb.ts\n#EXT-X-ENDLIST\n")
	if err != nil || len(malformed) != 2 || malformed[0].HasMediaTime || malformed[1].HasMediaTime ||
		malformed[0].HasMediaDuration || malformed[1].HasMediaDuration {
		t.Fatalf("ambiguous VOD timeline must abstain: entries=%+v err=%v", malformed, err)
	}
}

const lowLatencyFixture = `#EXTM3U
#EXT-X-VERSION:10
#EXT-X-TARGETDURATION:4
#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,CAN-SKIP-UNTIL=24.0
#EXT-X-PART-INF:PART-TARGET=1.0
#EXT-X-MEDIA-SEQUENCE:100
#EXT-X-SKIP:SKIPPED-SEGMENTS=2
#EXT-X-MAP:URI="init.mp4"
#EXT-X-PART:DURATION=1.0,URI="part102.0.m4s",INDEPENDENT=YES,BYTERANGE="100@0"
#EXT-X-PART:DURATION=1.0,URI="part102.1.m4s",BYTERANGE="120"
#EXTINF:4.0,
segment102.m4s
#EXT-X-PART:DURATION=1.0,URI="part103.0.m4s"
#EXT-X-PRELOAD-HINT:TYPE=PART,URI="part103.1.m4s",BYTERANGE-START=0
#EXT-X-PRELOAD-HINT:TYPE=MAP,URI="init-next.mp4"
#EXT-X-RENDITION-REPORT:URI="../audio/live.m3u8",LAST-MSN=103,LAST-PART=0
#EXT-X-RENDITION-REPORT:URI="../low/live.m3u8",LAST-MSN=103,LAST-PART=0
`

func TestLowLatencyRewriteCoordinatesAndDeltaIdentity(t *testing.T) {
	if !IsMedia("#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-PART:DURATION=1,URI=\"p.m4s\"\n") {
		t.Fatal("part-only LL-HLS playlist was not recognized as media")
	}
	entries, err := ParseMedia(lowLatencyFixture)
	if err != nil {
		t.Fatalf("ParseMedia LL-HLS: %v", err)
	}
	if len(entries) != 9 {
		t.Fatalf("entries=%d, want 9: %+v", len(entries), entries)
	}
	assertCoord := func(i int, kind hlssession.ResourceKind, msn int64, part int, hasPart bool) {
		t.Helper()
		got := entries[i]
		if got.Kind != kind || !got.HasMediaSequence || got.MediaSequence != msn ||
			got.HasPartIndex != hasPart || (hasPart && got.PartIndex != part) {
			t.Fatalf("entry %d=%+v, want kind=%s msn=%d part=(%t,%d)", i, got, kind, msn, hasPart, part)
		}
	}
	assertCoord(0, hlssession.KindMap, 102, 0, true)
	assertCoord(1, hlssession.KindSegment, 102, 0, true)
	assertCoord(2, hlssession.KindSegment, 102, 1, true)
	assertCoord(3, hlssession.KindSegment, 102, 0, false)
	assertCoord(4, hlssession.KindSegment, 103, 0, true)
	assertCoord(5, hlssession.KindSegment, 103, 1, true)
	assertCoord(6, hlssession.KindMap, 103, 1, true)
	for i, report := range entries[7:] {
		if report.Kind != hlssession.KindMedia || report.HasMediaSequence ||
			!report.HasRenditionIndex || report.RenditionIndex != i {
			t.Fatalf("rendition report %d=%+v", i, report)
		}
	}

	rewritten, err := RewriteMedia(lowLatencyFixture, func(ordinal int, _ Entry) (string, error) {
		return "/hls/opaque-" + itoa(ordinal), nil
	})
	if err != nil {
		t.Fatalf("RewriteMedia LL-HLS: %v", err)
	}
	for _, leaked := range []string{
		"init.mp4", "part102.0.m4s", "part102.1.m4s", "segment102.m4s",
		"part103.0.m4s", "part103.1.m4s", "init-next.mp4", "../audio/", "../low/",
	} {
		if strings.Contains(rewritten, leaked) {
			t.Fatalf("rewritten LL-HLS leaked %q:\n%s", leaked, rewritten)
		}
	}
	for _, preserved := range []string{
		"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,CAN-SKIP-UNTIL=24.0",
		"#EXT-X-SKIP:SKIPPED-SEGMENTS=2",
		`INDEPENDENT=YES,BYTERANGE="100@0"`,
		"LAST-MSN=103,LAST-PART=0",
	} {
		if !strings.Contains(rewritten, preserved) {
			t.Fatalf("LL-HLS attribute/control tag %q changed:\n%s", preserved, rewritten)
		}
	}

	full, err := ParseMedia("#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-MEDIA-SEQUENCE:102\n#EXT-X-PART:DURATION=1,URI=\"part.m4s\"\n")
	if err != nil {
		t.Fatalf("ParseMedia full LL-HLS: %v", err)
	}
	if full[0].MediaSequence != entries[1].MediaSequence || full[0].PartIndex != entries[1].PartIndex {
		t.Fatalf("delta/full coordinates differ: delta=%+v full=%+v", entries[1], full[0])
	}
}

func TestLowLatencyMalformedTagsFailClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
		err  error
	}{
		{"part missing URI", "#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-PART:DURATION=1\n", ErrLowLatencyTag},
		{"part missing duration", "#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-PART:URI=\"p\"\n", ErrLowLatencyTag},
		{"part bad range", "#EXTM3U\n#EXT-X-PART:DURATION=1,URI=\"p\",BYTERANGE=\"x\"\n", ErrLowLatencyTag},
		{"preload bad type", "#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-PRELOAD-HINT:TYPE=OTHER,URI=\"p\"\n", ErrLowLatencyTag},
		{"preload bad start", "#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"p\",BYTERANGE-START=-1\n", ErrLowLatencyTag},
		{"report missing URI", "#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-RENDITION-REPORT:LAST-MSN=1\n", ErrLowLatencyTag},
		{"skip missing count", "#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-SKIP:FOO=1\n", ErrDeltaUpdate},
		{"skip duplicate", "#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-SKIP:SKIPPED-SEGMENTS=1\n#EXT-X-SKIP:SKIPPED-SEGMENTS=1\n", ErrDeltaUpdate},
		{"skip overflow", "#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-MEDIA-SEQUENCE:9223372036854775807\n#EXT-X-SKIP:SKIPPED-SEGMENTS=1\n", ErrDeltaUpdate},
		{"skip on VOD", "#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-SKIP:SKIPPED-SEGMENTS=1\n#EXT-X-ENDLIST\n", ErrDeltaUpdate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMedia(tc.body); !errors.Is(err, tc.err) {
				t.Fatalf("error=%v, want %v", err, tc.err)
			}
		})
	}
}

const masterFixture = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,URI="audio/en/index.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",URI="subs/en.m3u8?tok=abc"
#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID="cc",NAME="English",INSTREAM-ID="CC1"
#EXT-X-STREAM-INF:BANDWIDTH=1280000,RESOLUTION=640x360,AUDIO="aac",SUBTITLES="subs"
low/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2560000,RESOLUTION=1280x720,AUDIO="aac",SUBTITLES="subs"
https://cdn.example.test/hi/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5120000,RESOLUTION=1920x1080,AUDIO="aac",SUBTITLES="subs"
../hi2/index.m3u8
`

func TestParseMasterOrderAndKinds(t *testing.T) {
	entries, err := ParseMaster(masterFixture)
	if err != nil {
		t.Fatalf("ParseMaster: %v", err)
	}
	// Document order: EXT-X-MEDIA(audio), EXT-X-MEDIA(subs) [CC has no URI,
	// skipped], then the three EXT-X-STREAM-INF variants.
	wantKinds := []hlssession.ResourceKind{
		hlssession.KindMedia, hlssession.KindSubtitle,
		hlssession.KindMedia, hlssession.KindMedia, hlssession.KindMedia,
	}
	if len(entries) != len(wantKinds) {
		t.Fatalf("got %d entries, want %d: %+v", len(entries), len(wantKinds), entries)
	}
	for i, k := range wantKinds {
		if entries[i].Kind != k {
			t.Errorf("entry %d: kind = %q, want %q (uri=%q)", i, entries[i].Kind, k, entries[i].URI)
		}
	}
	if entries[0].URI != "audio/en/index.m3u8" {
		t.Errorf("entry 0 uri = %q", entries[0].URI)
	}
	if entries[2].URI != "low/index.m3u8" {
		t.Errorf("entry 2 (first variant) uri = %q", entries[2].URI)
	}
	if entries[3].URI != "https://cdn.example.test/hi/index.m3u8" {
		t.Errorf("entry 3 (absolute variant) uri = %q", entries[3].URI)
	}
	if entries[4].URI != "../hi2/index.m3u8" {
		t.Errorf("entry 4 (relative-parent variant) uri = %q", entries[4].URI)
	}
}

func TestRewriteMasterPreservesUnrelatedContentAndSubstitutesURIsOnly(t *testing.T) {
	var got []Entry
	rewritten, err := RewriteMaster(masterFixture, func(ordinal int, e Entry) (string, error) {
		got = append(got, e)
		return "/hls/rz" + strings.Repeat("0", 10) + itoa(ordinal), nil
	})
	if err != nil {
		t.Fatalf("RewriteMaster: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("mint called %d times, want 5", len(got))
	}
	if !strings.Contains(rewritten, `URI="/hls/rz00000000000"`) {
		t.Errorf("audio rendition URI not substituted:\n%s", rewritten)
	}
	if strings.Contains(rewritten, "low/index.m3u8") || strings.Contains(rewritten, "cdn.example.test") {
		t.Errorf("original variant URIs leaked into rewritten output:\n%s", rewritten)
	}
	// Every attribute on untouched tags must survive byte-identical.
	if !strings.Contains(rewritten, `BANDWIDTH=1280000,RESOLUTION=640x360`) {
		t.Errorf("unrelated EXT-X-STREAM-INF attributes were altered:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, `INSTREAM-ID="CC1"`) {
		t.Errorf("closed-captions tag (no URI) must pass through unchanged:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, "#EXT-X-VERSION:6") {
		t.Errorf("unrelated tag lost:\n%s", rewritten)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := "0123456789"
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}

const mediaFixture = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:6
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-KEY:METHOD=AES-128,URI="key.bin?tok=xyz",IV=0x00000000000000000000000000000001
#EXT-X-MAP:URI="init.mp4",BYTERANGE="800@0"
#EXT-X-BYTERANGE:500000@1000
#EXTINF:6.000,
seg0.m4s
#EXT-X-DISCONTINUITY
#EXTINF:6.000,
seg1.m4s
#EXT-X-KEY:METHOD=NONE
#EXTINF:6.000,
seg2.m4s
#EXT-X-ENDLIST
`

func TestParseMediaOrderKindsAndByteRanges(t *testing.T) {
	entries, err := ParseMedia(mediaFixture)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	// order: KEY(aes), MAP, seg0(byterange), seg1(whole; no EXT-X-BYTERANGE
	// preceded it), seg2(whole; the second EXT-X-KEY has METHOD=NONE so
	// that tag itself is skipped, but the seg2 segment line it precedes
	// still counts).
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5: %+v", len(entries), entries)
	}
	if entries[0].Kind != hlssession.KindKey || entries[0].URI != "key.bin?tok=xyz" {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[1].Kind != hlssession.KindMap || entries[1].URI != "init.mp4" {
		t.Errorf("entry 1 = %+v", entries[1])
	}
	if entries[1].ByteStart != 0 || entries[1].ByteEnd != 799 {
		t.Errorf("map byte range = [%d,%d], want [0,799]", entries[1].ByteStart, entries[1].ByteEnd)
	}
	if entries[2].Kind != hlssession.KindSegment || entries[2].URI != "seg0.m4s" {
		t.Errorf("entry 2 = %+v", entries[2])
	}
	if entries[2].ByteStart != 1000 || entries[2].ByteEnd != 501000-1 {
		t.Errorf("seg0 byte range = [%d,%d]", entries[2].ByteStart, entries[2].ByteEnd)
	}
	wholeBS, wholeBE := hlssession.NewWholeReference()
	if entries[3].ByteStart != wholeBS || entries[3].ByteEnd != wholeBE {
		t.Errorf("seg1 expected whole-resource sentinel, got [%d,%d]", entries[3].ByteStart, entries[3].ByteEnd)
	}
	if entries[3].URI != "seg1.m4s" {
		t.Errorf("entry 3 = %+v", entries[3])
	}
	if entries[4].Kind != hlssession.KindSegment || entries[4].URI != "seg2.m4s" {
		t.Errorf("entry 4 = %+v", entries[4])
	}
	if entries[4].ByteStart != wholeBS || entries[4].ByteEnd != wholeBE {
		t.Errorf("seg2 expected whole-resource sentinel, got [%d,%d]", entries[4].ByteStart, entries[4].ByteEnd)
	}
}

func TestRewriteMediaPassesThroughDiscontinuityAndUnknownTags(t *testing.T) {
	rewritten, err := RewriteMedia(mediaFixture, func(ordinal int, e Entry) (string, error) {
		return "/hls/leaf" + itoa(ordinal), nil
	})
	if err != nil {
		t.Fatalf("RewriteMedia: %v", err)
	}
	if !strings.Contains(rewritten, "#EXT-X-DISCONTINUITY") {
		t.Errorf("discontinuity tag lost:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, "#EXT-X-ENDLIST") {
		t.Errorf("endlist tag lost:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, `IV=0x00000000000000000000000000000001`) {
		t.Errorf("EXT-X-KEY IV attribute lost:\n%s", rewritten)
	}
	if strings.Contains(rewritten, "seg0.m4s") || strings.Contains(rewritten, "key.bin") || strings.Contains(rewritten, "init.mp4") {
		t.Errorf("original URIs leaked into rewritten output:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, "#EXT-X-KEY:METHOD=NONE") {
		t.Errorf("METHOD=NONE key tag must pass through unrewritten:\n%s", rewritten)
	}
}

func TestResolveURIRefRelativeAbsoluteAndQueryInheritance(t *testing.T) {
	cases := []struct {
		name, base, ref, want string
	}{
		{
			name: "relative sibling, no query anywhere",
			base: "https://cdn.example.test/vod/master.m3u8",
			ref:  "low/index.m3u8",
			want: "https://cdn.example.test/vod/low/index.m3u8",
		},
		{
			name: "absolute cross-origin ref is used as-is",
			base: "https://cdn.example.test/vod/master.m3u8",
			ref:  "https://other.example.test/x/index.m3u8",
			want: "https://other.example.test/x/index.m3u8",
		},
		{
			name: "relative parent-dir traversal",
			base: "https://cdn.example.test/vod/hi/master.m3u8",
			ref:  "../hi2/index.m3u8",
			want: "https://cdn.example.test/vod/hi2/index.m3u8",
		},
		{
			name: "query-token inheritance: same host, ref has no query",
			base: "https://cdn.example.test/vod/master.m3u8?tok=abc123",
			ref:  "low/index.m3u8",
			want: "https://cdn.example.test/vod/low/index.m3u8?tok=abc123",
		},
		{
			name: "ref's own query is never overwritten by inheritance",
			base: "https://cdn.example.test/vod/master.m3u8?tok=abc123",
			ref:  "low/index.m3u8?tok=child",
			want: "https://cdn.example.test/vod/low/index.m3u8?tok=child",
		},
		{
			name: "no inheritance across a different absolute host",
			base: "https://cdn.example.test/vod/master.m3u8?tok=abc123",
			ref:  "https://other.example.test/low/index.m3u8",
			want: "https://other.example.test/low/index.m3u8",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveURIRef(c.base, c.ref)
			if err != nil {
				t.Fatalf("ResolveURIRef: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseMasterRejectsNonPlaylist(t *testing.T) {
	if _, err := ParseMaster("not a playlist at all"); err != ErrNotPlaylist {
		t.Fatalf("got err %v, want ErrNotPlaylist", err)
	}
}

func TestParseMediaRejectsNonPlaylist(t *testing.T) {
	if _, err := ParseMedia("#EXT-X-STREAM-INF:BANDWIDTH=1\nfoo.m3u8\n"); err != ErrNotPlaylist {
		t.Fatalf("got err %v, want ErrNotPlaylist", err)
	}
}

func TestParseMasterRejectsOversized(t *testing.T) {
	huge := "#EXT-X-STREAM-INF:BANDWIDTH=1\n" + strings.Repeat("x", MaxPlaylistBytes+1)
	if _, err := ParseMaster(huge); err != ErrOversized {
		t.Fatalf("got err %v, want ErrOversized", err)
	}
}

func TestParseMasterRejectsTruncatedStreamInf(t *testing.T) {
	truncated := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n"
	if _, err := ParseMaster(truncated); err != ErrTruncatedTag {
		t.Fatalf("got err %v, want ErrTruncatedTag", err)
	}
}

func TestRewriteMasterMintErrorPropagates(t *testing.T) {
	wantErr := ErrOversized // reused as a distinguishable sentinel for this test
	_, err := RewriteMaster(masterFixture, func(int, Entry) (string, error) {
		return "", wantErr
	})
	if err != wantErr {
		t.Fatalf("got err %v, want %v", err, wantErr)
	}
}

func TestIdempotentOrdinalNumberingAcrossReparse(t *testing.T) {
	// Parsing the identical content twice must produce identical ordinals
	// and kinds — the property deriveHLSLiveURL relies on to relocate a
	// resource deterministically after a restart with no cached state.
	first, err := ParseMedia(mediaFixture)
	if err != nil {
		t.Fatalf("first ParseMedia: %v", err)
	}
	second, err := ParseMedia(mediaFixture)
	if err != nil {
		t.Fatalf("second ParseMedia: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("entry count differs across reparse: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("entry %d differs across reparse: %+v vs %+v", i, first[i], second[i])
		}
	}
}
