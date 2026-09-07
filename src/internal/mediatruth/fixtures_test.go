package mediatruth

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// ---- EBML / Matroska fixture builder -------------------------------------

// ebmlID encodes an element ID in its conventional marker-inclusive form.
func ebmlIDBytes(id uint64) []byte {
	var full [8]byte
	binary.BigEndian.PutUint64(full[:], id)
	i := 0
	for i < 7 && full[i] == 0 {
		i++
	}
	return full[i:]
}

// ebmlSizeBytes encodes a data size as an EBML variable-length integer.
func ebmlSizeBytes(n uint64) []byte {
	for length := 1; length <= 8; length++ {
		capacity := uint64(1)<<(uint(length)*7) - 1
		if n < capacity {
			out := make([]byte, length)
			v := n | uint64(0x80)>>uint(length-1)<<(uint(length-1)*8)
			for i := length - 1; i >= 0; i-- {
				out[i] = byte(v)
				v >>= 8
			}
			return out
		}
	}
	panic("ebml size too large")
}

func ebmlElem(id uint64, payload []byte) []byte {
	out := append([]byte{}, ebmlIDBytes(id)...)
	out = append(out, ebmlSizeBytes(uint64(len(payload)))...)
	return append(out, payload...)
}

func ebmlUint(v uint64) []byte {
	var full [8]byte
	binary.BigEndian.PutUint64(full[:], v)
	i := 0
	for i < 7 && full[i] == 0 {
		i++
	}
	return full[i:]
}

type cuePoint struct {
	timeMS     uint64
	clusterPos uint64
}

func buildCues(points []cuePoint) []byte {
	var body []byte
	for _, p := range points {
		cp := ebmlElem(idCueTime, ebmlUint(p.timeMS))
		cp = append(cp, ebmlElem(idCueTrackPos, ebmlElem(idCueClusterPos, ebmlUint(p.clusterPos)))...)
		body = append(body, ebmlElem(idCuePoint, cp)...)
	}
	return ebmlElem(idCues, body)
}

// mkvOptions shapes a synthetic Matroska file.
type mkvOptions struct {
	points []cuePoint
	// padding is filler inserted before Cues, used to push Cues out of the
	// leading window so the SeekHead pointer path is exercised.
	padding int
	// omitSeekHead removes the directory pointer, forcing the bounded
	// trailing scan.
	omitSeekHead bool
	// omitCues removes the index entirely.
	omitCues bool
	// trailing is filler written after Cues.
	trailing int
}

// buildMKV returns a synthetic Matroska file and the absolute offset of the
// Segment element's data (which cluster positions are relative to).
func buildMKV(o mkvOptions) ([]byte, int64) {
	header := ebmlElem(idEBMLHeader, ebmlElem(0xEC, []byte{0x00, 0x00}))
	info := ebmlElem(idInfo, ebmlElem(idTimecodeScale, ebmlUint(defaultTimecodeScale)))

	cues := []byte{}
	if !o.omitCues {
		cues = buildCues(o.points)
	}
	pad := ebmlElem(0xEC, make([]byte, o.padding))
	trail := []byte{}
	if o.trailing > 0 {
		trail = ebmlElem(0xEC, make([]byte, o.trailing))
	}

	// Two passes: the SeekHead's own encoded length shifts the Cues offset,
	// so build with a placeholder, then rebuild with the settled value.
	build := func(seekPos uint64) ([]byte, int64, uint64) {
		var seekHead []byte
		if !o.omitSeekHead {
			seek := ebmlElem(idSeekID, ebmlUint(idCues))
			seek = append(seek, ebmlElem(idSeekPosition, ebmlUint(seekPos))...)
			seekHead = ebmlElem(idSeekHead, ebmlElem(idSeek, seek))
		}
		segBody := append([]byte{}, seekHead...)
		segBody = append(segBody, info...)
		segBody = append(segBody, pad...)
		cuesRel := uint64(len(segBody))
		segBody = append(segBody, cues...)
		segBody = append(segBody, trail...)
		seg := ebmlElem(idSegment, segBody)
		file := append(append([]byte{}, header...), seg...)
		segDataStart := int64(len(header)) + int64(len(seg)-len(segBody))
		return file, segDataStart, cuesRel
	}

	// Settle the pointer: the Segment-relative Cues offset depends on the
	// encoded width of the position itself, so build until it is a fixed
	// point. The offset is taken structurally from the builder, never by
	// scanning for the Cues ID -- that pattern also occurs inside SeekHead's
	// own SeekID payload.
	var file []byte
	var segStart int64
	pos := uint64(0)
	for i := 0; i < 6; i++ {
		f, seg, actual := build(pos)
		file, segStart = f, seg
		if actual == pos {
			return file, segStart
		}
		pos = actual
	}
	panic("mkv fixture: cues pointer did not settle")
}

