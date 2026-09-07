// Package identitycheck implements ID-01: grab-time identity verification of
// a resolved release against the grabbing Arr's own authoritative metadata
// (internal/mediaidentity, captured by ID-02) and this program's own
// already-computed media truth (internal/mediatruth, HR5.2/SF-04/TS-3.3).
//
// Every individual check is independently safely-abstaining: a check whose
// required input is missing, zero-value, or ambiguous contributes no
// verdict at all rather than guessing a mismatch. This mirrors every other
// row in the program (DG-05/DG-09 abstain-not-guess convention) and is
// deliberately conservative — a false "mismatch" is far more costly (a
// perfectly good release gets repaired/suppressed) than a missed one.
//
// This package is pure and deterministic: no I/O, no goroutines, no clock.
// The one exception explicitly named by the master-plan row — reserved
// suppression Reason wiring and the strictness action taken on a verdict —
// is the caller's job (cmd/darkharrbor/main.go), not this package's.
package identitycheck

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
)

// Strictness governs what a caller does with a Verdict, not whether
// Verify itself runs — Verify always computes the same verdicts regardless
// of strictness, so deterministic tests never need to vary it.
type Strictness string

const (
	StrictnessOff     Strictness = "off"
	StrictnessWarn    Strictness = "warn"
	StrictnessEnforce Strictness = "enforce"
)

// ParseStrictness returns the matching Strictness for a case-insensitive,
// trimmed value, or the documented default (warn) for anything unknown or
// empty — an unrecognized configuration value must never fail startup or
// silently disable verification (matches every other program config knob's
// "unknown/removed key warns, never fails" convention).
func ParseStrictness(v string) Strictness {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case string(StrictnessOff):
		return StrictnessOff
	case string(StrictnessEnforce):
		return StrictnessEnforce
	case string(StrictnessWarn), "":
		return StrictnessWarn
	default:
		return StrictnessWarn
	}
}

// Reason is a stable, machine-readable verdict reason token. Every value is
// distinct and namespaced under the single identity_mismatch suppression
// reason (internal/suppress.ReasonIdentityMismatch) this row wires — Reason
// is for logging/observability granularity only, never a separate
// suppression key space.
type Reason string

const (
	ReasonYearMismatch          Reason = "year_mismatch"
	ReasonRuntimeMismatch       Reason = "runtime_mismatch"
	ReasonAudioLanguageMismatch Reason = "audio_language_mismatch"
	ReasonConflictingSourceTags Reason = "conflicting_source_tags"
)

// Verdict is one individual check's finding. Detail is a short,
// secret-free, upstream-URL-free human string (DG-04) safe to log verbatim.
type Verdict struct {
	Reason Reason
	Detail string
}

// runtimeTolerance returns the accepted absolute difference (in
// milliseconds) between a probed duration and an Arr-reported runtime,
// generous enough to absorb intro/credit-trim and container rounding
// without absorbing an actually-wrong file: the greater of 12 minutes or
// 12% of the expected runtime.
func runtimeTolerance(expectedMinutes int) int64 {
	pct := int64(expectedMinutes) * 12 * 60 * 1000 / 100
	const floor = 12 * 60 * 1000 // 12 minutes in ms
	if pct > floor {
		return pct
	}
	return floor
}

// seasonEpisodeRe extracts a leading SxxEyy token from a release title,
// e.g. "Show.Name.S03E05.1080p.WEB.x264-GROUP" -> (3, 5). This mirrors the
// existing exact-match-only convention already established by
// internal/api's own nntpSeasonEpisodePattern (NS-5.4) and the
// .strm-naming heuristics in cmd/darkharrbor/main.go -- deliberately not a
// second classifier, just this package's own small, pure, bounded copy so
// callers in both internal/api and cmd/darkharrbor can reach it without an
// import cycle.
var seasonEpisodeRe = regexp.MustCompile(`(?i)\bs0*([0-9]{1,3})e0*([0-9]{1,3})(-e0*[0-9]{1,3})?\b`)

// ParseSeasonEpisode extracts season/episode from releaseTitle. ok is false
// (season/episode both 0) when no exact SxxEyy token is present -- never a
// partial or inferred guess. A season pack or multi-episode title (more
// than one distinct SxxEyy token) is treated as ambiguous and also returns
// ok=false, since a single probed Facts.DurationMS cannot be attributed to
// one specific episode's expected runtime in that case.
func ParseSeasonEpisode(releaseTitle string) (season, episode int, ok bool) {
	matches := seasonEpisodeRe.FindAllStringSubmatch(releaseTitle, -1)
	if len(matches) != 1 || matches[0][3] != "" {
		// Zero, more-than-one, or an explicit multi-episode range (e.g.
		// "S01E01-E02") is ambiguous -- never attribute a single probed
		// duration to one specific episode's expected runtime.
		return 0, 0, false
	}
	s, serr := strconv.Atoi(matches[0][1])
	e, eerr := strconv.Atoi(matches[0][2])
	if serr != nil || eerr != nil || s < 1 || e < 1 {
		return 0, 0, false
	}
	return s, e, true
}

