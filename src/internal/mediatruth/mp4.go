package mediatruth

import (
	"context"
	"encoding/binary"
	"errors"
)

// errMP4 is the internal parse-failure sentinel; it never reaches a caller.
var errMP4 = errors.New("mediatruth: malformed mp4")

// maxSampleWalk bounds the forward sample walk so a hostile or corrupt header
// cannot turn index construction into unbounded CPU work.
const maxSampleWalk = 4_000_000

// mp4Box is one parsed box header.
type mp4Box struct {
	typ     string
	dataAbs int64 // absolute offset of the box body
	dataLen int64 // body length; -1 when the box runs to EOF
	hdrLen  int64
}

// parseBoxHeader decodes a box header at buf[pos]. base is the absolute
// offset of buf[0].
func parseBoxHeader(buf []byte, pos int, base int64) (mp4Box, bool) {
	if pos < 0 || pos+8 > len(buf) {
		return mp4Box{}, false
	}
	size := int64(binary.BigEndian.Uint32(buf[pos : pos+4]))
	typ := string(buf[pos+4 : pos+8])
	hdr := int64(8)
	switch {
	case size == 1:
		if pos+16 > len(buf) {
			return mp4Box{}, false
		}
		u := binary.BigEndian.Uint64(buf[pos+8 : pos+16])
		if u > 1<<62 {
			return mp4Box{}, false
		}
		size = int64(u)
		hdr = 16
		if size < hdr {
			return mp4Box{}, false
		}
	case size == 0:
		return mp4Box{typ: typ, dataAbs: base + int64(pos) + hdr, dataLen: -1, hdrLen: hdr}, true
	case size < 8:
		return mp4Box{}, false
	}
	return mp4Box{
		typ:     typ,
		dataAbs: base + int64(pos) + hdr,
		dataLen: size - hdr,
		hdrLen:  hdr,
	}, true
}

// eachBox iterates the boxes contained in body, invoking fn for each. It stops
// early when fn returns false. It never reads past body.
func eachBox(body []byte, fn func(typ string, data []byte) bool) {
	pos := 0
	for pos+8 <= len(body) {
		b, ok := parseBoxHeader(body, pos, 0)
		if !ok {
			return
		}
		start := pos + int(b.hdrLen)
		end := len(body)
		if b.dataLen >= 0 {
			if b.dataLen > int64(len(body)-start) {
				return
			}
			end = start + int(b.dataLen)
		}
		if !fn(b.typ, body[start:end]) {
			return
		}
		if b.dataLen < 0 {
			return
		}
		pos = end
	}
}

// findBox returns the first child box of the given type.
func findBox(body []byte, typ string) ([]byte, bool) {
	var out []byte
	var found bool
	eachBox(body, func(t string, d []byte) bool {
		if t == typ {
			out, found = d, true
			return false
		}
		return true
	})
	return out, found
}

// sttsRun is one run of samples sharing a delta.
type sttsRun struct {
	count uint32
	delta uint32
}

// stscRun maps a first chunk to its samples-per-chunk.
type stscRun struct {
	firstChunk      uint32
	samplesPerChunk uint32
}

// stbl holds the sample tables needed to turn a keyframe into a byte offset.
type stbl struct {
	stts         []sttsRun
	stss         []uint32
	hasStss      bool
	stsc         []stscRun
	chunkOffsets []int64
	sampleSize   uint32
	sampleSizes  []uint32
}

// fullBoxBody strips a FullBox's version+flags, returning the remainder and
// the version byte.
func fullBoxBody(d []byte) ([]byte, byte, bool) {
	if len(d) < 4 {
		return nil, 0, false
	}
	return d[4:], d[0], true
}

// entryCountFits guards every table allocation against a declared count that
// the box body cannot possibly contain.
func entryCountFits(count uint64, remaining int, perEntry int) bool {
	return perEntry > 0 && count <= uint64(remaining/perEntry)
}

func parseSTTS(d []byte) ([]sttsRun, bool) {
	body, _, ok := fullBoxBody(d)
	if !ok || len(body) < 4 {
		return nil, false
	}
	n := uint64(binary.BigEndian.Uint32(body[:4]))
	body = body[4:]
	if !entryCountFits(n, len(body), 8) {
		return nil, false
	}
	out := make([]sttsRun, 0, n)
	for i := uint64(0); i < n; i++ {
		off := int(i) * 8
		out = append(out, sttsRun{
			count: binary.BigEndian.Uint32(body[off : off+4]),
			delta: binary.BigEndian.Uint32(body[off+4 : off+8]),
		})
	}
	return out, true
}

