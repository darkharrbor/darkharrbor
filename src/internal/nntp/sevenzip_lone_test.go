package nntp

import "testing"

func TestIsSevenZipSidecarSubject(t *testing.T) {
	for subject, want := range map[string]bool{
		`[1/1] - "6ih3mw6msirtw3yds354" yEnc (1/1156)`:           false,
		`[2/5] - "Show.S03E25-AndreMor.vol-02.par2" yEnc (1/17)`: true,
		`"Show.nfo" yEnc (1/1)`:                                  true,
		`"Show.S03E25.7z" yEnc (1/900)`:                          false,
		`Show.sfv`:                                               true,
	} {
		if got := isSevenZipSidecarSubject(subject); got != want {
			t.Errorf("%q: got %v want %v", subject, got, want)
		}
	}
}
