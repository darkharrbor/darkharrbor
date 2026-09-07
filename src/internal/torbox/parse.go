package torbox

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

// apiEnvelope is the standard TorBox response wrapper.
type apiEnvelope struct {
	Success bool            `json:"success"`
	Error   any             `json:"error"`
	Detail  string          `json:"detail"`
	Data    json.RawMessage `json:"data"`
}

// parseCreateTask parses a createtorrent / createusenetdownload response.
func parseCreateTask(env *apiEnvelope) (*CreateTaskResponse, error) {
	if env == nil {
		return nil, fmt.Errorf("empty response envelope")
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return &CreateTaskResponse{}, nil
	}

	var object map[string]any
	if err := json.Unmarshal(env.Data, &object); err == nil {
		return &CreateTaskResponse{
			RemoteID:    extractActiveID("", object, true),
			QueuedID:    extractQueuedID(object),
			QueueAuthID: extractQueueAuthID("", object),
			RemoteHash:  firstString(object, "hash"),
			DisplayName: firstString(object, "name", "filename"),
		}, nil
	}

	var idOnly any
	if err := json.Unmarshal(env.Data, &idOnly); err == nil {
		if id := stringify(idOnly); id != "" {
			return &CreateTaskResponse{RemoteID: id}, nil
		}
	}
	return nil, fmt.Errorf("unable to parse create task response")
}

// parseItemsEnvelope returns the data payload as a slice of item maps,
// handling both a single-object response and an array response.
func parseItemsEnvelope(env *apiEnvelope) ([]map[string]any, error) {
	if env == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, fmt.Errorf("empty item response")
	}

	var object map[string]any
	if err := json.Unmarshal(env.Data, &object); err == nil {
		return []map[string]any{object}, nil
	}

	var list []map[string]any
	if err := json.Unmarshal(env.Data, &list); err == nil {
		return list, nil
	}

	return nil, fmt.Errorf("unable to parse item response")
}

// parseTaskStatus converts a raw item map from TorBox into a TaskStatus.
func parseTaskStatus(sourceType string, item map[string]any) *TaskStatus {
	files := extractRemoteFiles(item)
	progress := firstFloat(item, "progress", "download_progress")

	state := strings.ToLower(firstString(item, "download_state", "status", "state"))
	label := strings.ToLower(strings.TrimSpace(firstString(item, "download_label", "label")))
	errorText := firstString(item, "error", "detail", "message")

	downloadPresent, hasDownloadPresent := boolField(item, "download_present")
	downloadReadyField, hasDownloadReady := boolField(item, "download_ready")
	downloadReady := false
	switch {
	case hasDownloadPresent:
		downloadReady = downloadPresent
	case hasDownloadReady:
		downloadReady = downloadReadyField
	default:
		downloadReady = label == "download ready" || label == "cached"
	}

	stateFailed := strings.Contains(state, "fail") ||
		strings.Contains(state, "error") ||
		strings.Contains(state, "abort") ||
		strings.Contains(state, "cancel") ||
		strings.Contains(state, "cannot be completed") ||
		strings.Contains(state, "repair failed") ||
		strings.Contains(state, "incomplete")
	labelFailed := strings.Contains(label, "fail") || strings.Contains(label, "incomplete")
	failed := (stateFailed || labelFailed) && !downloadReady
	inactive := label == "inactive" || firstBool(item, "inactive")

	seeds := int(firstInt(item, "seeds", "seeders", "num_seeds"))
	eta := firstInt(item, "eta", "estimated_time_to_finish")
	// COR-11: stalled = provider explicitly says "stalled (no seeds)".
	// The zero-seeders/100-day-sentinel-ETA condition below is deliberately
	// NOT folded in here (it was until 2026-07-18): it is TorBox's own
	// "no metadata/swarm found *yet*" signal, observed live to appear within
	// seconds of an uncached add and later resolve successfully, so it gets
	// its own short dedicated grace period (TorrentOutcomeDeadSentinel)
	// rather than either the instant-fail or the full stall-timeout path.
	const torboxDeadETA = 8640000
	stalled := strings.Contains(state, "stalled")
	deadSentinel := strings.Contains(state, "checking") && seeds == 0 && eta >= torboxDeadETA

	outcome := provider.TorrentOutcomeUnknown
	failureCode := provider.TorrentFailureNone
	switch {
	case downloadReady:
		outcome = provider.TorrentOutcomeReady
	case deadSentinel:
		outcome = provider.TorrentOutcomeDeadSentinel
		failureCode = provider.TorrentFailureTerminalDeadSource
	case strings.Contains(state, "cannot be completed") || strings.Contains(state, "repair failed") || strings.Contains(state, "incomplete") || strings.Contains(label, "incomplete"):
		outcome = provider.TorrentOutcomeTerminalDeadSource
		failureCode = provider.TorrentFailureTerminalDeadSource
	case strings.Contains(state, "abort") || strings.Contains(state, "cancel"):
		outcome = provider.TorrentOutcomeCancelled
		failureCode = provider.TorrentFailureCancelled
	case inactive:
		outcome = provider.TorrentOutcomeRemoteRemoved
		failureCode = provider.TorrentFailureRemoteRemoved
	case stalled:
		outcome = provider.TorrentOutcomeStalled
		failureCode = provider.TorrentFailureTransientStall
	case failed:
		outcome = provider.TorrentOutcomeTransientError
		failureCode = provider.TorrentFailureProviderUnavailable
	}

	return &TaskStatus{
		RemoteID:        extractActiveID(sourceType, item, true),
		QueuedID:        extractQueuedID(item),
		QueueAuthID:     extractQueueAuthID(sourceType, item),
		Hash:            firstString(item, "hash"),
		Name:            firstString(item, "name", "filename", "title"),
		State:           state,
		Label:           label,
		Progress:        progress,
		Seeds:           seeds,
		ETA:             eta,
		DownloadPresent: downloadPresent,
		DownloadReady:   downloadReady,
		Outcome:         outcome,
		FailureCode:     failureCode,
		Failed:          failed,
		Stalled:         stalled,
		Inactive:        inactive,
		Error:           errorText,
		Files:           files,
	}
}

