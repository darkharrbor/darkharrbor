// Package bytesource defines DarkHarrbor's canonical, lane-agnostic byte
// substrate (TS-1.1; torrent-plan T2).
//
// One interface — a context-aware random-access reader with a known size and
// static capability hints — over which every lane's byte-producing fetch path
// is exposed identically. Archive parsers (RAR/ZIP/7z), container seek-index
// parsing (MKV Cues / MP4 moov), ffprobe feeds, and verification consume a
// ByteSource without knowing whether the bytes originate from NNTP segments,
// a debrid CDN range, or an HTTP-stream relay.
//
// This package is intentionally dependency-free (standard library only). The
// concrete adapters live next to their fetch machinery — the NNTP-segment
// adapter in internal/nntp and the debrid-CDN adapter in internal/api — and
// are BEHAVIOR-IDENTICAL wrappers over the existing fetch paths: they reuse
// the exact per-chunk fetch the streaming cache already uses, so no live
// playback behavior changes when this substrate is introduced. TS-1.2 archive
// readers and the HR5.2 media-truth service consume it; TS-1.1 established the
// contract, the two adapters, and the conformance suite.
//
// Declared vs decoded: sources whose declared chunk sizes equal their actual
// fetched sizes report Capabilities.ExactSize=true (debrid CDN windows).
// NNTP segments do not — yEnc-decoded length diverges from the NZB-declared
// byte count — so ReadAt positions in DECLARED space and emits in DECODED
// space, exactly as the streaming write loop does. Callers that need precise
// random access over a non-exact source supply a decoded-offset table
// (recorded segment offsets); a purely declared table is best-effort with the
// same small yEnc drift the cold-seek path already carries.
package bytesource

import (
	"context"
	"errors"
	"io"
	"sort"
)

// TailCost is a coarse hint about the cost of reading bytes near EOF, where
// archive central directories and MP4 moov atoms commonly live. It lets a
// consumer choose a tail-read strategy without lane-specific knowledge.
type TailCost int

const (
	// TailCostUnknown means the cost is unspecified.
	TailCostUnknown TailCost = iota
	// TailCostCheap means a tail read is a single bounded upstream range
	// (e.g. one CDN range GET).
	TailCostCheap
	// TailCostSegmented means the tail is the final segment(s); reading it
	// fetches and decodes them (NNTP).
	TailCostSegmented
)

// Capabilities are static, source-type-level hints. They never change over a
// source's lifetime and never carry secrets.
type Capabilities struct {
	// RangeSupport reports whether arbitrary sub-ranges are servable.
	RangeSupport bool
	// ExactSize reports whether Size() equals the true decoded total exactly.
	ExactSize bool
	// TailCost hints the cost of reading near EOF.
	TailCost TailCost
	// Alignment is the natural fetch granularity in bytes (segment or window
	// size); 0 means unknown / byte-granular.
	Alignment int64
}

// ByteSource is a context-aware random-access byte reader with a known size
// and capability hints.
type ByteSource interface {
	// ReadAt reads into p starting at byte offset off. It follows io.ReaderAt
	// semantics precisely: it returns a non-nil error whenever it returns
	// fewer than len(p) bytes, and returns io.EOF when the read reaches the
	// end of the source. The context bounds and cancels the read; a cancelled
	// context yields no bytes past what the caller can rely on.
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
	// Size returns the total size in bytes; exact iff Caps().ExactSize.
	Size() int64
	// Caps returns static capability hints.
	Caps() Capabilities
	// Key returns a stable, secret-free identity for the source.
	Key() string
}

// Chunk describes one addressable unit of a chunked source (an NNTP segment or
// a CDN window). Key is secret-free within the source namespace.
type Chunk struct {
	Index        int
	Key          string
	DeclaredSize int64
}

// FetchFunc returns the COMPLETE decoded bytes for one chunk, or an error. It
// must never return partial data with a nil error. Unlike the readahead
// contract of the streaming cache, a (nil, nil) result is NOT permitted here:
// a ByteSource read is always a demand read.
type FetchFunc func(ctx context.Context, c Chunk) ([]byte, error)

