package suppress

import "testing"

func TestFingerprint_NormalizesCasePunctuationSpacing(t *testing.T) {
	a := Fingerprint("The.Show.S01E01.720p.WEB-DL", 1000, LaneNZB)
	b := Fingerprint("the show s01e01 720p web dl", 1000, LaneNZB)
	c := Fingerprint("THE SHOW S01E01 720P WEB DL!!!", 1000, LaneNZB)
	if a != b || b != c {
		t.Fatalf("expected identical fingerprints for cosmetically-different names, got %q %q %q", a, b, c)
	}
}

func TestFingerprint_DifferentSizeDifferentFingerprint(t *testing.T) {
	a := Fingerprint("The Show S01E01", 1000, LaneNZB)
	b := Fingerprint("The Show S01E01", 2000, LaneNZB)
	if a == b {
		t.Fatal("expected different fingerprints for different sizes")
	}
}

func TestFingerprint_DifferentLaneDifferentFingerprint(t *testing.T) {
	a := Fingerprint("The Show S01E01", 1000, LaneNZB)
	b := Fingerprint("The Show S01E01", 1000, LaneTorrent)
	c := Fingerprint("The Show S01E01", 1000, LaneHTTP)
	if a == b || b == c || a == c {
		t.Fatal("expected different fingerprints for different lanes, same name/size")
	}
}

func TestFingerprint_UnknownSizeIsStableAndCoarser(t *testing.T) {
	a := Fingerprint("The Show S01E01", 0, LaneNZB)
	b := Fingerprint("The Show S01E01", -5, LaneNZB)
	if a != b {
		t.Fatal("expected zero and negative size to fold to the same 'unknown size' fingerprint")
	}
	known := Fingerprint("The Show S01E01", 1000, LaneNZB)
	if a == known {
		t.Fatal("unknown-size fingerprint must not collide with a known-size one")
	}
}

func TestFingerprint_DifferentNameDifferentFingerprint(t *testing.T) {
	a := Fingerprint("The Show S01E01", 1000, LaneNZB)
	b := Fingerprint("The Show S01E02", 1000, LaneNZB)
	if a == b {
		t.Fatal("expected different fingerprints for different episode titles")
	}
}
