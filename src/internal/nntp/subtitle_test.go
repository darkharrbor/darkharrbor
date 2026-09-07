package nntp

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestReadDirectSubtitlesUsesBoundedCancellableNNTPPath(t *testing.T) {
	server := startFakeArticleServer(t, map[string]string{"subtitle": "222 0 <subtitle> body follows"})
	host, rawPort, err := net.SplitHostPort(server.address())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	provider := New(Config{Host: host, Port: port, Connections: 1, SkipCRC: true}, testLogger())
	t.Cleanup(func() { provider.Close() })
	nzb := NZB{Files: []NZBFile{{
		Subject: `"Show.S01E01.en.srt" yEnc`, TotalBytes: 3,
		Segments: []NZBSegment{{Number: 1, Bytes: 3, MessageID: "subtitle"}},
	}}}
	targets := []SubtitleTarget{{Filename: `"Show.S01E01.mkv" yEnc`, FileID: "nzb-0"}}
	payloads, err := provider.ReadDirectSubtitles(context.Background(), "item", nzb, targets, 8, 1)
	if err != nil || len(payloads) != 1 || payloads[0].Filename != "Show.S01E01.en.srt" || payloads[0].FileID != "nzb-0" || string(payloads[0].Bytes) != "ABC" {
		t.Fatalf("payloads = %+v, err=%v", payloads, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.ReadDirectSubtitles(ctx, "item", nzb, targets, 8, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestSubtitleSubjectFilenameAndTargetMatch(t *testing.T) {
	t.Parallel()
	name, ok := subtitleSubjectFilename(`[3/9] &quot;Show.S01E02.en.srt&quot; yEnc (1/1)`)
	if !ok || name != "Show.S01E02.en.srt" {
		t.Fatalf("filename = %q, %v", name, ok)
	}
	targets := []SubtitleTarget{
		{Filename: "Show.S01E01.mkv", FileID: "nzb-0"},
		{Filename: "Show.S01E02.mkv", FileID: "nzb-1"},
	}
	if fileID, ok := matchSubtitleTarget(name, targets); !ok || fileID != "nzb-1" {
		t.Fatalf("target = %q, %v", fileID, ok)
	}
	if _, ok := subtitleSubjectFilename(`"../escape.srt" yEnc`); ok {
		t.Fatal("traversal subject accepted")
	}
	if _, ok := matchSubtitleTarget("unrelated.srt", targets); ok {
		t.Fatal("unmatched season-pack subtitle accepted")
	}
}

func FuzzSubtitleSubjectFilename(f *testing.F) {
	f.Add(`"Show.S01E01.en.srt" yEnc`)
	f.Add("../escape.ass")
	f.Add("show.mkv")
	f.Fuzz(func(t *testing.T, subject string) {
		name, ok := subtitleSubjectFilename(subject)
		if ok && (name == "" || strings.ContainsAny(name, "\\/\r\n\x00")) {
			t.Fatalf("unsafe accepted filename %q", name)
		}
	})
}