// ErrNegativeOffset is returned by ReadAt for a negative offset.
var ErrNegativeOffset = errors.New("bytesource: negative offset")

// NewChunked builds a ByteSource from an ordered chunk layout, a cumulative
// start-offset table, a total size, capability hints, and a per-chunk fetch.
//
// starts must have len(chunks)+1 entries: starts[i] is the byte offset of
// chunk i and starts[len(chunks)] is the total (declared) end, so the covering
// chunks of any range can be located without a fetch. When caps.ExactSize is
// false these are declared offsets; the actual decoded chunk bytes govern
// emission.
func NewChunked(key string, chunks []Chunk, starts []int64, size int64, caps Capabilities, fetch FetchFunc) (ByteSource, error) {
	if fetch == nil {
		return nil, errors.New("bytesource: nil fetch")
	}
	if len(starts) != len(chunks)+1 {
		return nil, errors.New("bytesource: starts must have len(chunks)+1 entries")
	}
	for i := 1; i < len(starts); i++ {
		if starts[i] < starts[i-1] {
			return nil, errors.New("bytesource: starts must be non-decreasing")
		}
	}
	cp := make([]Chunk, len(chunks))
	copy(cp, chunks)
	st := make([]int64, len(starts))
	copy(st, starts)
	return &chunked{key: key, chunks: cp, starts: st, size: size, caps: caps, fetch: fetch}, nil
}

type chunked struct {
	key    string
	chunks []Chunk
	starts []int64
	size   int64
	caps   Capabilities
	fetch  FetchFunc
}

func (c *chunked) Size() int64        { return c.size }
func (c *chunked) Caps() Capabilities { return c.caps }
func (c *chunked) Key() string        { return c.key }

// ReadAt assembles the requested range from covering chunks. Positioning uses
// declared offsets; emission is clamped to each chunk's actual decoded length,
// mirroring the streaming write loop's declared-space / decoded-space split.
func (c *chunked) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrNegativeOffset
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	end := c.starts[len(c.chunks)]
	if off >= end {
		return 0, io.EOF
	}
	want := len(p)
	// First covering chunk: the first whose declared end (starts[i+1]) > off.
	lo := sort.Search(len(c.chunks), func(i int) bool { return c.starts[i+1] > off })

	n := 0
	for i := lo; i < len(c.chunks) && n < want; i++ {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		data, err := c.fetch(ctx, c.chunks[i])
		if err != nil {
			return n, err
		}
		// Front-skip within this chunk in declared space (nonzero only for the
		// first covering chunk), clamped to the actual decoded length.
		var frontSkip int64
		if off > c.starts[i] {
			frontSkip = off - c.starts[i]
		}
		if frontSkip >= int64(len(data)) {
			// Decoded bytes fall short of the declared front-skip for this
			// chunk (only possible in the declared-offset fallback regime;
			// with recorded decoded offsets frontSkip is always < len(data)).
			// Skip to the next chunk exactly as the streaming write loop does,
			// never fabricating bytes for the declared-vs-decoded gap.
			continue
		}
		avail := data[frontSkip:]
		if len(avail) > want-n {
			avail = avail[:want-n]
		}
		n += copy(p[n:], avail)
	}
	if n < want {
		return n, io.EOF
	}
	return n, nil
}

// MemSource is a trivial in-memory ByteSource (exact size, secret-free). It is
// useful as a reference/fixture and carries no test-framework dependency.
type MemSource struct {
	key  string
	data []byte
}

// NewMemSource returns a MemSource over a copy of data.
func NewMemSource(key string, data []byte) *MemSource {
	return &MemSource{key: key, data: append([]byte(nil), data...)}
}

func (m *MemSource) Size() int64 { return int64(len(m.data)) }
func (m *MemSource) Key() string { return m.key }
func (m *MemSource) Caps() Capabilities {
	return Capabilities{RangeSupport: true, ExactSize: true, TailCost: TailCostCheap, Alignment: 1}
}

// ReadAt implements io.ReaderAt semantics over the in-memory buffer.
func (m *MemSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrNegativeOffset
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
