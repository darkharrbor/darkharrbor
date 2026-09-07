package wizard

// ONB-01: Zero-account wizard demo. Grabs and plays one curated,
// hard-coded public-domain Internet Archive item end-to-end (Radarr
// search -> grab -> DH import -> .strm -> HTTP range/seek) through the
// already-wired HTTP-stream lane (HR0.3), then offers cleanup.
//
// Deliberately NOT a general IA search surface: exactly two hard-coded
// candidates, tried in list order until one verifies. The first is
// expected absent from a fresh library and proves the full
// add -> search -> grab -> import -> cleanup cycle; the second is a
// well-known, extremely reliable public-domain title kept only as a
// verify-only fallback if the first is ever unavailable through the
// configured backend. If a curated candidate is found to already exist
// in the library (as it does on this operator's own instance -- real,
// non-disposable content), the demo NEVER adds, re-grabs, or deletes it;
// it only verifies playback of what is already there and never offers
// cleanup for it. Only items the demo itself added are ever candidates
// for cleanup.
//
// Every network call is bounded and context-cancellable. Malformed Arr
// responses, timeouts, or an unreachable Arr abstain for that candidate
// and advance to the next rather than guessing. Nothing here ever
// prints a .strm URL, its tok= token, or a raw Arr API key.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/util"
)

// curatedDemoItem is one hard-coded candidate. Order is preference order.
type curatedDemoItem struct {
	Title  string
	Year   int
	TMDBID int
}

// curatedDemoItems is the entire, hard-coded candidate set. This is never
// derived from a general search; every entry is chosen and reviewed by a
// human ahead of time.
var curatedDemoItems = []curatedDemoItem{
	// "Detour" (1945): live-verified 2026-07-31 to resolve through an
	// ID-native HTTP backend (aiostreams), which is required for Radarr's
	// automatic search to find it at all -- Radarr caches this indexer's
	// capabilities as advertising ID search (because ID-native handlers
	// are also registered) and always sends ID-only automatic-search
	// queries as a result. A title-only backend (e.g. "ia") never sees
	// those queries; an earlier candidate ("Plan 9 from Outer Space",
	// tmdbId 10513, only resolvable via "ia") was live-gated and found to
	// silently return zero automatic-search results for exactly this
	// reason -- confirmed directly against the torznab endpoint, not
	// guessed. Absent from this library at time of writing.
	{Title: "Detour", Year: 1945, TMDBID: 20367},
	// Night of the Living Dead (1968): kept only as a verify-only
	// fallback for a library where it doesn't already exist. On this
	// operator's own instance it is real, pre-existing content (Radarr
	// movie id 118) whose recorded movieFile.path does not currently
	// match anything on disk (stale metadata predating this row, live
	// finding 2026-07-31) -- the demo correctly abstains rather than
	// guessing a substitute path, which is exactly the fail-closed
	// behavior required here; fixing that stale Radarr record is outside
	// ONB-01's scope (real production library state, not a demo
	// artifact).
	{Title: "Night of the Living Dead", Year: 1968, TMDBID: 10331},
}

const (
	defaultDemoTimeout = 300 * time.Second
	demoHTTPTimeout    = 15 * time.Second
	// demoReleaseSearchTimeout bounds Radarr's own release-search call,
	// which queries every enabled indexer live and synchronously before
	// responding -- materially slower than a single lane's HTTP calls.
	demoReleaseSearchTimeout = 180 * time.Second
	demoMaxBodyBytes         = 1 << 20 // 1 MiB bounded read cap per verification request
)

// demoPollInterval is a var (not const) solely so deterministic tests can
// shrink it; production always uses the 5s default.
var demoPollInterval = 5 * time.Second

// demoArrTarget is the minimal Radarr connection info the demo needs. Kept
// separate from discoveredArrInstance/store.ArrInstance so this file has no
// dependency on how the caller obtained the credentials (wizard in-memory
// creds vs. a standalone re-run resolving a sealed secret).
type demoArrTarget struct {
	Name   string
	URL    string
	APIKey util.Redacted
}

