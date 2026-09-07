// Package mediaidentity defines the compact provider-ID vocabulary shared by
// Arr capture, item persistence, and NFO sidecar generation.
package mediaidentity

import (
	"fmt"
	"strings"
)

type ProviderIDs struct {
	TVDB string `json:"tvdb,omitempty"`
	TMDB string `json:"tmdb,omitempty"`
	IMDB string `json:"imdb,omitempty"`
}

type EpisodeProviderIDs struct {
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	TVDB    string `json:"tvdb,omitempty"`
	TMDB    string `json:"tmdb,omitempty"`
	IMDB    string `json:"imdb,omitempty"`

	// RuntimeMinutes is this exact episode's authoritative Arr-reported
	// runtime (ID-01). 0 means unknown/unset -- a verifier must fall back
	// to the series-level ProviderIdentity.RuntimeMinutes, never guess.
	RuntimeMinutes int `json:"runtime_minutes,omitempty"`
}

type ProviderIdentity struct {
	Kind     string               `json:"kind,omitempty"`
	Title    string               `json:"title,omitempty"`
	IDs      ProviderIDs          `json:"ids"`
	Episodes []EpisodeProviderIDs `json:"episodes,omitempty"`

	// Year is the Arr's authoritative release year (movie) or series-start
	// year (series). 0 means unknown/unset (ID-01 must abstain, never
	// guess).
	Year int `json:"year,omitempty"`
	// RuntimeMinutes is the Arr's authoritative typical runtime: movie
	// runtime for a movie identity, typical per-episode runtime for a
	// series identity (an individual episode may override via
	// EpisodeProviderIDs.RuntimeMinutes). 0 means unknown/unset (ID-01).
	RuntimeMinutes int `json:"runtime_minutes,omitempty"`
}

// minValidYear/maxYearSkew bound Year to a plausible cinema/broadcast range.
// A value outside this range is almost certainly a parsing/transport defect,
// not a real release year, so Validate rejects it rather than trusting it.
const minValidYear = 1878 // first known motion picture (Roundhay Garden Scene)

// maxValidYear is a generous static ceiling (not clock-derived: Year is a
// bounded data-validation range, not TTL/backoff/cadence timing logic, so
// no injectable clock applies here) that still rejects an obviously
// malformed far-future value.
const maxValidYear = 2100

// maxRuntimeMinutes bounds RuntimeMinutes to a generous but finite ceiling
// (10 hours) covering extended cuts / miniseries-as-movie edge cases without
// accepting an obviously malformed value.
const maxRuntimeMinutes = 600

// Validate rejects malformed or oversized transport/persistence context.
func Validate(identity *ProviderIdentity) error {
	if identity == nil {
		return nil
	}
	if identity.Kind != "series" && identity.Kind != "movie" {
		return fmt.Errorf("media identity: invalid kind")
	}
	if len(identity.Title) > 4096 || strings.ContainsAny(identity.Title, "\r\n\x00") {
		return fmt.Errorf("media identity: invalid title")
	}
	if len(identity.Episodes) > 2048 {
		return fmt.Errorf("media identity: episode limit exceeded")
	}
	if NormalizedIDs(identity.IDs) != identity.IDs {
		return fmt.Errorf("media identity: invalid provider id")
	}
	if identity.Year < 0 || (identity.Year != 0 && (identity.Year < minValidYear || identity.Year > maxValidYear)) {
		return fmt.Errorf("media identity: invalid year")
	}
	if identity.RuntimeMinutes < 0 || identity.RuntimeMinutes > maxRuntimeMinutes {
		return fmt.Errorf("media identity: invalid runtime")
	}
	for _, episode := range identity.Episodes {
		ids := ProviderIDs{TVDB: episode.TVDB, TMDB: episode.TMDB, IMDB: episode.IMDB}
		if episode.Season < 0 || episode.Episode < 1 || NormalizedIDs(ids) != ids {
			return fmt.Errorf("media identity: invalid episode")
		}
		if episode.RuntimeMinutes < 0 || episode.RuntimeMinutes > maxRuntimeMinutes {
			return fmt.Errorf("media identity: invalid episode runtime")
		}
	}
	return nil
}

func NormalizedIDs(ids ProviderIDs) ProviderIDs {
	if !validNumericID(ids.TVDB) {
		ids.TVDB = ""
	}
	if !validNumericID(ids.TMDB) {
		ids.TMDB = ""
	}
	ids.IMDB = strings.ToLower(strings.TrimSpace(ids.IMDB))
	if len(ids.IMDB) < 3 || len(ids.IMDB) > 32 || !strings.HasPrefix(ids.IMDB, "tt") ||
		!validNumericID(strings.TrimPrefix(ids.IMDB, "tt")) {
		ids.IMDB = ""
	}
	return ids
}

func validNumericID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 20 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != "0"
}
