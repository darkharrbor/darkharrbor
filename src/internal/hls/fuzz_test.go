package hls

import "testing"

// FuzzParsePlaylist is the DG-03 fuzz target for the new M3U8 parser: no
// input, however malformed, truncated, or adversarial, may panic ParseMaster,
// ParseMedia, or the Rewrite* walkers they share. A crash here is the only
// failure mode this target checks for; parse errors themselves are an
// entirely expected, correctly-handled outcome for malformed seed input.
func FuzzParsePlaylist(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"#EXTM3U\n",
		masterFixture,
		mediaFixture,
		"#EXT-X-STREAM-INF:BANDWIDTH=1\n",
		"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\nunterminated",
		"#EXT-X-MEDIA:TYPE=AUDIO,URI=\"\n",
		"#EXT-X-MEDIA:TYPE=SUBTITLES,URI=\"x\"\n#EXT-X-STREAM-INF:BANDWIDTH=1\nfoo\n",
		"#EXTINF:6,\n#EXT-X-BYTERANGE:notanumber\nseg.ts\n",
		"#EXTINF:6,\n#EXT-X-BYTERANGE:@5\nseg.ts\n",
		"#EXTINF:6,\n#EXT-X-MAP:URI=\"m\",BYTERANGE=\"x@y\"\nseg.ts\n",
		"#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:6,\nseg.ts\n",
		"#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:41\n#EXTINF:2,\nseg41.ts\n#EXTINF:2,\nseg42.ts\n",
		"#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:9223372036854775808\n#EXTINF:2,\nseg.ts\n",
		"#EXTM3U\n#EXTINF:not-a-duration,\nseg.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXTINF:1e300,\nseg.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-PART:DURATION=0.000000001,URI=\"p\"\n#EXTINF:1,\nseg.ts\n#EXT-X-ENDLIST\n",
		lowLatencyFixture,
		"#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-MEDIA-SEQUENCE:7\n#EXT-X-PART:DURATION=1,URI=\"p\",BYTERANGE=\"10@0\"\n#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"n\",BYTERANGE-START=10\n#EXT-X-RENDITION-REPORT:URI=\"r.m3u8\",LAST-MSN=7,LAST-PART=0\n",
		"#EXTM3U\n#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-SKIP:SKIPPED-SEGMENTS=not-a-number\n#EXT-X-PART:DURATION=1,URI=\"p\"\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		mint := func(ordinal int, e Entry) (string, error) { return "/hls/rz", nil }

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ParseMaster/RewriteMaster panicked on input %q: %v", body, r)
				}
			}()
			_, _ = ParseMaster(body)
			_, _ = RewriteMaster(body, mint)
		}()

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ParseMedia/RewriteMedia panicked on input %q: %v", body, r)
				}
			}()
			_, _ = ParseMedia(body)
			_, _ = RewriteMedia(body, mint)
		}()

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ResolveURIRef panicked on input %q: %v", body, r)
				}
			}()
			_, _ = ResolveURIRef("https://example.test/a/b.m3u8?x=1", body)
		}()
	})
}

// FuzzParseSteeringManifest covers the new bounded JSON trust boundary and
// its synthesized, secret-free response.
func FuzzParseSteeringManifest(f *testing.F) {
	f.Add([]byte(`{"VERSION":1,"TTL":300,"PATHWAY-PRIORITY":["A","B"]}`))
	f.Add([]byte(`{"VERSION":1,"TTL":1,"PATHWAY-PRIORITY":[]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		manifest, err := ParseSteeringManifest(body)
		if err != nil {
			return
		}
		candidates := make([]PathwayCandidate, 0, len(manifest.PathwayPriority))
		for _, id := range manifest.PathwayPriority {
			candidates = append(candidates, PathwayCandidate{ID: id, OriginSupplied: true})
		}
		if _, err := SynthesizeSteeringManifest(manifest, candidates, nil); err != nil {
			t.Fatalf("valid steering manifest failed synthesis: %v", err)
		}
	})
}