// demoResult is the sanitized, printable outcome of one demo run.
type demoResult struct {
	Title       string
	TMDBID      int
	PreExisting bool // candidate already existed in the library before this run
	Added       bool // demo itself added the movie
	HasFile     bool // movie has an imported file (grab completed)
	Verified    bool // HTTP range/seek check passed
	CleanedUp   bool // demo removed what it added
	Detail      []string
	Err         error
}

// runZeroAccountDemo tries each curated candidate in order until one
// verifies or the list is exhausted. cleanupDecision is asked only for a
// candidate the demo itself added (never for pre-existing library content);
// returning true removes the movie (and its file) via Radarr.
func runZeroAccountDemo(ctx context.Context, out io.Writer, radarr demoArrTarget, dataRoot string, timeout time.Duration, cleanupDecision func(title string) bool) *demoResult {
	if timeout <= 0 {
		timeout = defaultDemoTimeout
	}
	// No blanket client.Timeout: Go's http.Client.Timeout is an absolute
	// per-request cap that overrides a longer context deadline entirely
	// (confirmed live 2026-07-31 -- a 15s client.Timeout silently
	// truncated a call given an explicit 180s context, with the error
	// message crediting "Client.Timeout" rather than the context). Every
	// call already carries its own context.WithTimeout (demoHTTPTimeout
	// for ordinary calls, demoReleaseSearchTimeout for release search),
	// so bounding happens per-call via context alone.
	client := &http.Client{}

	var lastErr error
	for _, cand := range curatedDemoItems {
		if ctx.Err() != nil {
			return &demoResult{Title: cand.Title, TMDBID: cand.TMDBID, Err: ctx.Err()}
		}
		res := attemptCuratedDemo(ctx, out, client, radarr, cand, dataRoot, timeout, cleanupDecision)
		if res.Verified {
			return res
		}
		if res.Err != nil {
			fmt.Fprintf(out, "  ⚠ demo candidate %q: %v — trying next curated item\n", cand.Title, res.Err)
			lastErr = res.Err
			continue
		}
		lastErr = fmt.Errorf("candidate %q did not verify", cand.Title)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no curated demo candidate available")
	}
	return &demoResult{Err: fmt.Errorf("zero-account demo: %w (abstaining — no candidate forced or fabricated)", lastErr)}
}

