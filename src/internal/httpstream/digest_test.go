package httpstream

import (
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestValidSourceDigest(t *testing.T) {
	cases := []struct {
		name string
		d    SourceDigest
		want bool
	}{
		{"valid md5", SourceDigest{Algorithm: "md5", Hex: "d41d8cd98f00b204e9800998ecf8427e"}, true},
		{"valid sha1", SourceDigest{Algorithm: "sha1", Hex: "da39a3ee5e6b4b0d3255bfef95601890afd80709"}, true},
		{"valid sha256", SourceDigest{Algorithm: "sha256", Hex: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"[:64]}, true},
		{"unknown algorithm", SourceDigest{Algorithm: "crc32", Hex: "deadbeef"}, false},
		{"wrong length", SourceDigest{Algorithm: "md5", Hex: "abcd"}, false},
		{"uppercase rejected", SourceDigest{Algorithm: "md5", Hex: "D41D8CD98F00B204E9800998ECF8427E"}, false},
		{"non-hex chars", SourceDigest{Algorithm: "md5", Hex: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}, false},
		{"empty", SourceDigest{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidSourceDigest(tc.d); got != tc.want {
				t.Fatalf("ValidSourceDigest(%+v) = %v, want %v", tc.d, got, tc.want)
			}
		})
	}
}

func TestParseDigestHeader(t *testing.T) {
	sha256Raw := make([]byte, 32)
	md5Raw := make([]byte, 16)
	for i := range sha256Raw {
		sha256Raw[i] = byte(i)
	}
	for i := range md5Raw {
		md5Raw[i] = byte(i + 1)
	}
	header := "sha-256=" + base64.StdEncoding.EncodeToString(sha256Raw) +
		", md5=" + base64.StdEncoding.EncodeToString(md5Raw)

	got := ParseDigestHeader(header)
	if len(got) != 2 {
		t.Fatalf("expected 2 parsed digests, got %d: %+v", len(got), got)
	}
	byAlg := map[string]SourceDigest{}
	for _, d := range got {
		if !ValidSourceDigest(d) {
			t.Fatalf("ParseDigestHeader returned an invalid digest: %+v", d)
		}
		byAlg[d.Algorithm] = d
	}
	if byAlg["sha256"].Hex != hex.EncodeToString(sha256Raw) {
		t.Fatalf("sha256 digest = %q, want %q", byAlg["sha256"].Hex, hex.EncodeToString(sha256Raw))
	}
	if byAlg["md5"].Hex != hex.EncodeToString(md5Raw) {
		t.Fatalf("md5 digest = %q, want %q", byAlg["md5"].Hex, hex.EncodeToString(md5Raw))
	}
}

func TestParseDigestHeaderMalformedEntriesSkipped(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"not-a-digest-header",
		"sha-256=",
		"=aGVsbG8=",
		"unknown-alg=aGVsbG8=",
		"sha-256=not-valid-base64!!!",
		",,,",
	}
	for _, c := range cases {
		if got := ParseDigestHeader(c); got != nil {
			t.Fatalf("ParseDigestHeader(%q) = %+v, want nil", c, got)
		}
	}
}

func TestParseDigestHeaderBoundedLength(t *testing.T) {
	huge := make([]byte, maxDigestHeaderBytes+100)
	for i := range huge {
		huge[i] = 'a'
	}
	if got := ParseDigestHeader(string(huge)); got != nil {
		t.Fatalf("oversized Digest header must be rejected outright, got %+v", got)
	}
}

// FuzzParseDigestHeader exercises the RFC 3230 Digest header parser against
// arbitrary attacker/misconfigured-origin-controlled bytes (DG-03's
// standing "every new parser ships a committed fuzz target" convention). It
// must never panic and every returned digest must be well-formed.
func FuzzParseDigestHeader(f *testing.F) {
	seeds := []string{
		"",
		"sha-256=aGVsbG8=",
		"md5=aGVsbG8=, sha-256=aGVsbG8=",
		"not a digest header at all",
		"=====",
		"sha-256=" + string([]byte{0x00, 0xff, 0xfe}),
		",,,,,,,,,,",
		"sha-256=aGVsbG8=,,,,,,,,,,,,,,,,,,,,",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ParseDigestHeader panicked on %q: %v", s, r)
			}
		}()
		digests := ParseDigestHeader(s)
		if len(digests) > maxDigestHeaderEntries {
			t.Fatalf("ParseDigestHeader returned %d entries, bound is %d", len(digests), maxDigestHeaderEntries)
		}
		for _, d := range digests {
			if !ValidSourceDigest(d) {
				t.Fatalf("ParseDigestHeader returned an invalid digest %+v for input %q", d, s)
			}
		}
	})
}
