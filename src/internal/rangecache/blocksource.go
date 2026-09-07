package rangecache

// Sparse block identity over a ByteSource (HR1.3; HTTP-plan HR-D12 "sparse
// block cache").
//
// The shipped cache is chunk-addressed: an NNTP segment or a debrid CDN
// window is the unit, and a source enumerates its chunks up front. That works
// for lanes whose upstream is already quantised, but not for a byte-granular
// source — the HTTP ByteSource from HR1.1 reports Alignment=0 and can serve
// any offset.
//
// This adapter quantises any bytesource.ByteSource into fixed-size blocks and
// presents them as a ChunkSource, so byte-granular lanes address the SAME
// shared cache, get the same sparse residency (only touched blocks are held),
// and — because PayloadKey below is derived from the source's own stable
// identity — participate in the same global disk deduplication and in-flight
// coalescing as every other lane. Nothing here is lane-specific.
//
// This row supplies the mechanism only. HR3.1 wires progressive HTTP
// playback through it and HR5.3 uses it for prewarm/readahead;
// multi-client overlap policy remains HR3.5.

import (
	"context"
	"errors"
	"io"
	"strconv"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// DefaultBlockBytes is the default sparse block size. It is large enough that
// a sequential read does not thrash the block map and small enough that a
// seek does not drag a whole window: between an NNTP segment (~700KiB) and a
// debrid CDN window (16MiB). Consuming rows pass their own size when their
// origin's behavior justifies it.
const DefaultBlockBytes int64 = 4 << 20

type byteSourceBlocks struct {
	bs        bytesource.ByteSource
	blockSize int64
	key       string
	refs      []ChunkRef
}

// NewByteSourceBlocks presents bs as a block-addressed ChunkSource. A
// blockSize of 0 uses DefaultBlockBytes.
func NewByteSourceBlocks(bs bytesource.ByteSource, blockSize int64) (ChunkSource, error) {
	if bs == nil {
		return nil, errors.New("rangecache: nil byte source")
	}
	if blockSize <= 0 {
		blockSize = DefaultBlockBytes
	}
	size := bs.Size()
	if size <= 0 {
		return nil, errors.New("rangecache: byte source has no positive size")
	}
	key := bs.Key()
	if key == "" {
		return nil, errors.New("rangecache: byte source has no key")
	}
	n := (size + blockSize - 1) / blockSize
	refs := make([]ChunkRef, 0, n)
	for i := int64(0); i < n; i++ {
		sz := blockSize
		if rem := size - i*blockSize; rem < sz {
			sz = rem
		}
		refs = append(refs, ChunkRef{Index: int(i), Key: strconv.FormatInt(i, 10), Size: sz})
	}
	return &byteSourceBlocks{bs: bs, blockSize: blockSize, key: "bs|" + key, refs: refs}, nil
}

func (b *byteSourceBlocks) Key() string        { return b.key }
func (b *byteSourceBlocks) Chunks() []ChunkRef { return b.refs }

// ExactSizes mirrors the underlying source: a block's declared size equals
// its fetched size exactly when the source's own total is exact.
func (b *byteSourceBlocks) ExactSizes() bool { return b.bs.Caps().ExactSize }

// PayloadKey gives a block an identity independent of any item or session,
// derived from the source's own stable, secret-free key plus the block
// geometry. Two consumers of the same source at the same block size resolve
// to one payload, so they share disk entries and coalesce in flight.
func (b *byteSourceBlocks) PayloadKey(ref ChunkRef) string {
	return b.bs.Key() + "|blk" + strconv.FormatInt(b.blockSize, 10) + "|" + strconv.Itoa(ref.Index)
}

// Fetch reads exactly one block. It returns the complete block or an error,
// never partial data, matching the ChunkSource contract. The readahead class
// carries no separate pool here, so both classes read identically; a
// consuming row that needs class-aware pooling supplies its own source.
func (b *byteSourceBlocks) Fetch(ctx context.Context, ref ChunkRef, _ FetchClass) ([]byte, error) {
	if ref.Index < 0 || ref.Index >= len(b.refs) {
		return nil, errors.New("rangecache: block index out of range")
	}
	want := b.refs[ref.Index]
	buf := make([]byte, want.Size)
	n, err := b.bs.ReadAt(ctx, buf, int64(ref.Index)*b.blockSize)
	if err != nil && !(errors.Is(err, io.EOF) && int64(n) == want.Size) {
		return nil, err
	}
	if int64(n) != want.Size {
		return nil, errors.New("rangecache: short block read")
	}
	return buf, nil
}