func attemptCuratedDemo(ctx context.Context, out io.Writer, client *http.Client, radarr demoArrTarget, cand curatedDemoItem, dataRoot string, timeout time.Duration, cleanupDecision func(title string) bool) *demoResult {
	res := &demoResult{Title: cand.Title, TMDBID: cand.TMDBID}
	attemptStart := time.Now()

	existing, err := radarrFindMovieByTMDBID(ctx, client, radarr, cand.TMDBID)
	if err != nil {
		res.Err = fmt.Errorf("lookup: %w", err)
		return res
	}

	var movieID int
	if existing != nil {
		res.PreExisting = true
		movieID = existing.ID
		if !existing.HasFile {
			// Genuine pre-existing library entry with no file yet (e.g. a
			// real pending user grab). Never force a search on someone
			// else's in-flight item -- abstain and try the next candidate.
			res.Err = fmt.Errorf("pre-existing library entry has no file yet; not forcing a search on it")
			return res
		}
		res.HasFile = true
		fmt.Fprintf(out, "  ℹ %q already in library (pre-existing, real content) — verifying playback only, no cleanup will be offered\n", cand.Title)
	} else {
		profileID, rootFolder, perr := radarrDefaultAddTargets(ctx, client, radarr)
		if perr != nil {
			res.Err = fmt.Errorf("resolve add targets: %w", perr)
			return res
		}
		id, addErr := radarrAddMovie(ctx, client, radarr, cand, profileID, rootFolder)
		if addErr != nil {
			res.Err = fmt.Errorf("add movie: %w", addErr)
			return res
		}
		movieID = id
		res.Added = true
		fmt.Fprintf(out, "  ✓ %q added to Radarr (id=%d) — searching the real HTTP-stream indexer\n", cand.Title, movieID)

		// Radarr's release-search endpoint queries every enabled indexer
		// live and synchronously before responding -- live finding
		// 2026-07-31: this took long enough with this operator's real
		// indexer set (usenet + torrent + http-stream, some backed by
		// real external trackers) that demoHTTPTimeout*4 (60s) was not
		// enough and the call was cut off mid-flight. Release search gets
		// its own materially larger bound instead of being tied to the
		// per-request HTTP timeout.
		searchCtx, searchCancel := context.WithTimeout(ctx, demoReleaseSearchTimeout)
		grabErr := searchAndGrabHTTPStreamRelease(searchCtx, client, radarr, movieID)
		searchCancel()
		if grabErr != nil {
			res.Err = fmt.Errorf("search/grab http-stream release: %w", grabErr)
			offerDemoCleanup(ctx, out, client, radarr, movieID, cand, dataRoot, attemptStart, res, cleanupDecision)
			return res
		}
		fmt.Fprintf(out, "  ✓ %q grabbed from the real HTTP-stream/Internet-Archive lane\n", cand.Title)

		pollCtx, cancel := context.WithTimeout(ctx, timeout)
		hasFile, pollErr := radarrPollForFile(pollCtx, client, radarr, movieID)
		cancel()
		if pollErr != nil {
			res.Err = fmt.Errorf("poll for import: %w", pollErr)
			// Partial state: added but not (yet) imported. Still offer
			// cleanup for what we added -- never silently leave it, never
			// silently delete it either.
			offerDemoCleanup(ctx, out, client, radarr, movieID, cand, dataRoot, attemptStart, res, cleanupDecision)
			return res
		}
		res.HasFile = hasFile
		if !hasFile {
			res.Err = fmt.Errorf("timed out waiting for import")
			offerDemoCleanup(ctx, out, client, radarr, movieID, cand, dataRoot, attemptStart, res, cleanupDecision)
			return res
		}
		fmt.Fprintf(out, "  ✓ %q imported — verifying real HTTP range/seek\n", cand.Title)
	}

	movieFile, err := radarrGetMovieFile(ctx, client, radarr, movieID)
	if err != nil {
		res.Err = fmt.Errorf("read movie file record: %w", err)
		if res.Added {
			offerDemoCleanup(ctx, out, client, radarr, movieID, cand, dataRoot, attemptStart, res, cleanupDecision)
		}
		return res
	}

	strmPath := mapArrPathToDHPath(movieFile.Path, dataRoot)
	if res.Added {
		// Live finding 2026-07-31 (reproduced identically for a
		// pre-existing library item too, so this is a systemic
		// characteristic of this deployment, not specific to the demo):
		// Radarr's recorded movieFile.path reflects its own post-import
		// "renamed" naming convention, but the .strm DarkHarrbor actually
		// wrote and never moved sits at the original ingest-time path
		// (named from the release title, not Radarr's canonical movie
		// name). For content the demo just added itself, fall back to
		// locating the most recently written matching .strm under
		// dataRoot/movies rather than trusting the exact reported path --
		// safe here specifically because recency plus a title/year token
		// match is unambiguous for something created moments ago. This
		// fallback is deliberately NOT applied to the pre-existing/
		// verify-only branch above, where the same ambiguity against a
		// real, possibly years-old library full of similarly-named files
		// would risk verifying the wrong file entirely; that branch
		// stays strict and fails closed instead (see live finding on the
		// NOTLD fallback in WORKLOG).
		if _, statErr := os.Stat(strmPath); statErr != nil {
			if found, findErr := locateRecentStrmFile(dataRoot, cand.Title, cand.Year, attemptStart.Add(-time.Minute)); findErr == nil {
				strmPath = found
			}
		}
	}
	if verr := verifyStrmPlayback(ctx, client, strmPath); verr != nil {
		res.Err = fmt.Errorf("range/seek verification: %w", verr)
		if res.Added {
			offerDemoCleanup(ctx, out, client, radarr, movieID, cand, dataRoot, attemptStart, res, cleanupDecision)
		}
		return res
	}
	res.Verified = true
	fmt.Fprintf(out, "  ✅ %q: HEAD + range + seek all verified against the real stream\n", cand.Title)

	if res.Added {
		offerDemoCleanup(ctx, out, client, radarr, movieID, cand, dataRoot, attemptStart, res, cleanupDecision)
	} else {
		fmt.Fprintln(out, "  ℹ Pre-existing library item — left untouched, no cleanup offered.")
	}
	return res
}

