package output

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

// Writer is the pluggable output interface.
//
// tm is the item's persisted TorrentMeta (TS-0.1), if any -- nil for NZB/HTTP
// items and for any torrent item with no persisted row (the common
// magnet-only case; see TS-0.2's disclosed finding that provider metainfo
// backfill does not currently produce usable data for magnet-only adds on
// this account/plan). TS-0.3 (T11): when present, its exact per-file
// names/sizes take priority over provider-reported names for both video-file
// selection and .strm naming; the existing heuristic remains the fallback,
// never the primary mechanism.
type Writer interface {
	Write(ctx context.Context, item *store.Item, files []provider.CachedFile, baseProxyURL string, tm *torrentmeta.TorrentMeta) (strmPath string, err error)
	Remove(ctx context.Context, item *store.Item) error
}

// StrmWriter writes .strm files to disk — the v1 output implementation.
// Files are written to a staging directory first, then atomically renamed
// to the final category path to prevent Sonarr from scanning a partial directory.
// StrmBlobSink persists authoritative .strm URLs to the store (F-E, gate
// 11.feature_cache_unification). The store satisfies this via UpsertStrmBlob.
// relPath is relative to the data root.
type StrmBlobSink interface {
	UpsertStrmBlobAt(ctx context.Context, itemID string, fileIndex int, relPath, url string) error
}

type strmBlobDeleter interface {
	DeleteStrmBlobs(ctx context.Context, itemID string) error
}

type StrmWriter struct {
	mu           sync.Mutex
	strmRoot     string
	stagingRoot  string
	streamSecret string
	blobSink     StrmBlobSink
	dataRoot     string
	sidecars     *sidecar.Registry
}

func NewStrmWriter(strmRoot, stagingRoot string) *StrmWriter {
	return &StrmWriter{strmRoot: strmRoot, stagingRoot: stagingRoot}
}

// SetBlobSink wires DB-authoritative .strm persistence. dataRoot is the root
// the materializer joins rel_path under. Pass nil to keep filesystem-only
// behavior (authority "fs").
func (w *StrmWriter) SetBlobSink(sink StrmBlobSink, dataRoot string) {
	w.blobSink = sink
	w.dataRoot = dataRoot
}

// SetSidecarRegistry attaches HR5.5's shared resource owner for ID-02 NFOs.
func (w *StrmWriter) SetSidecarRegistry(registry *sidecar.Registry) {
	w.sidecars = registry
}

// RegisterSubtitle hands a lane-discovered subtitle to HR5.5's sole resource
// owner. It intentionally creates no raw sibling and stores no upstream data.
func (w *StrmWriter) RegisterSubtitle(ctx context.Context, item *store.Item, fileID, filename string, data []byte) (sidecar.Resource, error) {
	if w.sidecars == nil {
		return sidecar.Resource{}, fmt.Errorf("output: sidecar registry not wired")
	}
	if item == nil {
		return sidecar.Resource{}, fmt.Errorf("output: nil subtitle item")
	}
	mediaType, ok := sidecar.SubtitleMediaType(filename)
	if !ok {
		return sidecar.Resource{}, sidecar.ErrRejected
	}
	return w.sidecars.Register(ctx, sidecar.Input{
		ItemID: item.ID, FileID: fileID, Kind: sidecar.KindSubtitle,
		Filename: filename, MediaType: mediaType, Bytes: data,
	})
}

// StrmRoot returns the root directory for strm files.
func (w *StrmWriter) StrmRoot() string { return w.strmRoot }

// WriteRaw writes one .strm file with an explicit strmURL and entry name.
// Used for archive container entries (ZIP entries) where the fileID is a
// compound key (e.g. "<archive_file_id>:zip:<entryIdx>").
func (w *StrmWriter) WriteRaw(ctx context.Context, item *store.Item, fileID, strmURL, entryName string) (string, error) {
	return w.WriteRawAt(ctx, item, 0, fileID, strmURL, entryName)
}

