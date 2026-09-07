package api

import (
	"context"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

type radarrMovie struct {
	Title   string `json:"title"`
	Year    int    `json:"year"`
	TMDBID  int    `json:"tmdbId"`
	IMDBID  string `json:"imdbId"`
	Runtime int    `json:"runtime"`
}

// providerIdentityForQuery returns one exact authoritative Arr identity.
// Multiple distinct library matches are ambiguity and return nil.
func (s *Server) providerIdentityForQuery(
	ctx context.Context,
	kind, title, tvdbID, tmdbID, imdbID string,
) *store.ProviderIdentity {
	fallback := &store.ProviderIdentity{
		Kind: kind, Title: strings.TrimSpace(title),
		IDs: store.ProviderIDs{
			TVDB: numericProviderID(tvdbID), TMDB: numericProviderID(tmdbID),
			IMDB: imdbProviderID(imdbID),
		},
	}
	if kind != "series" && kind != "movie" {
		return nil
	}
	if s == nil || s.cfg == nil || s.arrMetadataClient == nil {
		if fallback.IDs != (store.ProviderIDs{}) {
			return fallback
		}
		return nil
	}
	if len(s.cfg.Arrs) == 0 {
		s.log.Debug("provider identity: no arr targets configured, abstaining",
			"event", "identity_lookup_no_targets", "kind", kind)
	}
	var matches []store.ProviderIdentity
	for _, target := range s.cfg.Arrs {
		if target.BaseURL == "" || target.APIKey == "" {
			continue
		}
		if kind == "series" {
			var series []sonarrSeries
			if err := s.getArrJSON(ctx, target.BaseURL+"/api/v3/series", target.APIKey, &series); err != nil {
				s.log.Debug("provider identity: arr series list fetch failed (non-fatal, abstaining for this target)",
					"event", "identity_lookup_fetch_failed", "arr", target.Name, "kind", kind, "error", err)
				continue
			}
			for _, candidate := range series {
				if !seriesIdentityMatch(candidate, fallback) {
					continue
				}
				identity := store.ProviderIdentity{
					Kind: "series", Title: strings.TrimSpace(candidate.Title),
					IDs: store.ProviderIDs{
						TVDB: positiveIntString(candidate.TVDBID),
						TMDB: positiveIntString(candidate.TMDBID),
						IMDB: imdbProviderID(candidate.IMDBID),
					},
					Year:           boundedYear(candidate.Year),
					RuntimeMinutes: boundedRuntime(candidate.Runtime),
				}
				var episodes []sonarrEpisode
				if candidate.ID > 0 && s.getArrJSON(ctx,
					target.BaseURL+"/api/v3/episode?seriesId="+strconv.Itoa(candidate.ID),
					target.APIKey, &episodes) == nil && len(episodes) <= 2048 {
					for _, episode := range episodes {
						if episode.Season < 0 || episode.Episode < 1 || episode.TVDBID < 1 {
							continue
						}
						identity.Episodes = append(identity.Episodes, store.EpisodeProviderIDs{
							Season: episode.Season, Episode: episode.Episode,
							TVDB: positiveIntString(episode.TVDBID),
						})
					}
				}
				matches = appendIdentity(matches, identity)
			}
			continue
		}

		var movies []radarrMovie
		if err := s.getArrJSON(ctx, target.BaseURL+"/api/v3/movie", target.APIKey, &movies); err != nil {
			s.log.Debug("provider identity: arr movie list fetch failed (non-fatal, abstaining for this target)",
				"event", "identity_lookup_fetch_failed", "arr", target.Name, "kind", kind, "error", err)
			continue
		}
		queryTitle, queryYear := splitTrailingYear(fallback.Title)
		for _, candidate := range movies {
			if fallback.IDs.TMDB != "" && fallback.IDs.TMDB != positiveIntString(candidate.TMDBID) {
				continue
			}
			if fallback.IDs.IMDB != "" && fallback.IDs.IMDB != imdbProviderID(candidate.IMDBID) {
				continue
			}
			if fallback.IDs.TMDB == "" && fallback.IDs.IMDB == "" &&
				!strings.EqualFold(strings.TrimSpace(candidate.Title), strings.TrimSpace(queryTitle)) {
				continue
			}
			if queryYear > 0 && candidate.Year != queryYear {
				continue
			}
			matches = appendIdentity(matches, store.ProviderIdentity{
				Kind: "movie", Title: strings.TrimSpace(candidate.Title),
				IDs: store.ProviderIDs{
					TMDB: positiveIntString(candidate.TMDBID),
					IMDB: imdbProviderID(candidate.IMDBID),
				},
				Year:           boundedYear(candidate.Year),
				RuntimeMinutes: boundedRuntime(candidate.Runtime),
			})
		}
	}
	if len(matches) == 1 {
		return &matches[0]
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.Kind+":"+firstNonEmpty(m.IDs.TMDB, m.IDs.TVDB, m.IDs.IMDB))
		}
		s.log.Warn("provider identity: ambiguous match across arr targets, abstaining",
			"event", "identity_lookup_ambiguous", "kind", kind, "match_count", len(matches), "matched_ids", ids)
		return nil
	}
	if fallback.IDs != (store.ProviderIDs{}) {
		s.log.Debug("provider identity: no arr library title match, using query-supplied ids as fallback",
			"event", "identity_lookup_fallback_ids_only", "kind", kind)
		return fallback
	}
	s.log.Debug("provider identity: no arr library match and no query-supplied ids, abstaining",
		"event", "identity_lookup_no_match", "kind", kind)
	return nil
}

