package realdebrid

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

// parseUserResponse extracts AccountInfo from the /user JSON response body.
func parseUserResponse(body []byte) (*AccountInfo, error) {
	var data struct {
		Premium    int    `json:"premium"` // seconds of premium remaining
		Expiration string `json:"expiration"`
		Points     int    `json:"points"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("rd: parse /user: %w", err)
	}
	info := &AccountInfo{
		Premium:        data.Premium > 0,
		ExpirationDays: data.Premium / 86400,
		Points:         data.Points,
	}
	return info, nil
}

// parseInstantAvailability parses the /torrents/instantAvailability response.
// RD returns: { "<hash>": { "rd": [ { "<fileID>": {filename, filesize}, ... }, ... ] } }
// or an empty object {} when nothing is cached.
func parseInstantAvailability(body []byte, hashes []string) (map[string][]CachedGroup, error) {
	// RD returns the top-level as a JSON object keyed by lowercase hash.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("rd: parse instantAvailability outer: %w", err)
	}

	result := make(map[string][]CachedGroup, len(hashes))
	for _, hash := range hashes {
		lower := strings.ToLower(hash)
		v, ok := raw[lower]
		if !ok {
			continue
		}
		// Per-hash structure: { "rd": [ { fileID: {filename, filesize} }, ... ] }
		var perHash struct {
			RD []map[string]struct {
				Filename string `json:"filename"`
				Filesize int64  `json:"filesize"`
			} `json:"rd"`
		}
		if err := json.Unmarshal(v, &perHash); err != nil {
			continue // treat parse error as cache miss for this hash
		}
		if len(perHash.RD) == 0 {
			continue
		}
		groups := make([]CachedGroup, 0, len(perHash.RD))
		for _, group := range perHash.RD {
			g := CachedGroup{Files: make(map[string]CachedFile, len(group))}
			for id, f := range group {
				g.Files[id] = CachedFile{Filename: f.Filename, Filesize: f.Filesize}
			}
			if len(g.Files) > 0 {
				groups = append(groups, g)
			}
		}
		if len(groups) > 0 {
			result[lower] = groups
		}
	}
	return result, nil
}

// parseTorrentInfo parses the /torrents/info/{id} response.
func parseTorrentInfo(body []byte) (*TorrentInfo, error) {
	var data struct {
		ID       string  `json:"id"`
		Filename string  `json:"filename"`
		Status   string  `json:"status"`
		Progress float64 `json:"progress"`
		Files    []struct {
			ID       int    `json:"id"`
			Path     string `json:"path"`
			Bytes    int64  `json:"bytes"`
			Selected int    `json:"selected"`
		} `json:"files"`
		Links []string `json:"links"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("rd: parse torrent info: %w", err)
	}
	info := &TorrentInfo{
		ID:       data.ID,
		Filename: data.Filename,
		Status:   data.Status,
		Progress: data.Progress,
		Links:    data.Links,
	}
	for _, f := range data.Files {
		info.Files = append(info.Files, TorrentFile{
			ID:       f.ID,
			Path:     f.Path,
			Bytes:    f.Bytes,
			Selected: f.Selected == 1,
		})
	}
	return info, nil
}

// parseUnrestrictResponse extracts the direct download URL from /unrestrict/link.
func parseUnrestrictResponse(body []byte) (string, error) {
	var data struct {
		Download string `json:"download"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("rd: parse unrestrict: %w", err)
	}
	if data.Download == "" {
		return "", fmt.Errorf("rd: unrestrict returned empty download URL")
	}
	return data.Download, nil
}

// parseAddMagnetResponse extracts the RD item ID from /torrents/addMagnet.
func parseAddMagnetResponse(body []byte) (string, error) {
	var data struct {
		ID  string `json:"id"`
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("rd: parse addMagnet: %w", err)
	}
	if data.ID == "" {
		return "", fmt.Errorf("rd: addMagnet returned empty id")
	}
	return data.ID, nil
}

// torrentInfoToTaskStatus converts a TorrentInfo into the provider-neutral TaskStatus.
// RD status strings: magnet_error, waiting_files_selection, queued, downloading,
// downloaded, error, virus, compressing, uploading, dead.
func torrentInfoToTaskStatus(info *TorrentInfo) *provider.TaskStatus {
	state := strings.ToLower(strings.TrimSpace(info.Status))

	// Terminal failure states — fail immediately, no age-threshold needed.
	terminalFail := state == "magnet_error" ||
		state == "virus" ||
		state == "dead"

	// Ready when fully downloaded.
	ready := state == "downloaded"

	// RD's "downloading" state at 0% progress is normal — RD queues the
	// download and starts fetching from peers; progress increments over time.
	// Unlike TorBox where checking+seeds=0+ETA=sentinel signals a dead magnet,
	// RD's "downloading" always means an active transfer is in progress.
	// Only terminal states (magnet_error, dead, error, virus) signal failure.
	// Stalled is never set for RD; the resolver stall timeout (COR-11) is
	// not applicable to RD's lifecycle model.
	stalled := false

	outcome := provider.TorrentOutcomeUnknown
	failureCode := provider.TorrentFailureNone
	switch state {
	case "downloaded":
		outcome = provider.TorrentOutcomeReady
	case "magnet_error", "dead", "virus":
		outcome = provider.TorrentOutcomeTerminalDeadSource
		failureCode = provider.TorrentFailureTerminalDeadSource
	case "error":
		outcome = provider.TorrentOutcomeTransientError
		failureCode = provider.TorrentFailureProviderUnavailable
	}

	var files []provider.RemoteFile
	for i, f := range info.Files {
		if !f.Selected {
			continue
		}
		fileID := fmt.Sprintf("%d", f.ID)
		// Use the link at the same index if available; links align with
		// selected files in RD's response ordering.
		name := f.Path
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		_ = i
		files = append(files, provider.RemoteFile{
			FileID:       fileID,
			Name:         name,
			RelativePath: f.Path,
			Size:         f.Bytes,
		})
	}

	return &provider.TaskStatus{
		RemoteID:      info.ID,
		Name:          info.Filename,
		State:         state,
		Progress:      info.Progress / 100.0, // RD gives 0–100, DH uses 0–1
		DownloadReady: ready,
		Outcome:       outcome,
		FailureCode:   failureCode,
		Failed:        terminalFail,
		Stalled:       stalled,
		Files:         files,
	}
}

// cachedGroupToCachedFiles converts the first non-empty RD cache group to
// provider.CachedFile entries. fileID is set to the RD file key string;
// RequestDLURL is empty at this stage (populated at resolve time via
// UnrestrictLink on the per-item links from TorrentInfo).
func cachedGroupToCachedFiles(groups []CachedGroup) []provider.CachedFile {
	for _, g := range groups {
		if len(g.Files) == 0 {
			continue
		}
		files := make([]provider.CachedFile, 0, len(g.Files))
		for id, f := range g.Files {
			name := f.Filename
			if idx := strings.LastIndex(name, "/"); idx >= 0 {
				name = name[idx+1:]
			}
			files = append(files, provider.CachedFile{
				FileID:       id,
				Name:         name,
				RelativePath: f.Filename,
				Size:         f.Filesize,
			})
		}
		return files
	}
	return nil
}