// yearTokenRe finds a plausible bare 4-digit release-year token, bounded to
// mediaidentity's own accepted year range so an unrelated 4-digit number
// (a resolution-adjacent number, a group tag, part of a longer number) is
// never mistaken for a year.
var yearTokenRe = regexp.MustCompile(`(?:^|[^0-9])(1[89][0-9]{2}|20[0-9]{2})(?:[^0-9]|$)`)

// SeriesTitleFromRelease returns the series-title portion of releaseTitle --
// everything before its single, unambiguous SxxEyy token, with dots/
// underscores normalized to spaces -- for ID-04's grab-time identity
// fallback: when neither a durable ProviderIdentity nor ID-02's cache
// bridge has anything for a submission, this lets the caller re-derive a
// title to match against an Arr's own library directly (the same
// mechanism internal/api's providerIdentityForQuery already uses at search
// time), rather than leaving the item with no identity at all. Reuses
// ParseSeasonEpisode's own exact-single-token ambiguity rule verbatim: zero
// or more than one SxxEyy token (a season pack, a multi-episode range, or
// no season/episode token at all) means abstain, never guess a boundary.
func SeriesTitleFromRelease(releaseTitle string) (string, bool) {
	if _, _, ok := ParseSeasonEpisode(releaseTitle); !ok {
		return "", false
	}
	loc := seasonEpisodeRe.FindStringIndex(releaseTitle)
	if loc == nil {
		return "", false
	}
	title := cleanReleaseTitleSegment(releaseTitle[:loc[0]])
	if title == "" {
		return "", false
	}
	return title, true
}

// MovieTitleFromRelease returns the movie-title portion of releaseTitle --
// everything before its single, unambiguous release-year token, with dots/
// underscores normalized to spaces -- for the same ID-04 grab-time
// fallback purpose as SeriesTitleFromRelease. Reuses extractReleaseYear's
// own exact-single-distinct-year ambiguity rule: zero or more than one
// distinct year token means abstain, never guess which one is the release
// year or where the title ends.
func MovieTitleFromRelease(releaseTitle string) (string, bool) {
	if extractReleaseYear(releaseTitle) == 0 {
		return "", false
	}
	loc := yearTokenRe.FindStringIndex(releaseTitle)
	if loc == nil {
		return "", false
	}
	title := cleanReleaseTitleSegment(releaseTitle[:loc[0]])
	if title == "" {
		return "", false
	}
	return title, true
}

var releaseTitleCleaner = strings.NewReplacer(".", " ", "_", " ")

func cleanReleaseTitleSegment(segment string) string {
	return strings.TrimSpace(releaseTitleCleaner.Replace(segment))
}

// extractReleaseYear returns the release title's own embedded year token,
// or 0 if none is found or more than one distinct candidate year is found
// (ambiguous — abstain, never guess which one is authoritative).
func extractReleaseYear(releaseTitle string) int {
	matches := yearTokenRe.FindAllStringSubmatch(releaseTitle, -1)
	years := map[int]bool{}
	for _, m := range matches {
		if y, err := strconv.Atoi(m[1]); err == nil {
			years[y] = true
		}
	}
	if len(years) != 1 {
		return 0
	}
	for y := range years {
		return y
	}
	return 0
}

// conflictingSourceGroups lists source/origin tags that can never
// legitimately co-occur in one real release name because they name
// mutually exclusive acquisition generations of the same content (a single
// rip cannot simultaneously be a theatrical cam capture and a Blu-ray/UHD
// disc rip). This is deliberately narrow and intra-release only — it never
// compares a tag against the underlying content's real-world vintage (a
// BluRay or WEB-DL rip of a decades-old film is completely normal and is
// NOT flagged here; see package doc).
var conflictingSourceGroups = [][2]*regexp.Regexp{
	{regexp.MustCompile(`(?i)\bCAM\b`), regexp.MustCompile(`(?i)\b(BluRay|BDRip|BRRip|UHD|REMUX)\b`)},
	{regexp.MustCompile(`(?i)\bTELESYNC\b|\bTS\b`), regexp.MustCompile(`(?i)\b(BluRay|BDRip|BRRip|UHD|REMUX)\b`)},
	{regexp.MustCompile(`(?i)\bHDTV\b`), regexp.MustCompile(`(?i)\b(BluRay|BDRip|BRRip|UHD|REMUX|WEB-?DL)\b`)},
	{regexp.MustCompile(`(?i)\bCAM\b`), regexp.MustCompile(`(?i)\bWEB-?DL\b`)},
}

// checkConflictingSourceTags flags a release title carrying two mutually
// exclusive source-generation tags at once.
func checkConflictingSourceTags(releaseTitle string) *Verdict {
	for _, pair := range conflictingSourceGroups {
		if pair[0].MatchString(releaseTitle) && pair[1].MatchString(releaseTitle) {
			return &Verdict{
				Reason: ReasonConflictingSourceTags,
				Detail: "release title carries mutually exclusive source-generation tags",
			}
		}
	}
	return nil
}