func parseSTSS(d []byte) ([]uint32, bool) {
	body, _, ok := fullBoxBody(d)
	if !ok || len(body) < 4 {
		return nil, false
	}
	n := uint64(binary.BigEndian.Uint32(body[:4]))
	body = body[4:]
	if !entryCountFits(n, len(body), 4) {
		return nil, false
	}
	out := make([]uint32, 0, n)
	for i := uint64(0); i < n; i++ {
		out = append(out, binary.BigEndian.Uint32(body[int(i)*4:int(i)*4+4]))
	}
	return out, true
}

func parseSTSC(d []byte) ([]stscRun, bool) {
	body, _, ok := fullBoxBody(d)
	if !ok || len(body) < 4 {
		return nil, false
	}
	n := uint64(binary.BigEndian.Uint32(body[:4]))
	body = body[4:]
	if !entryCountFits(n, len(body), 12) {
		return nil, false
	}
	out := make([]stscRun, 0, n)
	for i := uint64(0); i < n; i++ {
		off := int(i) * 12
		out = append(out, stscRun{
			firstChunk:      binary.BigEndian.Uint32(body[off : off+4]),
			samplesPerChunk: binary.BigEndian.Uint32(body[off+4 : off+8]),
		})
	}
	return out, true
}

func parseSTSZ(d []byte) (uint32, []uint32, bool) {
	body, _, ok := fullBoxBody(d)
	if !ok || len(body) < 8 {
		return 0, nil, false
	}
	sampleSize := binary.BigEndian.Uint32(body[:4])
	count := uint64(binary.BigEndian.Uint32(body[4:8]))
	body = body[8:]
	if sampleSize != 0 {
		return sampleSize, nil, true
	}
	if !entryCountFits(count, len(body), 4) {
		return 0, nil, false
	}
	out := make([]uint32, 0, count)
	for i := uint64(0); i < count; i++ {
		out = append(out, binary.BigEndian.Uint32(body[int(i)*4:int(i)*4+4]))
	}
	return 0, out, true
}

func parseChunkOffsets(d []byte, wide bool) ([]int64, bool) {
	body, _, ok := fullBoxBody(d)
	if !ok || len(body) < 4 {
		return nil, false
	}
	n := uint64(binary.BigEndian.Uint32(body[:4]))
	body = body[4:]
	per := 4
	if wide {
		per = 8
	}
	if !entryCountFits(n, len(body), per) {
		return nil, false
	}
	out := make([]int64, 0, n)
	for i := uint64(0); i < n; i++ {
		off := int(i) * per
		if wide {
			u := binary.BigEndian.Uint64(body[off : off+8])
			if u > 1<<62 {
				return nil, false
			}
			out = append(out, int64(u))
			continue
		}
		out = append(out, int64(binary.BigEndian.Uint32(body[off:off+4])))
	}
	return out, true
}

// mdhdTimescale reads the media timescale from an mdhd box.
func mdhdTimescale(d []byte) (uint32, bool) {
	body, version, ok := fullBoxBody(d)
	if !ok {
		return 0, false
	}
	if version == 1 {
		if len(body) < 20 {
			return 0, false
		}
		return binary.BigEndian.Uint32(body[16:20]), true
	}
	if len(body) < 12 {
		return 0, false
	}
	return binary.BigEndian.Uint32(body[8:12]), true
}

// handlerType reads the four-character handler type from an hdlr box.
func handlerType(d []byte) (string, bool) {
	body, _, ok := fullBoxBody(d)
	if !ok || len(body) < 8 {
		return "", false
	}
	return string(body[4:8]), true
}

// parseMP4Moov converts a moov box body into absolute seek points for the
// first video track. It is a pure function over a byte slice — no I/O — so it
// is directly fuzzable (DG-03). Every table read is bounded by the body it
// came from, and the forward sample walk is capped.
func parseMP4Moov(moov []byte, maxEntries int) ([]Entry, error) {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxIndexEntries
	}
	var entries []Entry
	var parseErr = errMP4

	eachBox(moov, func(typ string, trak []byte) bool {
		if typ != "trak" {
			return true
		}
		mdia, ok := findBox(trak, "mdia")
		if !ok {
			return true
		}
		hdlr, ok := findBox(mdia, "hdlr")
		if !ok {
			return true
		}
		ht, ok := handlerType(hdlr)
		if !ok || ht != "vide" {
			return true
		}
		mdhd, ok := findBox(mdia, "mdhd")
		if !ok {
			return true
		}
		timescale, ok := mdhdTimescale(mdhd)
		if !ok || timescale == 0 {
			return true
		}
		minf, ok := findBox(mdia, "minf")
		if !ok {
			return true
		}
		stblBody, ok := findBox(minf, "stbl")
		if !ok {
			return true
		}
		tables, ok := parseStbl(stblBody)
		if !ok {
			return true
		}
		out, err := walkSamples(tables, timescale, maxEntries)
		if err != nil {
			parseErr = err
			return false
		}
		entries = out
		parseErr = nil
		return false
	})

	if parseErr != nil {
		return nil, parseErr
	}
	return entries, nil
}