func offerDemoCleanup(ctx context.Context, out io.Writer, client *http.Client, radarr demoArrTarget, movieID int, cand curatedDemoItem, dataRoot string, attemptStart time.Time, res *demoResult, cleanupDecision func(title string) bool) {
	title := cand.Title
	if cleanupDecision == nil || !cleanupDecision(title) {
		fmt.Fprintf(out, "  ℹ Leaving %q in your library (cleanup declined).\n", title)
		return
	}
	if err := radarrDeleteMovie(ctx, client, radarr, movieID); err != nil {
		fmt.Fprintf(out, "  ⚠ cleanup of %q failed: %v (remove it manually from Radarr if desired)\n", title, err)
		return
	}
	res.CleanedUp = true

	// Best-effort supplementary cleanup: live finding 2026-07-31 --
	// Radarr's deleteFiles=true silently no-ops here because its
	// recorded movieFile.path doesn't match the file DarkHarrbor actually
	// wrote (the same rename mismatch documented on the verification
	// path above), leaving the real .strm orphaned on disk after Radarr
	// itself reports the movie gone. Uses the identical bounded,
	// recency-scoped, added-content-only heuristic as the verification
	// fallback -- never touches anything not created moments ago by this
	// same demo run.
	if found, err := locateRecentStrmFile(dataRoot, cand.Title, cand.Year, attemptStart.Add(-time.Minute)); err == nil {
		dir := filepath.Dir(found)
		if rmErr := os.Remove(found); rmErr == nil {
			if entries, derr := os.ReadDir(dir); derr == nil && len(entries) == 0 {
				_ = os.Remove(dir)
			}
		}
	}

	fmt.Fprintf(out, "  ✓ %q removed (demo cleanup).\n", title)
}

// ── Radarr HTTP calls ─────────────────────────────────────────────────────

type radarrMovie struct {
	ID               int    `json:"id"`
	TMDBID           int    `json:"tmdbId"`
	Title            string `json:"title"`
	Monitored        bool   `json:"monitored"`
	HasFile          bool   `json:"hasFile"`
	QualityProfileID int    `json:"qualityProfileId"`
	RootFolderPath   string `json:"rootFolderPath"`
	Path             string `json:"path"`
	MovieFile        *struct {
		Path string `json:"path"`
	} `json:"movieFile"`
}

type radarrRootFolder struct {
	Path string `json:"path"`
}

type radarrQualityProfile struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Items []struct {
		Allowed bool `json:"allowed"`
	} `json:"items"`
}