// checkYear compares the release title's own embedded year token against
// the Arr's authoritative year. For a movie, an exact match is required.
// For a series, the release year must be no earlier than the series'
// start year (a season cannot air before the show began) and no more than
// 50 years later (a generous bound for the longest-running real shows),
// since matching mid-run seasons exactly is not possible from the release
// title alone. Abstains when either year is unknown/zero.
func checkYear(identity *mediaidentity.ProviderIdentity, releaseTitle string) *Verdict {
	if identity == nil || identity.Year == 0 {
		return nil
	}
	releaseYear := extractReleaseYear(releaseTitle)
	if releaseYear == 0 {
		return nil
	}
	switch identity.Kind {
	case "movie":
		if releaseYear != identity.Year {
			return &Verdict{
				Reason: ReasonYearMismatch,
				Detail: fmt.Sprintf("release year %d does not match Arr movie year %d", releaseYear, identity.Year),
			}
		}
	case "series":
		if releaseYear < identity.Year || releaseYear > identity.Year+50 {
			return &Verdict{
				Reason: ReasonYearMismatch,
				Detail: fmt.Sprintf("release year %d implausible for series starting %d", releaseYear, identity.Year),
			}
		}
	}
	return nil
}

// expectedRuntimeMinutes resolves the applicable runtime for the exact
// requested episode (falling back to the series-level typical runtime),
// or the movie runtime. Returns 0 (unknown, abstain) when nothing applies.
func expectedRuntimeMinutes(identity *mediaidentity.ProviderIdentity, season, episode int) int {
	if identity == nil {
		return 0
	}
	if identity.Kind == "series" && season > 0 && episode > 0 {
		for _, ep := range identity.Episodes {
			if ep.Season == season && ep.Episode == episode && ep.RuntimeMinutes > 0 {
				return ep.RuntimeMinutes
			}
		}
	}
	return identity.RuntimeMinutes
}

// checkRuntime compares the already-computed bounded media-truth duration
// (HR5.2/TS-3.3's own resolved Facts, never a fresh probe this row would
// have to trigger itself) against the Arr's expected runtime, within
// runtimeTolerance. Abstains when either side is unknown/zero.
func checkRuntime(identity *mediaidentity.ProviderIdentity, season, episode int, facts *mediatruth.Facts) *Verdict {
	if facts == nil || facts.DurationMS <= 0 {
		return nil
	}
	expected := expectedRuntimeMinutes(identity, season, episode)
	if expected <= 0 {
		return nil
	}
	expectedMS := int64(expected) * 60 * 1000
	diff := facts.DurationMS - expectedMS
	if diff < 0 {
		diff = -diff
	}
	if diff > runtimeTolerance(expected) {
		return &Verdict{
			Reason: ReasonRuntimeMismatch,
			Detail: fmt.Sprintf("probed duration %dms outside tolerance of expected %d-minute runtime", facts.DurationMS, expected),
		}
	}
	return nil
}

// checkAudioLanguage is the row's explicitly "optional" check: it only ever
// fires when the Arr identity names an expected original-language code AND
// the probe found at least one audio track, and only when NONE of the
// probed audio languages match — any unrecognized/ambiguous language code
// on either side abstains rather than guessing a mismatch.
func checkAudioLanguage(expectedLangCode string, facts *mediatruth.Facts) *Verdict {
	expectedLangCode = strings.ToLower(strings.TrimSpace(expectedLangCode))
	if expectedLangCode == "" || len(expectedLangCode) != 3 || facts == nil || len(facts.AudioLangs) == 0 {
		return nil
	}
	for _, lang := range facts.AudioLangs {
		if strings.EqualFold(strings.TrimSpace(lang), expectedLangCode) {
			return nil
		}
	}
	return &Verdict{
		Reason: ReasonAudioLanguageMismatch,
		Detail: fmt.Sprintf("no probed audio track matched expected language %q", expectedLangCode),
	}
}

// Verify runs every applicable check and returns every verdict reached
// (usually zero or one, but never deduplicated across distinct reasons —
// a caller may legitimately want to log/count more than one simultaneous
// mismatch class). releaseTitle is the exact resolved release/display
// name (item.DisplayName), never a search query. season/episode are 0 for
// a movie or an unscoped series request. expectedAudioLangCode is the
// caller-resolved ISO 639-2/B three-letter code, or "" if unknown
// (optional-check precondition; see checkAudioLanguage).
func Verify(
	identity *mediaidentity.ProviderIdentity,
	releaseTitle string,
	season, episode int,
	facts *mediatruth.Facts,
	expectedAudioLangCode string,
) []Verdict {
	var verdicts []Verdict
	if v := checkYear(identity, releaseTitle); v != nil {
		verdicts = append(verdicts, *v)
	}
	if v := checkRuntime(identity, season, episode, facts); v != nil {
		verdicts = append(verdicts, *v)
	}
	if v := checkAudioLanguage(expectedAudioLangCode, facts); v != nil {
		verdicts = append(verdicts, *v)
	}
	if v := checkConflictingSourceTags(releaseTitle); v != nil {
		verdicts = append(verdicts, *v)
	}
	return verdicts
}
