package nntp

import "testing"

func TestIsRARPartSubjectRecognizesRecoveredVolumeNames(t *testing.T) {
	for _, subject := range []string{
		`"release.part01.rar"`,
		`"release.r00"`,
		`"release.r99"`,
	} {
		if !isRARPartSubject(subject) {
			t.Fatalf("subject %q was not recognized as a RAR part", subject)
		}
	}
	for _, subject := range []string{
		`"release.par2"`,
		`"release.vol000+004.par2"`,
		`"release.mkv"`,
	} {
		if isRARPartSubject(subject) {
			t.Fatalf("subject %q was incorrectly recognized as a RAR part", subject)
		}
	}
}

func TestExtractRARPartNumOrdersRecoveredNames(t *testing.T) {
	if first, second := extractRARPartNum(`"release.part01.rar"`), extractRARPartNum(`"release.part02.rar"`); first >= second {
		t.Fatalf("part order = %d, %d", first, second)
	}
	if first, second := extractRARPartNum(`"release.r00"`), extractRARPartNum(`"release.r01"`); first >= second {
		t.Fatalf("legacy part order = %d, %d", first, second)
	}
}
