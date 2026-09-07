package torbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TS-0.2 (T1): deterministic gates for the best-effort provider metainfo
// fetch. exportdata is documented by TorBox as incompatible with
// already-cached downloads, and can also answer with maintenance/challenge
// pages -- every one of these must be a plain error the caller treats as
// absence, never a panic or a silently-accepted malformed parse input.

func newTestExportClient(rt roundTripFunc) *HTTPClient {
	c := NewHTTPClient(slog.New(slog.NewTextHandler(io.Discard, nil)), "https://torbox.invalid", "secret-token", "test", time.Second, nil)
	c.httpClient.Transport = rt
	return c
}

func staticResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestExportTorrentFileSuccess(t *testing.T) {
	want := []byte("d4:infod6:lengthi1e4:name1:a12:piece lengthi1e6:pieces20:AAAAAAAAAAAAAAAAAAAAee")
	c := newTestExportClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.Path, "exportdata") {
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		if got := req.URL.Query().Get("type"); got != "file" {
			t.Fatalf("type=%q, want file", got)
		}
		if got := req.URL.Query().Get("torrent_id"); got != "123" {
			t.Fatalf("torrent_id=%q, want 123", got)
		}
		return staticResponse(http.StatusOK, want), nil
	}))

	got, err := c.ExportTorrentFile(context.Background(), "123")
	if err != nil {
		t.Fatalf("ExportTorrentFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExportTorrentFileNon2xxIsError(t *testing.T) {
	c := newTestExportClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return staticResponse(http.StatusNotFound, []byte(`{"error":"not found"}`)), nil
	}))
	if _, err := c.ExportTorrentFile(context.Background(), "123"); err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestExportTorrentFileServerErrorIsRetryable(t *testing.T) {
	c := newTestExportClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return staticResponse(http.StatusInternalServerError, []byte("maintenance")), nil
	}))
	_, err := c.ExportTorrentFile(context.Background(), "123")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !IsRetryable(err) {
		t.Fatalf("expected retryable error, got %v", err)
	}
}

// TorBox's documented "incompatible with already-cached downloads" rejection,
// plus maintenance/Cloudflare pages, can surface as a 200 with a non-bencode
// body. This must not be handed to torrentmeta.Parse -- it must fail here.
func TestExportTorrentFileNonBencode200IsError(t *testing.T) {
	c := newTestExportClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return staticResponse(http.StatusOK, []byte(`{"success":false,"detail":"export not available for cached downloads"}`)), nil
	}))
	if _, err := c.ExportTorrentFile(context.Background(), "123"); err == nil {
		t.Fatal("expected error for non-bencode 200 response")
	}
}

func TestExportTorrentFileEmptyBodyIsError(t *testing.T) {
	c := newTestExportClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return staticResponse(http.StatusOK, nil), nil
	}))
	if _, err := c.ExportTorrentFile(context.Background(), "123"); err == nil {
		t.Fatal("expected error for empty response")
	}
}

func TestExportTorrentFileOversizedIsError(t *testing.T) {
	big := bytes.Repeat([]byte("d"), maxTorrentFileBytes+16)
	c := newTestExportClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return staticResponse(http.StatusOK, big), nil
	}))
	if _, err := c.ExportTorrentFile(context.Background(), "123"); err == nil {
		t.Fatal("expected error for oversized response")
	}
}

func TestExportTorrentFileTransportErrorRedacted(t *testing.T) {
	var logs bytes.Buffer
	c := NewHTTPClient(slog.New(slog.NewTextHandler(&logs, nil)), "https://torbox.invalid", "secret-token", "test", time.Second, nil)
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial https://torbox.invalid/api/torrents/exportdata?torrent_id=123")
	})
	_, err := c.ExportTorrentFile(context.Background(), "123")
	if err == nil {
		t.Fatal("expected transport error")
	}
	for _, got := range []string{logs.String(), err.Error()} {
		if strings.Contains(got, "secret-token") {
			t.Fatalf("leaked secret token: %s", got)
		}
	}
}
