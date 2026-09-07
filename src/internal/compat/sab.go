package compat

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

type SABAddResponse struct {
	Status bool     `json:"status"`
	NzoIDs []string `json:"nzo_ids"`
}

type SABQueueResponse struct {
	Queue SABQueue `json:"queue"`
}

type SABQueue struct {
	Version   string         `json:"version"`
	Paused    bool           `json:"paused"`
	NoOfSlots string         `json:"noofslots"`
	Start     int            `json:"start"`
	Limit     int            `json:"limit"`
	Finish    int            `json:"finish"`
	Slots     []SABQueueSlot `json:"slots"`
	Status    string         `json:"status"`
}

type SABQueueSlot struct {
	NzoID      string `json:"nzo_id"`
	Filename   string `json:"filename"`
	Cat        string `json:"cat"`
	MB         string `json:"mb"`
	MBLeft     string `json:"mbleft"`
	Percentage int    `json:"percentage"`
	Status     string `json:"status"`
	TimeLeft   string `json:"timeleft"`
	Priority   string `json:"priority"`
	PP         string `json:"pp"`
	Script     string `json:"script"`
}

type SABHistoryResponse struct {
	History SABHistory `json:"history"`
}

type SABHistory struct {
	Version   string           `json:"version"`
	NoOfSlots string           `json:"noofslots"`
	Slots     []SABHistorySlot `json:"slots"`
}

