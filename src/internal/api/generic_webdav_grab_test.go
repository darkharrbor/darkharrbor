package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/httpstream/generic"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestGenericWebDAVResolvePersistsStableStrmAndPlaysAfterHandlerRestart(t *testing.T) {
	media := bytes.Repeat([]byte("generic-webdav-fixture"), 4096)
	var listing atomic.Value
	listing.Store(webDAVListing(webDAVEntry("movie.mkv", len(media))))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			if r.Header.Get("Depth") != "1" {
				t.Errorf("Depth=%q, want 1", r.Header.Get("Depth"))
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(listing.Load().(string)))
			return
		}
		if r.URL.Path != "/library/movie.mkv" {
			http.NotFound(w, r)
			return
		}
		start, end := requestedFixtureRange(r.Header.Get("Range"), int64(len(media)))
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(media)))
		w.Header().Set("ETag", `"generic-webdav-v1"`)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(media[start : end+1])
	}))
	defer origin.Close()

	descriptorDir := t.TempDir()
	if err := os.Chmod(descriptorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	descriptorPath := filepath.Join(descriptorDir, "descriptors.json")
	descriptor := fmt.Sprintf(`[{"source_id":"webdav-movie","kind":"webdav","title":"WebDAV Movie","year":2026,"url":%q}]`, origin.URL+"/library/")
	if err := os.WriteFile(descriptorPath, []byte(descriptor), 0o600); err != nil {
		t.Fatal(err)
	}

	s, st := newHR31TestServer(t, rangecache.ModeNone, origin.Client())
	s.cfg.Server.BaseURL = "http://darkharrbor.invalid"
	mediaRoot := filepath.Join(t.TempDir(), "media")
	writer := output.NewStrmWriter(mediaRoot, filepath.Join(t.TempDir(), "staging"))
	writer.SetStreamSecret("generic-webdav-test-secret")
	s.writer = writer

	newHandler := func() *generic.Handler { return generic.New("cloud", descriptorPath, origin.Client()) }
	handler := newHandler()
	registry := httpstream.NewRegistry()
	registry.Register("cloud", 0, handler)
	s.httpHandlers = registry
	results, err := handler.Search(context.Background(), httpstream.StreamQuery{Title: "WebDAV Movie", Year: 2026})
	if err != nil || len(results) != 1 {
		t.Fatalf("search results=%d err=%v", len(results), err)
	}
	canonical, err := results[0].Key.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := &store.Item{
		ID: "generic-webdav-item", PublicID: "generic-webdav-public", SourceType: store.SourceTypeHTTP,
		ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateResolving,
		SubmissionKey: "generic-webdav-item", DisplayName: results[0].Title, ResolveKey: &canonical,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	s.resolveHTTPItem(context.Background(), item)

	ready, err := st.GetItemByID(context.Background(), item.ID)
	if err != nil || ready == nil || ready.State != store.StateReady || ready.FileList == nil || ready.StrmPath == nil {
		t.Fatalf("ready=%v err=%v", ready != nil && ready.State == store.StateReady, err)
	}
	files, err := httpstream.ParseFiles(*ready.FileList)
	if err != nil || len(files) != 1 || !files[0].RangeVerified || files[0].Selector != results[0].Key.Selector {
		t.Fatalf("persisted files=%+v err=%v", files, err)
	}
	if strings.Contains(*ready.FileList, origin.URL) {
		t.Fatal("upstream WebDAV URL persisted in item file list")
	}
	strm, err := os.ReadFile(*ready.StrmPath)
	if err != nil || strings.Contains(string(strm), origin.URL) ||
		!strings.Contains(string(strm), "http://darkharrbor.invalid/stream/"+item.ID+"/"+files[0].FileID+"?tok=") {
		t.Fatalf("stable strm invalid: body=%q err=%v", strm, err)
	}

	listing.Store(webDAVListing(webDAVEntry("000-new.mkv", 1234), webDAVEntry("movie.mkv", len(media))))
	restartedRegistry := httpstream.NewRegistry()
	restartedRegistry.Register("cloud", 0, newHandler())
	s.httpHandlers = restartedRegistry
	req := httptest.NewRequest(http.MethodGet, "/stream/"+item.ID+"/"+files[0].FileID, nil)
	req.SetPathValue("file_id", files[0].FileID)
	req.Header.Set("Range", "bytes=4097-4873")
	rec := httptest.NewRecorder()
	s.streamHTTPItem(rec, req, ready)
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), media[4097:4874]) {
		t.Fatalf("playback status=%d bytes=%d", rec.Code, rec.Body.Len())
	}
}

func webDAVListing(entries ...string) string {
	return `<D:multistatus xmlns:D="DAV:">` + strings.Join(entries, "") + `</D:multistatus>`
}

func webDAVEntry(name string, size int) string {
	return fmt.Sprintf(`<D:response><D:href>/library/%s</D:href><D:propstat><D:prop><D:getcontentlength>%d</D:getcontentlength></D:prop></D:propstat></D:response>`, name, size)
}

func requestedFixtureRange(raw string, total int64) (int64, int64) {
	parts := strings.SplitN(strings.TrimPrefix(raw, "bytes="), "-", 2)
	start, _ := strconv.ParseInt(parts[0], 10, 64)
	end := total - 1
	if len(parts) == 2 && parts[1] != "" {
		end, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	if end >= total {
		end = total - 1
	}
	return start, end
}
