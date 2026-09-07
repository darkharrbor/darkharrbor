package reactivecommit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const (
	maxArrBody       = 8 << 20
	maxArrCandidates = 256
	maxPolls         = 40
)

// ErrArrFileAlreadyPresent is a permanent, non-mutating refusal: the target
// Arr already owns a file for the exact reactive coordinate. Callers may use
// errors.Is to distinguish this from transient Arr/network failures without
// matching rendered error text.
var ErrArrFileAlreadyPresent = errors.New("reactivecommit: Arr file already present")

type ArrClient struct {
	targets []config.ArrTarget
	client  *http.Client
	sleep   func(context.Context, time.Duration) error
}

func NewArrClient(targets []config.ArrTarget) *ArrClient {
	copyTargets := append([]config.ArrTarget(nil), targets...)
	return &ArrClient{targets: copyTargets, client: &http.Client{Timeout: 10 * time.Second}, sleep: sleepContext}
}

type arrOwner struct {
	target    config.ArrTarget
	kind      string
	id        int
	monitored bool
}

type manualCandidate struct {
	Path   string `json:"path"`
	Series struct {
		ID int `json:"id"`
	} `json:"series"`
	Movie struct {
		ID int `json:"id"`
	} `json:"movie"`
	Episodes []struct {
		ID            int `json:"id"`
		SeasonNumber  int `json:"seasonNumber"`
		EpisodeNumber int `json:"episodeNumber"`
	} `json:"episodes"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages"`
	ReleaseGroup string          `json:"releaseGroup"`
	DownloadID   string          `json:"downloadId"`
	IndexerFlags int             `json:"indexerFlags"`
}