type SABHistorySlot struct {
	NzoID       string `json:"nzo_id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Status      string `json:"status"`
	FailMessage string `json:"fail_message"`
	Path        string `json:"path"`
	Storage     string `json:"storage"`
	Completed   int64  `json:"completed"`
	Downloaded  int64  `json:"downloaded"`
	Size        string `json:"size"`
}

func SABNZOID(publicID string) string {
	return "TBOX-" + publicID
}

func NormalizeSABNZOID(v string) string {
	return strings.TrimPrefix(v, "TBOX-")
}

func ProjectSABQueue(version string, items []*store.Item) SABQueueResponse {
	slots := make([]SABQueueSlot, 0)
	status := "Idle"
	for _, item := range items {
		if !isSABQueueState(item.State) {
			continue
		}
		slot := ProjectSABQueueSlot(item)
		slots = append(slots, slot)
		if slot.Status == "Downloading" {
			status = "Downloading"
		}
	}
	return SABQueueResponse{Queue: SABQueue{
		Version:   version,
		Paused:    false,
		NoOfSlots: fmt.Sprintf("%d", len(slots)),
		Start:     0,
		Limit:     len(slots),
		Finish:    len(slots),
		Slots:     slots,
		Status:    status,
	}}
}

func ProjectSABHistory(version string, items []*store.Item, containerRoot, reportedPrefix string) SABHistoryResponse {
	slots := make([]SABHistorySlot, 0)
	for _, item := range items {
		if !isSABHistoryState(item.State) {
			continue
		}
		slots = append(slots, ProjectSABHistorySlot(item, containerRoot, reportedPrefix))
	}
	return SABHistoryResponse{History: SABHistory{
		Version:   version,
		NoOfSlots: fmt.Sprintf("%d", len(slots)),
		Slots:     slots,
	}}
}

func ProjectSABQueueSlot(item *store.Item) SABQueueSlot {
	return SABQueueSlot{
		NzoID:      SABNZOID(item.PublicID),
		Filename:   item.DisplayName,
		Cat:        item.Category,
		MB:         sabMBString(item.TotalSize),
		MBLeft:     sabMBString(sabRemainingBytes(item)),
		Percentage: projectSABPercent(item),
		Status:     projectSABQueueStatus(item.State),
		TimeLeft:   sabTimeLeft(item),
		Priority:   "Normal",
		PP:         projectSABPP(item.Metadata.PostProcessing),
		Script:     "None",
	}
}

func ProjectSABHistorySlot(item *store.Item, containerRoot, reportedPrefix string) SABHistorySlot {
	// Report the release DIRECTORY as storage — matching real SABnzbd, which
	// reports the completed job folder; the arr scans the folder and imports
	// every eligible file it contains. Reporting the first .strm file (previous
	// behavior) broke multi-episode packs: usenet imports MOVE files, so after
	// the arr imported that one episode the remaining queue records pointed at
	// a path that no longer existed ("No files found are eligible for import")
	// and the rest of the pack was stranded at importPending forever.
	// NZB items always materialize into their own release dir; the category-
	// root guard keeps any legacy flat-file item on the old file-path behavior.
	storagePath := ""
	if item.StrmPath != nil && *item.StrmPath != "" {
		storagePath = *item.StrmPath
		if dir := filepath.Dir(storagePath); dir != "/" && dir != "." &&
			filepath.Base(dir) != item.Category {
			storagePath = dir
		}
		// Rewrite container-internal path to reported path so Sonarr can find it.
		if containerRoot != "" && reportedPrefix != "" && containerRoot != reportedPrefix &&
			strings.HasPrefix(storagePath, containerRoot) {
			storagePath = reportedPrefix + storagePath[len(containerRoot):]
		}
	}
	failMessage := ""
	if item.ErrorMessage != nil {
		failMessage = *item.ErrorMessage
	}
	return SABHistorySlot{
		NzoID:       SABNZOID(item.PublicID),
		Name:        item.DisplayName,
		Category:    item.Category,
		Status:      projectSABHistoryStatus(item.State),
		FailMessage: failMessage,
		Path:        storagePath,
		Storage:     storagePath,
		Completed:   item.UpdatedAt.Unix(),
		Downloaded:  item.TotalSize,
		Size:        sabMBString(item.TotalSize) + " MB",
	}
}

func sabMBString(bytes int64) string {
	if bytes <= 0 {
		return "0.00"
	}
	return fmt.Sprintf("%.2f", float64(bytes)/(1024*1024))
}

func isSABQueueState(state store.ItemState) bool {
	return state == store.StateAccepted || state == store.StateResolving
}

func isSABHistoryState(state store.ItemState) bool {
	return state == store.StateReady || state == store.StateRemoved || state == store.StateFailed
}

func projectSABQueueStatus(state store.ItemState) string {
	if state == store.StateResolving {
		return "Downloading"
	}
	return "Queued"
}

func projectSABHistoryStatus(state store.ItemState) string {
	if state == store.StateFailed {
		return "Failed"
	}
	return "Completed"
}

// projectSABPercent reports live materialization progress (F-9): 100 once
// ready, provider progress while resolving (capped at 99 -- never claim done
// before StateReady), 0 otherwise. Direct-NNTP resolves carry no provider
// progress and briefly show "Downloading 0%", which is honest: the resolve is
// active and typically completes in seconds.
func projectSABPercent(item *store.Item) int {
	switch item.State {
	case store.StateReady, store.StateRemoved:
		return 100
	case store.StateResolving:
		p := item.Metadata.DownloadProgress
		if p <= 0 {
			return 0
		}
		pct := int(p * 100)
		if pct > 99 {
			pct = 99
		}
		if pct < 1 {
			pct = 1
		}
		return pct
	default:
		return 0
	}
}

// sabRemainingBytes is the MBLeft counterpart of projectSABPercent: 0 once
// ready, progress-proportional remainder while materializing, full size
// otherwise. (F-9: MBLeft was pinned to the full size, so the arr progress
// bar never moved even though Status said Downloading.)
func sabRemainingBytes(item *store.Item) int64 {
	switch item.State {
	case store.StateReady, store.StateRemoved:
		return 0
	case store.StateResolving:
		p := item.Metadata.DownloadProgress
		if p <= 0 {
			return item.TotalSize
		}
		if p >= 1 {
			p = 0.99
		}
		return int64(float64(item.TotalSize) * (1 - p))
	default:
		return item.TotalSize
	}
}

// sabTimeLeft renders the persisted provider ETA as SAB's H:MM:SS timeleft
// while an item is materializing; "0:00:00" otherwise. (F-9)
func sabTimeLeft(item *store.Item) string {
	if item.State != store.StateResolving {
		return "0:00:00"
	}
	eta := item.Metadata.DownloadETASeconds
	if eta <= 0 {
		return "0:00:00"
	}
	return fmt.Sprintf("%d:%02d:%02d", eta/3600, (eta%3600)/60, eta%60)
}

func projectSABPP(v int) string {
	switch v {
	case 0:
		return "0"
	case 1:
		return "R"
	case 2:
		return "U"
	case 3:
		return "D"
	default:
		return "D"
	}
}
