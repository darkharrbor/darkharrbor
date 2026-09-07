package main

// TS-1.4: prewarm must target the actual video file on a multi-file
// release, not assume index 0 — a real live-gate grab caught cachedFiles[0]
// being a 52KB cover-art JPEG while the real video sat at a later index.

import (
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/torbox"
)

func TestLargestCachedFileSelectsBiggestEntry(t *testing.T) {
	files := []torbox.CachedFile{
		{FileID: "0", Name: "cover.jpg", Size: 53226},
		{FileID: "1", Name: "readme.txt", Size: 583},
		{FileID: "2", Name: "movie.mkv", Size: 5_887_437_558},
	}
	got := largestCachedFile(files)
	if got == nil || got.FileID != "2" {
		t.Fatalf("expected file_id 2 (the largest), got %+v", got)
	}
}

func TestLargestCachedFileEmptyIsNil(t *testing.T) {
	if got := largestCachedFile(nil); got != nil {
		t.Fatalf("expected nil for an empty slice, got %+v", got)
	}
}

func TestLargestCachedFileSingleEntry(t *testing.T) {
	files := []torbox.CachedFile{{FileID: "0", Name: "only.mkv", Size: 1000}}
	got := largestCachedFile(files)
	if got == nil || got.FileID != "0" {
		t.Fatalf("expected the sole entry, got %+v", got)
	}
}
