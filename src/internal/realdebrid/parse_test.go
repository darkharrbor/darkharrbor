package realdebrid

import (
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

func TestParseInstantAvailability_Cached(t *testing.T) {
	body := []byte(`{
		"abc123def456abc123def456abc123def456abc123": {
			"rd": [
				{
					"1": {"filename": "Frasier.S02E05.mkv", "filesize": 1073741824},
					"2": {"filename": "Frasier.S02E05.srt", "filesize": 4096}
				}
			]
		}
	}`)
	hashes := []string{"abc123def456abc123def456abc123def456abc123"}
	result, err := parseInstantAvailability(body, hashes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	groups, ok := result["abc123def456abc123def456abc123def456abc123"]
	if !ok {
		t.Fatal("expected hash to be cached")
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0].Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(groups[0].Files))
	}
}

func TestParseInstantAvailability_Miss(t *testing.T) {
	body := []byte(`{}`)
	result, err := parseInstantAvailability(body, []string{"deadbeef00000000deadbeef00000000deadbeef"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty result for cache miss, got %d entries", len(result))
	}
}

func TestParseInstantAvailability_CaseInsensitive(t *testing.T) {
	// RD returns hashes lowercase; DH may pass uppercase
	body := []byte(`{
		"aabbccdd11223344aabbccdd11223344aabbccdd": {
			"rd": [{"1": {"filename": "test.mkv", "filesize": 100}}]
		}
	}`)
	hashes := []string{"AABBCCDD11223344AABBCCDD11223344AABBCCDD"}
	result, err := parseInstantAvailability(body, hashes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := result["aabbccdd11223344aabbccdd11223344aabbccdd"]; !ok {
		t.Fatal("expected case-insensitive match")
	}
}

func TestParseTorrentInfo_Downloaded(t *testing.T) {
	body := []byte(`{
		"id": "RD123",
		"filename": "Frasier.S02",
		"status": "downloaded",
		"progress": 100,
		"files": [
			{"id": 1, "path": "/Frasier.S02E05.mkv", "bytes": 1073741824, "selected": 1}
		],
		"links": ["https://real-debrid.com/dl/abc/Frasier.S02E05.mkv"]
	}`)
	info, err := parseTorrentInfo(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Status != "downloaded" {
		t.Errorf("expected downloaded, got %s", info.Status)
	}
	if len(info.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(info.Files))
	}
	if len(info.Links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(info.Links))
	}
}

func TestTorrentInfoToTaskStatus_TerminalStates(t *testing.T) {
	cases := []struct {
		status  string
		failed  bool
		ready   bool
		outcome provider.TorrentOutcome
		code    provider.TorrentFailureCode
	}{
		{"downloaded", false, true, provider.TorrentOutcomeReady, provider.TorrentFailureNone},
		{"downloading", false, false, provider.TorrentOutcomeUnknown, provider.TorrentFailureNone},
		{"magnet_error", true, false, provider.TorrentOutcomeTerminalDeadSource, provider.TorrentFailureTerminalDeadSource},
		{"error", false, false, provider.TorrentOutcomeTransientError, provider.TorrentFailureProviderUnavailable},
		{"virus", true, false, provider.TorrentOutcomeTerminalDeadSource, provider.TorrentFailureTerminalDeadSource},
		{"dead", true, false, provider.TorrentOutcomeTerminalDeadSource, provider.TorrentFailureTerminalDeadSource},
		{"queued", false, false, provider.TorrentOutcomeUnknown, provider.TorrentFailureNone},
	}
	for _, tc := range cases {
		info := &TorrentInfo{ID: "x", Status: tc.status, Progress: 50}
		ts := torrentInfoToTaskStatus(info)
		if ts.Failed != tc.failed {
			t.Errorf("status=%s: Failed=%v want %v", tc.status, ts.Failed, tc.failed)
		}
		if ts.DownloadReady != tc.ready {
			t.Errorf("status=%s: DownloadReady=%v want %v", tc.status, ts.DownloadReady, tc.ready)
		}
		if ts.Outcome != tc.outcome {
			t.Errorf("status=%s: Outcome=%q want %q", tc.status, ts.Outcome, tc.outcome)
		}
		if ts.FailureCode != tc.code {
			t.Errorf("status=%s: FailureCode=%q want %q", tc.status, ts.FailureCode, tc.code)
		}
	}
}

func TestTorrentInfoToTaskStatus_ProgressNormalized(t *testing.T) {
	// RD gives 0-100; DH expects 0-1
	info := &TorrentInfo{ID: "x", Status: "downloading", Progress: 75}
	ts := torrentInfoToTaskStatus(info)
	if ts.Progress != 0.75 {
		t.Errorf("expected 0.75, got %f", ts.Progress)
	}
}

func TestTorrentInfoToTaskStatus_Stalled(t *testing.T) {
	// RD's "downloading" at 0% is normal startup behavior, not a stall.
	// Stalled is always false for RD — only terminal states matter.
	info := &TorrentInfo{ID: "x", Status: "downloading", Progress: 0}
	ts := torrentInfoToTaskStatus(info)
	if ts.Stalled {
		t.Error("expected Stalled=false for RD downloading+progress=0 (normal startup)")
	}
	// Confirm it's also not failed or ready
	if ts.Failed {
		t.Error("expected Failed=false for downloading state")
	}
	if ts.DownloadReady {
		t.Error("expected DownloadReady=false for downloading state")
	}
}

func TestParseUnrestrictResponse(t *testing.T) {
	body := []byte(`{"id":"x","filename":"f.mkv","mimeType":"video/x-matroska","filesize":100,"link":"https://rd.com/dl/x","host":"example","chunks":16,"crc":0,"download":"https://cdn.example.com/f.mkv","streamable":1}`)
	dl, err := parseUnrestrictResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dl != "https://cdn.example.com/f.mkv" {
		t.Errorf("unexpected download URL: %s", dl)
	}
}

func TestFindLinkForFile(t *testing.T) {
	info := &TorrentInfo{
		ID: "RD1",
		Files: []TorrentFile{
			{ID: 1, Path: "/ep1.mkv", Selected: true},
			{ID: 2, Path: "/ep2.mkv", Selected: false},
			{ID: 3, Path: "/ep3.mkv", Selected: true},
		},
		Links: []string{"https://rd.com/link1", "https://rd.com/link3"},
	}
	// File 1 is first selected → link[0]
	link, err := findLinkForFile(info, "1")
	if err != nil || link != "https://rd.com/link1" {
		t.Errorf("file 1: got %q %v", link, err)
	}
	// File 3 is second selected → link[1]
	link, err = findLinkForFile(info, "3")
	if err != nil || link != "https://rd.com/link3" {
		t.Errorf("file 3: got %q %v", link, err)
	}
	// File 2 is not selected → error
	_, err = findLinkForFile(info, "2")
	if err == nil {
		t.Error("expected error for unselected file 2")
	}
}

func TestCachedGroupToCachedFiles(t *testing.T) {
	groups := []CachedGroup{
		{Files: map[string]CachedFile{
			"1": {Filename: "show/ep1.mkv", Filesize: 500},
			"2": {Filename: "show/ep2.mkv", Filesize: 600},
		}},
	}
	files := cachedGroupToCachedFiles(groups)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
}
