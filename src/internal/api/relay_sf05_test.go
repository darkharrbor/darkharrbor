package api

import "testing"

func TestParseDavRelayPath(t *testing.T) {
	cases := []struct {
		url  string
		ok   bool
		cat  string
		rel  string
		file string
	}{
		{"http://darkharrbor:8381/dav/tv-modern/Rel.Name/Rel.Name.mkv", true, "tv-modern", "Rel.Name", "Rel.Name.mkv"},
		{"http://darkharrbor:8381/dav/tv/Rel/Some%20File.mkv", true, "tv", "Rel", "Some File.mkv"},
		{"http://darkharrbor:8381/stream/abc/0?tok=x", false, "", "", ""},
		{"http://darkharrbor:8381/dav/", false, "", "", ""},
		{"http://darkharrbor:8381/dav/onlycat/rel", false, "", "", ""},
		{"http://darkharrbor:8381/dav/a//f.mkv", false, "", "", ""},
		{"http://darkharrbor:8381/dav/../etc/passwd", false, "", "", ""},
		{"http://darkharrbor:8381/dav/tv/../secret/f.mkv", false, "", "", ""},
		{"://bad", false, "", "", ""},
	}
	for _, c := range cases {
		cat, rel, file, ok := parseDavRelayPath(c.url)
		if ok != c.ok || cat != c.cat || rel != c.rel || file != c.file {
			t.Fatalf("parseDavRelayPath(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				c.url, cat, rel, file, ok, c.cat, c.rel, c.file, c.ok)
		}
	}
}

// FuzzParseDavRelayPath: the parser must never panic and, when it accepts,
// must return three non-empty parts that pass the WebDAV validation.
func FuzzParseDavRelayPath(f *testing.F) {
	f.Add("http://h/dav/a/b/c.mkv")
	f.Add("http://h/dav/../x/y")
	f.Add("/dav/%2e%2e/a/b")
	f.Fuzz(func(t *testing.T, raw string) {
		cat, rel, file, ok := parseDavRelayPath(raw)
		if ok {
			if cat == "" || rel == "" || file == "" {
				t.Fatalf("accepted with empty part: %q -> (%q,%q,%q)", raw, cat, rel, file)
			}
			if !validateWebDAVPathParts([]string{cat, rel, file}) {
				t.Fatalf("accepted parts failing webdav validation: %q", raw)
			}
		}
	})
}