func seriesIdentityMatch(candidate sonarrSeries, query *store.ProviderIdentity) bool {
	if query.IDs.TVDB != "" {
		return query.IDs.TVDB == positiveIntString(candidate.TVDBID)
	}
	if query.IDs.TMDB != "" {
		return query.IDs.TMDB == positiveIntString(candidate.TMDBID)
	}
	if query.IDs.IMDB != "" {
		return query.IDs.IMDB == imdbProviderID(candidate.IMDBID)
	}
	return strings.EqualFold(strings.TrimSpace(candidate.Title), strings.TrimSpace(query.Title))
}

func appendIdentity(existing []store.ProviderIdentity, identity store.ProviderIdentity) []store.ProviderIdentity {
	key := identity.Kind + "|" + identity.IDs.TVDB + "|" + identity.IDs.TMDB + "|" + identity.IDs.IMDB
	for i, candidate := range existing {
		if candidate.Kind+"|"+candidate.IDs.TVDB+"|"+candidate.IDs.TMDB+"|"+candidate.IDs.IMDB == key {
			if len(identity.Episodes) > len(candidate.Episodes) {
				existing[i] = identity
			}
			return existing
		}
	}
	return append(existing, identity)
}

// scopeProviderIdentity keeps only episode IDs selected by the Arr request.
// A title-only series search carries series IDs but no speculative episode
// catalog; exact episode and season-pack searches carry only their requested
// episode scope. This also keeps signed HTTP grab envelopes bounded.
func scopeProviderIdentity(identity *store.ProviderIdentity, season, episode int) *store.ProviderIdentity {
	if identity == nil {
		return nil
	}
	scoped := *identity
	scoped.Episodes = nil
	if identity.Kind != "series" || season < 1 {
		return &scoped
	}
	for _, candidate := range identity.Episodes {
		if candidate.Season != season || (episode > 0 && candidate.Episode != episode) {
			continue
		}
		scoped.Episodes = append(scoped.Episodes, candidate)
	}
	return &scoped
}

// boundedYear returns year when it falls in mediaidentity's accepted range,
// otherwise 0 (unknown) -- an Arr-side data glitch must never fail the whole
// identity capture, it just leaves ID-01's year check abstaining.
func boundedYear(year int) int {
	if year < 1878 || year > 2100 {
		return 0
	}
	return year
}

// boundedRuntime returns runtime when it falls in mediaidentity's accepted
// range, otherwise 0 (unknown), same abstain-not-fail rationale as
// boundedYear.
func boundedRuntime(minutes int) int {
	if minutes < 1 || minutes > 600 {
		return 0
	}
	return minutes
}

func numericProviderID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 20 {
		return ""
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return ""
		}
	}
	if value == "0" {
		return ""
	}
	return value
}

func positiveIntString(value int) string {
	if value < 1 {
		return ""
	}
	return strconv.Itoa(value)
}

func imdbProviderID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.HasPrefix(value, "tt") || numericProviderID(strings.TrimPrefix(value, "tt")) == "" {
		return ""
	}
	return value
}
