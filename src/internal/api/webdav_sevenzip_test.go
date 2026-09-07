package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestSevenZipWebDAVUsesPersistedMemberIdentityAndSize(t *testing.T) {
	manifest := nntp.SevenZipManifest{
		Volumes: []nntp.SevenZipVolume{{
			Number: 1, NZBFileIdx: 0, Size: 100, Segments: []nntp.NZBSegment{{Number: 1, Bytes: 100}},
		}},
		Entries: []nntp.SevenZipEntry{
			{EntryIdx: 0, Filename: "z.mkv", DataOff: 10, Size: 20},
			{EntryIdx: 1, Filename: "a.mkv", DataOff: 30, Size: 40},
		},
	}
	raw, err := nntp.MarshalSevenZipManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	first := filepath.Join(dir, "z.strm")
	if err := os.WriteFile(first, []byte("http://darkharrbor.invalid/stream/item/0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.strm"), []byte("http://darkharrbor.invalid/stream/item/1"), 0o600); err != nil {
		t.Fatal(err)
	}
	item := &store.Item{StrmPath: &first, TotalSize: 60, Metadata: store.SubmissionMetadata{SevenZipManifest: &raw}}
	index, ok, err := nzbPersistedStreamIndex(item, "a")
	if err != nil || !ok || index != 1 {
		t.Fatalf("persisted index = %d %v %v", index, ok, err)
	}
	server := &Server{}
	if got := server.nzbEffectiveSize(t.Context(), item, nil, index); got != 40 {
		t.Fatalf("effective size = %d, want 40", got)
	}
	if got := nzbVisibleFileSize(item, "a"); got != 40 {
		t.Fatalf("visible size = %d, want 40", got)
	}
}
