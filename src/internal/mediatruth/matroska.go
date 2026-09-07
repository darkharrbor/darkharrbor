package mediatruth

import (
	"context"
	"encoding/binary"
	"errors"
)

// Matroska/EBML element IDs, marker bits included (the conventional form).
const (
	idEBMLHeader    = 0x1A45DFA3
	idSegment       = 0x18538067
	idSeekHead      = 0x114D9B74
	idSeek          = 0x4DBB
	idSeekID        = 0x53AB
	idSeekPosition  = 0x53AC
	idInfo          = 0x1549A966
	idTimecodeScale = 0x2AD7B1
	idCues          = 0x1C53BB6B
	idCuePoint      = 0xBB
	idCueTime       = 0xB3
	idCueTrackPos   = 0xB7
	idCueClusterPos = 0xF1
	idCluster       = 0x1F43B675
)

// defaultTimecodeScale is Matroska's own default: timecodes are in units of
// 1,000,000 ns, i.e. milliseconds.
const defaultTimecodeScale = 1_000_000

// errEBML is the internal parse-failure sentinel. It never reaches a caller;
// it is mapped to this package's fixed reason vocabulary first.
var errEBML = errors.New("mediatruth: malformed ebml")

// element is one parsed EBML element header.
type element struct {
	id      uint64
	dataAbs int64 // absolute offset of the element's data
	size    int64 // data size; valid only when !unknown
	unknown bool  // the element declared an unknown size
	hdrLen  int
}

// readVInt decodes an EBML variable-length integer at buf[pos]. When keepMark
// is true the leading marker bit is retained (element IDs); when false it is
// cleared (sizes and unsigned values). It never reads past len(buf).
func readVInt(buf []byte, pos int, keepMark bool) (val uint64, n int, allOnes bool, ok bool) {
	if pos < 0 || pos >= len(buf) {
		return 0, 0, false, false
	}
	b := buf[pos]
	if b == 0 {
		// A leading zero byte would mean a length above 8; unsupported.
		return 0, 0, false, false
	}
	length := 1
	for mask := byte(0x80); mask != 0 && b&mask == 0; mask >>= 1 {
		length++
	}
	if length > 8 || pos+length > len(buf) {
		return 0, 0, false, false
	}
	v := uint64(b)
	if !keepMark {
		v = uint64(b & (0xFF >> uint(length)))
	}
	ones := b&(0xFF>>uint(length)) == 0xFF>>uint(length)
	for i := 1; i < length; i++ {
		c := buf[pos+i]
		if c != 0xFF {
			ones = false
		}
		v = v<<8 | uint64(c)
	}
	return v, length, ones, true
}

// nextElement parses one element header at buf[pos]. base is the absolute
// offset of buf[0].
func nextElement(buf []byte, pos int, base int64) (element, int, bool) {
	id, idLen, _, ok := readVInt(buf, pos, true)
	if !ok {
		return element{}, pos, false
	}
	size, szLen, allOnes, ok := readVInt(buf, pos+idLen, false)
	if !ok {
		return element{}, pos, false
	}
	hdr := idLen + szLen
	e := element{
		id:      id,
		dataAbs: base + int64(pos) + int64(hdr),
		size:    int64(size),
		unknown: allOnes,
		hdrLen:  hdr,
	}
	if !allOnes && e.size < 0 {
		return element{}, pos, false
	}
	return e, pos + hdr, true
}

