package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// The fixtures in testdata/storedrar are a genuine store-mode multi-volume RAR
// set produced by RAR 7.12 (rar a -m0 -v300000b), not hand-assembled headers.
// The rest of this package's RAR coverage builds volume headers with the same
// code style the parser expects, which cannot catch a shared misreading of the
// real format. These fixtures round-trip through real unrar to the recorded
// digest, so they pin the parser against an independent encoder.
//
// This exercises the full stored-RAR streaming path -- parse, manifest, byte
// source, seeks, cross-volume boundaries, concurrency, EOF -- and deliberately
// does NOT stand in for TS-1.3's live gate, which additionally requires real
// torrent acquisition and a process-restart replay.

const realRARPayloadSize = 1200000

func loadRealRARVolumes(t *testing.T) ([][]byte, string) {
	t.Helper()
	dir := filepath.Join("testdata", "storedrar")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".rar") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) < 3 {
		t.Fatalf("expected a multi-volume set, got %d volumes", len(names))
	}
	var volumes [][]byte
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		volumes = append(volumes, b)
	}
	digest, err := os.ReadFile(filepath.Join(dir, "media.sha256"))
	if err != nil {
		t.Fatalf("read digest: %v", err)
	}
	return volumes, strings.TrimSpace(string(digest))
}

// expectedPayload regenerates the archived payload deterministically rather
// than committing a second copy of it alongside the volumes.
func expectedPayload() []byte {
	out := make([]byte, 0, realRARPayloadSize+sha256.Size)
	h := sha256.Sum256([]byte("ts13-storedrar-seed"))
	for len(out) < realRARPayloadSize {
		h = sha256.Sum256(h[:])
		out = append(out, h[:]...)
	}
	return out[:realRARPayloadSize]
}

func buildRealRARSource(t *testing.T) (bytesource.ByteSource, []byte) {
	t.Helper()
	volumes, digest := loadRealRARVolumes(t)
	want := expectedPayload()
	sum := sha256.Sum256(want)
	if hex.EncodeToString(sum[:]) != digest {
		t.Fatalf("regenerated payload digest %s != recorded %s", hex.EncodeToString(sum[:]), digest)
	}

	manifest := StoredRARManifest{MemberName: "media.bin"}
	var sources []bytesource.ByteSource
	for i, vol := range volumes {
		key := fmt.Sprintf("vol%d", i)
		member, err := ParseStoredRARMember(context.Background(), bytesource.NewMemSource(key, vol))
		if err != nil {
			t.Fatalf("parse volume %d produced by real rar: %v", i+1, err)
		}
		manifest.Parts = append(manifest.Parts, StoredRARPart{
			FileID:      fmt.Sprint(i + 1),
			ArchiveSize: int64(len(vol)),
			DataOffset:  member.DataOffset,
			DataSize:    member.DataSize,
		})
		manifest.TotalSize += member.DataSize
		sources = append(sources, bytesource.NewMemSource(key, vol))
	}
	if manifest.TotalSize != realRARPayloadSize {
		t.Fatalf("manifest TotalSize = %d, want %d (parser mis-measured real volumes)",
			manifest.TotalSize, realRARPayloadSize)
	}
	src, err := NewStoredRARByteSource("torrent-rar|real", manifest, sources)
	if err != nil {
		t.Fatalf("NewStoredRARByteSource: %v", err)
	}
	return src, want
}

// TestRealStoredRARFullReadByteIdentity is the headline check: streaming the
// whole logical member out of the volume set must reproduce the archived bytes
// exactly, with no header or trailer bytes leaking into the media stream.
func TestRealStoredRARFullReadByteIdentity(t *testing.T) {
	src, want := buildRealRARSource(t)
	if src.Size() != int64(len(want)) {
		t.Fatalf("Size() = %d, want %d", src.Size(), len(want))
	}
	got := make([]byte, len(want))
	if _, err := src.ReadAt(context.Background(), got, 0); err != nil {
		t.Fatalf("full read: %v", err)
	}
	if !bytes.Equal(got, want) {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("byte mismatch at offset %d", i)
			}
		}
	}
}

// TestRealStoredRARCrossVolumeBoundaries reads across every volume seam. A
// stored member is split mid-byte-stream, so an off-by-one in the per-volume
// span arithmetic shows up here and nowhere else.
func TestRealStoredRARCrossVolumeBoundaries(t *testing.T) {
	src, want := buildRealRARSource(t)
	var boundary int64
	volumes, _ := loadRealRARVolumes(t)
	for i := 0; i < len(volumes)-1; i++ {
		member, err := ParseStoredRARMember(context.Background(),
			bytesource.NewMemSource("v", volumes[i]))
		if err != nil {
			t.Fatal(err)
		}
		boundary += member.DataSize
		for _, span := range []int64{1, 2, 64, 4096} {
			start := boundary - span
			if start < 0 {
				continue
			}
			size := span * 2
			if start+size > int64(len(want)) {
				size = int64(len(want)) - start
			}
			buf := make([]byte, size)
			if _, err := src.ReadAt(context.Background(), buf, start); err != nil {
				t.Fatalf("boundary %d span %d: %v", boundary, span, err)
			}
			if !bytes.Equal(buf, want[start:start+size]) {
				t.Fatalf("boundary %d span %d: bytes differ", boundary, span)
			}
		}
	}
}

// TestRealStoredRAREightDistributedSeeks mirrors the seek pattern the live gate
// requires, against real volumes.
func TestRealStoredRAREightDistributedSeeks(t *testing.T) {
	src, want := buildRealRARSource(t)
	total := int64(len(want))
	for i := 0; i < 8; i++ {
		off := total * int64(i) / 8
		size := int64(8192)
		if off+size > total {
			size = total - off
		}
		buf := make([]byte, size)
		if _, err := src.ReadAt(context.Background(), buf, off); err != nil {
			t.Fatalf("seek %d at %d: %v", i, off, err)
		}
		if !bytes.Equal(buf, want[off:off+size]) {
			t.Fatalf("seek %d at %d: bytes differ", i, off)
		}
	}
}

// TestRealStoredRARConcurrentReaders checks the byte source is safe under the
// concurrent range reads a player and a prewarm pass issue together.
func TestRealStoredRARConcurrentReaders(t *testing.T) {
	src, want := buildRealRARSource(t)
	total := int64(len(want))
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := (total / 16) * int64(i)
			buf := make([]byte, 32768)
			if off+int64(len(buf)) > total {
				buf = buf[:total-off]
			}
			if _, err := src.ReadAt(context.Background(), buf, off); err != nil {
				errs <- fmt.Errorf("reader %d: %w", i, err)
				return
			}
			if !bytes.Equal(buf, want[off:off+int64(len(buf))]) {
				errs <- fmt.Errorf("reader %d: bytes differ", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestRealStoredRARBeyondEOF pins the behaviour backing the live gate's 416.
func TestRealStoredRARBeyondEOF(t *testing.T) {
	src, want := buildRealRARSource(t)
	buf := make([]byte, 16)
	if _, err := src.ReadAt(context.Background(), buf, int64(len(want))+1); err == nil {
		t.Fatal("read past EOF succeeded; range handler could not emit 416")
	}
	tail := make([]byte, 32)
	if _, err := src.ReadAt(context.Background(), tail, int64(len(want))-32); err != nil {
		t.Fatalf("exact tail read: %v", err)
	}
	if !bytes.Equal(tail, want[len(want)-32:]) {
		t.Fatal("tail bytes differ")
	}
}