// ---- MP4 fixture builder --------------------------------------------------

func makeBox(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(payload)))
	copy(out[4:8], typ)
	return append(out, payload...)
}

func makeFullBox(typ string, version byte, payload []byte) []byte {
	body := append([]byte{version, 0, 0, 0}, payload...)
	return makeBox(typ, body)
}

func be32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

type mp4Options struct {
	timescale   uint32
	sttsRuns    []sttsRun
	syncSamples []uint32 // 1-based; empty means no stss box
	sampleSize  uint32
	stscRuns    []stscRun
	chunkOffs   []uint32
	// moovLast places moov after mdat (the moov-at-end layout).
	moovLast bool
	// mdatFiller sizes the mdat payload.
	mdatFiller int
	// wideOffsets emits co64 instead of stco.
	wideOffsets bool
}

func buildMP4(o mp4Options) []byte {
	stts := []byte{}
	stts = append(stts, be32(uint32(len(o.sttsRuns)))...)
	for _, r := range o.sttsRuns {
		stts = append(stts, be32(r.count)...)
		stts = append(stts, be32(r.delta)...)
	}
	stblBody := makeFullBox("stts", 0, stts)

	if len(o.syncSamples) > 0 {
		stss := be32(uint32(len(o.syncSamples)))
		for _, s := range o.syncSamples {
			stss = append(stss, be32(s)...)
		}
		stblBody = append(stblBody, makeFullBox("stss", 0, stss)...)
	}

	stsc := be32(uint32(len(o.stscRuns)))
	for _, r := range o.stscRuns {
		stsc = append(stsc, be32(r.firstChunk)...)
		stsc = append(stsc, be32(r.samplesPerChunk)...)
		stsc = append(stsc, be32(1)...)
	}
	stblBody = append(stblBody, makeFullBox("stsc", 0, stsc)...)

	stsz := append(be32(o.sampleSize), be32(0)...)
	stblBody = append(stblBody, makeFullBox("stsz", 0, stsz)...)

	if o.wideOffsets {
		co64 := be32(uint32(len(o.chunkOffs)))
		for _, c := range o.chunkOffs {
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], uint64(c))
			co64 = append(co64, b[:]...)
		}
		stblBody = append(stblBody, makeFullBox("co64", 0, co64)...)
	} else {
		stco := be32(uint32(len(o.chunkOffs)))
		for _, c := range o.chunkOffs {
			stco = append(stco, be32(c)...)
		}
		stblBody = append(stblBody, makeFullBox("stco", 0, stco)...)
	}

	minf := makeBox("minf", makeBox("stbl", stblBody))
	mdhd := makeFullBox("mdhd", 0, append(append(be32(0), be32(0)...), append(be32(o.timescale), be32(0)...)...))
	hdlr := makeFullBox("hdlr", 0, append(be32(0), []byte("vide")...))
	mdia := makeBox("mdia", append(append(mdhd, hdlr...), minf...))
	moov := makeBox("moov", makeBox("trak", mdia))

	ftyp := makeBox("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))
	mdat := makeBox("mdat", make([]byte, o.mdatFiller))

	out := append([]byte{}, ftyp...)
	if o.moovLast {
		out = append(out, mdat...)
		return append(out, moov...)
	}
	out = append(out, moov...)
	return append(out, mdat...)
}

