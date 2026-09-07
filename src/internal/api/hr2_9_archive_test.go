package api

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type hr29ArchiveHandler struct {
	descriptor       httpstream.RemoteArchiveDescriptor
	progressiveCalls atomic.Int32
	archiveCalls     atomic.Int32
}

func (*hr29ArchiveHandler) Name() string { return "stremio" }
func (*hr29ArchiveHandler) Search(context.Context, httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	return nil, nil
}
func (h *hr29ArchiveHandler) Resolve(context.Context, httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	h.progressiveCalls.Add(1)
	return nil, httpstream.NewError(httpstream.ClassNoSource, "progressive resolver must not handle archive keys")
}
func (h *hr29ArchiveHandler) ResolveRemoteArchives(ctx context.Context, _ httpstream.ResolveRequest) ([]httpstream.RemoteArchiveDescriptor, error) {
	h.archiveCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []httpstream.RemoteArchiveDescriptor{h.descriptor}, nil
}

func hr29StoredZIP(t *testing.T, name string, payload []byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	zw := zip.NewWriter(&raw)
	header := &zip.FileHeader{Name: name, Method: zip.Store}
	w, err := zw.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func hr29RangeOrigin(t *testing.T, data []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var fullRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spec := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Range"), "bytes="))
		parts := strings.SplitN(spec, "-", 2)
		if len(parts) != 2 {
			fullRequests.Add(1)
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		start, startErr := strconv.ParseInt(parts[0], 10, 64)
		end, endErr := strconv.ParseInt(parts[1], 10, 64)
		if startErr != nil || endErr != nil || start < 0 || end < start || end >= int64(len(data)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	t.Cleanup(server.Close)
	return server, &fullRequests
}

func TestHR29ArchiveDispatchResolveAndPlayback(t *testing.T) {
	payload := bytes.Repeat([]byte("stored-archive-member"), 4096)
	archiveBytes := hr29StoredZIP(t, "safe/Movie.1080p.mkv", payload)
	origin, fullRequests := hr29RangeOrigin(t, archiveBytes)

	s, st := newHR31TestServer(t, rangecache.ModeNone, origin.Client())
	s.cfg.Server.BaseURL = "http://darkharrbor.invalid"
	s.cfg.Cache.StreamMinBufferSegments = 1
	mediaRoot := filepath.Join(t.TempDir(), "media")
	writer := output.NewStrmWriter(mediaRoot, filepath.Join(t.TempDir(), "staging"))
	writer.SetStreamSecret("hr29-test-secret")
	s.writer = writer

	selector := "archive:zip:file:Movie.1080p.mkv"
	handler := &hr29ArchiveHandler{descriptor: httpstream.RemoteArchiveDescriptor{
		Selector: selector, Format: httpstream.RemoteArchiveZIP, MemberHint: "safe/Movie.1080p.mkv",
		Parts: []httpstream.ResolvedFile{{Selector: "archive-part:0", URL: origin.URL}},
	}}
	registry := httpstream.NewRegistry()
	registry.Register("archive-test", 0, handler)
	s.httpHandlers = registry

	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "archive-test", Handler: "stremio",
		Kind: "movie", IDs: map[string]string{"imdb": "tt0000001"}, Selector: selector,
	}
	canonical, err := key.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := &store.Item{
		ID: "hr29-item", PublicID: "hr29-public", SourceType: store.SourceTypeHTTP,
		ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateResolving,
		SubmissionKey: "hr29-item", DisplayName: "Movie.1080p", ResolveKey: &canonical,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	s.resolveHTTPItem(context.Background(), item)

	ready, err := st.GetItemByID(context.Background(), item.ID)
	if err != nil || ready == nil || ready.State != store.StateReady || ready.FileList == nil {
		t.Fatalf("ready=%v err=%v", ready != nil && ready.State == store.StateReady, err)
	}
	files, err := httpstream.ParseFiles(*ready.FileList)
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%+v err=%v", files, err)
	}
	file := files[0]
	if file.Selector != selector || file.ArchiveSize != int64(len(archiveBytes)) || file.Size != int64(len(payload)) || !file.RangeVerified {
		t.Fatalf("persisted archive metadata=%+v", file)
	}
	if strings.Contains(*ready.FileList, origin.URL) {
		t.Fatal("upstream archive location persisted")
	}
	if ready.StrmPath == nil {
		t.Fatal("stable strm path missing")
	}
	strmBytes, err := os.ReadFile(*ready.StrmPath)
	if err != nil || strings.Contains(string(strmBytes), origin.URL) || !strings.Contains(string(strmBytes), "darkharrbor.invalid") {
		t.Fatalf("stable strm invalid: err=%v", err)
	}
	if handler.progressiveCalls.Load() != 0 {
		t.Fatalf("progressive resolve calls=%d", handler.progressiveCalls.Load())
	}

	request := httptest.NewRequest(http.MethodGet, "/stream/hr29/item", nil)
	request.SetPathValue("file_id", file.FileID)
	request.Header.Set("Range", fmt.Sprintf("bytes=0-%d", len(payload)-1))
	recorder := httptest.NewRecorder()
	s.streamHTTPItem(recorder, request, ready)
	if recorder.Code != http.StatusPartialContent || !bytes.Equal(recorder.Body.Bytes(), payload) {
		t.Fatalf("playback status=%d bytes=%d", recorder.Code, recorder.Body.Len())
	}
	if fullRequests.Load() != 0 {
		t.Fatalf("whole-archive requests=%d", fullRequests.Load())
	}

	head := httptest.NewRequest(http.MethodHead, "/stream/hr29/item", nil)
	head.SetPathValue("file_id", file.FileID)
	headRecorder := httptest.NewRecorder()
	s.streamHTTPItem(headRecorder, head, ready)
	if headRecorder.Code != http.StatusOK || headRecorder.Header().Get("Content-Length") != strconv.Itoa(len(payload)) {
		t.Fatalf("HEAD status=%d length=%q", headRecorder.Code, headRecorder.Header().Get("Content-Length"))
	}

	before416 := handler.archiveCalls.Load()
	pastEnd := httptest.NewRequest(http.MethodGet, "/stream/hr29/item", nil)
	pastEnd.SetPathValue("file_id", file.FileID)
	pastEnd.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", len(payload), len(payload)+31))
	pastRecorder := httptest.NewRecorder()
	s.streamHTTPItem(pastRecorder, pastEnd, ready)
	if pastRecorder.Code != http.StatusRequestedRangeNotSatisfiable || handler.archiveCalls.Load() != before416 {
		t.Fatalf("past-end status=%d archive_calls=%d->%d", pastRecorder.Code, before416, handler.archiveCalls.Load())
	}

	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			start := worker * 32
			req := httptest.NewRequest(http.MethodGet, "/stream/hr29/item", nil)
			req.SetPathValue("file_id", file.FileID)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+31))
			rec := httptest.NewRecorder()
			s.streamHTTPItem(rec, req, ready)
			if rec.Code == http.StatusPartialContent && bytes.Equal(rec.Body.Bytes(), payload[start:start+32]) {
				results <- true
				return
			}
			results <- false
		}(worker)
	}
	wg.Wait()
	close(results)
	concurrentOK := 0
	for ok := range results {
		if ok {
			concurrentOK++
		}
	}
	if concurrentOK != 8 {
		t.Fatalf("concurrent ranges=%d/8", concurrentOK)
	}

	changedArchive := hr29StoredZIP(t, "safe/Other.1080p.mkv", payload)
	changedOrigin, _ := hr29RangeOrigin(t, changedArchive)
	handler.descriptor.Parts[0].URL = changedOrigin.URL
	changedRequest := httptest.NewRequest(http.MethodGet, "/stream/hr29/item", nil)
	changedRequest.SetPathValue("file_id", file.FileID)
	changedRequest.Header.Set("Range", "bytes=0-31")
	changedRecorder := httptest.NewRecorder()
	s.streamHTTPItem(changedRecorder, changedRequest, ready)
	if changedRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("changed member status=%d", changedRecorder.Code)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = s.openHTTPRemoteArchive(canceled, item.ID, file.FileID, key, file.ArchiveSize, handler, handler.descriptor)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestHR29ArchivePlaybackRejectsUnsupportedFormat(t *testing.T) {
	descriptor := httpstream.RemoteArchiveDescriptor{Selector: "archive:zip:changed", Format: httpstream.RemoteArchiveRAR}
	handler := &hr29ArchiveHandler{descriptor: descriptor}
	s, _ := newHR31TestServer(t, rangecache.ModeNone, http.DefaultClient)
	key := httpstream.ResolveKey{Version: httpstream.ResolveKeyVersion, BackendID: "archive-test", Handler: "stremio", Kind: "movie", IDs: map[string]string{"imdb": "tt0000001"}, Selector: descriptor.Selector}
	_, _, err := s.openHTTPRemoteArchive(context.Background(), "item", "file", key, 100, handler, descriptor)
	if httpstream.ClassOf(err) != httpstream.ClassNoSource {
		t.Fatalf("unsupported format class=%s err=%v", httpstream.ClassOf(err), err)
	}
}