// uintValue decodes an EBML unsigned integer of up to 8 bytes.
func uintValue(b []byte) (uint64, bool) {
	if len(b) == 0 || len(b) > 8 {
		return 0, false
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, true
}

// matroskaHead is what the leading window tells us about a Matroska source.
type matroskaHead struct {
	segStart  int64 // absolute offset of the Segment element's data
	scaleNS   uint64
	cuesAbs   int64  // absolute offset of the Cues element's data, -1 if unknown
	cuesSize  int64  // declared Cues data size, -1 if unknown
	cuesInBuf []byte // Cues data when it lies wholly inside the head window
}

// parseMatroskaHead walks the leading window: it locates the Segment, reads
// the timecode scale from Info, takes Cues directly if it is in the window,
// and otherwise follows SeekHead's own pointer to where Cues lives. It is a
// pure function over a byte slice — no I/O, no allocation beyond its result —
// so it is directly fuzzable (DG-03).
func parseMatroskaHead(head []byte) (matroskaHead, error) {
	out := matroskaHead{scaleNS: defaultTimecodeScale, cuesAbs: -1, cuesSize: -1}
	if len(head) < 4 {
		return out, errEBML
	}
	pos := 0
	// Top level: EBML header, then Segment.
	var seg element
	found := false
	for pos < len(head) {
		e, next, ok := nextElement(head, pos, 0)
		if !ok {
			return out, errEBML
		}
		if e.id == idSegment {
			seg = e
			found = true
			break
		}
		if e.id != idEBMLHeader && e.id != 0xEC /*Void*/ && e.id != 0xBF /*CRC-32*/ {
			return out, errEBML
		}
		if e.unknown {
			return out, errEBML
		}
		if e.size > int64(len(head)-next) {
			return out, errEBML
		}
		pos = next + int(e.size)
	}
	if !found {
		return out, errEBML
	}
	out.segStart = seg.dataAbs

	// Level 1, within whatever part of the Segment the window covers.
	pos = int(seg.dataAbs)
	for pos < len(head) {
		e, next, ok := nextElement(head, pos, 0)
		if !ok {
			break
		}
		avail := int64(len(head) - next)
		switch e.id {
		case idInfo:
			if !e.unknown && e.size <= avail {
				if s, ok := matroskaTimecodeScale(head[next : next+int(e.size)]); ok {
					out.scaleNS = s
				}
			}
		case idCues:
			out.cuesAbs = e.dataAbs
			out.cuesSize = e.size
			if !e.unknown && e.size <= avail {
				out.cuesInBuf = head[next : next+int(e.size)]
			}
		case idSeekHead:
			if !e.unknown && e.size <= avail && out.cuesAbs < 0 {
				if rel, ok := matroskaSeekPosition(head[next:next+int(e.size)], idCues); ok {
					out.cuesAbs = out.segStart + int64(rel)
				}
			}
		case idCluster:
			// Clusters begin the payload; nothing further of interest lies
			// inside the leading window.
			if out.cuesAbs >= 0 {
				return out, nil
			}
		}
		if e.unknown {
			break
		}
		if e.size > avail {
			break
		}
		pos = next + int(e.size)
	}
	return out, nil
}

// matroskaTimecodeScale reads TimecodeScale out of an Info element's data.
func matroskaTimecodeScale(info []byte) (uint64, bool) {
	pos := 0
	for pos < len(info) {
		e, next, ok := nextElement(info, pos, 0)
		if !ok || e.unknown || e.size > int64(len(info)-next) {
			return 0, false
		}
		if e.id == idTimecodeScale {
			if v, ok := uintValue(info[next : next+int(e.size)]); ok && v > 0 {
				return v, true
			}
			return 0, false
		}
		pos = next + int(e.size)
	}
	return 0, false
}

// matroskaSeekPosition finds the Segment-relative position SeekHead records
// for wantID.
func matroskaSeekPosition(seekHead []byte, wantID uint64) (uint64, bool) {
	pos := 0
	for pos < len(seekHead) {
		e, next, ok := nextElement(seekHead, pos, 0)
		if !ok || e.unknown || e.size > int64(len(seekHead)-next) {
			return 0, false
		}
		if e.id == idSeek {
			body := seekHead[next : next+int(e.size)]
			var gotID bool
			var position uint64
			var havePos bool
			p := 0
			for p < len(body) {
				se, snext, ok := nextElement(body, p, 0)
				if !ok || se.unknown || se.size > int64(len(body)-snext) {
					break
				}
				data := body[snext : snext+int(se.size)]
				switch se.id {
				case idSeekID:
					if v, ok := uintValue(data); ok && v == wantID {
						gotID = true
					}
				case idSeekPosition:
					if v, ok := uintValue(data); ok {
						position, havePos = v, true
					}
				}
				p = snext + int(se.size)
			}
			if gotID && havePos {
				return position, true
			}
		}
		pos = next + int(e.size)
	}
	return 0, false
}

// parseMatroskaCues converts a Cues element's data into absolute seek points.
// segStart is the Segment data offset that CueClusterPosition is relative to;
// scaleNS is the timecode scale in nanoseconds. Pure and fuzzable (DG-03).
func parseMatroskaCues(cues []byte, segStart int64, scaleNS uint64) []Entry {
	if scaleNS == 0 {
		scaleNS = defaultTimecodeScale
	}
	var out []Entry
	pos := 0
	for pos < len(cues) {
		e, next, ok := nextElement(cues, pos, 0)
		if !ok || e.unknown || e.size > int64(len(cues)-next) {
			break
		}
		if e.id == idCuePoint {
			if ent, ok := parseCuePoint(cues[next:next+int(e.size)], segStart, scaleNS); ok {
				out = append(out, ent)
			}
		}
		pos = next + int(e.size)
	}
	return out
}

func parseCuePoint(body []byte, segStart int64, scaleNS uint64) (Entry, bool) {
	var timecode uint64
	var haveTime bool
	var clusterPos uint64
	var havePos bool
	pos := 0
	for pos < len(body) {
		e, next, ok := nextElement(body, pos, 0)
		if !ok || e.unknown || e.size > int64(len(body)-next) {
			return Entry{}, false
		}
		data := body[next : next+int(e.size)]
		switch e.id {
		case idCueTime:
			if v, ok := uintValue(data); ok {
				timecode, haveTime = v, true
			}
		case idCueTrackPos:
			p := 0
			for p < len(data) {
				te, tnext, ok := nextElement(data, p, 0)
				if !ok || te.unknown || te.size > int64(len(data)-tnext) {
					break
				}
				if te.id == idCueClusterPos && !havePos {
					if v, ok := uintValue(data[tnext : tnext+int(te.size)]); ok {
						clusterPos, havePos = v, true
					}
				}
				p = tnext + int(te.size)
			}
		}
		pos = next + int(e.size)
	}
	if !haveTime || !havePos {
		return Entry{}, false
	}
	// timecode * scaleNS nanoseconds, expressed in milliseconds, guarding the
	// multiply against overflow on a hostile input.
	if scaleNS > 0 && timecode > (1<<62)/scaleNS {
		return Entry{}, false
	}
	ms := int64(timecode*scaleNS) / 1_000_000
	off := segStart + int64(clusterPos)
	if off < 0 || ms < 0 {
		return Entry{}, false
	}
	return Entry{TimeMS: ms, Offset: off}, true
}

// matroskaIndex builds the compact seek index for a Matroska source. It
// prefers the container's own SeekHead pointer to Cues over any heuristic
// scan, so the targeted read is one bounded read at a known offset. It
// returns any trailing region it fetched so the prober can reuse those bytes
// rather than transferring them twice.
func matroskaIndex(ctx context.Context, r *reader, head []byte, opts Options) (Index, []byte, int64, string) {
	mh, err := parseMatroskaHead(head)
	if err != nil {
		return Index{}, nil, 0, reasonIndexUnparseable
	}

	// Cues already inside the leading window: no further read at all.
	if len(mh.cuesInBuf) > 0 {
		raw := parseMatroskaCues(mh.cuesInBuf, mh.segStart, mh.scaleNS)
		if len(raw) == 0 {
			return Index{}, nil, 0, reasonIndexUnparseable
		}
		return finalizeIndex(IndexSourceMatroskaCues, raw, opts.MaxIndexEntries), nil, 0, ""
	}

	// Cues located by the container's own directory: one targeted read.
	if mh.cuesAbs > 0 && opts.IndexBytes > 0 {
		buf, rerr := r.readAt(ctx, mh.cuesAbs, opts.IndexBytes)
		if rerr != nil && len(buf) == 0 {
			return Index{}, nil, 0, readErrReason(rerr)
		}
		// The pointer addresses the Cues element header, not its data, when
		// it came from SeekHead; accept either shape.
		data := buf
		if e, next, ok := nextElement(buf, 0, mh.cuesAbs); ok && e.id == idCues {
			end := next + int(e.size)
			if e.unknown || e.size > int64(len(buf)-next) {
				end = len(buf)
			}
			data = buf[next:end]
		}
		if raw := parseMatroskaCues(data, mh.segStart, mh.scaleNS); len(raw) > 0 {
			return finalizeIndex(IndexSourceMatroskaCues, raw, opts.MaxIndexEntries), buf, mh.cuesAbs, ""
		}
		// The pointer did not land on Cues. On a source whose offsets are
		// exact this means the structure is malformed. On an estimated-offset
		// source (NNTP declared-vs-decoded segment sizes) it means the
		// pointer's true-file coordinate cannot be resolved to a byte offset
		// here at all -- the drift over the unrecorded tail exceeds any
		// bounded window. Fall through to the trailing scan, which addresses
		// from EOF and so carries no accumulated drift.
		exact := r.src.Caps().ExactSize
		if idx, tb, to, ok := matroskaTailScan(ctx, r, head, mh, opts); ok {
			return idx, tb, to, ""
		}
		if !exact {
			return Index{}, buf, mh.cuesAbs, reasonIndexPointerDrift
		}
		return Index{}, buf, mh.cuesAbs, reasonIndexUnparseable
	}

	// No directory pointer: one bounded trailing read, where a muxer that
	// wrote Cues last would have put them.
	if opts.TailBytes > 0 {
		size := r.src.Size()
		if size <= int64(len(head)) {
			return Index{}, nil, 0, reasonNoIndexStructure
		}
		off := size - opts.TailBytes
		if off < int64(len(head)) {
			off = int64(len(head))
		}
		buf, rerr := r.readAt(ctx, off, size-off)
		if rerr != nil && len(buf) == 0 {
			return Index{}, nil, 0, readErrReason(rerr)
		}
		if start, data, ok := scanForCues(buf, off); ok {
			raw := parseMatroskaCues(data, mh.segStart, mh.scaleNS)
			if len(raw) > 0 {
				_ = start
				return finalizeIndex(IndexSourceMatroskaCues, raw, opts.MaxIndexEntries), buf, off, ""
			}
		}
		return Index{}, buf, off, reasonNoIndexStructure
	}
	return Index{}, nil, 0, reasonNoIndexStructure
}

// matroskaTailScan performs the bounded trailing read and Cues scan. It
// addresses from EOF, so it is unaffected by any drift accumulated across a
// lane's estimated offset table.
func matroskaTailScan(ctx context.Context, r *reader, head []byte, mh matroskaHead, opts Options) (Index, []byte, int64, bool) {
	if opts.TailBytes <= 0 {
		return Index{}, nil, 0, false
	}
	size := r.src.Size()
	if size <= int64(len(head)) {
		return Index{}, nil, 0, false
	}
	off := size - opts.TailBytes
	if off < int64(len(head)) {
		off = int64(len(head))
	}
	buf, rerr := r.readAt(ctx, off, size-off)
	if rerr != nil && len(buf) == 0 {
		return Index{}, nil, 0, false
	}
	_, data, ok := scanForCues(buf, off)
	if !ok {
		return Index{}, buf, off, false
	}
	raw := parseMatroskaCues(data, mh.segStart, mh.scaleNS)
	if len(raw) == 0 {
		return Index{}, buf, off, false
	}
	return finalizeIndex(IndexSourceMatroskaCues, raw, opts.MaxIndexEntries), buf, off, true
}

// scanForCues finds a Cues element header inside a trailing buffer. The scan
// is a bounded last resort for files whose SeekHead is absent or unreachable.
func scanForCues(buf []byte, base int64) (int64, []byte, bool) {
	var marker [4]byte
	binary.BigEndian.PutUint32(marker[:], idCues)
	for i := 0; i+4 <= len(buf); i++ {
		if buf[i] != marker[0] || buf[i+1] != marker[1] || buf[i+2] != marker[2] || buf[i+3] != marker[3] {
			continue
		}
		e, next, ok := nextElement(buf, i, base)
		if !ok || e.id != idCues {
			continue
		}
		end := next + int(e.size)
		if e.unknown || e.size > int64(len(buf)-next) {
			end = len(buf)
		}
		return e.dataAbs, buf[next:end], true
	}
	return 0, nil, false
}