// standardMP4 is the shared arithmetic fixture: 10 samples of 50 bytes at
// 100 ticks each (timescale 1000 => 100 ms), sync samples 1/5/9, five samples
// per chunk, chunks at 1000 and 2000.
func standardMP4(moovLast bool) ([]byte, []Entry) {
	data := buildMP4(mp4Options{
		timescale:   1000,
		sttsRuns:    []sttsRun{{count: 10, delta: 100}},
		syncSamples: []uint32{1, 5, 9},
		sampleSize:  50,
		stscRuns:    []stscRun{{firstChunk: 1, samplesPerChunk: 5}},
		chunkOffs:   []uint32{1000, 2000},
		moovLast:    moovLast,
		mdatFiller:  4096,
	})
	want := []Entry{
		{TimeMS: 0, Offset: 1000},
		{TimeMS: 400, Offset: 1200},
		{TimeMS: 800, Offset: 2150},
	}
	return data, want
}

// ---- ByteSource shims -----------------------------------------------------

// failingSource serves data up to failAfter bytes read, then returns err. It
// proves a mid-analysis source failure never yields a fabricated result.
type failingSource struct {
	data     []byte
	failAt   int64 // absolute offset at or past which reads fail
	err      error
	exact    bool
	readCall int
}

func (f *failingSource) Size() int64 { return int64(len(f.data)) }
func (f *failingSource) Key() string { return "test:failing" }
func (f *failingSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: f.exact, TailCost: bytesource.TailCostCheap, Alignment: 1}
}
func (f *failingSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	f.readCall++
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off >= f.failAt {
		return 0, f.err
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// errSourceMutated mirrors the HR1.1 adapter's stable message shape without
// importing internal/httpstream (which would create an import cycle).
var errSourceMutated = errors.New("httpstream: source mutated (strong validator changed)")

// countingSource wraps a MemSource and records the bytes actually read, so a
// bound can be asserted against real transfer rather than intent.
type countingSource struct {
	inner *bytesource.MemSource
	read  int64
	calls int
}

func newCountingSource(key string, data []byte) *countingSource {
	return &countingSource{inner: bytesource.NewMemSource(key, data)}
}
func (c *countingSource) Size() int64                   { return c.inner.Size() }
func (c *countingSource) Key() string                   { return c.inner.Key() }
func (c *countingSource) Caps() bytesource.Capabilities { return c.inner.Caps() }
func (c *countingSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	c.calls++
	n, err := c.inner.ReadAt(ctx, p, off)
	c.read += int64(n)
	return n, err
}

// chunkedOver exposes the same bytes through the chunked assembly path with
// the declared/decoded split an NNTP segment source carries, so lane-neutral
// identity can be asserted against the flat in-memory shape.
func chunkedOver(key string, data []byte, chunkSize int64) (bytesource.ByteSource, error) {
	var chunks []bytesource.Chunk
	starts := []int64{0}
	for off := int64(0); off < int64(len(data)); off += chunkSize {
		end := off + chunkSize
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		chunks = append(chunks, bytesource.Chunk{
			Index:        len(chunks),
			Key:          key + ":" + strings.Repeat("c", 1),
			DeclaredSize: end - off,
		})
		starts = append(starts, end)
	}
	cp := append([]byte(nil), data...)
	return bytesource.NewChunked(key, chunks, starts, int64(len(data)),
		bytesource.Capabilities{RangeSupport: true, ExactSize: true, TailCost: bytesource.TailCostSegmented, Alignment: chunkSize},
		func(ctx context.Context, c bytesource.Chunk) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			start := starts[c.Index]
			end := starts[c.Index+1]
			return append([]byte(nil), cp[start:end]...), nil
		})
}
