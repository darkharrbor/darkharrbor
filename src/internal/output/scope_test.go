package output

import (
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

func names(fs []provider.CachedFile) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

func files(ns ...string) ([]provider.CachedFile, []*torrentmeta.FileEntry) {
	v := make([]provider.CachedFile, len(ns))
	for i, n := range ns {
		v[i] = provider.CachedFile{Name: n}
	}
	return v, make([]*torrentmeta.FileEntry, len(ns))
}

func TestScopeVideoToReleaseTitle(t *testing.T) {
	series := []string{"How.I.Met.Your.Mother.S01E04.1080p.mkv", "How.I.Met.Your.Mother.S06E13.1080p.mkv", "How.I.Met.Your.Mother.S06E14.1080p.mkv", "How.I.Met.Your.Mother.S07E08.1080p.mkv"}
	for _, tc := range []struct {
		name, title string
		in          []string
		want        []string
	}{
		// 2026-10-17: a release titled S06 carried all nine seasons.
		{"single-season title trims a whole-series payload", "How I Met Your Mother S06 1080p WEB-DL DD5 1 H 264 HUN ENG-Szabotazs", series,
			[]string{"How.I.Met.Your.Mother.S06E13.1080p.mkv", "How.I.Met.Your.Mother.S06E14.1080p.mkv"}},
		{"single-episode title keeps only that episode", "How.I.Met.Your.Mother.S06E14.1080p.WEB-DL", series,
			[]string{"How.I.Met.Your.Mother.S06E14.1080p.mkv"}},
		{"multi-episode title keeps the range", "SpongeBob SquarePants S17E19E20 1080p", []string{"SpongeBob.S17E18.mkv", "SpongeBob.S17E19.mkv", "SpongeBob.S17E20.mkv"},
			[]string{"SpongeBob.S17E19.mkv", "SpongeBob.S17E20.mkv"}},
		{"NxNN file names are understood", "Show Season 2 1080p", []string{"Show 1x01.mkv", "Show 2x01.mkv", "Show 2x02.mkv"},
			[]string{"Show 2x01.mkv", "Show 2x02.mkv"}},
		{"multi-season title is untouched", "How I Met Your Mother S01-S09 1080p", series, series},
		{"complete series title is untouched", "How I Met Your Mother Complete Series 1080p", series, series},
		{"unparseable title is untouched", "HIMYM 1080p", series, series},
		{"unparseable file names are kept", "Show S02 1080p", []string{"Show.S01E01.mkv", "Show.S02E01.mkv", "extras-featurette.mkv"},
			[]string{"Show.S02E01.mkv", "extras-featurette.mkv"}},
		{"never filters to nothing", "Show S05 1080p", []string{"Show.S01E01.mkv", "Show.S02E01.mkv"},
			[]string{"Show.S01E01.mkv", "Show.S02E01.mkv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, tm := files(tc.in...)
			gotV, gotT := scopeVideoToReleaseTitle(tc.title, v, tm)
			if len(gotV) != len(gotT) {
				t.Fatalf("video/tm out of lockstep: %d vs %d", len(gotV), len(gotT))
			}
			got := names(gotV)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
