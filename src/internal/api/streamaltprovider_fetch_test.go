package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

// always403 answers every request with 403 Forbidden -- a refresh-worthy
// status (cdnRefreshStatus) that lets rungs 2/3 exhaust deterministically.
func always403(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func always206(t *testing.T, body string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestCDNChunkSourceRung4AdoptsAlternateAndStaysAdopted(t *testing.T) {
	primary, primaryHits := always403(t)
	dead, _ := always403(t) // rung 3's reresolve target: also dead
	alt, altHits := always206(t, "ALT!")

	p := &altFakeProvider{
		name:       "rd",
		submitResp: &provider.CreateTaskResponse{RemoteID: "rid"},
		pollStatus: &provider.TaskStatus{
			DownloadReady: true,
			Files:         []provider.RemoteFile{{FileID: "f1", Name: "movie.mkv", Size: 4}},
		},
		dlURL: alt.URL,
	}

	src := makeCDNSourceFromCache(primary.URL, 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		return dead.URL, nil
	})
	src.log = streamCDNTestLog
	src.altCandidates = []altProviderCandidate{{
		name: "rd",
		confirm: func(ctx context.Context) (cdnReResolver, error) {
			return confirmAltProvider(ctx, p, torrentItemWithHash("abc123"), "movie.mkv", 4)
		},
	}}

	ref := src.Chunks()[0]

	first, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if string(first) != "ALT!" {
		t.Fatalf("first fetch body = %q, want ALT!", first)
	}
	if p.submitCalls != 1 {
		t.Fatalf("submitCalls after first fetch = %d, want 1", p.submitCalls)
	}

	second, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if string(second) != "ALT!" {
		t.Fatalf("second fetch body = %q, want ALT! (adopted alternate must serve the rest of the stream)", second)
	}
	if got := atomic.LoadInt32(primaryHits); got != 1 {
		t.Fatalf("primary origin hit %d times, want exactly 1 (never retried after adoption)", got)
	}
	if got := atomic.LoadInt32(altHits); got != 2 {
		t.Fatalf("alt origin hit %d times, want 2 (one per fetch, after adoption)", got)
	}
	if p.submitCalls != 1 {
		t.Fatalf("submitCalls after second fetch = %d, want 1 (bounded: confirm never re-runs once adopted)", p.submitCalls)
	}
}

func TestCDNChunkSourceRung4RecoversFromPrimaryTransportFailure(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	primaryURL := primary.URL
	primary.Close() // a fast connection refusal has status 0, not an HTTP status
	alt, altHits := always206(t, "ALT!")

	var reresolveCalls, confirmCalls int32
	src := makeCDNSourceFromCache(primaryURL, 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		atomic.AddInt32(&reresolveCalls, 1)
		return "", errors.New("primary provider unreachable")
	})
	src.log = streamCDNTestLog
	src.altCandidates = []altProviderCandidate{{
		name: "rd",
		confirm: func(context.Context) (cdnReResolver, error) {
			atomic.AddInt32(&confirmCalls, 1)
			return func(context.Context) (string, error) { return alt.URL, nil }, nil
		},
	}}

	data, err := src.Fetch(context.Background(), src.Chunks()[0], rangecache.FetchDemand)
	if err != nil || string(data) != "ALT!" {
		t.Fatalf("data=%q err=%v, want alternate bytes after primary transport failure", data, err)
	}
	if reresolveCalls != 1 || confirmCalls != 1 || atomic.LoadInt32(altHits) != 1 {
		t.Fatalf("reresolve=%d confirm=%d alt_hits=%d, want 1/1/1", reresolveCalls, confirmCalls, atomic.LoadInt32(altHits))
	}
}