func radarrRequest(ctx context.Context, client *http.Client, radarr demoArrTarget, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(radarr.URL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", radarr.APIKey.Value())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

func radarrFindMovieByTMDBID(ctx context.Context, client *http.Client, radarr demoArrTarget, tmdbID int) (*radarrMovie, error) {
	resp, err := radarrRequest(ctx, client, radarr, http.MethodGet,
		fmt.Sprintf("/api/v3/movie?tmdbId=%d", tmdbID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, demoMaxBodyBytes))
	if err != nil {
		return nil, err
	}
	var movies []radarrMovie
	if err := json.Unmarshal(raw, &movies); err != nil {
		return nil, fmt.Errorf("malformed movie list: %w", err)
	}
	for i := range movies {
		if movies[i].TMDBID == tmdbID {
			return &movies[i], nil
		}
	}
	return nil, nil
}

func radarrDefaultAddTargets(ctx context.Context, client *http.Client, radarr demoArrTarget) (profileID int, rootFolder string, err error) {
	resp, err := radarrRequest(ctx, client, radarr, http.MethodGet, "/api/v3/rootfolder", nil)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("rootfolder: unexpected status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, demoMaxBodyBytes))
	if err != nil {
		return 0, "", err
	}
	var folders []radarrRootFolder
	if err := json.Unmarshal(raw, &folders); err != nil {
		return 0, "", fmt.Errorf("malformed rootfolder list: %w", err)
	}
	if len(folders) == 0 {
		return 0, "", fmt.Errorf("no root folder configured")
	}
	rootFolder = folders[0].Path

	presp, err := radarrRequest(ctx, client, radarr, http.MethodGet, "/api/v3/qualityprofile", nil)
	if err != nil {
		return 0, "", err
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("qualityprofile: unexpected status %d", presp.StatusCode)
	}
	praw, err := io.ReadAll(io.LimitReader(presp.Body, demoMaxBodyBytes))
	if err != nil {
		return 0, "", err
	}
	var profiles []radarrQualityProfile
	if err := json.Unmarshal(praw, &profiles); err != nil {
		return 0, "", fmt.Errorf("malformed qualityprofile list: %w", err)
	}
	if len(profiles) == 0 {
		return 0, "", fmt.Errorf("no quality profile configured")
	}
	// The demo needs the single most PERMISSIVE profile available, not an
	// arbitrary one: an operator's real acquisition profiles (e.g. a
	// "1080p+ only" profile) can legitimately allow zero of a public-domain
	// fixture's actual available qualities, which silently zeroes every
	// automatic search ("0 reports downloaded") without erroring -- live
	// finding 2026-07-31. Selecting by allowed-quality count is portable to
	// a fresh install's stock Radarr profiles too (e.g. "Any").
	best := profiles[0]
	bestCount := allowedCount(profiles[0])
	for _, p := range profiles[1:] {
		if n := allowedCount(p); n > bestCount {
			best, bestCount = p, n
		}
	}
	return best.ID, rootFolder, nil
}

func allowedCount(p radarrQualityProfile) int {
	n := 0
	for _, item := range p.Items {
		if item.Allowed {
			n++
		}
	}
	return n
}

func radarrAddMovie(ctx context.Context, client *http.Client, radarr demoArrTarget, cand curatedDemoItem, profileID int, rootFolder string) (int, error) {
	body := map[string]any{
		"title":               cand.Title,
		"tmdbId":              cand.TMDBID,
		"year":                cand.Year,
		"qualityProfileId":    profileID,
		"rootFolderPath":      rootFolder,
		"monitored":           true,
		"minimumAvailability": "released",
		"addOptions": map[string]any{
			// Deliberately false: automatic search scores across every
			// enabled indexer (usenet/torrent/http-stream alike) and can
			// select a real non-HTTP-stream release instead of the
			// curated Internet Archive item this row exists to prove --
			// confirmed live 2026-07-31. The demo searches and grabs the
			// http-stream release itself instead (searchAndGrabHTTPStreamRelease).
			"searchForMovie": false,
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	resp, err := radarrRequest(ctx, client, radarr, http.MethodPost, "/api/v3/movie", strings.NewReader(string(raw)))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		errRaw, _ := io.ReadAll(io.LimitReader(resp.Body, demoMaxBodyBytes))
		return 0, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, sanitizeArrError(errRaw))
	}
	respRaw, err := io.ReadAll(io.LimitReader(resp.Body, demoMaxBodyBytes))
	if err != nil {
		return 0, err
	}
	var created radarrMovie
	if err := json.Unmarshal(respRaw, &created); err != nil {
		return 0, fmt.Errorf("malformed add response: %w", err)
	}
	if created.ID == 0 {
		return 0, fmt.Errorf("add response missing id")
	}
	return created.ID, nil
}

// searchAndGrabHTTPStreamRelease deterministically proves the HTTP-stream
// lane (HR0.3) specifically, rather than letting Radarr's own automatic
// search pick whichever indexer scores highest across every enabled
// source (usenet/torrent/http-stream alike) -- live finding 2026-07-31:
// automatic search picked a real Newshosting/NNTP release over the
// intended Internet-Archive-backed one for a title that happened to also
// be available via usenet. It resolves the http-stream indexer's id by
// its configured baseUrl (never by operator-assigned name, which could be
// renamed) and grabs the first non-rejected release Radarr's own release
// search returns from exactly that indexer.
func searchAndGrabHTTPStreamRelease(ctx context.Context, client *http.Client, radarr demoArrTarget, movieID int) error {
	indexerID, err := radarrHTTPStreamIndexerID(ctx, client, radarr)
	if err != nil {
		return fmt.Errorf("resolve http-stream indexer: %w", err)
	}

	resp, err := radarrRequest(ctx, client, radarr, http.MethodGet, fmt.Sprintf("/api/v3/release?movieId=%d", movieID), nil)
	if err != nil {
		return fmt.Errorf("release search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("release search: unexpected status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, demoMaxBodyBytes*8)) // release lists can be large
	if err != nil {
		return err
	}
	var releases []map[string]any
	if err := json.Unmarshal(raw, &releases); err != nil {
		return fmt.Errorf("malformed release list: %w", err)
	}

	for _, rel := range releases {
		idFloat, ok := rel["indexerId"].(float64)
		if !ok || int(idFloat) != indexerID {
			continue
		}
		if rejected, _ := rel["rejected"].(bool); rejected {
			continue
		}
		grabResp, err := radarrRequest(ctx, client, radarr, http.MethodPost, "/api/v3/release", bytesReaderFromAny(rel))
		if err != nil {
			return fmt.Errorf("grab release: %w", err)
		}
		defer grabResp.Body.Close()
		if grabResp.StatusCode != http.StatusOK && grabResp.StatusCode != http.StatusCreated {
			errRaw, _ := io.ReadAll(io.LimitReader(grabResp.Body, demoMaxBodyBytes))
			return fmt.Errorf("grab release: unexpected status %d: %s", grabResp.StatusCode, sanitizeArrError(errRaw))
		}
		return nil
	}
	return fmt.Errorf("no non-rejected release found from the http-stream indexer")
}

// radarrHTTPStreamIndexerID finds the http-stream Torznab indexer by its
// configured baseUrl (ends in "/torznab/http-stream"), never by an
// operator-assigned display name.
func radarrHTTPStreamIndexerID(ctx context.Context, client *http.Client, radarr demoArrTarget) (int, error) {
	resp, err := radarrRequest(ctx, client, radarr, http.MethodGet, "/api/v3/indexer", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, demoMaxBodyBytes*4))
	if err != nil {
		return 0, err
	}
	var indexers []struct {
		ID     int `json:"id"`
		Fields []struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &indexers); err != nil {
		return 0, fmt.Errorf("malformed indexer list: %w", err)
	}
	for _, idx := range indexers {
		for _, f := range idx.Fields {
			if f.Name != "baseUrl" {
				continue
			}
			if s, ok := f.Value.(string); ok && strings.HasSuffix(s, "/torznab/http-stream") {
				return idx.ID, nil
			}
		}
	}
	return 0, fmt.Errorf("no configured indexer has a baseUrl ending in /torznab/http-stream")
}

func bytesReaderFromAny(v any) io.Reader {
	raw, err := json.Marshal(v)
	if err != nil {
		return strings.NewReader("{}")
	}
	return strings.NewReader(string(raw))
}

func radarrPollForFile(ctx context.Context, client *http.Client, radarr demoArrTarget, movieID int) (bool, error) {
	ticker := time.NewTicker(demoPollInterval)
	defer ticker.Stop()
	for {
		m, err := radarrGetMovie(ctx, client, radarr, movieID)
		if err == nil && m.HasFile {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func radarrGetMovie(ctx context.Context, client *http.Client, radarr demoArrTarget, movieID int) (*radarrMovie, error) {
	resp, err := radarrRequest(ctx, client, radarr, http.MethodGet, fmt.Sprintf("/api/v3/movie/%d", movieID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, demoMaxBodyBytes))
	if err != nil {
		return nil, err
	}
	var m radarrMovie
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("malformed movie response: %w", err)
	}
	return &m, nil
}

// radarrMovieFileRef is the minimal file-location info the demo needs.
type radarrMovieFileRef struct {
	Path string
}

func radarrGetMovieFile(ctx context.Context, client *http.Client, radarr demoArrTarget, movieID int) (*radarrMovieFileRef, error) {
	m, err := radarrGetMovie(ctx, client, radarr, movieID)
	if err != nil {
		return nil, err
	}
	if m.MovieFile == nil || m.MovieFile.Path == "" {
		return nil, fmt.Errorf("movie has no file path recorded")
	}
	return &radarrMovieFileRef{Path: m.MovieFile.Path}, nil
}

func radarrDeleteMovie(ctx context.Context, client *http.Client, radarr demoArrTarget, movieID int) error {
	resp, err := radarrRequest(ctx, client, radarr, http.MethodDelete,
		fmt.Sprintf("/api/v3/movie/%d?deleteFiles=true", movieID), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

func sanitizeArrError(raw []byte) string {
	s := string(raw)
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// mapArrPathToDHPath remaps Radarr's own absolute movie-file path (inside
// Radarr's mount namespace, rooted at its configured /movies) onto
// DarkHarrbor's shared-filesystem view (dataRoot/movies/...). Both
// containers bind-mount the same underlying host directory (confirmed
// live); this never guesses across an unmounted or unrelated path.
// locateRecentStrmFile is a bounded fallback for when Radarr's recorded
// movieFile.path doesn't exist on disk (see the live finding in
// attemptCuratedDemo). It scans dataRoot/movies up to two levels deep --
// capped at demoLocateMaxEntries total directory entries examined, never
// an unbounded or recursive-deeper walk -- for a .strm file whose name
// contains the movie's year and its first significant title word, modified
// no earlier than notBefore, and returns the most recently modified match.
// Deliberately conservative: if nothing unambiguous turns up, it abstains
// (returns an error) rather than guessing.
func locateRecentStrmFile(dataRoot, title string, year int, notBefore time.Time) (string, error) {
	moviesRoot := filepath.Join(dataRoot, "movies")
	entries, err := os.ReadDir(moviesRoot)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", moviesRoot, err)
	}
	yearTok := strconv.Itoa(year)
	titleTok := firstSignificantWord(title)
	if titleTok == "" {
		return "", fmt.Errorf("no usable title token")
	}

	var best string
	var bestMod time.Time
	scanned := 0
	consider := func(fullPath, name string, modTime time.Time) {
		if modTime.Before(notBefore) {
			return
		}
		if !strings.HasSuffix(strings.ToLower(name), ".strm") {
			return
		}
		lower := strings.ToLower(fullPath)
		if !strings.Contains(lower, yearTok) || !strings.Contains(lower, titleTok) {
			return
		}
		if modTime.After(bestMod) {
			best, bestMod = fullPath, modTime
		}
	}

	for _, e := range entries {
		if scanned >= demoLocateMaxEntries {
			break
		}
		scanned++
		name := e.Name()
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if e.IsDir() {
			full := filepath.Join(moviesRoot, name)
			sub, serr := os.ReadDir(full)
			if serr != nil {
				continue
			}
			for _, se := range sub {
				if scanned >= demoLocateMaxEntries {
					break
				}
				scanned++
				sinfo, sierr := se.Info()
				if sierr != nil {
					continue
				}
				consider(filepath.Join(full, se.Name()), se.Name(), sinfo.ModTime())
			}
			continue
		}
		consider(filepath.Join(moviesRoot, name), name, info.ModTime())
	}
	if best == "" {
		return "", fmt.Errorf("no recent matching .strm found under %s", moviesRoot)
	}
	return best, nil
}

// demoLocateMaxEntries bounds locateRecentStrmFile's directory scan.
const demoLocateMaxEntries = 20000

// firstSignificantWord returns the first alphabetic word of title, lowercased,
// for use as a loose but distinctive match token.
func firstSignificantWord(title string) string {
	for _, w := range strings.Fields(title) {
		lower := strings.ToLower(w)
		clean := strings.TrimFunc(lower, func(r rune) bool {
			return !(r >= 'a' && r <= 'z')
		})
		if len(clean) >= 3 {
			return clean
		}
	}
	return ""
}

func mapArrPathToDHPath(arrPath, dataRoot string) string {
	const arrMoviesRoot = "/movies"
	if !strings.HasPrefix(arrPath, arrMoviesRoot+"/") && arrPath != arrMoviesRoot {
		return ""
	}
	rel := strings.TrimPrefix(arrPath, arrMoviesRoot)
	rel = strings.TrimPrefix(rel, "/")
	return filepath.Join(dataRoot, "movies", rel)
}

// verifyStrmPlayback reads the .strm file's DH URL directly off disk (never
// reconstructs or signs one) and runs a small bounded HEAD + two ranged GETs
// against it -- enough to prove real range/seek wiring without attempting a
// full soak (that remains the row's dedicated live-gate matrix, run
// separately against the deployed image). Never prints the URL itself
// (carries a tok= bearer value) -- only sanitized status codes/sizes.
func verifyStrmPlayback(ctx context.Context, client *http.Client, strmPath string) error {
	if strmPath == "" {
		return fmt.Errorf("could not map arr path to a DarkHarrbor data path")
	}
	raw, err := os.ReadFile(strmPath)
	if err != nil {
		return fmt.Errorf("read .strm: %w", err)
	}
	url := strings.TrimSpace(string(raw))
	if url == "" || !strings.Contains(url, "://") {
		return fmt.Errorf(".strm content is not a URL")
	}

	headCtx, cancel := context.WithTimeout(ctx, demoHTTPTimeout)
	defer cancel()
	headReq, err := http.NewRequestWithContext(headCtx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	headResp, err := client.Do(headReq)
	if err != nil {
		return fmt.Errorf("HEAD failed: %w", err)
	}
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		return fmt.Errorf("HEAD unexpected status %d", headResp.StatusCode)
	}
	size, _ := strconv.ParseInt(headResp.Header.Get("Content-Length"), 10, 64)
	if size <= 0 {
		return fmt.Errorf("HEAD missing usable Content-Length")
	}

	if err := verifyRange(ctx, client, url, "bytes=0-65535"); err != nil {
		return fmt.Errorf("start range: %w", err)
	}
	mid := size / 2
	if err := verifyRange(ctx, client, url, fmt.Sprintf("bytes=%d-%d", mid, mid+65535)); err != nil {
		return fmt.Errorf("mid-file seek range: %w", err)
	}
	return nil
}

func verifyRange(ctx context.Context, client *http.Client, url, rangeHeader string) error {
	rctx, cancel := context.WithTimeout(ctx, demoHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", rangeHeader)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status %d (want 206)", resp.StatusCode)
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, demoMaxBodyBytes)); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return nil
}

// demoTimeoutFromEnv resolves the optional HARRBOR_DEMO_TIMEOUT_SECONDS
// override. An unset or unparseable value warns (if out is non-nil) and
// falls back to the default rather than failing.
func demoTimeoutFromEnv(out io.Writer) time.Duration {
	raw := strings.TrimSpace(os.Getenv("HARRBOR_DEMO_TIMEOUT_SECONDS"))
	if raw == "" {
		return defaultDemoTimeout
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		if out != nil {
			fmt.Fprintf(out, "  ⚠ HARRBOR_DEMO_TIMEOUT_SECONDS=%q invalid — using default %s\n", raw, defaultDemoTimeout)
		}
		return defaultDemoTimeout
	}
	return time.Duration(secs) * time.Second
}

// RunDemo is the exported entry point for a standalone re-run (the
// `darkharrbor demo` CLI command), independent of the wizard's S1–S11
// bootstrap flow so it can be safely re-invoked against an already-
// configured, already-bootstrapped install without touching anything else.
// It is the exact same logic S12 runs inline during a fresh `setup`.
func RunDemo(ctx context.Context, out io.Writer, radarrName, radarrURL string, radarrAPIKey util.Redacted, dataRoot string, timeout time.Duration, cleanupDecision func(title string) bool) error {
	res := runZeroAccountDemo(ctx, out, demoArrTarget{Name: radarrName, URL: radarrURL, APIKey: radarrAPIKey}, dataRoot, timeout, cleanupDecision)
	return res.Err
}

// DemoTimeoutFromEnv exports demoTimeoutFromEnv for the standalone CLI command.
func DemoTimeoutFromEnv(out io.Writer) time.Duration { return demoTimeoutFromEnv(out) }
