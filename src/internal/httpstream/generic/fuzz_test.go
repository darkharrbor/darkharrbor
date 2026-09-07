package generic

import (
	"net/url"
	"testing"
)

// FuzzValidateDescriptors exercises the private catalog trust boundary. It
// must never panic or return more than the documented descriptor bound.
func FuzzValidateDescriptors(f *testing.F) {
	seeds := []string{
		`[]`,
		`null`,
		`[{"kind":"fixed","title":"Movie","url":"https://example.com/movie.mkv"}]`,
		`[{"source_id":"movie-primary","kind":"fixed","title":"Movie","url":"https://example.com/movie.mkv"}]`,
		`[{"kind":"fixed","url":"https://safe.invalid/","url":"https://other.invalid/"}]`,
		`[{"kind":"fixed","URL":"https://example.com/movie.mkv"}]`,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		descriptors, _ := ValidateDescriptors([]byte(body))
		if len(descriptors) > maxDescriptors {
			t.Fatalf("ValidateDescriptors returned %d entries, bound is %d", len(descriptors), maxDescriptors)
		}
	})
}

// FuzzParseM3U exercises the M3U/M3U8 parser against arbitrary attacker- or
// misconfigured-origin-controlled bytes (DG-03's standing "every new parser
// ships a committed fuzz target" convention). It must never panic and must
// never return more than the bounded entry count.
func FuzzParseM3U(f *testing.F) {
	seeds := []string{
		"",
		"#EXTM3U\n#EXTINF:-1,Name\nhttps://example.com/a.mp4\n",
		"#EXTM3U\n/relative/a.mp4\n",
		"not a playlist",
		"#EXTM3U\n" + string([]byte{0x00, 0xff, 0xfe}),
		"#EXTINF:-1,\n\n\n",
		"https://[::1",
		"#EXTM3U\r\nhttps://example.com/a.mp4\r\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	base, _ := url.Parse("https://example.com/base/playlist.m3u8")
	f.Fuzz(func(t *testing.T, s string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("parseM3U panicked on %q: %v", s, r)
			}
		}()
		entries := parseM3U([]byte(s), base)
		if len(entries) > maxM3UEntries {
			t.Fatalf("parseM3U returned %d entries, bound is %d", len(entries), maxM3UEntries)
		}
		for _, e := range entries {
			if e.url == "" {
				t.Fatalf("parseM3U returned an entry with an empty URL for input %q", s)
			}
		}
	})
}

// FuzzParseMetalink exercises the RFC 5854 XML parser. It must never panic
// on malformed/adversarial XML and must never exceed the bounded file count.
func FuzzParseMetalink(f *testing.F) {
	seeds := []string{
		"",
		"<not-xml",
		`<?xml version="1.0"?><metalink><file name="a.mkv"><size>10</size><url priority="1">https://example.com/a.mkv</url></file></metalink>`,
		"<metalink><file/></metalink>",
		"<metalink>" + "<file>" + string([]byte{0x00, 0x01}) + "</file></metalink>",
		"<metalink><file name=\"a\"><size>-1</size></file></metalink>",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("parseMetalink panicked on %q: %v", s, r)
			}
		}()
		files, _ := parseMetalink([]byte(s))
		if len(files) > maxMetalinkFiles {
			t.Fatalf("parseMetalink returned %d files, bound is %d", len(files), maxMetalinkFiles)
		}
		for _, fl := range files {
			if fl.bestURL == "" {
				t.Fatalf("parseMetalink returned a file with no URL for input %q", s)
			}
		}
	})
}

// FuzzParseWebDAVMultistatus exercises the RFC 4918 PROPFIND response
// parser. It must never panic on malformed/adversarial XML and must never
// exceed the bounded member count.
func FuzzParseWebDAVMultistatus(f *testing.F) {
	seeds := []string{
		"",
		"<multistatus>",
		`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.mkv</D:href><D:propstat><D:prop><D:getcontentlength>5</D:getcontentlength></D:prop></D:propstat></D:response></D:multistatus>`,
		"<D:multistatus xmlns:D=\"DAV:\"><D:response><D:href>../../etc/passwd</D:href></D:response></D:multistatus>",
		"not xml at all",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	base, _ := url.Parse("https://example.com/dav/")
	f.Fuzz(func(t *testing.T, s string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("parseWebDAVMultistatus panicked on %q: %v", s, r)
			}
		}()
		members, _ := parseWebDAVMultistatus([]byte(s), base)
		if len(members) > maxWebDAVMembers {
			t.Fatalf("parseWebDAVMultistatus returned %d members, bound is %d", len(members), maxWebDAVMembers)
		}
	})
}
