package torbox

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// captureClient records the CreateTorrentTaskRequest passed to it.
type captureClient struct {
	Client // embed; unimplemented methods panic if called
	got    CreateTorrentTaskRequest
}

func (c *captureClient) CreateTorrentTask(_ context.Context, req CreateTorrentTaskRequest) (*CreateTaskResponse, error) {
	c.got = req
	return &CreateTaskResponse{}, nil
}

func strp(s string) *string { return &s }

// An HTTP .torrent source URI must be downloaded and uploaded as a file —
// never passed to TorBox as magnet (BOZO_TORRENT, live failure 2026-07-07).
func TestSubmitHTTPSourceUploadsTorrentFile(t *testing.T) {
	payload := []byte("d8:announce0:e") // arbitrary bencode-ish bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	cc := &captureClient{}
	a := &Adapter{name: "torbox", client: cc}
	item := &store.Item{ID: "t1", SourceType: store.SourceTypeTorrent,
		DisplayName: "Some.Pack.S01", SourceURI: strp(srv.URL + "/dl?x=1")}
	if _, err := a.Submit(context.Background(), item, provider.SubmitOptions{}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if cc.got.Magnet != "" {
		t.Fatalf("magnet must be empty for file upload, got %q", cc.got.Magnet)
	}
	if !bytes.Equal(cc.got.TorrentFile, payload) {
		t.Fatalf("torrent file bytes not passed through")
	}
}

// Fetch failure with a known infohash falls back to a bare magnet.
func TestSubmitHTTPSourceFetchFailFallsBackToMagnet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cc := &captureClient{}
	a := &Adapter{name: "torbox", client: cc}
	hash := "f42c3d52b99e935b602b3efbdc402622846617fa"
	item := &store.Item{ID: "t2", SourceType: store.SourceTypeTorrent,
		DisplayName: "Some Pack", SourceURI: strp(srv.URL), InfoHash: strp(hash)}
	if _, err := a.Submit(context.Background(), item, provider.SubmitOptions{}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if len(cc.got.TorrentFile) != 0 {
		t.Fatalf("no file expected on fetch failure")
	}
	if !strings.HasPrefix(cc.got.Magnet, "magnet:?xt=urn:btih:"+hash) {
		t.Fatalf("magnet fallback missing, got %q", cc.got.Magnet)
	}
}

// Plain magnet source URIs pass through unchanged.
func TestSubmitMagnetPassthrough(t *testing.T) {
	cc := &captureClient{}
	a := &Adapter{name: "torbox", client: cc}
	m := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x"
	item := &store.Item{ID: "t3", SourceType: store.SourceTypeTorrent,
		DisplayName: "x", SourceURI: strp(m)}
	if _, err := a.Submit(context.Background(), item, provider.SubmitOptions{}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if cc.got.Magnet != m || len(cc.got.TorrentFile) != 0 {
		t.Fatalf("magnet passthrough broken: %+v", cc.got)
	}
}