func parseManualCandidates(r io.Reader) ([]manualCandidate, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxArrBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxArrBody {
		return nil, errors.New("reactivecommit: Arr response exceeds byte bound")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var candidates []manualCandidate
	if err := dec.Decode(&candidates); err != nil {
		return nil, err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("reactivecommit: trailing Arr response data")
	}
	if len(candidates) > maxArrCandidates {
		return nil, errors.New("reactivecommit: too many manual-import candidates")
	}
	for _, c := range candidates {
		if c.Path == "" {
			return nil, errors.New("reactivecommit: malformed manual-import candidate")
		}
	}
	return candidates, nil
}

func (c *ArrClient) Commit(ctx context.Context, req CommitRequest) (registration Registration, err error) {
	if req.Identity == nil || req.SourcePath == "" {
		return Registration{}, errors.New("reactivecommit: invalid Arr request")
	}
	arrPath, err := arrVisiblePath(req.SourcePath)
	if err != nil {
		return Registration{}, err
	}
	owner, found, err := c.findOwner(ctx, req.Identity)
	if err != nil {
		return Registration{}, err
	}
	ownerCreated := !found
	if ownerCreated {
		owner, err = c.addOwner(ctx, req)
		if err != nil {
			return Registration{}, err
		}
		defer func() {
			if err == nil {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			err = errors.Join(err, c.removeOwner(cleanupCtx, owner))
		}()
	}
	if req.TargetName != "" && owner.target.Name != req.TargetName {
		return Registration{}, errors.New("reactivecommit: explicit Arr target does not own identity")
	}
	registration = Registration{ArrName: owner.target.Name, Kind: owner.kind, ItemID: owner.id, OwnerCreated: ownerCreated}
	if owner.kind == "movie" {
		prior := owner.monitored
		registration.PriorMonitored = &prior
	}
	candidate, episodes, err := c.exactCandidate(ctx, owner, arrPath, req.Identity)
	if err != nil {
		return Registration{}, err
	}
	registration.Episodes = episodes
	commandID, err := c.importCandidate(ctx, owner, candidate)
	if err != nil {
		return Registration{}, err
	}
	if err := c.waitCommand(ctx, owner.target, commandID); err != nil {
		return Registration{}, err
	}
	fileIDs, err := c.registeredFiles(ctx, owner, episodes)
	if err != nil {
		return Registration{}, err
	}
	registration.FileIDs = fileIDs
	if owner.kind == "series" && req.MonitorEpisodes {
		if err := c.setEpisodeMonitoring(ctx, owner.target, episodes, true); err != nil {
			_ = c.Undo(ctx, registration)
			return Registration{}, err
		}
	}
	if owner.kind == "movie" && req.MonitorMovie && !owner.monitored {
		if err := c.setMovieMonitoring(ctx, owner, true); err != nil {
			_ = c.Undo(ctx, registration)
			return Registration{}, err
		}
	}
	return registration, nil
}

func arrVisiblePath(source string) (string, error) {
	clean := filepath.Clean(source)
	if !filepath.IsAbs(clean) {
		return "", errors.New("reactivecommit: source path is not absolute")
	}
	if clean == "/data" || strings.HasPrefix(clean, "/data/") {
		return filepath.Join("/mnt/darkharrbor", strings.TrimPrefix(clean, "/data")), nil
	}
	if clean == "/mnt/darkharrbor" || strings.HasPrefix(clean, "/mnt/darkharrbor/") {
		return clean, nil
	}
	return "", errors.New("reactivecommit: source path has no declared Arr mapping")
}

// findOwner locates the Arr already holding this identity. RD-34 D6: a captured
// Stremio identity carries an IMDb ID and NEITHER a TVDB nor a TMDB one, so the
// native-ID query is unavailable and this would hard-fail BEFORE addOwner is
// ever reached. When only IMDb is present it lists the resource and matches on
// `imdbId`, the same fallback `reactivequeue.Router.targetState` already uses.
func (c *ArrClient) findOwner(ctx context.Context, identity *store.ProviderIdentity) (arrOwner, bool, error) {
	var path, imdbOnly string
	switch identity.Kind {
	case "series":
		switch {
		case strings.TrimSpace(identity.IDs.TVDB) != "":
			path = "/api/v3/series?tvdbId=" + url.QueryEscape(identity.IDs.TVDB)
		case strings.TrimSpace(identity.IDs.IMDB) != "":
			path, imdbOnly = "/api/v3/series", strings.TrimSpace(identity.IDs.IMDB)
		default:
			return arrOwner{}, false, errors.New("reactivecommit: series lacks a usable identity")
		}
	case "movie":
		switch {
		case strings.TrimSpace(identity.IDs.TMDB) != "":
			path = "/api/v3/movie?tmdbId=" + url.QueryEscape(identity.IDs.TMDB)
		case strings.TrimSpace(identity.IDs.IMDB) != "":
			path, imdbOnly = "/api/v3/movie", strings.TrimSpace(identity.IDs.IMDB)
		default:
			return arrOwner{}, false, errors.New("reactivecommit: movie lacks a usable identity")
		}
	default:
		return arrOwner{}, false, errors.New("reactivecommit: unsupported identity kind")
	}
	var matches []arrOwner
	for _, target := range c.targets {
		var rows []struct {
			ID        int    `json:"id"`
			Monitored bool   `json:"monitored"`
			IMDBID    string `json:"imdbId"`
		}
		status, err := c.doJSON(ctx, target, http.MethodGet, path, nil, &rows)
		if err != nil {
			return arrOwner{}, false, err
		}
		if status == http.StatusNotFound {
			continue
		}
		if status != http.StatusOK {
			continue
		}
		for _, row := range rows {
			if row.ID <= 0 {
				continue
			}
			// An IMDb-only lookup lists the whole resource, so every row must
			// be filtered down to the exact coordinate rather than trusted.
			if imdbOnly != "" && !strings.EqualFold(strings.TrimSpace(row.IMDBID), imdbOnly) {
				continue
			}
			matches = append(matches, arrOwner{target: target, kind: identity.Kind, id: row.ID, monitored: row.Monitored})
		}
	}
	if len(matches) > 1 {
		return arrOwner{}, false, errors.New("reactivecommit: ambiguous Arr ownership")
	}
	if len(matches) == 0 {
		return arrOwner{}, false, nil
	}
	return matches[0], true, nil
}

func (c *ArrClient) addOwner(ctx context.Context, req CommitRequest) (arrOwner, error) {
	if req.TargetName == "" || req.RootFolder == "" || req.QualityProfile <= 0 {
		return arrOwner{}, errors.New("reactivecommit: absent identity requires explicit Arr, root folder, and quality profile")
	}
	var target *config.ArrTarget
	for i := range c.targets {
		if c.targets[i].Name == req.TargetName {
			if target != nil {
				return arrOwner{}, errors.New("reactivecommit: duplicate Arr target")
			}
			target = &c.targets[i]
		}
	}
	if target == nil {
		return arrOwner{}, errors.New("reactivecommit: explicit Arr target not configured")
	}
	// RD-34 D4: the Stremio coordinate carries an IMDb ID and NEITHER a TVDB
	// nor a TMDB one, so without this branch `addOwner` fails on every
	// captured identity the moment RX-9.4 makes it reachable. IMDb is used
	// ONLY as a fallback; a TVDB/TMDB value still wins where present, and the
	// ambiguity refusal below is unchanged.
	var lookupPath string
	if req.Identity.Kind == "series" {
		if tvdb := strings.TrimSpace(req.Identity.IDs.TVDB); tvdb != "" {
			lookupPath = "/api/v3/series/lookup?term=tvdb:" + url.QueryEscape(tvdb)
		} else if imdb := strings.TrimSpace(req.Identity.IDs.IMDB); imdb != "" {
			lookupPath = "/api/v3/series/lookup?term=imdb:" + url.QueryEscape(imdb)
		} else {
			return arrOwner{}, errors.New("reactivecommit: identity carries no series lookup key")
		}
	} else {
		if tmdb := strings.TrimSpace(req.Identity.IDs.TMDB); tmdb != "" {
			lookupPath = "/api/v3/movie/lookup/tmdb?tmdbId=" + url.QueryEscape(tmdb)
		} else if imdb := strings.TrimSpace(req.Identity.IDs.IMDB); imdb != "" {
			lookupPath = "/api/v3/movie/lookup/imdb?imdbId=" + url.QueryEscape(imdb)
		} else {
			return arrOwner{}, errors.New("reactivecommit: identity carries no movie lookup key")
		}
	}
	var body map[string]any
	if req.Identity.Kind == "series" {
		var candidates []map[string]any
		status, err := c.doJSON(ctx, *target, http.MethodGet, lookupPath, nil, &candidates)
		if err != nil || status != http.StatusOK {
			return arrOwner{}, errors.New("reactivecommit: Arr lookup failed")
		}
		if len(candidates) != 1 {
			return arrOwner{}, errors.New("reactivecommit: Arr lookup is ambiguous")
		}
		body = candidates[0]
	} else {
		status, err := c.doJSON(ctx, *target, http.MethodGet, lookupPath, nil, &body)
		if err != nil || status != http.StatusOK || len(body) == 0 {
			return arrOwner{}, errors.New("reactivecommit: Arr lookup failed")
		}
	}
	body["rootFolderPath"] = req.RootFolder
	body["qualityProfileId"] = req.QualityProfile
	monitored := req.Identity.Kind == "movie" && req.MonitorMovie
	body["monitored"] = monitored
	if req.Identity.Kind == "series" {
		body["seasonFolder"] = true
		body["addOptions"] = map[string]any{"monitor": "none", "searchForMissingEpisodes": false}
	}
	var added struct {
		ID        int  `json:"id"`
		Monitored bool `json:"monitored"`
	}
	path := "/api/v3/" + req.Identity.Kind
	if req.Identity.Kind == "movie" {
		path = "/api/v3/movie"
	}
	status, err := c.doJSON(ctx, *target, http.MethodPost, path, body, &added)
	if err != nil || (status != http.StatusCreated && status != http.StatusAccepted) || added.ID <= 0 {
		return arrOwner{}, errors.New("reactivecommit: Arr add failed")
	}
	return arrOwner{target: *target, kind: req.Identity.Kind, id: added.ID, monitored: added.Monitored}, nil
}

func (c *ArrClient) exactCandidate(ctx context.Context, owner arrOwner, path string, identity *store.ProviderIdentity) (manualCandidate, []store.ReactiveCommitEpisode, error) {
	candidate, err := c.manualCandidateAt(ctx, owner.target, path)
	if err != nil {
		return manualCandidate{}, nil, err
	}
	if owner.kind == "movie" {
		if candidate.Movie.ID != owner.id {
			return manualCandidate{}, nil, errors.New("reactivecommit: movie candidate mismatch")
		}
		return candidate, nil, nil
	}
	if candidate.Series.ID != owner.id || len(candidate.Episodes) == 0 || len(candidate.Episodes) > MaxEpisodes {
		return manualCandidate{}, nil, errors.New("reactivecommit: series candidate mismatch")
	}
	wanted := make(map[[2]int]bool, len(identity.Episodes))
	for _, ep := range identity.Episodes {
		wanted[[2]int{ep.Season, ep.Episode}] = true
	}
	if len(wanted) == 0 {
		return manualCandidate{}, nil, errors.New("reactivecommit: series proposal lacks episode scope")
	}
	episodes := make([]store.ReactiveCommitEpisode, 0, len(candidate.Episodes))
	for _, ep := range candidate.Episodes {
		if ep.ID <= 0 || !wanted[[2]int{ep.SeasonNumber, ep.EpisodeNumber}] {
			return manualCandidate{}, nil, errors.New("reactivecommit: episode candidate mismatch")
		}
		var current map[string]any
		status, err := c.doJSON(ctx, owner.target, http.MethodGet, "/api/v3/episode/"+strconv.Itoa(ep.ID), nil, &current)
		if err != nil || status != http.StatusOK {
			return manualCandidate{}, nil, errors.New("reactivecommit: episode lookup failed")
		}
		if has, _ := current["hasFile"].(bool); has {
			return manualCandidate{}, nil, fmt.Errorf("%w: episode already has a file", ErrArrFileAlreadyPresent)
		}
		monitored, _ := current["monitored"].(bool)
		episodes = append(episodes, store.ReactiveCommitEpisode{ID: ep.ID, PriorMonitored: monitored})
	}
	if len(episodes) != len(wanted) {
		return manualCandidate{}, nil, errors.New("reactivecommit: incomplete episode mapping")
	}
	return candidate, episodes, nil
}

func (c *ArrClient) manualCandidateAt(ctx context.Context, target config.ArrTarget, path string) (manualCandidate, error) {
	folder := filepath.Dir(path)
	req, err := c.request(ctx, target, http.MethodGet, "/api/v3/manualimport?folder="+url.QueryEscape(folder)+"&filterExistingFiles=false", nil)
	if err != nil {
		return manualCandidate{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return manualCandidate{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return manualCandidate{}, fmt.Errorf("reactivecommit: manual import status %d", resp.StatusCode)
	}
	candidates, err := parseManualCandidates(resp.Body)
	if err != nil {
		return manualCandidate{}, err
	}
	var exact []manualCandidate
	for _, candidate := range candidates {
		if filepath.Clean(candidate.Path) == path {
			exact = append(exact, candidate)
		}
	}
	if len(exact) != 1 {
		return manualCandidate{}, errors.New("reactivecommit: manual import candidate is absent or ambiguous")
	}
	return exact[0], nil
}

func (c *ArrClient) importCandidate(ctx context.Context, owner arrOwner, candidate manualCandidate) (int, error) {
	return c.importCandidateEpisodes(ctx, owner, candidate, nil)
}

func (c *ArrClient) importCandidateEpisodes(ctx context.Context, owner arrOwner, candidate manualCandidate, episodeIDs []int) (int, error) {
	file := map[string]any{"path": candidate.Path, "quality": candidate.Quality, "languages": candidate.Languages, "releaseGroup": candidate.ReleaseGroup, "downloadId": candidate.DownloadID, "indexerFlags": candidate.IndexerFlags}
	if owner.kind == "series" {
		file["seriesId"] = owner.id
		if episodeIDs == nil {
			episodeIDs = make([]int, 0, len(candidate.Episodes))
			for _, ep := range candidate.Episodes {
				episodeIDs = append(episodeIDs, ep.ID)
			}
		}
		file["episodeIds"] = episodeIDs
	} else {
		file["movieId"] = owner.id
	}
	body := map[string]any{"name": "ManualImport", "files": []any{file}, "importMode": "copy"}
	var command struct {
		ID int `json:"id"`
	}
	status, err := c.doJSON(ctx, owner.target, http.MethodPost, "/api/v3/command", body, &command)
	if err != nil || (status != http.StatusCreated && status != http.StatusAccepted) || command.ID <= 0 {
		return 0, errors.New("reactivecommit: manual import command failed")
	}
	return command.ID, nil
}

func (c *ArrClient) waitCommand(ctx context.Context, target config.ArrTarget, id int) error {
	for i := 0; i < maxPolls; i++ {
		var command struct {
			Status string `json:"status"`
			Result string `json:"result"`
		}
		status, err := c.doJSON(ctx, target, http.MethodGet, "/api/v3/command/"+strconv.Itoa(id), nil, &command)
		if err != nil || status != http.StatusOK {
			return errors.New("reactivecommit: command poll failed")
		}
		switch strings.ToLower(command.Status) {
		case "completed":
			if strings.EqualFold(command.Result, "successful") || command.Result == "" {
				return nil
			}
			return errors.New("reactivecommit: import command failed")
		case "failed", "aborted":
			return errors.New("reactivecommit: import command failed")
		}
		if err := c.sleep(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
	return errors.New("reactivecommit: import command timed out")
}

// Rescan asks the exact Arr owner recorded by RX-4.1 to discover the
// materialized replacement. It never searches for or guesses a new owner.
func (c *ArrClient) Rescan(ctx context.Context, record store.ReactiveCommit) error {
	if record.ArrName == "" || record.ArrItemID <= 0 || (record.ArrKind != "series" && record.ArrKind != "movie") {
		return errors.New("reactivecommit: invalid promotion rescan owner")
	}
	var target *config.ArrTarget
	for i := range c.targets {
		if c.targets[i].Name == record.ArrName {
			if target != nil {
				return errors.New("reactivecommit: ambiguous promotion rescan owner")
			}
			target = &c.targets[i]
		}
	}
	if target == nil {
		return errors.New("reactivecommit: promotion rescan owner unavailable")
	}
	name, idKey := "RescanSeries", "seriesId"
	if record.ArrKind == "movie" {
		name, idKey = "RescanMovie", "movieId"
	}
	var command struct {
		ID int `json:"id"`
	}
	status, err := c.doJSON(ctx, *target, http.MethodPost, "/api/v3/command", map[string]any{"name": name, idKey: record.ArrItemID}, &command)
	if err != nil || (status != http.StatusCreated && status != http.StatusAccepted) || command.ID <= 0 {
		return errors.New("reactivecommit: promotion rescan command failed")
	}
	return c.waitCommand(ctx, *target, command.ID)
}

// Materialize imports the verified promoted bytes through the exact Arr owner,
// then rescans and verifies that Arr no longer registers the proxy .strm.
func (c *ArrClient) Materialize(ctx context.Context, record store.ReactiveCommit, sourcePath string, expectedSize int64) error {
	if record.ArrName == "" || record.ArrItemID <= 0 || expectedSize <= 0 || (record.ArrKind != "series" && record.ArrKind != "movie") {
		return errors.New("reactivecommit: invalid promotion materialization")
	}
	arrPath, err := arrVisiblePath(sourcePath)
	if err != nil {
		return errors.New("reactivecommit: invalid promotion materialization")
	}
	var target *config.ArrTarget
	for i := range c.targets {
		if c.targets[i].Name == record.ArrName {
			if target != nil {
				return errors.New("reactivecommit: ambiguous promotion materialization owner")
			}
			target = &c.targets[i]
		}
	}
	if target == nil {
		return errors.New("reactivecommit: promotion materialization owner unavailable")
	}
	owner := arrOwner{target: *target, kind: record.ArrKind, id: record.ArrItemID}
	candidate, err := c.manualCandidateAt(ctx, *target, arrPath)
	if err != nil {
		return err
	}
	var episodeIDs []int
	if record.ArrKind == "series" {
		if len(record.Episodes) == 0 || len(record.Episodes) > MaxEpisodes {
			return errors.New("reactivecommit: invalid promotion episode scope")
		}
		episodeIDs = make([]int, 0, len(record.Episodes))
		for _, episode := range record.Episodes {
			if episode.ID <= 0 {
				return errors.New("reactivecommit: invalid promotion episode scope")
			}
			episodeIDs = append(episodeIDs, episode.ID)
		}
	}
	commandID, err := c.importCandidateEpisodes(ctx, owner, candidate, episodeIDs)
	if err != nil {
		return errors.New("reactivecommit: promotion import failed")
	}
	if err := c.waitCommand(ctx, *target, commandID); err != nil {
		return errors.New("reactivecommit: promotion import failed")
	}
	if err := c.Rescan(ctx, record); err != nil {
		return err
	}
	return c.verifyMaterializedFile(ctx, owner, record, expectedSize)
}

func (c *ArrClient) verifyMaterializedFile(ctx context.Context, owner arrOwner, record store.ReactiveCommit, expectedSize int64) error {
	fileIDs, err := c.registeredFiles(ctx, owner, record.Episodes)
	if err != nil || len(fileIDs) == 0 {
		return errors.New("reactivecommit: promoted file registration missing")
	}
	for _, id := range fileIDs {
		var file struct {
			Size         int64  `json:"size"`
			RelativePath string `json:"relativePath"`
		}
		path := "/api/v3/episodefile/"
		if owner.kind == "movie" {
			path = "/api/v3/moviefile/"
		}
		status, getErr := c.doJSON(ctx, owner.target, http.MethodGet, path+strconv.Itoa(id), nil, &file)
		if getErr != nil || status != http.StatusOK || file.Size != expectedSize || strings.EqualFold(filepath.Ext(file.RelativePath), ".strm") {
			return errors.New("reactivecommit: promoted file registration mismatch")
		}
	}
	return nil
}

func (c *ArrClient) registeredFiles(ctx context.Context, owner arrOwner, episodes []store.ReactiveCommitEpisode) ([]int, error) {
	if owner.kind == "movie" {
		var movie struct {
			HasFile     bool `json:"hasFile"`
			MovieFileID int  `json:"movieFileId"`
		}
		status, err := c.doJSON(ctx, owner.target, http.MethodGet, "/api/v3/movie/"+strconv.Itoa(owner.id), nil, &movie)
		if err != nil || status != http.StatusOK || !movie.HasFile || movie.MovieFileID <= 0 {
			return nil, errors.New("reactivecommit: movie file registration missing")
		}
		return []int{movie.MovieFileID}, nil
	}
	ids := make(map[int]bool)
	for _, ep := range episodes {
		var current struct {
			HasFile       bool `json:"hasFile"`
			EpisodeFileID int  `json:"episodeFileId"`
		}
		status, err := c.doJSON(ctx, owner.target, http.MethodGet, "/api/v3/episode/"+strconv.Itoa(ep.ID), nil, &current)
		if err != nil || status != http.StatusOK || !current.HasFile || current.EpisodeFileID <= 0 {
			return nil, errors.New("reactivecommit: episode file registration missing")
		}
		ids[current.EpisodeFileID] = true
	}
	out := make([]int, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	return out, nil
}

func (c *ArrClient) setEpisodeMonitoring(ctx context.Context, target config.ArrTarget, episodes []store.ReactiveCommitEpisode, monitored bool) error {
	for _, ep := range episodes {
		var current map[string]any
		status, err := c.doJSON(ctx, target, http.MethodGet, "/api/v3/episode/"+strconv.Itoa(ep.ID), nil, &current)
		if err != nil || status != http.StatusOK {
			return errors.New("reactivecommit: episode monitoring lookup failed")
		}
		current["monitored"] = monitored
		status, err = c.doJSON(ctx, target, http.MethodPut, "/api/v3/episode/"+strconv.Itoa(ep.ID), current, &map[string]any{})
		if err != nil || status != http.StatusAccepted {
			return errors.New("reactivecommit: episode monitoring update failed")
		}
	}
	return nil
}

func (c *ArrClient) setMovieMonitoring(ctx context.Context, owner arrOwner, monitored bool) error {
	var current map[string]any
	status, err := c.doJSON(ctx, owner.target, http.MethodGet, "/api/v3/movie/"+strconv.Itoa(owner.id), nil, &current)
	if err != nil || status != http.StatusOK {
		return errors.New("reactivecommit: movie monitoring lookup failed")
	}
	current["monitored"] = monitored
	status, err = c.doJSON(ctx, owner.target, http.MethodPut, "/api/v3/movie/"+strconv.Itoa(owner.id), current, &map[string]any{})
	if err != nil || status != http.StatusAccepted {
		return errors.New("reactivecommit: movie monitoring update failed")
	}
	return nil
}

func (c *ArrClient) Undo(ctx context.Context, registration Registration) error {
	var target *config.ArrTarget
	for i := range c.targets {
		if c.targets[i].Name == registration.ArrName {
			target = &c.targets[i]
			break
		}
	}
	if target == nil {
		return errors.New("reactivecommit: recorded Arr target missing")
	}
	for _, id := range registration.FileIDs {
		path := "/api/v3/episodefile/"
		if registration.Kind == "movie" {
			path = "/api/v3/moviefile/"
		}
		status, err := c.doJSON(ctx, *target, http.MethodDelete, path+strconv.Itoa(id), nil, nil)
		if err != nil || (status != http.StatusOK && status != http.StatusAccepted && status != http.StatusNoContent && status != http.StatusNotFound) {
			return errors.New("reactivecommit: Arr file unregister failed")
		}
	}
	if registration.OwnerCreated {
		return c.removeOwner(ctx, arrOwner{target: *target, kind: registration.Kind, id: registration.ItemID})
	}
	if registration.Kind == "series" {
		for _, ep := range registration.Episodes {
			if err := c.setEpisodeMonitoring(ctx, *target, []store.ReactiveCommitEpisode{ep}, ep.PriorMonitored); err != nil {
				return err
			}
		}
	} else if registration.PriorMonitored != nil {
		owner := arrOwner{target: *target, kind: "movie", id: registration.ItemID}
		if err := c.setMovieMonitoring(ctx, owner, *registration.PriorMonitored); err != nil {
			return err
		}
	}
	return nil
}

func (c *ArrClient) removeOwner(ctx context.Context, owner arrOwner) error {
	if owner.id <= 0 || (owner.kind != "series" && owner.kind != "movie") {
		return errors.New("reactivecommit: invalid recorded Arr owner")
	}
	path := "/api/v3/series/"
	if owner.kind == "movie" {
		path = "/api/v3/movie/"
	}
	status, err := c.doJSON(ctx, owner.target, http.MethodDelete, path+strconv.Itoa(owner.id)+"?deleteFiles=false&addImportExclusion=false", nil, nil)
	if err != nil || (status != http.StatusOK && status != http.StatusAccepted && status != http.StatusNoContent && status != http.StatusNotFound) {
		return errors.New("reactivecommit: Arr owner cleanup failed")
	}
	return nil
}

func (c *ArrClient) request(ctx context.Context, target config.ArrTarget, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", target.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *ArrClient) doJSON(ctx context.Context, target config.ArrTarget, method, path string, body, out any) (int, error) {
	req, err := c.request(ctx, target, method, path, body)
	if err != nil {
		return 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxArrBody+1))
		if readErr != nil {
			return resp.StatusCode, readErr
		}
		if len(data) > maxArrBody {
			return resp.StatusCode, errors.New("reactivecommit: Arr response exceeds byte bound")
		}
		if err := json.Unmarshal(data, out); err != nil && len(data) != 0 {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