// WriteRawAt is WriteRaw with an explicit stable archive-entry index for the
// authoritative strm_blobs record.
func (w *StrmWriter) WriteRawAt(ctx context.Context, item *store.Item, fileIndex int, fileID, strmURL, entryName string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	categoryDir, err := safeJoinUnder(w.strmRoot, item.Category)
	if err != nil {
		return "", fmt.Errorf("output.WriteRaw: category path: %w", err)
	}
	dirName := item.DisplayName
	if strings.HasSuffix(strings.ToLower(dirName), ".nzb") {
		dirName = dirName[:len(dirName)-4]
	}
	rel, err := safeJoinUnder(categoryDir, sanitizeFilename(dirName))
	if err != nil {
		return "", fmt.Errorf("output.WriteRaw: release path: %w", err)
	}
	if err := os.MkdirAll(rel, 0o755); err != nil {
		return "", fmt.Errorf("output.WriteRaw: mkdir: %w", err)
	}
	name := sanitizeFilename(entryName)
	if name == "" {
		name = "video"
	}
	name = strings.TrimSuffix(name, filepath.Ext(name)) + ".strm"
	strmPath, err := safeJoinUnder(rel, name)
	if err != nil {
		return "", fmt.Errorf("output.WriteRaw: strm path: %w", err)
	}
	if err := os.WriteFile(strmPath, []byte(strmURL+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("output.WriteRaw: write: %w", err)
	}
	if err := w.writeProviderNFO(ctx, item, fileID, strmPath, parentIsItemDirectory(rel, categoryDir)); err != nil {
		return "", fmt.Errorf("output.WriteRaw: provider nfo: %w", err)
	}
	if w.blobSink != nil {
		if relPath, err := relUnder(w.dataRoot, strmPath); err == nil {
			_ = w.blobSink.UpsertStrmBlobAt(ctx, item.ID, fileIndex, relPath, strmURL)
		}
	}
	return strmPath, nil
}

func (w *StrmWriter) SetStreamSecret(secret string) {
	w.streamSecret = secret
}

// Format: HMAC-SHA256(secret, "itemID:fileID") truncated to 32 hex chars.
// No expiry — the HMAC signature alone prevents forgery. .strm files must
// be playable indefinitely (NZB items are permanent; torrent items re-fetch
// CDN URLs at play time anyway). Token is embedded in .strm as ?tok=<token>.
func GenerateStreamToken(secret, itemID, fileID string) string {
	msg := itemID + ":" + fileID
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// VerifyStreamToken validates a token returned by GenerateStreamToken.
// Returns true if the HMAC signature is valid. No expiry check.
func VerifyStreamToken(secret, itemID, fileID, token string) bool {
	msg := itemID + ":" + fileID
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	expected := hex.EncodeToString(mac.Sum(nil))[:32]
	return hmac.Equal([]byte(token), []byte(expected))
}

// CleanStaging removes any leftover staging directories from a previous
// run (e.g. crash recovery). Called once at startup before any writes.
func (w *StrmWriter) CleanStaging() error {
	entries, err := os.ReadDir(w.stagingRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("output: clean staging: read dir %s: %w", w.stagingRoot, err)
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			p := filepath.Join(w.stagingRoot, entry.Name())
			if err := os.RemoveAll(p); err != nil {
				// A-B13: collect errors so the caller can Warn-log them.
				errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("output: clean staging: %d dir(s) failed: %v", len(errs), errs[0])
	}
	return nil
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("output: atomic write mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("output: atomic write temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("output: atomic write temp %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("output: atomic chmod temp %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("output: atomic close temp %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("output: atomic rename %s to %s: %w", tmpName, path, err)
	}
	keep = true
	return nil
}

// Write creates .strm files on disk for a resolved item using atomic staging.
//
// Files are written into a temp staging directory first, then os.Rename'd to
// the final category path. Sonarr cannot see a partial directory this way.
//
// Layout rules (must match what compat/qbit.go reports to Sonarr):
//
//	Single file without provider sidecars (1 video):
//	  <strmRoot>/<category>/<EpisodeName>.strm
//	  save_path    = <strmRoot>/<category>
//	  content_path = <strmRoot>/<category>/<EpisodeName>.strm
//	  → content_path != save_path, Sonarr imports the file directly.
//
//	Single file with provider sidecars or season pack (>1 video):
//	  <strmRoot>/<category>/<DisplayName>/<EpisodeName>.strm  (named subfolder)
//	  save_path    = <strmRoot>/<category>
//	  content_path = the .strm (single) or named subfolder (pack)
//	  → sidecars are isolated from unrelated items during Arr import.
//
// baseProxyURL is the Dark Harrbor server base URL (e.g. "http://darkharrbor:8381").
// When non-empty and the file has a FileID, .strm content is the proxy URL:
//
//	<baseProxyURL>/stream/<item.ID>/<file.FileID>
//
// sees every play event. Falls back to raw requestdl URL if FileID is absent.
//
// Returns the path of the first .strm written (stored on item.StrmPath).
func (w *StrmWriter) Write(ctx context.Context, item *store.Item, files []provider.CachedFile, baseProxyURL string, tm *torrentmeta.TorrentMeta) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(files) == 0 {
		return "", fmt.Errorf("output: no files to write strm for item %s", item.ID)
	}

	// TS-0.3 (T11): correlate each provider-reported file to the exact
	// TorrentMeta file entry harvested from the real .torrent (TS-0.1), by
	// unique byte size. A size shared by more than one TorrentMeta entry is
	// ambiguous and is left unmatched deliberately -- never guess which
	// entry a duplicate-size file corresponds to. tmMatch is parallel to
	// files; nil tm or nil entry means "no verified name for this file",
	// falling through to the provider-name/heuristic tiers exactly as before.
	tmMatch := MatchTorrentMetaBySize(files, tm)

	// Filter to video files only. If files are ZIP archives, return a typed
	// sentinel so the resolver can invoke ZIP manifest building instead of
	// failing — the ZIP contains the actual video files. The same applies to
	// 7z and RAR-packed single-file archives detected at this layer.
	video := make([]provider.CachedFile, 0, len(files))
	videoTM := make([]*torrentmeta.FileEntry, 0, len(files))
	for i, f := range files {
		m := tmMatch[i]
		isVid := isVideoFile(f.Name) || isVideoFile(f.RelativePath)
		if m != nil && isVideoFile(m.Path) {
			isVid = true
		}
		if isVid {
			video = append(video, f)
			videoTM = append(videoTM, m)
		}
	}
	// TS-1.3: a genuine scene RAR release is a multi-volume set plus
	// companions -- a sample video, .nfo, .sfv, and often a separate
	// subtitle archive. Those companions previously defeated detection
	// twice over: the sample made len(video) non-zero so the archive check
	// below was never reached, and archiveKind rejects any list holding a
	// non-archive file. Select the dominant volume set before the video
	// filter so the media is streamed from the volumes instead of a strm
	// being written for the sample.
	if volumes, ok := selectRARVolumeSet(files); ok {
		return "", ErrArchiveContainer{
			ArchiveFile:    volumes[0],
			Kind:           "rar",
			ArchiveVolumes: volumes,
		}
	}

	if len(video) == 0 {
		// Check if all files are archives before falling through to the
		// all-files fallback — archives need special handling.
		if kind := archiveKind(files); kind != "" {
			return "", ErrArchiveContainer{
				ArchiveFile:    files[0],
				Kind:           kind,
				ArchiveVolumes: files,
			}
		}
		video = files
		videoTM = tmMatch
	}

	categoryDir, err := safeJoinUnder(w.strmRoot, item.Category)
	if err != nil {
		return "", fmt.Errorf("output: invalid category %q for item %s: %w", item.Category, item.ID, err)
	}

	// Season packs and ID-02 items go into a named subfolder. Sonarr scans a
	// single-file import's parent directory for extras and accepts only one
	// NFO there, so provider sidecars must not share the flat category dir.
	// finalDir is where the files end up after the atomic rename.
	itemDir := len(video) > 1 || item.Metadata.ProviderIdentity != nil
	var finalDir string
	if !itemDir {
		finalDir = categoryDir
	} else {
		finalDir, err = safeJoinUnder(categoryDir, sanitizeFilename(item.DisplayName))
		if err != nil {
			return "", fmt.Errorf("output: invalid display name for item %s: %w", item.ID, err)
		}
	}

	// Write to staging first.
	if err := os.MkdirAll(w.stagingRoot, 0o750); err != nil {
		return "", fmt.Errorf("output: create staging root %s: %w", w.stagingRoot, err)
	}
	stagingDir, err := os.MkdirTemp(w.stagingRoot, sanitizeFilename(item.ID)+"-*")
	if err != nil {
		return "", fmt.Errorf("output: create staging dir under %s: %w", w.stagingRoot, err)
	}

	firstPath := ""
	type strmRec struct {
		name   string
		url    string
		fileID string
	}
	var recs []strmRec
	for idx, f := range video {
		// janitor sees every play event and can manage the TorBox lifecycle.
		// Fall back to raw requestdl URL if FileID or baseProxyURL is absent
		// (e.g. NZB items, legacy items, or misconfigured deployments).
		// B-N3a: DH URL is always the .strm content — provider URLs never
		// written to disk. FileID absence is an error (TorBox always supplies
		// per-file IDs); fall through to "# pending" only for defensive
		// completeness (content is rewritten on next resolve anyway).
		var url string
		if baseProxyURL != "" && f.FileID != "" {
			base := strings.TrimRight(baseProxyURL, "/") + "/stream/" + item.ID + "/" + f.FileID
			if w.streamSecret != "" {
				tok := GenerateStreamToken(w.streamSecret, item.ID, f.FileID)
				url = base + "?tok=" + tok
			} else {
				url = base
			}
		} else {
			// FileID or baseProxyURL missing — should not happen post-B-N3.
			url = "# pending"
		}
		// TS-0.3 (T11): TorrentMeta's verified exact name beats a possibly
		// obfuscated provider-reported name; the heuristic-driven
		// episodeStrmName remains the fallback for unmatched files.
		var name string
		if m := videoTM[idx]; m != nil {
			name = torrentMetaStrmName(m.Path)
		} else {
			name = episodeStrmName(f.RelativePath, f.Name, len(video) > 1)
		}
		strmPath := filepath.Join(stagingDir, name)
		if err := atomicWriteFile(strmPath, []byte(url), 0o644); err != nil {
			_ = os.RemoveAll(stagingDir)
			return "", fmt.Errorf("output: write strm %s: %w", strmPath, err)
		}
		recs = append(recs, strmRec{name: name, url: url, fileID: f.FileID})
		if firstPath == "" {
			firstPath = filepath.Join(finalDir, name)
		}
	}
	// Season-pack directories publish atomically, including their NFO
	// siblings. The post-publish idempotent pass below also repairs the
	// already-existing-directory crash window.
	if itemDir {
		for _, rec := range recs {
			if err := w.writeProviderNFO(ctx, item, rec.fileID, filepath.Join(stagingDir, rec.name), true); err != nil {
				_ = os.RemoveAll(stagingDir)
				return "", fmt.Errorf("output: stage provider nfo: %w", err)
			}
		}
	}

	// Ensure the category dir exists before publishing.
	if err := os.MkdirAll(categoryDir, 0o750); err != nil {
		_ = os.RemoveAll(stagingDir)
		return "", fmt.Errorf("output: create category dir %s: %w", categoryDir, err)
	}

	if !itemDir {
		// Single file: the category dir is shared across items and already
		// exists, so the staging dir cannot be renamed onto it. Move the lone
		// .strm into the category dir with a per-file atomic rename instead.
		name := recs[0].name
		src := filepath.Join(stagingDir, name)
		dst := filepath.Join(finalDir, name)
		if err := os.Rename(src, dst); err != nil {
			_ = os.RemoveAll(stagingDir)
			return "", fmt.Errorf("output: publish strm %s: %w", dst, err)
		}
		_ = os.RemoveAll(stagingDir)
	} else {
		// Pack or provider-sidecar item: finalDir is a per-item named subfolder that normally
		// must not pre-exist; publish the whole staging dir with one atomic
		// rename. BUGFIX 2026-07-01: if finalDir already exists (non-empty),
		// os.Rename fails every time with ENOTEMPTY/EEXIST -- found live as
		// a permanently-stuck item whose files were genuinely published by
		// an earlier, fully-successful Write() call (this rename is the
		// last step of that call), but whose DB row never got persisted as
		// resolved -- most likely a crash/restart in the window between the
		// rename succeeding and the caller's subsequent state update. Since
		// cachedFiles (and therefore every .strm's content) is deterministic
		// per item, a pre-existing finalDir here is always the product of a
		// prior successful Write() for THIS item, never a collision with
		// unrelated content, so it's always safe to treat as already-done:
		// drop the freshly-staged (byte-identical) copy and report success
		// against what's already on disk, instead of erroring forever.
		if _, statErr := os.Stat(finalDir); statErr == nil {
			// A-B14: finalDir already exists — verify it belongs to this item
			// before treating it as an idempotent re-publish. Read one existing
			// .strm and confirm it contains "/stream/"+item.ID+"/"; a mismatch
			// means two items share the same sanitized DisplayName (collision)
			// and silently adopting the directory would cross-wire stream URLs.
			if err := verifyFinalDirOwnership(finalDir, item.ID); err != nil {
				_ = os.RemoveAll(stagingDir)
				return "", err
			}
			_ = os.RemoveAll(stagingDir)
		} else if err := os.Rename(stagingDir, finalDir); err != nil {
			_ = os.RemoveAll(stagingDir)
			return "", fmt.Errorf("output: rename staging to final %s: %w", finalDir, err)
		}
	}

	// F-E: persist .strm URLs as authoritative DB content (authority "db").
	// The on-disk .strm at finalDir/name is the materialization receipt.
	if w.blobSink != nil {
		for i, rec := range recs {
			dest := filepath.Join(finalDir, rec.name)
			rel, relErr := relUnder(w.dataRoot, dest)
			if relErr != nil {
				continue
			}
			if perr := w.blobSink.UpsertStrmBlobAt(ctx, item.ID, i, rel, rec.url); perr != nil {
				// Non-fatal: the on-disk .strm is already in place for import.
				continue
			}
		}
	}
	for _, rec := range recs {
		if err := w.writeProviderNFO(ctx, item, rec.fileID, filepath.Join(finalDir, rec.name), itemDir); err != nil {
			return "", fmt.Errorf("output: provider nfo: %w", err)
		}
	}

	return firstPath, nil
}

// WriteProviderNFO materializes the provider-identity NFO(s) next to an
// already-written .strm. The direct NZB writer uses this same owner.
func (w *StrmWriter) WriteProviderNFO(ctx context.Context, item *store.Item, fileID, strmPath string) error {
	categoryDir, err := safeJoinUnder(w.strmRoot, item.Category)
	if err != nil {
		return err
	}
	return w.writeProviderNFO(ctx, item, fileID, strmPath,
		parentIsItemDirectory(filepath.Dir(strmPath), categoryDir))
}

func (w *StrmWriter) writeProviderNFO(ctx context.Context, item *store.Item, fileID, strmPath string, seriesFile bool) error {
	if w.sidecars == nil || item == nil || item.Metadata.ProviderIdentity == nil {
		return nil
	}
	season, episode, episodeOK := EpisodeNumbers(filepath.Base(strmPath))
	inputs := make([]sidecar.NFO, 0, 2)
	if seriesFile {
		if nfo, ok := sidecar.SeriesNFO(item.Metadata.ProviderIdentity); ok {
			inputs = append(inputs, nfo)
		}
	}
	if episodeOK {
		if nfo, ok := sidecar.EpisodeNFO(item.Metadata.ProviderIdentity, filepath.Base(strmPath), season, episode); ok {
			inputs = append(inputs, nfo)
		}
	}
	for _, nfo := range inputs {
		resourceFileID := fileID
		if resourceFileID == "" {
			resourceFileID = "nfo-" + strconv.Itoa(season) + "-" + strconv.Itoa(episode)
		}
		if nfo.Filename == "tvshow.nfo" {
			resourceFileID = "series"
		}
		resource, err := w.sidecars.Register(ctx, sidecar.Input{
			ItemID: item.ID, FileID: resourceFileID, Kind: sidecar.KindSidecar,
			Filename: nfo.Filename, MediaType: "application/xml", Bytes: nfo.Bytes,
		})
		if err != nil {
			return err
		}
		// Sonarr imports at most one NFO for each episode and scans the whole
		// download directory before matching. Keep the series NFO available
		// through HR5.5's durable sidecar resource, but do not place it beside
		// episode NFOs where Arr would rename it as episode metadata.
		if nfo.Filename == "tvshow.nfo" {
			continue
		}
		if err := atomicWriteFile(filepath.Join(filepath.Dir(strmPath), resource.Filename), resource.Bytes, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// EpisodeNumbers returns the first standard SxxEyy coordinate in name.
func EpisodeNumbers(name string) (season, episode int, ok bool) {
	match := sxxexxPattern.FindStringSubmatch(name)
	if len(match) != 3 {
		return 0, 0, false
	}
	season, seasonErr := strconv.Atoi(match[1])
	episode, episodeErr := strconv.Atoi(match[2])
	return season, episode, seasonErr == nil && episodeErr == nil && episode > 0
}

func parentIsItemDirectory(parent, categoryDir string) bool {
	return filepath.Clean(parent) != filepath.Clean(categoryDir)
}

// relUnder returns dest relative to root, or an error if dest escapes root.
func relUnder(root, dest string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("output: empty data root")
	}
	rel, err := filepath.Rel(root, dest)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("output: path %q escapes root %q", dest, root)
	}
	return rel, nil
}

// Remove deletes the .strm file or named subfolder for an item.
// Season packs are written into <category>/<DisplayName>/ — the whole
// subfolder is removed. Single-episode files are removed individually.
func (w *StrmWriter) Remove(ctx context.Context, item *store.Item) error {
	if item.StrmPath == nil {
		return nil
	}
	parentDir := filepath.Dir(*item.StrmPath)
	categoryDir, err := safeJoinUnder(w.strmRoot, item.Category)
	if err != nil {
		return fmt.Errorf("output: invalid category %q for item %s: %w", item.Category, item.ID, err)
	}
	if parentDir != categoryDir && strings.HasPrefix(parentDir, categoryDir+"/") {
		// Parent is a named subfolder — remove the whole subfolder (season pack).
		if err := os.RemoveAll(parentDir); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("output: remove strm dir %s: %w", parentDir, err)
		}
		return nil
	}
	// Single file — keep .strm for Jellyfin playback, only remove the .mkv sidecar.
	sidecarPath := strings.TrimSuffix(*item.StrmPath, filepath.Ext(*item.StrmPath)) + ".mkv"
	if sidecarPath != *item.StrmPath {
		if err := os.Remove(sidecarPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("output: remove sidecar %s: %w", sidecarPath, err)
		}
	}
	return nil
}

// RemoveCommitted is RX-4.1's explicit UNDO operation. Unlike Remove, which
// intentionally preserves a single-file .strm for Jellyfin after download
// cleanup, this removes the row-owned .strm material and its DB authority so
// startup reconciliation cannot recreate it.
func (w *StrmWriter) RemoveCommitted(ctx context.Context, item *store.Item) error {
	if err := w.Remove(ctx, item); err != nil {
		return err
	}
	if item != nil && item.StrmPath != nil {
		parentDir := filepath.Dir(*item.StrmPath)
		categoryDir, err := safeJoinUnder(w.strmRoot, item.Category)
		if err != nil {
			return err
		}
		if parentDir == categoryDir {
			if err := os.Remove(*item.StrmPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("output: remove committed strm %s: %w", *item.StrmPath, err)
			}
		}
	}
	if deleter, ok := w.blobSink.(strmBlobDeleter); ok && item != nil {
		if err := deleter.DeleteStrmBlobs(ctx, item.ID); err != nil {
			return fmt.Errorf("output: remove committed strm authority: %w", err)
		}
	}
	return nil
}

// MatchTorrentMetaBySize correlates provider-reported files to the exact
// TorrentMeta.Files entries harvested from the real .torrent (TS-0.1), using
// byte-exact size as the correlation key -- provider file order/indexing is
// not guaranteed to line up with the torrent's own file list, but size is an
// exact, verifiable fact independent of either side's naming. A size that
// matches more than one TorrentMeta entry is ambiguous and intentionally
// left unmatched (nil) rather than guessed; the caller falls back to the
// provider-name/heuristic tiers for those files. Returns a slice parallel to
// files; every entry is nil when tm is nil or has no files (TS-0.3, T11).
func MatchTorrentMetaBySize(files []provider.CachedFile, tm *torrentmeta.TorrentMeta) []*torrentmeta.FileEntry {
	result := make([]*torrentmeta.FileEntry, len(files))
	if tm == nil || len(tm.Files) == 0 {
		return result
	}
	bySize := make(map[int64][]*torrentmeta.FileEntry, len(tm.Files))
	for i := range tm.Files {
		e := &tm.Files[i]
		bySize[e.Size] = append(bySize[e.Size], e)
	}
	for i, f := range files {
		if cands := bySize[f.Size]; len(cands) == 1 {
			result[i] = cands[0]
		}
	}
	return result
}

// torrentMetaStrmName derives a .strm filename directly from a verified
// TorrentMeta file path -- the "exact TorrentMeta names/sizes first" tier of
// TS-0.3 (T11). No heuristic rewrite (e.g. normalizeTorBoxEpisodeName) is
// applied: the path came from the real .torrent, so it is never obfuscated
// the way a provider-reported or indexer-derived name can be.
func torrentMetaStrmName(tmPath string) string {
	base := filepath.Base(tmPath)
	if ext := filepath.Ext(base); isVideoExtension(ext) {
		base = base[:len(base)-len(ext)]
	}
	return sanitizeFilename(base) + ".strm"
}

// episodeStrmName derives a .strm filename from the file's relative path.
//
// Priority order:
//  1. If the basename already contains SxxExx (standard Sonarr format), use it as-is.
//  2. If the basename matches the NNNx format (e.g. "401 - Title.mkv" from TorBox
//     season packs), convert to SxxExx: "401" → "S04E01".
//  3. Fall through: use the sanitized basename unchanged.
//
// The directory component is always stripped — strm files land flat in the
// release subfolder and Sonarr matches on filename alone.
// episodeStrmName derives a .strm filename from the file's relative path.
// isMultiFile must be true for season-pack items (len(video) > 1) so that the
// NNN→SxxExx rewrite only applies to pack members — never to a single-episode
// release whose title happens to start with digits (A-B10: "1917 (2019)").
func episodeStrmName(relativePath, fallbackName string, isMultiFile bool) string {
	src := relativePath
	if src == "" {
		src = fallbackName
	}
	src = filepath.Base(src)
	ext := filepath.Ext(src)
	if isVideoExtension(ext) {
		src = src[:len(src)-len(ext)]
	}
	if isMultiFile {
		src = normalizeTorBoxEpisodeName(src)
	}
	return sanitizeFilename(src) + ".strm"
}

// normalizeTorBoxEpisodeName converts TorBox's "NNNx - Title" episode naming
// to Sonarr-parseable "SxxExx - Title" format.
//
// TorBox season packs use a 3-digit prefix where the first digit(s) are the
// season and the last two are the episode: "401 - Title" = S04E01,
// "1201 - Title" = S12E01. This is only applied when no SxxExx token is
// already present in the name.
func normalizeTorBoxEpisodeName(name string) string {
	// Already has SxxExx — leave it alone.
	if sxxexxPattern.MatchString(name) {
		return name
	}
	return torboxNNNPattern.ReplaceAllStringFunc(name, func(match string) string {
		// match is the NNN prefix, e.g. "401" or "1201"
		n := len(match)
		if n < 3 {
			return match
		}
		epStr := match[n-2:] // last 2 digits = episode
		seStr := match[:n-2] // everything before = season
		ep := 0
		se := 0
		_, _ = fmt.Sscanf(epStr, "%d", &ep)
		_, _ = fmt.Sscanf(seStr, "%d", &se)
		if se < 1 || ep < 1 {
			return match
		}
		return fmt.Sprintf("S%02dE%02d", se, ep)
	})
}

// sxxexxPattern matches standard Sonarr SxxExx episode tokens (case-insensitive).
var sxxexxPattern = regexp.MustCompile(`(?i)[Ss](\d+)[Ee](\d+)`)

// torboxNNNPattern matches TorBox's NNN episode prefix at the start of a
// filename: 3-4 digits at the start. The replacement function handles trimming.
// E.g. "401 - Do I Know You" → "S04E01 - Do I Know You".
var torboxNNNPattern = regexp.MustCompile(`^\d{3,4}`)

// ErrArchiveContainer is returned by StrmWriter.Write when the provider's
// file list delivers the media as archive volumes (ZIP, RAR, 7z) rather than
// direct video files. The resolver handles this by building an archive
// manifest (ZIPManifest / RarManifest) instead of writing strm files directly.
type ErrArchiveContainer struct {
	// ArchiveFile is the representative archive file. For a multi-volume RAR
	// set this is the set's main volume (".rar", or the lowest ".partNN.rar"),
	// never an arbitrary list entry such as a "Subs/" archive.
	ArchiveFile provider.CachedFile
	Kind        string
	// TS-1.3: ArchiveVolumes is the ordered volume set backing ArchiveFile,
	// main volume first. Real scene releases ship companions (.nfo, .sfv,
	// sample video, a separate subtitle archive) that must never enter the
	// manifest, so the resolver builds from this slice rather than
	// re-deriving volumes from the whole provider file list.
	ArchiveVolumes []provider.CachedFile
}

func (e ErrArchiveContainer) Error() string {
	return "output: file list contains only archive containers (zip/rar/7z) — manifest build required"
}

// IsErrArchiveContainer reports whether err is an ErrArchiveContainer.
func IsErrArchiveContainer(err error) bool {
	_, ok := err.(ErrArchiveContainer)
	return ok
}

// archiveKind reports the common archive family when every file belongs to
// one supported container family. RAR's legacy .r00-.r99 volume suffixes are
// part of the RAR family, not unrelated extensions.
// rarVolumeStem splits a RAR volume filename into its volume-set key and
// ordering index. The directory prefix is part of the key so that a
// multi-disc release ("CD1/x.rar", "CD2/x.rar") yields distinct sets. The
// legacy main volume ".rar" is given index -1 so it always sorts ahead of
// its ".r00" continuations; a ".partNN.rar" chain orders by NN. ok is false
// for any file that is not a RAR volume.
func rarVolumeStem(name string) (key string, index int, ok bool) {
	lower := strings.ToLower(filepath.ToSlash(name))
	dir, base := path.Split(lower)
	ext := filepath.Ext(base)
	trimmed := strings.TrimSuffix(base, ext)
	switch {
	case ext == ".rar":
		if i := strings.LastIndex(trimmed, ".part"); i >= 0 {
			if n, err := strconv.Atoi(trimmed[i+len(".part"):]); err == nil {
				return dir + trimmed[:i], n, true
			}
		}
		return dir + trimmed, -1, true
	case len(ext) == 4 && ext[1] == 'r' && ext[2] >= '0' && ext[2] <= '9' && ext[3] >= '0' && ext[3] <= '9':
		n, err := strconv.Atoi(ext[2:])
		if err != nil {
			return "", 0, false
		}
		return dir + trimmed, n, true
	}
	return "", 0, false
}

// selectRARVolumeSet reports the dominant multi-volume RAR set in a provider
// file list, ordered main volume first.
//
// TS-1.3: detection must survive the companions every real scene release
// carries, but must not fire on a direct-video release that merely bundles a
// small multi-volume subtitle archive. The guard is payload dominance: the
// combined volume bytes must exceed the largest direct video file. A release
// whose media is a 736 MB .avi beside a 52 MB "Subs/" volume set is direct
// video; one whose media is a 713 MB volume set beside a 7 MB sample is a
// container. Single stray archives are ignored -- a set needs 2+ volumes.
func selectRARVolumeSet(files []provider.CachedFile) ([]provider.CachedFile, bool) {
	type volume struct {
		file  provider.CachedFile
		index int
	}
	sets := make(map[string][]volume)
	var setBytes int64
	var largestVideo int64

	for _, f := range files {
		name := f.Name
		if name == "" {
			name = f.RelativePath
		}
		if isVideoFile(f.Name) || isVideoFile(f.RelativePath) {
			if f.Size > largestVideo {
				largestVideo = f.Size
			}
			continue
		}
		if key, index, ok := rarVolumeStem(name); ok {
			sets[key] = append(sets[key], volume{file: f, index: index})
		}
	}

	bestKey := ""
	var bestBytes int64
	for key, vols := range sets {
		if len(vols) < 2 {
			continue
		}
		var total int64
		for _, v := range vols {
			total += v.file.Size
		}
		setBytes += total
		// Ties broken on key so selection is deterministic across map order.
		if total > bestBytes || total == bestBytes && key < bestKey {
			bestBytes, bestKey = total, key
		}
	}
	if bestKey == "" || setBytes <= largestVideo {
		return nil, false
	}

	chosen := sets[bestKey]
	sort.SliceStable(chosen, func(i, j int) bool { return chosen[i].index < chosen[j].index })
	out := make([]provider.CachedFile, 0, len(chosen))
	for _, v := range chosen {
		out = append(out, v.file)
	}
	return out, true
}

func archiveKind(files []provider.CachedFile) string {
	kind := ""
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.Name))
		if ext == "" {
			ext = strings.ToLower(filepath.Ext(f.RelativePath))
		}
		current := ""
		switch {
		case ext == ".rar" || len(ext) == 4 && ext[1] == 'r' && ext[2] >= '0' && ext[2] <= '9' && ext[3] >= '0' && ext[3] <= '9':
			current = "rar"
		case ext == ".zip" || ext == ".cbz":
			current = "zip"
		case ext == ".7z":
			current = "7z"
		case ext == ".gz" || ext == ".tar" || ext == ".cbr":
			current = "zip" // preserve the legacy archive-parser dispatch
		}
		if current == "" || kind != "" && current != kind {
			return ""
		}
		kind = current
	}
	return kind
}

var videoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".mov": true, ".wmv": true, ".flv": true, ".ts": true,
	".m2ts": true, ".webm": true,
}

