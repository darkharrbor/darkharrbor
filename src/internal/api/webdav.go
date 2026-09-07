package api

//
// Jellyfin mounts http://darkharrbor:8381/dav/ as a WebDAV library source.
// NZB .strm files contain /dav/... URLs; Jellyfin follows them, hits this
// handler, and probes/streams as a seekable local file (no EBML issue).
//
// Routes:
//   OPTIONS  /dav/                                 — capability probe
//   PROPFIND /dav/                                 — root collection
//   PROPFIND /dav/{category}/                      — category collection
//   PROPFIND /dav/{category}/{release}/            — release collection
//   PROPFIND /dav/{category}/{release}/{file}.mkv  — file metadata
//   GET      /dav/{category}/{release}/{file}.mkv  — playback
//   HEAD     /dav/{category}/{release}/{file}.mkv  — size probe
//
// GET/HEAD route to existing NNTPProvider.Stream() / StreamRAR().
// No HMAC token — WebDAV is on internal arr-net only.

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// registerWebDAV wires all WebDAV routes onto mux.
func (s *Server) registerWebDAV(mux *http.ServeMux) {
	mux.HandleFunc("/dav/api", s.handleSABAPI)
	mux.HandleFunc("/dav/", s.handleWebDAV)
}

func (s *Server) handleWebDAV(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "OPTIONS":
		s.webdavOptions(w, r)
	case "PROPFIND":
		s.webdavPropfind(w, r)
	case http.MethodGet:
		s.webdavGet(w, r)
	case http.MethodHead:
		s.webdavHead(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) webdavOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("DAV", "1")
	w.Header().Set("Allow", "OPTIONS, PROPFIND, GET, HEAD")
	w.Header().Set("MS-Author-Via", "DAV")
	w.WriteHeader(http.StatusOK)
}

// ── PROPFIND ─────────────────────────────────────────────────────────────────

type davProp struct {
	XMLName       xml.Name        `xml:"D:prop"`
	DisplayName   string          `xml:"D:displayname"`
	ResourceType  davResourceType `xml:"D:resourcetype"`
	ContentLength string          `xml:"D:getcontentlength,omitempty"`
	ContentType   string          `xml:"D:getcontenttype,omitempty"`
	LastModified  string          `xml:"D:getlastmodified,omitempty"`
	CreationDate  string          `xml:"D:creationdate,omitempty"`
}

type davResourceType struct {
	Collection *struct{} `xml:"D:collection,omitempty"`
}

type davPropStat struct {
	XMLName xml.Name `xml:"D:propstat"`
	Prop    davProp  `xml:"D:prop"`
	Status  string   `xml:"D:status"`
}

type davResponse struct {
	XMLName  xml.Name      `xml:"D:response"`
	Href     string        `xml:"D:href"`
	PropStat []davPropStat `xml:"D:propstat"`
}

type davMultiStatus struct {
	XMLName   xml.Name      `xml:"D:multistatus"`
	XMLNS     string        `xml:"xmlns:D,attr"`
	Responses []davResponse `xml:"D:response"`
}

func collectionResponse(href, name string) davResponse {
	return davResponse{
		Href: href,
		PropStat: []davPropStat{{
			Prop: davProp{
				DisplayName:  name,
				ResourceType: davResourceType{Collection: &struct{}{}},
				LastModified: time.Now().UTC().Format(http.TimeFormat),
				CreationDate: time.Now().UTC().Format(time.RFC3339),
			},
			Status: "HTTP/1.1 200 OK",
		}},
	}
}

func fileResponse(href, name string, size int64, modTime time.Time) davResponse {
	return davResponse{
		Href: href,
		PropStat: []davPropStat{{
			Prop: davProp{
				DisplayName:   name,
				ResourceType:  davResourceType{},
				ContentLength: strconv.FormatInt(size, 10),
				ContentType:   "video/x-matroska",
				LastModified:  modTime.UTC().Format(http.TimeFormat),
				CreationDate:  modTime.UTC().Format(time.RFC3339),
			},
			Status: "HTTP/1.1 200 OK",
		}},
	}
}

