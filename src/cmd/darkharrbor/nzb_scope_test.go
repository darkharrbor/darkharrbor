package main

import "testing"

func TestNZBSubjectFileName(t *testing.T) {
	for in, want := range map[string]string{
		`[01/20] - "Show.S01E02.1080p.mkv" yEnc (1/300)`: "Show.S01E02.1080p.mkv",
		`Show.S01E02.mkv`: "Show.S01E02.mkv",
		`"unterminated`:   `"unterminated`,
	} {
		if got := nzbSubjectFileName(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	if countFalse([]bool{true, false, false}) != 2 {
		t.Error("countFalse")
	}
}
