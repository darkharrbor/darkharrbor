package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/bytesource/bytesourcetest"
)

// TestCDNByteSource_Conformance drives the debrid-CDN adapter against a real
// HTTP Range server (net/http ServeContent honours bytes= ranges with 206 +
// Content-Range exactly as the CDN does), proving byte-identity through the
// windowed fetch across multiple windows.
func TestCDNByteSource_Conformance(t *testing.T) {
	data := make([]byte, 40000)
	for i := range data {
		data[i] = byte((i*7 + 3) % 251)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	bytesourcetest.RunConformance(t, func() (bytesource.ByteSource, []byte) {
		bs, err := NewCDNByteSource("torrent|test", srv.URL, int64(len(data)), 4096, "application/octet-stream", nil, nil)
		if err != nil {
			t.Fatalf("NewCDNByteSource: %v", err)
		}
		return bs, data
	})
}
