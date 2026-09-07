package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func newTestStoreForRelay(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	db, err := store.Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	return store.New(db)
}

// TestHandleProbeRelayServesFromMediaTruth proves SF-04's core behavior: a
// probe for a file whose media facts were already persisted is answered
// without ever invoking ffprobe-full. The configured FFprobeFullPath points
// at a script that would fail the test if executed, so a pass here is only
// possible via the media-truth short-circuit.
func TestHandleProbeRelayServesFromMediaTruth(t *testing.T) {
	st := newTestStoreForRelay(t)
	ctx := context.Background()

	at := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID: "item1", PublicID: "pub1", SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateReady, SubmissionKey: "sub1", DisplayName: "movie",
		CreatedAt: at, UpdatedAt: at,
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	facts := mediatruth.Facts{
		Container: mediatruth.ContainerMatroska, DurationMS: 7_200_000,
		VideoCodec: "h264", Width: 1920, Height: 1080, AudioCodec: "aac",
	}
	if err := st.SetMediaFacts(ctx, item.ID, "0", "torrent|item1|0", facts); err != nil {
		t.Fatal(err)
	}

	fake := writeFakeFFprobe(t, "", 1, 0) // would fail the test if ever exec'd
	s := testRelayServer(t, fake, 4)
	s.store = st

	rec := doProbeRequest(s, relayRequest{
		URL:  "http://darkharrbor:8381/stream/item1/0?tok=xyz",
		Args: []string{"-v", "quiet", "-print_format", "json"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp relayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", resp.ExitCode)
	}
	var out struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal([]byte(resp.Stdout), &out); err != nil {
		t.Fatalf("stdout not valid ffprobe-shaped JSON: %v (%s)", err, resp.Stdout)
	}
	var sawVideo bool
	for _, str := range out.Streams {
		if str.CodecType == "video" && str.CodecName == "h264" {
			sawVideo = true
		}
	}
	if !sawVideo {
		t.Fatalf("expected a video/h264 stream in synthesized output: %s", resp.Stdout)
	}

	// The positive probe cache should now be populated from the media-truth
	// path, exactly as if a live probe had run -- a repeat request must hit
	// the pre-existing cache path unchanged.
	cached, found, err := st.GetFileProbe(ctx, item.ID, "0", relayArgsKey([]string{"-v", "quiet", "-print_format", "json"}))
	if err != nil || !found || cached != resp.Stdout {
		t.Fatalf("expected media-truth result to populate the positive probe cache: found=%v err=%v cached=%q", found, err, cached)
	}
}

// TestHandleProbeRelayFileIDMismatchFallsThroughToLiveProbe proves the
// exact-file binding: facts persisted for one file must never answer a
// probe for a different file on the same item.
func TestHandleProbeRelayFileIDMismatchFallsThroughToLiveProbe(t *testing.T) {
	st := newTestStoreForRelay(t)
	ctx := context.Background()

	at := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID: "item2", PublicID: "pub2", SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateReady, SubmissionKey: "sub2", DisplayName: "movie2",
		CreatedAt: at, UpdatedAt: at,
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	// Facts persisted for file "0" (e.g. a cover-art extra), request is for
	// file "1" (the actual video) -- must NOT be answered from these facts.
	facts := mediatruth.Facts{Container: mediatruth.ContainerMP4, VideoCodec: "mjpeg", Width: 300, Height: 200}
	if err := st.SetMediaFacts(ctx, item.ID, "0", "torrent|item2|0", facts); err != nil {
		t.Fatal(err)
	}

	fake := writeFakeFFprobe(t, `{"streams":[{"codec_type":"video","codec_name":"hevc"}]}`, 0, 0)
	s := testRelayServer(t, fake, 4)
	s.store = st

	rec := doProbeRequest(s, relayRequest{
		URL:  "http://darkharrbor:8381/stream/item2/1?tok=xyz",
		Args: []string{"-v", "quiet"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp relayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Stdout != `{"streams":[{"codec_type":"video","codec_name":"hevc"}]}` {
		t.Fatalf("expected the live-probe fixture output, got %q (media facts for a different file must not answer this request)", resp.Stdout)
	}
}

// TestHandleProbeRelayNoFactsFallsThroughToLiveProbe proves the default
// (unwired-yet) case is unaffected: an item with no persisted media facts
// behaves exactly as before this row.
func TestHandleProbeRelayNoFactsFallsThroughToLiveProbe(t *testing.T) {
	st := newTestStoreForRelay(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID: "item3", PublicID: "pub3", SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateReady, SubmissionKey: "sub3", DisplayName: "movie3",
		CreatedAt: at, UpdatedAt: at,
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}

	fake := writeFakeFFprobe(t, `{"streams":[]}`, 0, 0)
	s := testRelayServer(t, fake, 4)
	s.store = st

	rec := doProbeRequest(s, relayRequest{
		URL:  "http://darkharrbor:8381/stream/item3/0?tok=xyz",
		Args: []string{"-v", "quiet"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp relayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Stdout != `{"streams":[]}` {
		t.Fatalf("expected live-probe fixture output when no facts persisted, got %q", resp.Stdout)
	}
}
