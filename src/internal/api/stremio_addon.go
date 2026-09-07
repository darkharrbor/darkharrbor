package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const (
	maxStremioVideoIDBytes      = 64
	maxStremioClientConcurrency = 64
)

type stremioManifest struct {
	ID          string                    `json:"id"`
	Version     string                    `json:"version"`
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Resources   []stremioManifestResource `json:"resources"`
	Types       []string                  `json:"types"`
	Catalogs    []any                     `json:"catalogs"`
}

type stremioManifestResource struct {
	Name       string   `json:"name"`
	Types      []string `json:"types"`
	IDPrefixes []string `json:"idPrefixes"`
}

type stremioStream struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

type stremioStreamResponse struct {
	Streams []stremioStream `json:"streams"`
}

type stremioVideoIdentity struct {
	kind    string
	imdbID  string
	season  int
	episode int
}

// ClientRouter is the complete client-edge surface. It deliberately excludes
// the primary router's qBit, SAB, WebDAV, metrics, debug, and administration
// routes. Path logging is also excluded because the addon path contains the
// install credential.
func (s *Server) ClientRouter() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", getOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})))
	mux.Handle("/stream/{item_id}/{file_id}", http.HandlerFunc(s.handleStreamProxy))
	// MediaFlow playback only. Aggregator-facing /proxy/ip and /generate_urls
	// stay on the primary router so that serving playback to clients does not
	// expose the primary API surface.
	s.registerMediaFlowPlayback(mux)
	s.registerStremioAddon(mux)
	return stremioClientCORS(limitStremioClientConcurrency(mux, maxStremioClientConcurrency))
}

func (s *Server) registerStremioAddon(mux *http.ServeMux) {
	mux.Handle("/stremio/{install_token}/manifest.json", getOnly(http.HandlerFunc(s.handleStremioManifest)))
	mux.Handle("/stremio/{install_token}/stream/{type}/{video_id}", getOnly(http.HandlerFunc(s.handleStremioStream)))
}

func (s *Server) handleStremioManifest(w http.ResponseWriter, r *http.Request) {
	if !s.validStremioInstallToken(r.PathValue("install_token")) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, stremioManifest{
		ID:          "org.darkharrbor.library",
		Version:     "1.2.4",
		Name:        "DarkHarrbor Library",
		Description: "Streams ready in this DarkHarrbor library.",
		Resources: []stremioManifestResource{{
			Name: "stream", Types: []string{"movie", "series"}, IDPrefixes: []string{"tt"},
		}},
		Types:    []string{"movie", "series"},
		Catalogs: []any{},
	})
}

func (s *Server) handleStremioStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	empty := stremioStreamResponse{Streams: []stremioStream{}}
	if s == nil || s.store == nil || s.cfg == nil || !s.validStremioInstallToken(r.PathValue("install_token")) {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	identity, err := parseStremioVideoID(r.PathValue("type"), r.PathValue("video_id"))
	if err != nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	rows, err := s.store.ListStremioLibraryStreams(
		r.Context(), identity.kind, identity.imdbID, identity.season, identity.episode, store.MaxStremioLibraryStreams,
	)
	if err != nil {
		if r.Context().Err() == nil && s.log != nil {
			s.log.Warn("stremio: library lookup failed")
		}
		writeJSON(w, http.StatusOK, empty)
		return
	}
	streams := make([]stremioStream, 0, len(rows))
	for _, row := range rows {
		if !validStremioTitle(row.Title) || (identity.kind == "series" && !stremioLibraryEpisodeMatches(row, identity.season, identity.episode)) {
			continue
		}
		itemID, fileID, ok := parseStreamPath(row.URL)
		if !ok || itemID != row.ItemID || !validStremioLibraryURL(row.URL, s.cfg.Server.BaseURL, s.cfg.Server.StreamSecret, row.ItemID, fileID) {
			continue
		}
		clientURL, ok := rewriteStremioClientURL(row.URL, s.cfg.Stremio.ClientBaseURL)
		if !ok {
			continue
		}
		streams = append(streams, stremioStream{Name: "DarkHarrbor", Title: row.Title, URL: clientURL})
	}
	// RX-9.5 instrumentation (RD-31): this handler parses the Stremio
	// coordinate BEFORE consulting the library, so it observes EVERY title
	// the aggregator asks about -- including ones DarkHarrbor cannot serve,
	// which return an empty list rather than an error. That makes it the
	// pre-hop capture point. Nothing here was observable before: this path
	// logged only on lookup failure, and the aggregator does not cache empty
	// stream results, so neither side could show whether it was queried.
	// RX-9.5: record the coordinate for the /generate_urls batch that follows.
	// Repeats are idempotent -- the aggregator was observed querying twice for
	// a single resolution, so a repeat must not read as a second identity.
	s.rx95.Observe(rx95Identity{
		Kind:    identity.kind,
		IMDb:    identity.imdbID,
		Season:  identity.season,
		Episode: identity.episode,
	}, s.nowUTC())
	if s.log != nil {
		s.log.Info("rx95: addon stream request",
			"kind", identity.kind,
			"imdb", identity.imdbID,
			"season", identity.season,
			"episode", identity.episode,
			"library_rows", len(rows),
			"streams_returned", len(streams))
	}
	writeJSON(w, http.StatusOK, stremioStreamResponse{Streams: streams})
}