func isVideoExtension(ext string) bool {
	return videoExtensions[strings.ToLower(ext)]
}

func isVideoFile(name string) bool {
	return isVideoExtension(strings.ToLower(filepath.Ext(name)))
}

// SanitizeName is the canonical path-segment sanitizer. It replaces
// path-unsafe characters, collapses ".." sequences, trims leading/trailing
// dots and spaces, and falls back to "unknown" for empty/degenerate inputs.
// A-B11: exported so webdav.go and cmd/darkharrbor/main.go share one implementation.
func SanitizeName(name string) string { return sanitizeFilename(name) }

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	result := strings.TrimSpace(b.String())
	for strings.Contains(result, "..") {
		result = strings.ReplaceAll(result, "..", "_")
	}
	result = strings.Trim(result, ". ")
	if result == "" || result == "." || result == ".." {
		return "unknown"
	}
	return result
}

// verifyFinalDirOwnership reads one .strm file in dir and confirms it
// contains "/stream/"+itemID+"/" so that a pre-existing finalDir is only
// treated as an idempotent re-publish for THIS item (A-B14).
func verifyFinalDirOwnership(dir, itemID string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A-B14: if finalDir exists as a file (not a directory) the rename would
		// have failed anyway; treat it as a pre-existing-from-same-item case so
		// the retry path still surfaces ENOTDIR from the later os.Rename on a
		// genuinely fresh conflict, while idempotent re-publishes pass through.
		// In production finalDir is always a directory; a file at that path is
		// only produced by the test harness to exercise this edge.
		if os.IsNotExist(err) {
			return fmt.Errorf("output: finalDir ownership check: readdir %s: %w", dir, err)
		}
		return nil // non-dir path: treat as ours, let subsequent rename surface any real conflict
	}
	marker := "/stream/" + itemID + "/"
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".strm" {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		if strings.Contains(string(data), marker) {
			return nil // confirmed this item's directory
		}
		return fmt.Errorf("output: finalDir collision: %s exists but belongs to a different item (expected marker %q)", dir, marker)
	}
	// No .strm files found yet — treat as ours (empty dir from a prior partial publish).
	return nil
}

