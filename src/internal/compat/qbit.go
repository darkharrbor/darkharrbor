package compat

import (
	"encoding/json"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

type QBitCategory struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

type QBitTransferInfo struct {
	ConnectionStatus string `json:"connection_status"`
	DHTNodes         int    `json:"dht_nodes"`
	DLInfoData       int64  `json:"dl_info_data"`
	DLInfoSpeed      int64  `json:"dl_info_speed"`
	DLRateLimit      int64  `json:"dl_rate_limit"`
	UPInfoData       int64  `json:"up_info_data"`
	UPInfoSpeed      int64  `json:"up_info_speed"`
	UPRateLimit      int64  `json:"up_rate_limit"`
}

type QBitMainData struct {
	FullUpdate bool  `json:"full_update"`
	Torrents   any   `json:"torrents"`
	Rid        int64 `json:"rid"`
}

type QBitTorrentInfo struct {
	AddedOn      int64   `json:"added_on"`
	AmountLeft   int64   `json:"amount_left"`
	AutoTMM      bool    `json:"auto_tmm"`
	Availability float64 `json:"availability"`
	Category     string  `json:"category"`
	Completed    int64   `json:"completed"`
	CompletionOn int64   `json:"completion_on"`
	ContentPath  string  `json:"content_path"`
	DLSpeed      int64   `json:"dlspeed"`
	Downloaded   int64   `json:"downloaded"`
	Eta          int64   `json:"eta"`
	Hash         string  `json:"hash"`
	InfohashV1   string  `json:"infohash_v1"`
	MagnetURI    string  `json:"magnet_uri"`
	Name         string  `json:"name"`
	Priority     int     `json:"priority"`
	Progress     float64 `json:"progress"`
	Ratio        float64 `json:"ratio"`
	SavePath     string  `json:"save_path"`
	Size         int64   `json:"size"`
	State        string  `json:"state"`
	Tags         string  `json:"tags"`
	TotalSize    int64   `json:"total_size"`
	Upspeed      int64   `json:"upspeed"`
}

func ProjectQBitCategory(category, savePath string) QBitCategory {
	return QBitCategory{Name: category, SavePath: strings.TrimSpace(savePath)}
}

func ProjectQBitTransferInfo(items []*store.Item) QBitTransferInfo {
	return QBitTransferInfo{
		ConnectionStatus: "connected",
	}
}

// ProjectQBitTorrent builds the qBit torrent info struct Sonarr/Radarr poll.
// containerRoot is the path inside Dark Harrbor's container (e.g. /data).
// reportedPrefix is the path Sonarr sees for the same directory
// (e.g. /mnt/darkharrbor). Paths in StrmPath are rewritten from containerRoot
// to reportedPrefix so Sonarr resolves files without remote path mapping.
func ProjectQBitTorrent(item *store.Item, reportedPrefix, containerRoot string) QBitTorrentInfo {
	progress := projectQBitProgress(item)
	state := projectQBitState(item)

	// Path design: Dark Harrbor is bind-mounted to /mnt/hdd inside its container,
	// matching the host path exactly. Sonarr mounts /mnt/hdd/tv-modern as /tv.
	// StrmPath is an absolute host path e.g. /mnt/hdd/tv-modern/Show.S01E01.strm.
	//
	// Sonarr requires content_path != save_path to trigger import.
	// Real qBittorrent layout:
	//   save_path    = the category dir:  /mnt/hdd/tv-modern
	//   content_path = season pack dir:   /mnt/hdd/tv-modern  ← same, causes warning
	//                  OR single file:    /mnt/hdd/tv-modern/Show.S01E01.strm ← ok
	//
	// For season packs (multi-file): content_path must be a named subfolder
	// inside save_path so they differ. We use the torrent display name as the
	// Path rewrite: StrmPath is written using the container-internal root
	// (e.g. /data/tv-modern/Show S01/Ep.strm). Sonarr sees the same files
	// at reportedPrefix (e.g. /mnt/darkharrbor/tv-modern/Show S01/Ep.strm).
	// Rewrite so no remote path mapping is needed in Sonarr.
	rewritePath := func(p string) string {
		if containerRoot != "" && reportedPrefix != "" &&
			containerRoot != reportedPrefix &&
			strings.HasPrefix(p, containerRoot) {
			return reportedPrefix + p[len(containerRoot):]
		}
		return p
	}

	// save_path = category dir as Sonarr sees it.
	// content_path = named release subfolder (season pack) or strm file (single ep).
	// They must differ so Sonarr's completed-download handler fires.
	savePath := ""
	contentPath := ""
	if item.StrmPath != nil {
		strmFile := rewritePath(strings.TrimRight(*item.StrmPath, "/"))
		parentDir := strmFile
		if idx := strings.LastIndex(strmFile, "/"); idx > 0 {
			parentDir = strmFile[:idx]
		}
		if isSingleFileItem(item) {
			// Single file: save_path=categoryDir, content_path=strm file → differ.
			// P3 removed the on-disk .mkv stubs this used to point at; pointing at
			// a nonexistent .mkv made Sonarr report "No files found are eligible
			// for import" (found live, P11 gate). Sonarr v4 imports .strm directly
			// and quality probing goes through the ffprobe wrapper relay, so the
			// .strm path itself is correct.
			savePath = parentDir
			contentPath = strmFile
		} else {
			// Season pack: parentDir is the named release subfolder.
			// save_path = its parent (the category dir).
			categoryDir := parentDir
			if idx := strings.LastIndex(parentDir, "/"); idx > 0 {
				categoryDir = parentDir[:idx]
			}
			savePath = categoryDir
			contentPath = parentDir
		}
	}

	magnetURI := ""
	if item.SourceURI != nil && strings.HasPrefix(strings.ToLower(*item.SourceURI), "magnet:") {
		magnetURI = *item.SourceURI
	}

	tags := strings.Join(item.Metadata.Tags, ",")

	completedOn := int64(0)
	if item.State == store.StateReady || item.State == store.StateRemoved {
		completedOn = item.UpdatedAt.Unix()
	}

	// hash must be the real torrent infohash (lowercase hex) — this is what
	// Sonarr uses to match a grabbed torrent against the download client's queue.
	// PublicID is an internal identifier and will never match.
	// InfoHash is nil for NZB items; use PublicID as fallback only in that case.
	hash := item.PublicID
	if item.InfoHash != nil && *item.InfoHash != "" {
		hash = *item.InfoHash
	}

	return QBitTorrentInfo{
		AddedOn:      item.CreatedAt.Unix(),
		AmountLeft:   0,
		AutoTMM:      false,
		Availability: 1.0,
		Category:     item.Category,
		Completed:    qbitDownloadedBytes(item, progress),
		CompletionOn: completedOn,
		ContentPath:  contentPath,
		DLSpeed:      0,
		Downloaded:   qbitDownloadedBytes(item, progress),
		Eta:          projectQBitEta(item),
		Hash:         hash,
		InfohashV1:   hash,
		MagnetURI:    magnetURI,
		Name:         item.DisplayName,
		Priority:     0,
		Progress:     progress,
		Ratio:        0,
		SavePath:     savePath,
		Size:         item.TotalSize,
		State:        state,
		Tags:         tags,
		TotalSize:    item.TotalSize,
		Upspeed:      0,
	}
}

func projectQBitProgress(item *store.Item) float64 {
	switch item.State {
	case store.StateReady, store.StateRemoved:
		return 1.0
	case store.StateResolving:
		// F-7: return real TorBox download progress when available so Sonarr
		// shows actual progress instead of a frozen 0% for uncached grabs.
		// F-9: cap at 0.99 while still resolving -- provider progress can
		// reach 1.0 before DH finishes materializing; never claim done
		// before StateReady.
		if p := item.Metadata.DownloadProgress; p > 0 {
			if p >= 1.0 {
				return 0.99
			}
			return p
		}
		return 0
	default:
		return 0
	}
}

// projectQBitEta returns the provider ETA (seconds) for items still
// materializing; 0 otherwise. Sonarr/Radarr render this as the queue
// time-left column. (F-9)
func projectQBitEta(item *store.Item) int64 {
	if item.State == store.StateResolving && item.Metadata.DownloadProgress > 0 {
		return item.Metadata.DownloadETASeconds
	}
	return 0
}

// qbitDownloadedBytes reports honest completed-byte counts: full size once
// done, progress-proportional while materializing. The arrs derive remaining
// size from Size*Progress, but external qBit tooling reads
// downloaded/completed directly. (F-9)
func qbitDownloadedBytes(item *store.Item, progress float64) int64 {
	if progress >= 1.0 {
		return item.TotalSize
	}
	if progress <= 0 {
		return 0
	}
	return int64(float64(item.TotalSize) * progress)
}

func projectQBitState(item *store.Item) string {
	switch item.State {
	case store.StateAccepted:
		return "queuedDL"
	case store.StateResolving:
		// F-8: while TorBox is actively downloading an uncached grab, report
		// "downloading" (paired with the F-7 progress passthrough) instead of
		// "queuedDL" -- the arrs render queuedDL as a frozen "Queued", which
		// reads as a stuck import during long uncached materializations
		// (95-min FraMeSToR REMUX observed live 2026-07-12; import then fired
		// 73s after ready). Resolving with no progress yet (cachegate /
		// TorBox as_queued) stays queuedDL.
		// F-9 revision: provider progress at 1.0 with the item still
		// resolving means DH is finalizing (file list, strm write) -- that
		// is downloading-adjacent work, not queued. Any positive progress
		// reports "downloading".
		if p := item.Metadata.DownloadProgress; p > 0 {
			return "downloading"
		}
		return "queuedDL"
	case store.StateReady, store.StateRemoved:
		return "pausedUP"
	case store.StateFailed:
		return "error"
	default:
		return "stoppedDL"
	}
}

// isSingleFileItem returns true when the item contains exactly one video file.
// FileList is a JSON array of file entries; we count entries to determine this.
// If FileList is nil or unparseable we assume multi-file (safer default —
// single-file misidentified as multi-file causes a warning; multi-file
// misidentified as single-file causes an import failure).
func isSingleFileItem(item *store.Item) bool {
	if item.FileList == nil {
		return false
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(*item.FileList), &entries); err != nil {
		return false
	}
	return len(entries) == 1
}
