package rangecache

// HR1.3 sparse block identity gates.

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

func blockFixture(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + 7) % 251)
	}
	return b
}

func TestByteSourceBlocksGeometryAndBytes(t *testing.T) {
	data := blockFixture(10000)
	bs := bytesource.NewMemSource("http|item|file", data)
	src, err := NewByteSourceBlocks(bs, 4096)
	if err != nil {
		t.Fatalf("NewByteSourceBlocks: %v", err)
	}
	refs := src.Chunks()
	if len(refs) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(refs))
	}
	if refs[0].Size != 4096 || refs[1].Size != 4096 || refs[2].Size != 10000-8192 {
		t.Fatalf("block sizes wrong: %d %d %d", refs[0].Size, refs[1].Size, refs[2].Size)
	}
	// Every block returns exactly its own slice of the source.
	var got []byte
	for _, ref := range refs {
		b, err := src.Fetch(context.Background(), ref, FetchDemand)
		if err != nil {
			t.Fatalf("block %d: %v", ref.Index, err)
		}
		if int64(len(b)) != ref.Size {
			t.Fatalf("block %d returned %d bytes, want %d", ref.Index, len(b), ref.Size)
		}
		got = append(got, b...)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reassembled blocks differ from the source content")
	}
	if !src.ExactSizes() {
		t.Fatal("exact-size source must report exact block sizes")
	}
}

func TestByteSourceBlocksDefaultSizeAndValidation(t *testing.T) {
	bs := bytesource.NewMemSource("http|item|file", blockFixture(64))
	src, err := NewByteSourceBlocks(bs, 0)
	if err != nil {
		t.Fatalf("default block size: %v", err)
	}
	if len(src.Chunks()) != 1 {
		t.Fatalf("expected one block under the default size, got %d", len(src.Chunks()))
	}
	if _, err := NewByteSourceBlocks(nil, 4096); err == nil {
		t.Fatal("nil source must be rejected")
	}
	if _, err := NewByteSourceBlocks(bytesource.NewMemSource("k", nil), 4096); err == nil {
		t.Fatal("zero-size source must be rejected")
	}
	if _, err := NewByteSourceBlocks(bytesource.NewMemSource("", blockFixture(8)), 4096); err == nil {
		t.Fatal("keyless source must be rejected")
	}
}

// TestByteSourceBlocksPayloadKeyIsStableAndSecretFree: the sparse identity is
// what lets two consumers of the same source share cache entries, so it must
// be deterministic and must not carry a URL or query.
func TestByteSourceBlocksPayloadKeyIsStableAndSecretFree(t *testing.T) {
	bs := bytesource.NewMemSource("http|item-1|file-1", blockFixture(9000))
	a, _ := NewByteSourceBlocks(bs, 4096)
	b, _ := NewByteSourceBlocks(bytesource.NewMemSource("http|item-1|file-1", blockFixture(9000)), 4096)

	ka := a.(PayloadKeyer)
	kb := b.(PayloadKeyer)
	for i := 0; i < 3; i++ {
		ref := ChunkRef{Index: i}
		if ka.PayloadKey(ref) != kb.PayloadKey(ref) {
			t.Fatalf("block %d: identity not stable across instances", i)
		}
		if ka.PayloadKey(ref) == ka.PayloadKey(ChunkRef{Index: i + 1}) {
			t.Fatalf("block %d: identity collides with its neighbour", i)
		}
		key := ka.PayloadKey(ref)
		for _, bad := range []string{"://", "?", "tok=", "http://", "https://"} {
			if strings.Contains(key, bad) {
				t.Fatalf("payload key leaked %q: %s", bad, key)
			}
		}
	}
	// Different block geometry is a different identity: mixing sizes must not
	// alias two different byte ranges onto one cache entry.
	diff, _ := NewByteSourceBlocks(bytesource.NewMemSource("http|item-1|file-1", blockFixture(9000)), 8192)
	if diff.(PayloadKeyer).PayloadKey(ChunkRef{Index: 1}) == ka.PayloadKey(ChunkRef{Index: 1}) {
		t.Fatal("different block sizes must not share a payload identity")
	}
}

// TestByteSourceBlocksShareCacheAndCoalesce: two independent consumers of the
// same underlying source resolve to one payload — one disk entry and, when
// simultaneous, one upstream read.
func TestByteSourceBlocksShareCacheAndCoalesce(t *testing.T) {
	data := blockFixture(8192)
	c := testCache(t)

	counted := &countingByteSource{inner: bytesource.NewMemSource("http|item|file", data), gate: make(chan struct{})}
	srcA, _ := NewByteSourceBlocks(counted, 4096)
	srcB, _ := NewByteSourceBlocks(counted, 4096)
	ref := srcA.Chunks()[0]

	var wg sync.WaitGroup
	out := make([][]byte, 2)
	errs := make([]error, 2)
	for i, src := range []ChunkSource{srcA, srcB} {
		wg.Add(1)
		go func(i int, src ChunkSource) {
			defer wg.Done()
			out[i], errs[i] = c.GetChunk(context.Background(), src, ModeDisk, ref)
		}(i, src)
	}
	waitInflight(t, c, 1)
	waitJoins(t, c, 1)
	close(counted.gate)
	wg.Wait()

	for i := range out {
		if errs[i] != nil {
			t.Fatalf("consumer %d: %v", i, errs[i])
		}
		if !bytes.Equal(out[i], data[:4096]) {
			t.Fatalf("consumer %d got wrong bytes", i)
		}
	}
	if got := counted.reads.Load(); got != 1 {
		t.Fatalf("two consumers of one source must cause one read, got %d", got)
	}
}

type countingByteSource struct {
	inner bytesource.ByteSource
	gate  chan struct{}
	reads atomic.Int64
}

func (s *countingByteSource) Size() int64                   { return s.inner.Size() }
func (s *countingByteSource) Key() string                   { return s.inner.Key() }
func (s *countingByteSource) Caps() bytesource.Capabilities { return s.inner.Caps() }
func (s *countingByteSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	s.reads.Add(1)
	select {
	case <-s.gate:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return s.inner.ReadAt(ctx, p, off)
}