// parseCheckcachedResponse parses the checkcached endpoint response.
// If the hash is not cached, Cached=false is returned with no error.
func parseCheckcachedResponse(env *apiEnvelope, hash string) (*CheckCachedResult, error) {
	if env == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return &CheckCachedResult{Cached: false}, nil
	}

	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("unmarshal checkcached data: %w", err)
	}

	// Look up by exact hash first, then case-insensitive fallback.
	var entry any
	if v, ok := data[hash]; ok {
		entry = v
	} else {
		lower := strings.ToLower(hash)
		for k, v := range data {
			if strings.ToLower(k) == lower {
				entry = v
				break
			}
		}
	}

	if entry == nil {
		return &CheckCachedResult{Cached: false}, nil
	}

	entryMap, ok := entry.(map[string]any)
	if !ok {
		// TorBox checkcached only returns entries for cached hashes. Cache
		// state is therefore established by the hash entry's presence; file
		// metadata is optional enrichment requested via list_files=true.
		return &CheckCachedResult{Cached: true}, nil
	}

	filesRaw, ok := entryMap["files"]
	if !ok {
		return &CheckCachedResult{Cached: true}, nil
	}
	filesArr, ok := filesRaw.([]any)
	if !ok {
		return &CheckCachedResult{Cached: true}, nil
	}

	cachedFiles := make([]CachedFile, 0, len(filesArr))
	for _, f := range filesArr {
		fMap, ok := f.(map[string]any)
		if !ok {
			continue
		}
		name := firstString(fMap, "name", "filename")
		shortName := firstString(fMap, "short_name", "shortName")
		relPath := firstString(fMap, "s3_path", "path", "relative_path")
		if relPath == "" {
			if shortName != "" {
				relPath = shortName
			} else {
				relPath = name
			}
		}
		cachedFiles = append(cachedFiles, CachedFile{
			FileID:       firstString(fMap, "id", "file_id"),
			Name:         name,
			RelativePath: relPath,
			Size:         firstInt(fMap, "size", "bytes"),
			RequestDLURL: "", // populated by resolver via requestdl endpoint
		})
	}

	return &CheckCachedResult{Cached: true, Files: cachedFiles}, nil
}

// ── Extract helpers ─────────────────────────────────────────────────────────

func extractQueuedID(item map[string]any) string {
	return firstString(item, "queued_id", "queue_id", "id")
}

func extractActiveID(sourceType string, item map[string]any, allowGenericID bool) string {
	var keys []string
	switch strings.ToLower(sourceType) {
	case "torrent":
		keys = append(keys, "torrent_id", "download_id")
	case "nzb", "usenet":
		keys = append(keys, "usenetdownload_id", "usenet_id")
	default:
		keys = append(keys, "torrent_id", "usenetdownload_id", "usenet_id", "download_id")
	}
	if allowGenericID {
		keys = append(keys, "id")
	}
	return firstString(item, keys...)
}

