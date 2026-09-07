package compat

import (
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

func strPtr(s string) *string { return &s }

// Multi-episode (and all dir-materialized) NZB items must report the release
// DIRECTORY as storage so the arr scans and imports every episode. Reporting a
// single .strm strands the rest of a pack after the first move-import.
func TestSABHistoryStorageReportsReleaseDir(t *testing.T) {
	item := &store.Item{
		PublicID:    "abc123",
		DisplayName: "Show.S01.720p.WEB-DL",
		Category:    "tv-classic",
		State:       store.StateReady,
		StrmPath:    strPtr("/data/tv-classic/Show.S01.720p.WEB-DL/Show.S01E07.720p.WEB-DL.strm"),
	}
	slot := ProjectSABHistorySlot(item, "/data", "/mnt/darkharrbor")
	want := "/mnt/darkharrbor/tv-classic/Show.S01.720p.WEB-DL"
	if slot.Storage != want || slot.Path != want {
		t.Fatalf("storage=%q path=%q, want %q", slot.Storage, slot.Path, want)
	}
}

// Legacy flat items (strm directly in the category root) keep the file path:
// reporting the category dir would make the arr scan unrelated releases.
func TestSABHistoryStorageFlatItemKeepsFilePath(t *testing.T) {
	item := &store.Item{
		PublicID:    "def456",
		DisplayName: "Show S01E01",
		Category:    "tv-classic",
		State:       store.StateReady,
		StrmPath:    strPtr("/data/tv-classic/Show S01E01.strm"),
	}
	slot := ProjectSABHistorySlot(item, "/data", "/mnt/darkharrbor")
	want := "/mnt/darkharrbor/tv-classic/Show S01E01.strm"
	if slot.Storage != want {
		t.Fatalf("storage=%q, want %q", slot.Storage, want)
	}
}
