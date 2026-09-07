package nntp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

func TestStreamEncryptedRARRangeAcrossVolumes(t *testing.T) {
	t.Parallel()
	names, err := filepath.Glob("../archive/testdata/ns33-rar5/archive.part*.rar")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if len(names) != 4 {
		t.Fatalf("fixture volumes = %d, want 4", len(names))
	}

	manifest := make([]RARPart, 0, len(names))
	chunks := make([][][]byte, 0, len(names))
	var logicalOffset int64
	for i, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		member, err := archiveparser.ParseStoredRARMember(context.Background(), bytesource.NewMemSource("fixture", data))
		if err != nil {
			t.Fatal(err)
		}
		cuts := []int{0, len(data) / 3, len(data) * 2 / 3, len(data)}
		partChunks := make([][]byte, 0, 3)
		segments := make([]NZBSegment, 0, 3)
		for j := 0; j < 3; j++ {
			partChunks = append(partChunks, data[cuts[j]:cuts[j+1]])
			segments = append(segments, NZBSegment{Number: j + 1, Bytes: int64(cuts[j+1] - cuts[j])})
		}
		dataBytes := min(member.PackedSize, int64(2500)-logicalOffset)
		manifest = append(manifest, RARPart{
			PartNum:     i + 1,
			NZBFileIdx:  i,
			HeaderBytes: member.DataOffset,
			DataBytes:   dataBytes,
			PackedBytes: member.PackedSize,
			CumOffset:   logicalOffset,
			Encryption:  member.Encryption,
			Segments:    segments,
		})
		logicalOffset += dataBytes
		chunks = append(chunks, partChunks)
	}

	key, err := archiveparser.DeriveRARKey(context.Background(), "ns33-fixture", manifest[0].Encryption)
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(_ context.Context, _ NZBSegment, _, _ string, fileIndex, segIndex int) ([]byte, error) {
		return chunks[fileIndex][segIndex], nil
	}
	var got bytes.Buffer
	if err := streamEncryptedRARRange(context.Background(), fetch, manifest, 701, 1803, key, &got, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 2500)
	copy(want, []byte{0x1a, 0x45, 0xdf, 0xa3})
	if !bytes.Equal(got.Bytes(), want[701:1804]) {
		t.Fatal("cross-volume seek mismatch")
	}
}