func stremioLibraryEpisodeMatches(row store.StremioLibraryStream, season, episode int) bool {
	if row.BlobCount < 1 || row.EpisodeCount < 1 {
		return false
	}
	if row.BlobCount == 1 && row.EpisodeCount == 1 {
		return true
	}
	gotSeason, gotEpisode, ok := output.EpisodeNumbers(filepath.Base(row.RelPath))
	return ok && gotSeason == season && gotEpisode == episode
}

func (s *Server) validStremioInstallToken(token string) bool {
	if s == nil || s.cfg == nil || len(token) < 32 || len(token) > 128 {
		return false
	}
	want := s.cfg.Stremio.EffectiveInstallToken(s.cfg.Server.StreamSecret)
	return len(token) == len(want) && subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

func rewriteStremioClientURL(rawURL, clientBaseURL string) (string, bool) {
	if len(rawURL) > 16<<10 {
		return "", false
	}
	streamURL, err := url.Parse(rawURL)
	if err != nil || streamURL.User != nil || streamURL.Fragment != "" {
		return "", false
	}
	clientBase, err := config.ParseStremioClientBaseURL(clientBaseURL)
	if err != nil {
		return "", false
	}
	streamURL.Scheme = clientBase.Scheme
	streamURL.Host = clientBase.Host
	return streamURL.String(), true
}

func stremioClientCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

func limitStremioClientConcurrency(next http.Handler, limit int) http.Handler {
	sem := make(chan struct{}, limit)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		}
	})
}

func parseStremioVideoID(kind, raw string) (stremioVideoIdentity, error) {
	if len(raw) < len("tt1.json") || len(raw) > maxStremioVideoIDBytes || !strings.HasSuffix(raw, ".json") {
		return stremioVideoIdentity{}, errors.New("invalid Stremio video id")
	}
	raw = strings.TrimSuffix(raw, ".json")
	parts := strings.Split(raw, ":")
	switch kind {
	case "movie":
		if len(parts) != 1 {
			return stremioVideoIdentity{}, errors.New("invalid Stremio movie id")
		}
	case "series":
		if len(parts) != 3 {
			return stremioVideoIdentity{}, errors.New("invalid Stremio series id")
		}
	default:
		return stremioVideoIdentity{}, errors.New("unsupported Stremio type")
	}
	imdb := strings.ToLower(parts[0])
	if imdb == "" || mediaidentity.NormalizedIDs(mediaidentity.ProviderIDs{IMDB: imdb}).IMDB != imdb {
		return stremioVideoIdentity{}, errors.New("invalid Stremio IMDb id")
	}
	identity := stremioVideoIdentity{kind: kind, imdbID: imdb}
	if kind == "series" {
		season, errSeason := strconv.Atoi(parts[1])
		episode, errEpisode := strconv.Atoi(parts[2])
		if errSeason != nil || errEpisode != nil || season < 0 || season > 9999 || episode < 1 || episode > 9999 {
			return stremioVideoIdentity{}, errors.New("invalid Stremio episode coordinates")
		}
		identity.season, identity.episode = season, episode
	}
	return identity, nil
}

func validStremioLibraryURL(rawURL, baseURL, streamSecret, expectedItemID, expectedFileID string) bool {
	if len(rawURL) > 16<<10 || validateRelayURL(rawURL, baseURL) != nil {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	itemID, fileID, ok := parseStreamPath(rawURL)
	if !ok || itemID != expectedItemID || fileID != expectedFileID {
		return false
	}
	query := u.Query()
	if streamSecret == "" {
		return len(query) == 0
	}
	values, exists := query["tok"]
	return exists && len(query) == 1 && len(values) == 1 && output.VerifyStreamToken(streamSecret, itemID, fileID, values[0])
}

func validStremioTitle(title string) bool {
	return strings.TrimSpace(title) != "" && len(title) <= 4096 && !strings.ContainsAny(title, "\r\n\x00")
}