func parseStbl(body []byte) (stbl, bool) {
	var t stbl
	var sawSTTS, sawSTSC, sawOffsets, sawSTSZ bool
	eachBox(body, func(typ string, d []byte) bool {
		switch typ {
		case "stts":
			if v, ok := parseSTTS(d); ok {
				t.stts, sawSTTS = v, true
			}
		case "stss":
			if v, ok := parseSTSS(d); ok {
				t.stss, t.hasStss = v, true
			}
		case "stsc":
			if v, ok := parseSTSC(d); ok {
				t.stsc, sawSTSC = v, true
			}
		case "stsz":
			if size, sizes, ok := parseSTSZ(d); ok {
				t.sampleSize, t.sampleSizes, sawSTSZ = size, sizes, true
			}
		case "stco":
			if v, ok := parseChunkOffsets(d, false); ok {
				t.chunkOffsets, sawOffsets = v, true
			}
		case "co64":
			if v, ok := parseChunkOffsets(d, true); ok {
				t.chunkOffsets, sawOffsets = v, true
			}
		}
		return true
	})
	return t, sawSTTS && sawSTSC && sawOffsets && sawSTSZ
}

// sampleCount returns the total sample count implied by the time-to-sample
// table, saturating at the walk cap.
func (t stbl) sampleCount() int64 {
	var n int64
	for _, run := range t.stts {
		n += int64(run.count)
		if n > maxSampleWalk {
			return maxSampleWalk
		}
	}
	return n
}

func (t stbl) sizeOf(sample int64) int64 {
	if t.sampleSize != 0 {
		return int64(t.sampleSize)
	}
	if sample < 0 || sample >= int64(len(t.sampleSizes)) {
		return 0
	}
	return int64(t.sampleSizes[sample])
}

// walkSamples performs one forward pass over the sample table, emitting a seek
// point for each sync sample (or an evenly strided subset when the track
// declares no sync-sample table, meaning every sample is seekable).
func walkSamples(t stbl, timescale uint32, maxEntries int) ([]Entry, error) {
	total := t.sampleCount()
	if total <= 0 || len(t.chunkOffsets) == 0 || len(t.stsc) == 0 {
		return nil, errMP4
	}

	// Sync-sample cursor. Without stss, stride the whole track instead.
	var syncIdx int
	stride := int64(1)
	if !t.hasStss {
		stride = total / int64(maxEntries)
		if stride < 1 {
			stride = 1
		}
	} else if len(t.stss) == 0 {
		return nil, errMP4
	}

	var out []Entry
	var (
		sample      int64 // 0-based
		cumTicks    int64
		sttsRunIdx  int
		sttsRunLeft int64
		chunkIdx    int64 // 0-based
		sampleInChk uint32
		offsetInChk int64
		stscIdx     int
	)
	if len(t.stts) > 0 {
		sttsRunLeft = int64(t.stts[0].count)
	}
	samplesPerChunk := t.stsc[0].samplesPerChunk
	if samplesPerChunk == 0 {
		return nil, errMP4
	}

	for sample < total {
		// Advance the stts run cursor.
		for sttsRunLeft == 0 {
			sttsRunIdx++
			if sttsRunIdx >= len(t.stts) {
				return finishWalk(out)
			}
			sttsRunLeft = int64(t.stts[sttsRunIdx].count)
		}
		// Advance the stsc/chunk cursor.
		if sampleInChk >= samplesPerChunk {
			chunkIdx++
			sampleInChk = 0
			offsetInChk = 0
			if stscIdx+1 < len(t.stsc) && uint32(chunkIdx+1) >= t.stsc[stscIdx+1].firstChunk {
				stscIdx++
				samplesPerChunk = t.stsc[stscIdx].samplesPerChunk
				if samplesPerChunk == 0 {
					return nil, errMP4
				}
			}
		}
		if chunkIdx >= int64(len(t.chunkOffsets)) {
			break
		}

		emit := false
		if t.hasStss {
			for syncIdx < len(t.stss) && int64(t.stss[syncIdx]) < sample+1 {
				syncIdx++
			}
			if syncIdx < len(t.stss) && int64(t.stss[syncIdx]) == sample+1 {
				emit = true
			}
		} else if sample%stride == 0 {
			emit = true
		}
		if emit {
			ms := cumTicks * 1000 / int64(timescale)
			off := t.chunkOffsets[chunkIdx] + offsetInChk
			if ms >= 0 && off >= 0 {
				out = append(out, Entry{TimeMS: ms, Offset: off})
			}
			if len(out) >= maxEntries*4 {
				break
			}
		}

		sz := t.sizeOf(sample)
		offsetInChk += sz
		cumTicks += int64(t.stts[sttsRunIdx].delta)
		sttsRunLeft--
		sampleInChk++
		sample++
	}
	return finishWalk(out)
}

