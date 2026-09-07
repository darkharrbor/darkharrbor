package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

const maxArrMetadataBytes = 8 << 20

type sonarrSeries struct {
	ID     int    `json:"id"`
	TVDBID int    `json:"tvdbId"`
	TMDBID int    `json:"tmdbId"`
	IMDBID string `json:"imdbId"`
	Title  string `json:"title"`
	Year   int    `json:"year"`
	// Runtime is Sonarr's typical per-episode runtime in minutes, used as
	// the series-level ID-01 runtime-plausibility baseline. Sonarr's
	// episode resource carries no reliable per-episode override, so
	// EpisodeProviderIDs.RuntimeMinutes is deliberately left unset by this
	// capture path.
	Runtime int `json:"runtime"`
}

type sonarrEpisode struct {
	Season   int    `json:"seasonNumber"`
	Episode  int    `json:"episodeNumber"`
	Absolute int    `json:"absoluteEpisodeNumber"`
	AirDate  string `json:"airDate"`
	Title    string `json:"title"`
	TVDBID   int    `json:"tvdbId"`
}

// episodeContext returns bounded authoritative facts from the configured
// Sonarr instance that owns tvdbID. Radarr and Sonarr instances without the
// series are harmless skips. Credentials remain transient request headers.
func (s *Server) episodeContext(ctx context.Context, tvdbID string) (string, []httpstream.EpisodeIdentity) {
	id, err := strconv.Atoi(strings.TrimSpace(tvdbID))
	if err != nil || id < 1 || s.cfg == nil || s.arrMetadataClient == nil {
		return "", nil
	}
	for _, target := range s.cfg.Arrs {
		if target.BaseURL == "" || target.APIKey == "" {
			continue
		}
		var series []sonarrSeries
		if s.getArrJSON(ctx, target.BaseURL+"/api/v3/series", target.APIKey, &series) != nil {
			continue
		}
		var match *sonarrSeries
		for i := range series {
			if series[i].TVDBID == id && series[i].ID > 0 {
				match = &series[i]
				break
			}
		}
		if match == nil {
			continue
		}

		var episodes []sonarrEpisode
		endpoint := target.BaseURL + "/api/v3/episode?seriesId=" + strconv.Itoa(match.ID)
		if s.getArrJSON(ctx, endpoint, target.APIKey, &episodes) != nil || len(episodes) > httpstream.MaxEpisodeIdentities {
			return strings.TrimSpace(match.Title), nil
		}
		out := make([]httpstream.EpisodeIdentity, 0, len(episodes))
		for _, episode := range episodes {
			if episode.Season < 0 || episode.Episode < 1 {
				continue
			}
			out = append(out, httpstream.EpisodeIdentity{
				Season: episode.Season, Episode: episode.Episode, Absolute: episode.Absolute,
				AirDate: strings.TrimSpace(episode.AirDate), Title: strings.TrimSpace(episode.Title),
			})
		}
		return strings.TrimSpace(match.Title), out
	}
	return "", nil
}

func (s *Server) getArrJSON(ctx context.Context, endpoint, apiKey string, dst any) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return errors.New("invalid arr endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("build arr metadata request")
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := s.arrMetadataClient.Do(req)
	if err != nil {
		return errors.New("arr metadata unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("arr metadata rejected")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArrMetadataBytes+1))
	if err != nil || len(body) > maxArrMetadataBytes {
		return errors.New("arr metadata malformed")
	}
	if json.Unmarshal(body, dst) != nil {
		return errors.New("arr metadata malformed")
	}
	return nil
}
