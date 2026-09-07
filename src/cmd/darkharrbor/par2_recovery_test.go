package main

import (
	"os"
	"strings"
	"testing"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestApplyPAR2FileMatchesRecoversDirectVideoAtExactIndex(t *testing.T) {
	parsed := nntp.NZB{Files: []nntp.NZBFile{
		{Subject: `"large-recovery-volume"`, TotalBytes: 2_000},
		{Subject: `"obfuscated-video"`, TotalBytes: 1_020},
		{Subject: `"metadata"`, TotalBytes: 100},
	}}
	info := archiveparser.PAR2Info{Files: []archiveparser.PAR2FileDesc{{
		FileID: "v", Hash16K: "aa", Name: "Show.S01E01.Real.Name.mkv", Length: 1_000,
	}}}
	matches := []nntp.PAR2FileMatch{{NZBFileIdx: 1, File: parsed.Files[1], Desc: info.Files[0]}}

	enriched, exact, videos, rarParts := applyPAR2FileMatches(parsed, info, matches)
	if videos != 1 || rarParts != 0 || !exact[1] {
		t.Fatalf("videos=%d rar=%d exact=%v", videos, rarParts, exact)
	}
	if enriched.Files[1].Subject != `"Show.S01E01.Real.Name.mkv"` {
		t.Fatalf("subject = %q", enriched.Files[1].Subject)
	}
	if parsed.Files[1].Subject != `"obfuscated-video"` {
		t.Fatal("original parsed NZB was mutated")
	}
}

func TestApplyPAR2FileMatchesRequiresCompleteRARSet(t *testing.T) {
	parsed := nntp.NZB{Files: []nntp.NZBFile{
		{Subject: `"part-a"`, TotalBytes: 1_000},
		{Subject: `"part-b"`, TotalBytes: 1_000},
	}}
	info := archiveparser.PAR2Info{Files: []archiveparser.PAR2FileDesc{
		{FileID: "a", Name: "release.part01.rar", Length: 1_000},
		{FileID: "b", Name: "release.part02.rar", Length: 1_000},
	}}

	partial := []nntp.PAR2FileMatch{{NZBFileIdx: 0, File: parsed.Files[0], Desc: info.Files[0]}}
	enriched, _, _, rarParts := applyPAR2FileMatches(parsed, info, partial)
	if rarParts != 0 || enriched.Files[0].Subject != parsed.Files[0].Subject {
		t.Fatalf("partial RAR set was applied: %+v", enriched.Files)
	}

	complete := append(partial, nntp.PAR2FileMatch{NZBFileIdx: 1, File: parsed.Files[1], Desc: info.Files[1]})
	enriched, _, _, rarParts = applyPAR2FileMatches(parsed, info, complete)
	if rarParts != 2 || enriched.Files[0].Subject != `"release.part01.rar"` || enriched.Files[1].Subject != `"release.part02.rar"` {
		t.Fatalf("complete RAR set not applied: count=%d files=%+v", rarParts, enriched.Files)
	}
}

func TestWriteNZBStrmExactIndexKeepsRecoveredNameAndIdentity(t *testing.T) {
	root := t.TempDir()
	item := &store.Item{ID: "item", Category: "tv", DisplayName: "Fallback.S01E01.nzb"}
	path, url, err := writeNZBStrmExactNZBFile(item, "http://darkharrbor:8381", root, 3, `"Recovered.Show.S01E01.mkv"`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "/Recovered.Show.S01E01.mkv?nzb_file_index=3") {
		t.Fatalf("url = %q", url)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != url {
		t.Fatalf("strm payload = %q, want %q", data, url)
	}
}

func TestWriteNZBStrmOrdinaryURLHasNoExactIndex(t *testing.T) {
	root := t.TempDir()
	item := &store.Item{ID: "item", Category: "tv", DisplayName: "Show.S01E01.nzb"}
	_, url, err := writeNZBStrm(item, "http://darkharrbor:8381", root, 0, `"Show.S01E01.mkv"`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(url, "nzb_file_index") {
		t.Fatalf("ordinary URL unexpectedly changed: %q", url)
	}
}