func TestCDNChunkSourceTransportFailureCancellationStopsRecovery(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	primaryURL := primary.URL
	primary.Close()

	var reresolveCalls, confirmCalls int32
	src := makeCDNSourceFromCache(primaryURL, 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		atomic.AddInt32(&reresolveCalls, 1)
		return "", errors.New("must not run")
	})
	src.log = streamCDNTestLog
	src.altCandidates = []altProviderCandidate{{
		name: "rd",
		confirm: func(context.Context) (cdnReResolver, error) {
			atomic.AddInt32(&confirmCalls, 1)
			return nil, errors.New("must not run")
		},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Fetch(ctx, src.Chunks()[0], rangecache.FetchDemand); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if reresolveCalls != 0 || confirmCalls != 0 {
		t.Fatalf("recovery ran after cancellation: reresolve=%d confirm=%d", reresolveCalls, confirmCalls)
	}
}

func TestCDNChunkSourceRung4ConfirmFailureIsCachedAcrossWindows(t *testing.T) {
	primary, _ := always403(t)
	dead, _ := always403(t)
	altDead, altHits := always403(t) // resolved alt CDN URL also fails

	p := &altFakeProvider{
		name:       "rd",
		submitResp: &provider.CreateTaskResponse{RemoteID: "rid"},
		pollStatus: &provider.TaskStatus{
			DownloadReady: true,
			Files:         []provider.RemoteFile{{FileID: "f1", Name: "movie.mkv", Size: 4}},
		},
		dlURL: altDead.URL,
	}

	src := makeCDNSourceFromCache(primary.URL, 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		return dead.URL, nil
	})
	src.log = streamCDNTestLog
	src.altCandidates = []altProviderCandidate{{
		name: "rd",
		confirm: func(ctx context.Context) (cdnReResolver, error) {
			return confirmAltProvider(ctx, p, torrentItemWithHash("abc123"), "movie.mkv", 4)
		},
	}}

	ref := src.Chunks()[0]

	if _, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand); err == nil {
		t.Fatal("fetch should fail: primary dead, alt CDN also dead")
	}
	if _, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand); err == nil {
		t.Fatal("second fetch should also fail")
	}
	if p.submitCalls != 1 {
		t.Fatalf("submitCalls = %d, want exactly 1 -- a successful confirm's Submit/Poll round trip is bounded and cached, never re-run per window", p.submitCalls)
	}
	// The confirmed resolver itself (RequestDownloadURL) is deliberately NOT
	// bounded to one call: a debrid CDN URL is short-lived, exactly like
	// rung 3's own reresolve target, so each window must ask for a fresh
	// one from the already-confirmed candidate.
	if got := atomic.LoadInt32(altHits); got != 2 {
		t.Fatalf("alt CDN origin hit %d times, want 2 (one fresh resolve per fetch, confirm itself still bounded to 1)", got)
	}
}

func TestCDNChunkSourceRung4NoOpWhenNoCandidates(t *testing.T) {
	primary, _ := always403(t)
	dead, _ := always403(t)
	src := makeCDNSourceFromCache(primary.URL, 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		return dead.URL, nil
	})
	src.log = streamCDNTestLog
	// altCandidates left nil -- must behave exactly like pre-TS-2.2 Fetch.
	ref := src.Chunks()[0]
	if _, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand); err == nil {
		t.Fatal("with no rung 4 candidates, exhausted rungs 2/3 must still fail")
	}
}

func TestCDNChunkSourceRung4UsesGovernorAndReleasesLease(t *testing.T) {
	primary, _ := always403(t)
	dead, _ := always403(t)
	alt, _ := always206(t, "ALT!")

	p := &altFakeProvider{
		name:       "rd",
		submitResp: &provider.CreateTaskResponse{RemoteID: "rid"},
		pollStatus: &provider.TaskStatus{
			DownloadReady: true,
			Files:         []provider.RemoteFile{{FileID: "f1", Name: "movie.mkv", Size: 4}},
		},
		dlURL: alt.URL,
	}

	gov := accountgov.New("test-torrent-cdn-rung4")
	gov.SetCapacity(TorrentCDNGovOp, 1)

	src := makeCDNSourceFromCache(primary.URL, 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		return dead.URL, nil
	})
	src.log = streamCDNTestLog
	src.gov = gov
	src.govOp = TorrentCDNGovOp
	src.priority = accountgov.PriorityPlayback
	src.session = "item-1"
	src.altCandidates = []altProviderCandidate{{
		name: "rd",
		confirm: func(ctx context.Context) (cdnReResolver, error) {
			return confirmAltProvider(ctx, p, torrentItemWithHash("abc123"), "movie.mkv", 4)
		},
	}}

	ref := src.Chunks()[0]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := src.Fetch(ctx, ref, rangecache.FetchDemand)
	if err != nil || string(data) != "ALT!" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if gov.InUse(TorrentCDNGovOp) != 0 {
		t.Fatalf("governor lease leaked after rung 4 success: in_use=%d, want 0", gov.InUse(TorrentCDNGovOp))
	}
}

func TestCDNChunkSourceRung4LogsNeverLeakUpstreamURL(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	primary, _ := always403(t)
	dead, _ := always403(t)
	alt, _ := always206(t, "ALT!")

	p := &altFakeProvider{
		name:       "rd",
		submitResp: &provider.CreateTaskResponse{RemoteID: "rid"},
		pollStatus: &provider.TaskStatus{
			DownloadReady: true,
			Files:         []provider.RemoteFile{{FileID: "f1", Name: "movie.mkv", Size: 4}},
		},
		dlURL: alt.URL + "?tok=must-not-appear",
	}

	src := makeCDNSourceFromCache(primary.URL, 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		return dead.URL, nil
	})
	src.log = log
	src.altCandidates = []altProviderCandidate{{
		name: "rd",
		confirm: func(ctx context.Context) (cdnReResolver, error) {
			return confirmAltProvider(ctx, p, torrentItemWithHash("abc123"), "movie.mkv", 4)
		},
	}}

	ref := src.Chunks()[0]
	if _, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand); err != nil {
		t.Fatal(err)
	}
	got := logs.String()
	if strings.Contains(got, "must-not-appear") || strings.Contains(got, "tok=") ||
		strings.Contains(got, primary.URL) || strings.Contains(got, alt.URL) {
		t.Fatalf("rung 4 log leaked an upstream URL/token: %s", got)
	}
}