func extractQueueAuthID(sourceType string, item map[string]any) string {
	if authID := firstString(item, "auth_id"); authID != "" {
		return authID
	}
	if !strings.EqualFold(sourceType, "usenet") {
		return ""
	}
	torrentFile := firstString(item, "torrent_file")
	if torrentFile == "" {
		return ""
	}
	if slash := strings.Index(torrentFile, "/"); slash > 0 {
		return torrentFile[:slash]
	}
	return torrentFile
}

func extractRemoteFiles(item map[string]any) []RemoteFile {
	raw, ok := item["files"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	files := make([]RemoteFile, 0, len(list))
	for _, entry := range list {
		object, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		fileID := firstString(object, "id", "file_id")
		name := firstString(object, "name", "filename")
		shortName := firstString(object, "short_name", "shortName")
		relativePath := firstString(object, "path", "relative_path")
		if relativePath == "" {
			if shortName != "" {
				relativePath = shortName
			} else {
				relativePath = name
			}
		}
		files = append(files, RemoteFile{
			FileID:       fileID,
			Name:         name,
			ShortName:    shortName,
			RelativePath: relativePath,
			Size:         firstInt(object, "size", "bytes"),
		})
	}
	return files
}

// ── Generic map accessor helpers (variadic keys) ─────────────────────────────

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := m[key]
		if !ok {
			continue
		}
		if out := stringify(value); out != "" {
			return out
		}
	}
	return ""
}

func firstInt(m map[string]any, keys ...string) int64 {
	for _, key := range keys {
		value, ok := m[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return int64(typed)
		case float32:
			return int64(typed)
		case int:
			return int64(typed)
		case int64:
			return typed
		case json.Number:
			if out, err := typed.Int64(); err == nil {
				return out
			}
		case string:
			if parsed, err := strconv.ParseInt(typed, 10, 64); err == nil {
				return parsed
			}
		}
	}
	return 0
}

func firstFloat(m map[string]any, keys ...string) float64 {
	for _, key := range keys {
		value, ok := m[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return typed
		case float32:
			return float64(typed)
		case int:
			return float64(typed)
		case int64:
			return float64(typed)
		case json.Number:
			if out, err := typed.Float64(); err == nil {
				return out
			}
		case string:
			if parsed, err := strconv.ParseFloat(typed, 64); err == nil {
				return parsed
			}
		}
	}
	return 0
}

func firstBool(m map[string]any, keys ...string) bool {
	for _, key := range keys {
		value, ok := m[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed
		case string:
			parsed, err := strconv.ParseBool(typed)
			if err == nil {
				return parsed
			}
		case float64:
			return typed != 0
		case int:
			return typed != 0
		}
	}
	return false
}

func boolField(m map[string]any, key string) (bool, bool) {
	value, ok := m[key]
	if !ok {
		return false, false
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(typed)
		if err == nil {
			return parsed, true
		}
	case float64:
		return typed != 0, true
	case int:
		return typed != 0, true
	}
	return false, false
}

func stringify(v any) string {
	switch typed := v.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case float32:
		return strconv.FormatInt(int64(typed), 10)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// occupiesUncachedTorrentSlot reports whether a TorBox torrent is still
// consuming one of the account's uncached-download slots. A positive ready
// field or label wins over a stale state string.
func occupiesUncachedTorrentSlot(item map[string]any) bool {
	if firstBool(item, "download_present") || firstBool(item, "download_ready") {
		return false
	}
	label := strings.ToLower(strings.TrimSpace(firstString(item, "download_label", "label")))
	if label == "download ready" || label == "cached" {
		return false
	}
	return isSlotActiveState(firstString(item, "download_state", "status", "state"))
}

// isSlotActiveState classifies only states that still represent an uncached
// torrent download. Seeding/uploading is cached work and therefore slot-free.
func isSlotActiveState(state string) bool {
	state = strings.ToLower(strings.TrimSpace(state))
	if state == "" {
		return false
	}
	switch {
	case strings.Contains(state, "ready"),
		strings.Contains(state, "complete"),
		strings.Contains(state, "cache"):
		return false
	case strings.Contains(state, "download"):
		return true
	case strings.Contains(state, "checking"):
		return true
	case strings.Contains(state, "stalled"):
		return true
	case strings.Contains(state, "metadl"):
		return true
	default:
		return false
	}
}