func finishWalk(out []Entry) ([]Entry, error) {
	if len(out) == 0 {
		return nil, errMP4
	}
	return out, nil
}

// mp4Index builds the compact seek index for an MP4 source. It walks the
// top-level box chain with small header reads rather than guessing where moov
// lives, then reads exactly the moov box — which is the correct bounded answer
// for both moov-at-front and moov-at-end layouts. Any trailing region fetched
// is returned so the prober can reuse it.
func mp4Index(ctx context.Context, r *reader, head []byte, opts Options) (Index, []byte, int64, string) {
	boxAbs, boxLen, hdrLen, reason := locateMoov(ctx, r, head, opts)
	if reason != "" {
		return Index{}, nil, 0, reason
	}

	// moov already inside the leading window: no further read.
	if boxAbs+boxLen <= int64(len(head)) {
		raw, err := parseMP4Moov(head[boxAbs+hdrLen:boxAbs+boxLen], opts.MaxIndexEntries)
		if err != nil {
			return Index{}, nil, 0, reasonIndexUnparseable
		}
		return finalizeIndex(IndexSourceMP4Moov, raw, opts.MaxIndexEntries), nil, 0, ""
	}

	if opts.IndexBytes <= 0 {
		return Index{}, nil, 0, reasonIndexBudget
	}
	// Read the WHOLE box, header included: the returned region is handed to
	// the prober at its true offset, and a moov body without its own box
	// header is not a parseable container structure.
	want := boxLen
	if want > opts.IndexBytes {
		want = opts.IndexBytes
	}
	buf, rerr := r.readAt(ctx, boxAbs, want)
	if rerr != nil && len(buf) == 0 {
		return Index{}, nil, 0, readErrReason(rerr)
	}
	if int64(len(buf)) <= hdrLen {
		return Index{}, buf, boxAbs, reasonIndexBudget
	}
	raw, err := parseMP4Moov(buf[hdrLen:], opts.MaxIndexEntries)
	if err != nil {
		if int64(len(buf)) < boxLen {
			return Index{}, buf, boxAbs, reasonIndexBudget
		}
		return Index{}, buf, boxAbs, reasonIndexUnparseable
	}
	return finalizeIndex(IndexSourceMP4Moov, raw, opts.MaxIndexEntries), buf, boxAbs, ""
}

// locateMoov walks the top-level box chain and returns the moov box's
// absolute start offset, its total length including the header, and its
// header length. Boxes covered by the leading window cost no read; beyond it,
// each step is one bounded 16-byte header read.
func locateMoov(ctx context.Context, r *reader, head []byte, opts Options) (int64, int64, int64, string) {
	size := r.src.Size()
	var pos int64
	for i := 0; i < opts.MaxTopLevelBoxes; i++ {
		if size > 0 && pos >= size {
			return 0, 0, 0, reasonNoIndexStructure
		}
		var hdr []byte
		if pos+16 <= int64(len(head)) {
			hdr = head[pos : pos+16]
		} else if pos+8 <= int64(len(head)) {
			hdr = head[pos:]
		} else {
			b, err := r.readAt(ctx, pos, 16)
			if err != nil && len(b) < 8 {
				return 0, 0, 0, readErrReason(err)
			}
			hdr = b
		}
		b, ok := parseBoxHeader(hdr, 0, pos)
		if !ok {
			return 0, 0, 0, reasonIndexUnparseable
		}
		if b.typ == "moov" {
			if b.dataLen < 0 {
				if size <= 0 {
					return 0, 0, 0, reasonNoIndexStructure
				}
				return pos, size - pos, b.hdrLen, ""
			}
			return pos, b.hdrLen + b.dataLen, b.hdrLen, ""
		}
		if b.dataLen < 0 {
			return 0, 0, 0, reasonNoIndexStructure
		}
		pos = b.dataAbs + b.dataLen
	}
	return 0, 0, 0, reasonNoIndexStructure
}
