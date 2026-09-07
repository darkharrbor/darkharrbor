package identitycheck

import (
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
)

func hasReason(verdicts []Verdict, reason Reason) bool {
	for _, v := range verdicts {
		if v.Reason == reason {
			return true
		}
	}
	return false
}

func TestParseStrictness(t *testing.T) {
	cases := map[string]Strictness{
		"off":      StrictnessOff,
		"OFF":      StrictnessOff,
		" off ":    StrictnessOff,
		"warn":     StrictnessWarn,
		"":         StrictnessWarn,
		"enforce":  StrictnessEnforce,
		"ENFORCE":  StrictnessEnforce,
		"bogus":    StrictnessWarn,
		"enforceX": StrictnessWarn,
	}
	for in, want := range cases {
		if got := ParseStrictness(in); got != want {
			t.Errorf("ParseStrictness(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckYearMovie(t *testing.T) {
	movie := &mediaidentity.ProviderIdentity{Kind: "movie", Year: 1994}
	if v := checkYear(movie, "The.Shawshank.Redemption.1994.1080p.BluRay.x264-GROUP"); v != nil {
		t.Fatalf("exact match flagged: %+v", v)
	}
	if v := checkYear(movie, "The.Shawshank.Redemption.1995.1080p.BluRay.x264-GROUP"); v == nil {
		t.Fatalf("mismatched year not flagged")
	}
	if v := checkYear(movie, "The.Shawshank.Redemption.1080p.BluRay.x264-GROUP"); v != nil {
		t.Fatalf("no release year present should abstain, got: %+v", v)
	}
	unknownYear := &mediaidentity.ProviderIdentity{Kind: "movie"}
	if v := checkYear(unknownYear, "Some.Movie.1994.1080p-GROUP"); v != nil {
		t.Fatalf("unknown Arr year should abstain, got: %+v", v)
	}
	ambiguous := &mediaidentity.ProviderIdentity{Kind: "movie", Year: 1994}
	if v := checkYear(ambiguous, "Movie.1994.Extended.Cut.2020.Remaster-GROUP"); v != nil {
		t.Fatalf("ambiguous multi-year title should abstain, got: %+v", v)
	}
}

func TestCheckYearSeries(t *testing.T) {
	series := &mediaidentity.ProviderIdentity{Kind: "series", Year: 2011}
	if v := checkYear(series, "Show.Name.2019.S08E06.1080p.WEB-DL-GROUP"); v != nil {
		t.Fatalf("mid-run season after start year flagged: %+v", v)
	}
	if v := checkYear(series, "Show.Name.2005.S01E01.1080p.WEB-DL-GROUP"); v == nil {
		t.Fatalf("season predating series start not flagged")
	}
	if v := checkYear(series, "Show.Name.2200.S01E01.1080p.WEB-DL-GROUP"); v != nil {
		t.Fatalf("far-future implausible token should be filtered by year token regex, got: %+v", v)
	}
}

func TestCheckRuntime(t *testing.T) {
	movie := &mediaidentity.ProviderIdentity{Kind: "movie", RuntimeMinutes: 142}
	within := &mediatruth.Facts{DurationMS: 142 * 60 * 1000}
	if v := checkRuntime(movie, 0, 0, within); v != nil {
		t.Fatalf("exact runtime flagged: %+v", v)
	}
	tolerated := &mediatruth.Facts{DurationMS: (142 - 10) * 60 * 1000} // within 12min/12% tolerance
	if v := checkRuntime(movie, 0, 0, tolerated); v != nil {
		t.Fatalf("within-tolerance runtime flagged: %+v", v)
	}
	way_off := &mediatruth.Facts{DurationMS: 20 * 60 * 1000}
	if v := checkRuntime(movie, 0, 0, way_off); v == nil {
		t.Fatalf("20-minute file vs 142-minute movie not flagged")
	}
	if v := checkRuntime(movie, 0, 0, nil); v != nil {
		t.Fatalf("nil facts should abstain, got: %+v", v)
	}
	if v := checkRuntime(movie, 0, 0, &mediatruth.Facts{}); v != nil {
		t.Fatalf("zero duration should abstain, got: %+v", v)
	}
	unknownRuntime := &mediaidentity.ProviderIdentity{Kind: "movie"}
	if v := checkRuntime(unknownRuntime, 0, 0, way_off); v != nil {
		t.Fatalf("unknown Arr runtime should abstain, got: %+v", v)
	}
}

func TestCheckRuntimeEpisodeOverride(t *testing.T) {
	series := &mediaidentity.ProviderIdentity{
		Kind: "series", RuntimeMinutes: 22,
		Episodes: []mediaidentity.EpisodeProviderIDs{
			{Season: 1, Episode: 1, RuntimeMinutes: 65}, // series pilot double-length
		},
	}
	pilotLength := &mediatruth.Facts{DurationMS: 65 * 60 * 1000}
	if v := checkRuntime(series, 1, 1, pilotLength); v != nil {
		t.Fatalf("episode-level override should apply, got: %+v", v)
	}
	if v := checkRuntime(series, 1, 1, &mediatruth.Facts{DurationMS: 22 * 60 * 1000}); v == nil {
		t.Fatalf("normal-length pilot should be flagged against 65-minute override")
	}
	// Non-overridden episode falls back to series-level runtime.
	normalLength := &mediatruth.Facts{DurationMS: 22 * 60 * 1000}
	if v := checkRuntime(series, 1, 2, normalLength); v != nil {
		t.Fatalf("fallback to series runtime flagged: %+v", v)
	}
}

func TestCheckAudioLanguage(t *testing.T) {
	facts := &mediatruth.Facts{AudioLangs: []string{"eng", "spa"}}
	if v := checkAudioLanguage("eng", facts); v != nil {
		t.Fatalf("matching language flagged: %+v", v)
	}
	if v := checkAudioLanguage("jpn", facts); v == nil {
		t.Fatalf("non-matching language not flagged")
	}
	if v := checkAudioLanguage("", facts); v != nil {
		t.Fatalf("unknown expected language should abstain, got: %+v", v)
	}
	if v := checkAudioLanguage("eng", nil); v != nil {
		t.Fatalf("nil facts should abstain, got: %+v", v)
	}
	if v := checkAudioLanguage("eng", &mediatruth.Facts{}); v != nil {
		t.Fatalf("no probed audio should abstain, got: %+v", v)
	}
	if v := checkAudioLanguage("english", facts); v != nil {
		t.Fatalf("non-ISO-3 code should abstain rather than guess, got: %+v", v)
	}
}

func TestCheckConflictingSourceTags(t *testing.T) {
	if v := checkConflictingSourceTags("Movie.Name.1994.CAM.x264-GROUP"); v != nil {
		t.Fatalf("CAM alone flagged: %+v", v)
	}
	if v := checkConflictingSourceTags("Movie.Name.1994.BluRay.x264-GROUP"); v != nil {
		t.Fatalf("BluRay alone flagged: %+v", v)
	}
	if v := checkConflictingSourceTags("Movie.Name.1994.CAM.BluRay.x264-GROUP"); v == nil {
		t.Fatalf("CAM+BluRay conflict not flagged")
	}
	if v := checkConflictingSourceTags("Movie.Name.1994.HDTV.WEB-DL.x264-GROUP"); v == nil {
		t.Fatalf("HDTV+WEB-DL conflict not flagged")
	}
	// A decades-old film legitimately remastered onto Blu-ray or streamed as
	// WEB-DL today must never be flagged — no cross-reference to real-world
	// content vintage happens in this check by design.
	if v := checkConflictingSourceTags("Night.of.the.Living.Dead.1968.Criterion.BluRay.REMUX-GROUP"); v != nil {
		t.Fatalf("legitimate vintage remaster flagged: %+v", v)
	}
	if v := checkConflictingSourceTags("Old.Film.1940.WEB-DL.x264-GROUP"); v != nil {
		t.Fatalf("legitimate vintage WEB-DL flagged: %+v", v)
	}
}

func TestVerifyAggregatesAndAbstains(t *testing.T) {
	// Fully unknown identity/facts: every check abstains, zero verdicts.
	if got := Verify(nil, "Some.Release.Title-GROUP", 0, 0, nil, ""); len(got) != 0 {
		t.Fatalf("expected zero verdicts for fully unknown input, got %+v", got)
	}

	identity := &mediaidentity.ProviderIdentity{Kind: "movie", Year: 1994, RuntimeMinutes: 142}
	facts := &mediatruth.Facts{DurationMS: 142 * 60 * 1000, AudioLangs: []string{"eng"}}
	if got := Verify(identity, "Movie.Name.1994.BluRay.x264-GROUP", 0, 0, facts, "eng"); len(got) != 0 {
		t.Fatalf("expected zero verdicts for fully consistent input, got %+v", got)
	}

	badFacts := &mediatruth.Facts{DurationMS: 20 * 60 * 1000, AudioLangs: []string{"jpn"}}
	got := Verify(identity, "Movie.Name.1995.CAM.BluRay.x264-GROUP", 0, 0, badFacts, "eng")
	for _, want := range []Reason{
		ReasonYearMismatch, ReasonRuntimeMismatch, ReasonAudioLanguageMismatch, ReasonConflictingSourceTags,
	} {
		if !hasReason(got, want) {
			t.Errorf("expected reason %q among verdicts %+v", want, got)
		}
	}
}

func TestExtractReleaseYearBoundsAndAmbiguity(t *testing.T) {
	if y := extractReleaseYear("Movie.Name.1994.1080p-GROUP"); y != 1994 {
		t.Fatalf("extractReleaseYear = %d, want 1994", y)
	}
	if y := extractReleaseYear("Movie.Name.1080p-GROUP"); y != 0 {
		t.Fatalf("no year token should return 0, got %d", y)
	}
	if y := extractReleaseYear("Movie.Name.1994.Alt.2020-GROUP"); y != 0 {
		t.Fatalf("ambiguous multi-year should return 0, got %d", y)
	}
	if y := extractReleaseYear(strings.Repeat("x", 10) + ".3099.movie"); y != 0 {
		t.Fatalf("out-of-range 4-digit token should not be treated as a year, got %d", y)
	}
}

func TestParseSeasonEpisode(t *testing.T) {
	if season, episode, ok := ParseSeasonEpisode("Show.Name.S03E05.1080p.WEB.x264-GROUP"); !ok || season != 3 || episode != 5 {
		t.Fatalf("got season=%d episode=%d ok=%v, want 3,5,true", season, episode, ok)
	}
	if _, _, ok := ParseSeasonEpisode("Movie.Name.1994.1080p.BluRay.x264-GROUP"); ok {
		t.Fatalf("movie title should not parse a season/episode")
	}
	if _, _, ok := ParseSeasonEpisode("Show.Name.S01E01-E02.1080p.WEB-GROUP"); ok {
		t.Fatalf("multi-episode/season-pack title should abstain (ambiguous), got ok=true")
	}
}

func TestSeriesTitleFromRelease(t *testing.T) {
	tests := []struct {
		name    string
		release string
		want    string
		wantOK  bool
	}{
		{"real Roseanne release", "Roseanne.S01E18.1080p.WEBRip.x265", "Roseanne", true},
		{"real Mad About You release", "Mad.About.You.S08E10.Real.Estate.for.Beginners.720p.WEB-DL.AAC2.0.H.264-TOMMY", "Mad About You", true},
		{"multi-word with underscores", "The_Wire.S03E01.1080p", "The Wire", true},
		{"no season/episode token at all", "Movie.Name.1994.1080p.BluRay.x264-GROUP", "", false},
		{"season pack ambiguous", "Show.Name.S01E01-E02.1080p.WEB-GROUP", "", false},
		{"nothing before the token", "S01E01.1080p.WEB-GROUP", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SeriesTitleFromRelease(tt.release)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("title = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMovieTitleFromRelease(t *testing.T) {
	tests := []struct {
		name    string
		release string
		want    string
		wantOK  bool
	}{
		{"real Forrest Gump release", "Forrest.Gump.1994.REPACK.2160p.UHD.BluRay.TrueHD.7.1.DoVi.HDR10.x265-W4NK3R", "Forrest Gump", true},
		{"real Sonic release", "Sonic.the.Hedgehog.3.2024.UHD.BluRay.2160p.TrueHD.Atmos.7.1.DV.HEVC.REMUX-FraMeSToR", "Sonic the Hedgehog 3", true},
		{"no year token at all", "Some.Release.Name.WEB-DL.x264-GROUP", "", false},
		{"two distinct years is ambiguous", "Movie.Name.1994.EXTENDED.CUT.1988.x264-GROUP", "", false},
		{"nothing before the year", "1994.1080p.BluRay.x264-GROUP", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MovieTitleFromRelease(tt.release)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("title = %q, want %q", got, tt.want)
			}
		})
	}
}

// FuzzVerify exercises identitycheck's release-title parsing (extractReleaseYear,
// seasonEpisodeRe, conflictingSourceGroups) against arbitrary, potentially
// malformed/oversized/adversarial title strings, since these originate from
// external Arr/indexer metadata (DG-03: every new parser ships a committed
// fuzz target). Verify must never panic and must always return in bounded
// time regardless of input.
func FuzzVerify(f *testing.F) {
	f.Add("Movie.Name.1994.1080p.BluRay.x264-GROUP", 0, 0, int64(142*60*1000), "eng")
	f.Add("Show.Name.S03E05.1080p.WEB-DL-GROUP", 3, 5, int64(0), "")
	f.Add("Movie.Name.1994.CAM.BluRay-GROUP", 0, 0, int64(0), "")
	f.Add(strings.Repeat("1994.", 5000), 0, 0, int64(0), "")
	f.Fuzz(func(t *testing.T, releaseTitle string, season, episode int, durationMS int64, lang string) {
		identity := &mediaidentity.ProviderIdentity{Kind: "movie", Year: 1994, RuntimeMinutes: 142}
		facts := &mediatruth.Facts{DurationMS: durationMS, AudioLangs: []string{"eng"}}
		_ = Verify(identity, releaseTitle, season, episode, facts, lang)
		_, _, _ = ParseSeasonEpisode(releaseTitle)
		_, _ = SeriesTitleFromRelease(releaseTitle)
		_, _ = MovieTitleFromRelease(releaseTitle)
	})
}
