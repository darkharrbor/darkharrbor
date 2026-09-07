package availability

import "testing"

func TestNormalizeProvider_LowercasesAndTrims(t *testing.T) {
	got, ok := NormalizeProvider("  TorBox  ")
	if !ok || got != "torbox" {
		t.Fatalf("expected (\"torbox\", true), got (%q, %v)", got, ok)
	}
}

func TestNormalizeProvider_RefusesEmpty(t *testing.T) {
	if _, ok := NormalizeProvider("   "); ok {
		t.Fatal("expected empty provider to be refused")
	}
}

func TestNormalizeProvider_RefusesOversized(t *testing.T) {
	big := make([]byte, maxProviderBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if _, ok := NormalizeProvider(string(big)); ok {
		t.Fatal("expected oversized provider name to be refused")
	}
}

func TestNormalizeHash_LowercasesAndTrims(t *testing.T) {
	in := "  ABCDEF0123456789ABCDEF0123456789ABCDEF01  "
	got, ok := NormalizeHash(in)
	if !ok {
		t.Fatalf("expected valid hex hash to normalize, got ok=false")
	}
	want := "abcdef0123456789abcdef0123456789abcdef01"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestNormalizeHash_RefusesEmpty(t *testing.T) {
	if _, ok := NormalizeHash(""); ok {
		t.Fatal("expected empty hash to be refused")
	}
}

func TestNormalizeHash_RefusesNonHex(t *testing.T) {
	if _, ok := NormalizeHash("not-a-hex-hash!!"); ok {
		t.Fatal("expected non-hex input to be refused")
	}
}

func TestNormalizeHash_RefusesOversized(t *testing.T) {
	big := make([]byte, maxHashBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if _, ok := NormalizeHash(string(big)); ok {
		t.Fatal("expected oversized hash to be refused")
	}
}

func TestNormalizeHash_AcceptsV1AndV2Lengths(t *testing.T) {
	v1 := "0123456789abcdef0123456789abcdef01234567"                         // 40 hex chars (SHA-1)
	v2 := "0123456789abcdef0123456789abcdeffedcba9876543210fedcba9876543210" // 64 hex chars (SHA-256)
	if _, ok := NormalizeHash(v1); !ok {
		t.Fatal("expected v1-length hash to be accepted")
	}
	if _, ok := NormalizeHash(v2); !ok {
		t.Fatal("expected v2-length hash to be accepted")
	}
}

func TestValidSource(t *testing.T) {
	for _, s := range []Source{SourceSubmit, SourceStream, SourcePreflight} {
		if !ValidSource(s) {
			t.Fatalf("expected %q to be a valid source", s)
		}
	}
	if ValidSource(Source("bogus")) {
		t.Fatal("expected unrecognized source to be invalid")
	}
}