func writeMultiStatus(w http.ResponseWriter, ms davMultiStatus) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(ms)
}

func (s *Server) webdavPropfind(w http.ResponseWriter, r *http.Request) {
	path, ok := parseWebDAVPath(r.URL.Path)
	if !ok {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	parts := path.parts

	ctx := r.Context()

	switch len(parts) {
	case 0:
		if path.alias {
			responses := []davResponse{collectionResponse("/dav/content/", "content")}
			for _, category := range []string{"Movies", "TV"} {
				responses = append(responses, collectionResponse("/dav/content/"+category+"/", category))
			}
			writeMultiStatus(w, davMultiStatus{XMLNS: "DAV:", Responses: responses})
			return
		}
		// Root: list all categories that have NZB items.
		// FindNZBItemsByCategory with empty string returns all NZB items.
		items, err := s.store.FindNZBItemsByCategory(ctx, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		seen := map[string]struct{}{}
		responses := []davResponse{collectionResponse("/dav/", "")}
		for _, item := range items {
			if _, ok := seen[item.Category]; !ok {
				seen[item.Category] = struct{}{}
				responses = append(responses, collectionResponse("/dav/"+item.Category+"/", item.Category))
			}
		}
		writeMultiStatus(w, davMultiStatus{XMLNS: "DAV:", Responses: responses})

	case 1:
		// Category: list release directories.
		category := parts[0]
		visibleCategory := path.visibleCategory(category)
		items, err := s.store.FindNZBItemsByCategory(ctx, category)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		responses := []davResponse{collectionResponse(path.href(category)+"/", visibleCategory)}
		seen := map[string]struct{}{}
		for _, item := range items {
			relDir := nzbReleaseDirName(item)
			if _, ok := seen[relDir]; !ok {
				seen[relDir] = struct{}{}
				responses = append(responses, collectionResponse(
					path.href(category, relDir)+"/", relDir,
				))
			}
		}
		writeMultiStatus(w, davMultiStatus{XMLNS: "DAV:", Responses: responses})

	case 2:
		// Release directory: list .mkv virtual files.
		category, relDir := parts[0], parts[1]
		strmDirPrefix := filepath.Join(s.cfg.Data.Root, category, relDir)
		item, err := s.store.FindNZBItemByStrmDir(ctx, strmDirPrefix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if item == nil {
			http.NotFound(w, r)
			return
		}
		responses := []davResponse{collectionResponse(path.href(category, relDir)+"/", relDir)}
		for _, name := range nzbFileNames(item) {
			href := path.href(category, relDir, name+".mkv")
			responses = append(responses, fileResponse(href, name+".mkv", nzbVisibleFileSize(item, name), item.CreatedAt))
		}
		writeMultiStatus(w, davMultiStatus{XMLNS: "DAV:", Responses: responses})

	case 3:
		// Single file metadata.
		category, relDir, fileName := parts[0], parts[1], parts[2]
		strmDirPrefix := filepath.Join(s.cfg.Data.Root, category, relDir)
		item, err := s.store.FindNZBItemByStrmDir(ctx, strmDirPrefix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if item == nil {
			http.NotFound(w, r)
			return
		}
		href := path.href(category, relDir, fileName)
		stem := strings.TrimSuffix(fileName, ".mkv")
		responses := []davResponse{fileResponse(href, fileName, nzbVisibleFileSize(item, stem), item.CreatedAt)}
		writeMultiStatus(w, davMultiStatus{XMLNS: "DAV:", Responses: responses})

	default:
		http.NotFound(w, r)
	}
}

// ── GET / HEAD ────────────────────────────────────────────────────────────────

func webdavRangeUnsatisfiable(rangeHeader string, totalBytes int64) bool {
	if rangeHeader == "" || !strings.HasPrefix(rangeHeader, "bytes=") {
		return false
	}
	if totalBytes < 0 {
		totalBytes = 0
	}
	spec := strings.TrimPrefix(rangeHeader, "bytes=")
	if comma := strings.Index(spec, ","); comma >= 0 {
		spec = spec[:comma]
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return false
	}
	if parts[0] == "" {
		// Suffix ranges are satisfiable for any non-empty item; malformed suffixes
		// are ignored here and left to the downstream stream implementation.
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		return err == nil && suffix > 0 && totalBytes == 0
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	if parts[1] != "" {
		end, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return false
		}
		if end < start {
			return true
		}
	}
	return start >= totalBytes
}

func writeWebDAVRangeNotSatisfiable(w http.ResponseWriter, totalBytes int64) {
	if totalBytes < 0 {
		totalBytes = 0
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(totalBytes, 10))
	http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
}

// nzbEffectiveSize returns the corrected decoded byte total for an NZB item
// when the segment offset cache has enough data, falling back to the declared
// NZB total. Must be used consistently across HEAD, GET, and PROPFIND so that
// clients see the same Content-Length in every response and range requests
// computed from that value are satisfiable.
func (s *Server) nzbEffectiveSize(ctx context.Context, item *store.Item, prov provider.UsenetProvider, fileIndex int) int64 {
	if item.TotalSize <= 0 {
		return 0
	}
	// CorrectedTotal describes one ordinary NZB file. Archive members already
	// carry their exact logical sizes in resolver-time manifests.
	if item.IsRARSplit() {
		return item.TotalSize
	}
	if item.IsSevenZipArchive() {
		manifest, err := nntp.UnmarshalSevenZipManifest(*item.Metadata.SevenZipManifest)
		if err != nil || fileIndex < 0 || fileIndex >= len(manifest.Entries) {
			return 0
		}
		return manifest.Entries[fileIndex].Size
	}
	if prov == nil || item.SourceURI == nil || *item.SourceURI == "" {
		return item.TotalSize
	}
	if corrected := prov.CorrectedTotal(ctx, item.ID, []byte(*item.SourceURI), fileIndex); corrected > 0 {
		return corrected
	}
	return item.TotalSize
}

type webdavEffectiveSizeWriter struct {
	http.ResponseWriter
	total int64
}

func (w *webdavEffectiveSizeWriter) WriteHeader(status int) {
	if w.total > 0 {
		switch status {
		case http.StatusPartialContent:
			if raw := w.Header().Get("Content-Range"); raw != "" {
				if slash := strings.LastIndexByte(raw, '/'); slash >= 0 {
					w.Header().Set("Content-Range", raw[:slash+1]+strconv.FormatInt(w.total, 10))
				}
			}
		case http.StatusOK:
			w.Header().Set("Content-Length", strconv.FormatInt(w.total, 10))
		case http.StatusRequestedRangeNotSatisfiable:
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(w.total, 10))
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *webdavEffectiveSizeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) webdavHead(w http.ResponseWriter, r *http.Request) {
	item, fileIndex, err := s.webdavResolveItem(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if item == nil {
		http.NotFound(w, r)
		return
	}
	_, prov, _ := s.pickUsenetProvider(item)
	effSize := s.nzbEffectiveSize(r.Context(), item, prov, fileIndex)
	if webdavRangeUnsatisfiable(r.Header.Get("Range"), effSize) {
		writeWebDAVRangeNotSatisfiable(w, effSize)
		return
	}
	w.Header().Set("Content-Type", "video/x-matroska")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-store")
	if effSize > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(effSize, 10))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) webdavGet(w http.ResponseWriter, r *http.Request) {
	item, fileIndex, err := s.webdavResolveItem(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if item == nil {
		http.NotFound(w, r)
		return
	}
	usenetName, usenetProv, usenetOK := s.pickUsenetProvider(item)
	effSize := s.nzbEffectiveSize(r.Context(), item, usenetProv, fileIndex)
	ctx := playbackcoverage.WithRepresentation(r.Context(), s.playbackRepresentationID(r.Context(), item, strconv.Itoa(fileIndex)))
	ctx = withPlaybackProposalTarget(ctx, item, strconv.Itoa(fileIndex), effSize)
	r = r.WithContext(ctx)
	if webdavRangeUnsatisfiable(r.Header.Get("Range"), effSize) {
		writeWebDAVRangeNotSatisfiable(w, effSize)
		return
	}
	if !usenetOK {
		http.Error(w, "NNTP provider not configured", http.StatusServiceUnavailable)
		return
	}
	if item.SourceURI == nil || *item.SourceURI == "" {
		http.Error(w, "NZB data not found", http.StatusNotFound)
		return
	}

	if err := s.persistLastPlayedAt(r.Context(), item); err != nil {
		s.log.Warn("webdav: update last_played_at failed", "item_id", item.ID, "error", err)
	}

	s.log.Info("webdav: play started",
		"item_id", item.ID,
		"display_name", item.DisplayName,
		"file_index", fileIndex,
		"usenet_provider", usenetName,
	)

	contentKey := s.nntpContentKey(r.Context(), item)
	s.prefetchNNTPSeek(r.Context(), item, usenetProv, fileIndex, contentKey, r.Header.Get("Range"))
	streamWriter := http.ResponseWriter(w)
	if effSize > 0 {
		streamWriter = &webdavEffectiveSizeWriter{ResponseWriter: w, total: effSize}
	}
	if item.IsRARSplit() {
		rangeHeader := r.Header.Get("Range")
		var streamErr error
		if encryptedProv, ok := usenetProv.(encryptedRARStreamProvider); ok {
			streamErr = encryptedProv.StreamEncryptedRARManifest(r.Context(), *item.RarManifest, item.TotalSize, streamWriter, rangeHeader, item.ID, contentKey, nntpRARPassword(item))
		} else {
			streamErr = usenetProv.StreamRARManifest(r.Context(), *item.RarManifest, item.TotalSize, streamWriter, rangeHeader, item.ID, contentKey)
		}
		if streamErr != nil {
			s.log.Warn("webdav: rar stream error", "item_id", item.ID, "error", streamErr)
		} else {
			// NS-5.4: idle-only one-episode look-ahead prewarm, only after a
			// clean stream (never on a failed/aborted one). Best-effort,
			// bounded, and non-blocking -- see nntp_next_episode.go.
			s.queueNextEpisodeNNTPPrewarm(item, usenetProv, rangeHeader)
		}
		return
	}
	if item.IsSevenZipArchive() {
		sevenZipProv, ok := usenetProv.(interface {
			StreamSevenZipEntry(context.Context, string, int, http.ResponseWriter, string, string, string) error
		})
		if !ok {
			http.Error(w, "7z streaming unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := sevenZipProv.StreamSevenZipEntry(r.Context(), *item.Metadata.SevenZipManifest, fileIndex, streamWriter, r.Header.Get("Range"), item.ID, contentKey); err != nil {
			s.log.Warn("webdav: 7z stream error", "item_id", item.ID, "error", err)
		}
		return
	}

	if err := usenetProv.Stream(r.Context(), item.ID, []byte(*item.SourceURI), fileIndex, streamWriter, r.Header.Get("Range")); err != nil {
		s.log.Warn("webdav: stream error", "item_id", item.ID, "error", err)
	}
}

// webdavResolveItem parses /dav/{category}/{release}/{file}.mkv and returns
// the matching store item and file index within that item's video files.
func (s *Server) webdavResolveItem(r *http.Request) (*store.Item, int, error) {
	path, ok := parseWebDAVPath(r.URL.Path)
	if !ok || len(path.parts) != 3 {
		return nil, 0, nil
	}
	parts := path.parts
	category, relDir, fileName := parts[0], parts[1], parts[2]
	if category == "" || relDir == "" || fileName == "" {
		return nil, 0, nil
	}

	if !validateWebDAVPathParts(parts) {
		return nil, 0, nil
	}

	strmDirPrefix := filepath.Join(s.cfg.Data.Root, category, relDir)
	item, err := s.store.FindNZBItemByStrmDir(r.Context(), strmDirPrefix)
	if err != nil || item == nil {
		return nil, 0, err
	}

	// Require the visible path to match one of the materialized .strm names
	// before honoring any exact-index query. This prevents an arbitrary NZB
	// payload from being selected under an otherwise valid release path.
	stem := strings.TrimSuffix(fileName, ".mkv")
	fileIndex := -1
	for i, name := range nzbFileNames(item) {
		if strings.EqualFold(name, stem) {
			fileIndex = i
			break
		}
	}
	if fileIndex < 0 {
		return nil, 0, nil
	}
	if item.IsSevenZipArchive() {
		persisted, ok, indexErr := nzbPersistedStreamIndex(item, stem)
		if indexErr != nil {
			return nil, 0, indexErr
		}
		if !ok {
			return nil, 0, fmt.Errorf("7z stream index missing from materialized pointer")
		}
		manifest, manifestErr := nntp.UnmarshalSevenZipManifest(*item.Metadata.SevenZipManifest)
		if manifestErr != nil || persisted < 0 || persisted >= len(manifest.Entries) {
			return nil, 0, fmt.Errorf("7z materialized stream index is invalid")
		}
		fileIndex = persisted
	}

	// NS-2.2 exact PAR2 identity is bound to the materialized .strm artifact,
	// not trusted from the request. A client may omit the query (for example
	// after WebDAV discovery), but a conflicting query is rejected.
	persistedExact, hasPersistedExact, exactErr := nzbPersistedFileIndex(item, stem)
	if exactErr != nil {
		return nil, 0, exactErr
	}
	requestRaw := r.URL.Query().Get("nzb_file_index")
	if hasPersistedExact {
		if requestRaw != "" {
			requested, parseErr := strconv.Atoi(requestRaw)
			if parseErr != nil || requested != persistedExact {
				return nil, 0, fmt.Errorf("nzb_file_index does not match materialized stream")
			}
		}
		if item.SourceURI == nil || *item.SourceURI == "" {
			return nil, 0, fmt.Errorf("NZB data not found")
		}
		parsed, parseErr := nntp.ParseNZB([]byte(*item.SourceURI))
		if parseErr != nil {
			return nil, 0, fmt.Errorf("parse NZB for exact file index: %w", parseErr)
		}
		if persistedExact < 0 || persistedExact >= len(parsed.Files) {
			return nil, 0, fmt.Errorf("materialized nzb_file_index out of range")
		}
		fileIndex = persistedExact
	} else if requestRaw != "" {
		return nil, 0, fmt.Errorf("unexpected nzb_file_index")
	}
	return item, fileIndex, nil
}

func validateWebDAVPathParts(parts []string) bool {
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		if filepath.IsAbs(part) || strings.Contains(part, "/") || strings.Contains(part, "\\") {
			return false
		}
		cleaned := filepath.Clean(part)
		if cleaned != part || strings.HasPrefix(cleaned, "..") {
			return false
		}
	}
	return true
}

type webDAVPath struct {
	parts   []string
	alias   bool
	visible string
}

func parseWebDAVPath(path string) (webDAVPath, bool) {
	if path != "/dav" && !strings.HasPrefix(path, "/dav/") {
		return webDAVPath{}, false
	}
	raw := strings.Trim(strings.TrimPrefix(path, "/dav"), "/")
	if raw == "" {
		return webDAVPath{}, true
	}
	parts := strings.SplitN(raw, "/", 4)
	if parts[0] != "content" {
		if len(parts) > 3 || !validateWebDAVPathParts(parts) {
			return webDAVPath{}, false
		}
		return webDAVPath{parts: parts}, true
	}
	parts = parts[1:]
	result := webDAVPath{alias: true}
	if len(parts) == 0 {
		return result, true
	}
	switch {
	case strings.EqualFold(parts[0], "Movies"):
		parts[0], result.visible = "movies", "Movies"
	case strings.EqualFold(parts[0], "TV"):
		parts[0], result.visible = "tv", "TV"
	default:
		return webDAVPath{}, false
	}
	if len(parts) > 3 || !validateWebDAVPathParts(parts) {
		return webDAVPath{}, false
	}
	result.parts = parts
	return result, true
}

func (p webDAVPath) visibleCategory(category string) string {
	if p.alias {
		return p.visible
	}
	return category
}

func (p webDAVPath) href(parts ...string) string {
	base := "/dav"
	if p.alias {
		base = "/dav/content"
		if len(parts) > 0 {
			parts[0] = p.visible
		}
	}
	return base + "/" + strings.Join(parts, "/")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// nzbReleaseDirName returns the sanitized release directory name for an item,
// matching writeNZBStrm so WebDAV paths align with .strm paths on disk.
func nzbReleaseDirName(item *store.Item) string {
	name := item.DisplayName
	if strings.HasSuffix(strings.ToLower(name), ".nzb") {
		name = name[:len(name)-4]
	}
	return sanitizeWebDAVName(name)
}

func nzbVisibleFileSize(item *store.Item, stem string) int64 {
	if !item.IsSevenZipArchive() {
		return item.TotalSize
	}
	index, ok, err := nzbPersistedStreamIndex(item, stem)
	if err != nil || !ok {
		return 0
	}
	manifest, err := nntp.UnmarshalSevenZipManifest(*item.Metadata.SevenZipManifest)
	if err != nil || index < 0 || index >= len(manifest.Entries) {
		return 0
	}
	return manifest.Entries[index].Size
}

func nzbPersistedStreamIndex(item *store.Item, stem string) (int, bool, error) {
	if item.StrmPath == nil || *item.StrmPath == "" {
		return 0, false, nil
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(*item.StrmPath), stem+".strm"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read materialized stream pointer: %w", err)
	}
	parsed, err := url.Parse(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false, fmt.Errorf("parse materialized stream pointer: %w", err)
	}
	index, err := strconv.Atoi(filepath.Base(parsed.Path))
	if err != nil || index < 0 {
		return 0, false, fmt.Errorf("invalid materialized stream index")
	}
	return index, true, nil
}

// nzbPersistedFileIndex reads the exact parsed-NZB index from the materialized
// .strm for stem. It is the authority for NS-2.2 file selection; request query
// values are only consistency checks.
func nzbPersistedFileIndex(item *store.Item, stem string) (int, bool, error) {
	if item.StrmPath == nil || *item.StrmPath == "" {
		return 0, false, nil
	}
	path := filepath.Join(filepath.Dir(*item.StrmPath), stem+".strm")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read materialized stream identity: %w", err)
	}
	parsed, err := url.Parse(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false, fmt.Errorf("parse materialized stream identity: %w", err)
	}
	raw := parsed.Query().Get("nzb_file_index")
	if raw == "" {
		return 0, false, nil
	}
	idx, err := strconv.Atoi(raw)
	if err != nil || idx < 0 {
		return 0, false, fmt.Errorf("invalid materialized nzb_file_index")
	}
	return idx, true, nil
}

// nzbFileNames returns the episode stem names for an NZB item by reading the
// .strm filenames from the item's release directory on disk.
// Falls back to the sanitized display name for single-file or unreadable items.
func nzbFileNames(item *store.Item) []string {
	if item.StrmPath != nil && *item.StrmPath != "" {
		dir := filepath.Dir(*item.StrmPath)
		entries, err := os.ReadDir(dir)
		if err == nil {
			var names []string
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".strm") {
					names = append(names, strings.TrimSuffix(e.Name(), ".strm"))
				}
			}
			if len(names) > 0 {
				return names
			}
		}
	}
	return []string{nzbReleaseDirName(item)}
}

// sanitizeWebDAVName uses the canonical output.SanitizeName so WebDAV path
// segments always match filesystem directory names (A-B11).
func sanitizeWebDAVName(s string) string {
	return output.SanitizeName(s)
}