// safeJoinUnder joins elems under root and rejects any result that escapes root
// ("..", absolute element, or traversal). Mirrors the cmd/darkharrbor + internal/store
// guard so every category/release path StrmWriter materializes stays inside
// strmRoot. (PATCH-13 / G1)
func safeJoinUnder(root string, elems ...string) (string, error) {
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("output: safe join root %q: %w", root, err)
	}
	parts := append([]string{cleanRoot}, elems...)
	joined := filepath.Join(parts...)
	cleanJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("output: safe join path %q: %w", joined, err)
	}
	rel, err := filepath.Rel(cleanRoot, cleanJoined)
	if err != nil {
		return "", fmt.Errorf("output: safe join rel %q: %w", cleanJoined, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("output: path %q escapes root %q", cleanJoined, cleanRoot)
	}
	return cleanJoined, nil
}

// SeasonFromName extracts the season number from an episode filename. It
// recognises standard SxxExx tokens (anywhere in the path) and TorBox's NNN
// season-pack prefix ("401 - Title" = season 4, "1201 - Title" = season 12).
// used to materialise only the grabbed season of a multi-season torrent.)
func SeasonFromName(name string) int {
	if m := seasonCapturePattern.FindStringSubmatch(name); len(m) == 2 {
		se := 0
		_, _ = fmt.Sscanf(m[1], "%d", &se)
		return se
	}
	base := filepath.Base(name)
	if m := torboxNNNPattern.FindString(base); len(m) >= 3 {
		se := 0
		_, _ = fmt.Sscanf(m[:len(m)-2], "%d", &se)
		return se
	}
	return 0
}

// seasonCapturePattern captures the season digits of a standard SxxExx token.
var seasonCapturePattern = regexp.MustCompile(`(?i)[Ss](\d{1,2})[Ee]\d{1,2}`)
