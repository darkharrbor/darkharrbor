package mediatruth

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// ---- DG-03 negatives: malformed, oversized, and hostile structures --------

func TestParseMP4MoovRejectsMalformedTables(t *testing.T) {
	base := func(o mp4Options) []byte {
		full := buildMP4(o)
		start := bytes.Index(full, []byte("moov"))
		if start < 4 {
			t.Fatal("fixture has no moov")
		}
		box, ok := parseBoxHeader(full, start-4, 0)
		if !ok {
			t.Fatal("moov header unparseable")
		}
		return full[start-4+int(box.hdrLen) : start-4+int(box.hdrLen)+int(box.dataLen)]
	}
	good := mp4Options{
		timescale:   1000,
		sttsRuns:    []sttsRun{{count: 10, delta: 100}},
		syncSamples: []uint32{1, 5, 9},
		sampleSize:  50,
		stscRuns:    []stscRun{{firstChunk: 1, samplesPerChunk: 5}},
		chunkOffs:   []uint32{1000, 2000},
		mdatFiller:  64,
	}

	t.Run("declared_entry_count_far_beyond_body", func(t *testing.T) {
		moov := base(good)
		// Inflate the stts entry_count to a value the body cannot hold.
		at := bytes.Index(moov, []byte("stts"))
		if at < 0 {
			t.Fatal("no stts")
		}
		hostile := append([]byte(nil), moov...)
		binary.BigEndian.PutUint32(hostile[at+8:at+12], 0xFFFFFFF0)
		if _, err := parseMP4Moov(hostile, 512); err == nil {
			t.Error("parse accepted an entry count the body cannot contain")
		}
	})

	t.Run("zero_samples_per_chunk", func(t *testing.T) {
		bad := good
		bad.stscRuns = []stscRun{{firstChunk: 1, samplesPerChunk: 0}}
		if _, err := parseMP4Moov(base(bad), 512); err == nil {
			t.Error("parse accepted samples_per_chunk = 0")
		}
	})

	t.Run("no_chunk_offsets", func(t *testing.T) {
		bad := good
		bad.chunkOffs = nil
		if _, err := parseMP4Moov(base(bad), 512); err == nil {
			t.Error("parse accepted an empty chunk-offset table")
		}
	})

	t.Run("truncated_body", func(t *testing.T) {
		moov := base(good)
		for _, cut := range []int{1, 7, 16, len(moov) / 3, len(moov) - 1} {
			if cut <= 0 || cut >= len(moov) {
				continue
			}
			if _, err := parseMP4Moov(moov[:cut], 512); err == nil {
				t.Errorf("parse accepted a body truncated to %d bytes", cut)
			}
		}
	})

	t.Run("empty_and_garbage", func(t *testing.T) {
		for _, in := range [][]byte{nil, {}, {0, 0, 0, 0}, bytes.Repeat([]byte{0xFF}, 64)} {
			if _, err := parseMP4Moov(in, 512); err == nil {
				t.Errorf("parse accepted %q", in)
			}
		}
	})
}

func TestParseBoxHeaderRejectsAbsurdSizes(t *testing.T) {
	mk := func(size uint32, typ string, extra ...byte) []byte {
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b[:4], size)
		copy(b[4:8], typ)
		return append(b, extra...)
	}
	if _, ok := parseBoxHeader(mk(7, "moov"), 0, 0); ok {
		t.Error("accepted a box smaller than its own header")
	}
	if _, ok := parseBoxHeader(mk(1, "moov"), 0, 0); ok {
		t.Error("accepted a 64-bit box with no largesize field")
	}
	huge := make([]byte, 8)
	binary.BigEndian.PutUint64(huge, 1<<63)
	if _, ok := parseBoxHeader(mk(1, "moov", huge...), 0, 0); ok {
		t.Error("accepted a 64-bit box size beyond any file")
	}
	b, ok := parseBoxHeader(mk(0, "mdat"), 0, 0)
	if !ok || b.dataLen != -1 {
		t.Errorf("size-0 box = %+v,%v; want a to-EOF box", b, ok)
	}
}

func TestReadVIntRejectsOverlongAndTruncated(t *testing.T) {
	if _, _, _, ok := readVInt([]byte{0x00, 0xFF}, 0, false); ok {
		t.Error("accepted a length marker above 8 bytes")
	}
	if _, _, _, ok := readVInt([]byte{0x1A, 0x45}, 0, true); ok {
		t.Error("accepted a 4-byte ID with only 2 bytes present")
	}
	if _, _, _, ok := readVInt(nil, 0, false); ok {
		t.Error("accepted an empty buffer")
	}
	v, n, _, ok := readVInt([]byte{0x81}, 0, false)
	if !ok || n != 1 || v != 1 {
		t.Errorf("readVInt(0x81) = %d,%d,%v", v, n, ok)
	}
	_, _, allOnes, ok := readVInt([]byte{0xFF}, 0, false)
	if !ok || !allOnes {
		t.Error("0xFF must decode as the unknown-size marker")
	}
}

func TestParseMatroskaHeadRejectsMalformed(t *testing.T) {
	for name, in := range map[string][]byte{
		"empty":            nil,
		"short":            {0x1A, 0x45},
		"not_ebml":         bytes.Repeat([]byte{0x42}, 64),
		"header_only":      ebmlElem(idEBMLHeader, []byte{0x00}),
		"unknown_size_top": append(ebmlIDBytes(idEBMLHeader), 0xFF),
	} {
		if _, err := parseMatroskaHead(in); err == nil {
			t.Errorf("%s: parse accepted malformed input", name)
		}
	}
}

func TestParseMatroskaCuesIgnoresIncompleteCuePoints(t *testing.T) {
	// A CuePoint carrying a time but no cluster position yields no entry
	// rather than an entry at a guessed offset.
	body := ebmlElem(idCuePoint, ebmlElem(idCueTime, ebmlUint(1000)))
	body = append(body, ebmlElem(idCuePoint, ebmlElem(idCueTrackPos, ebmlElem(idCueClusterPos, ebmlUint(50))))...)
	if got := parseMatroskaCues(body, 0, defaultTimecodeScale); len(got) != 0 {
		t.Errorf("entries = %+v, want none from incomplete cue points", got)
	}
}

func TestParseMatroskaCuesGuardsTimecodeOverflow(t *testing.T) {
	cp := ebmlElem(idCueTime, ebmlUint(1<<63))
	cp = append(cp, ebmlElem(idCueTrackPos, ebmlElem(idCueClusterPos, ebmlUint(10)))...)
	body := ebmlElem(idCuePoint, cp)
	if got := parseMatroskaCues(body, 0, defaultTimecodeScale); len(got) != 0 {
		t.Errorf("entries = %+v, want the overflowing cue rejected", got)
	}
}

func TestAnalyzeRejectsTraversalShapedAndOversizedDeclarations(t *testing.T) {
	// A container declaring a moov far beyond EOF must not send the reader
	// chasing an offset outside the source.
	data := buildMP4(mp4Options{
		timescale:   1000,
		sttsRuns:    []sttsRun{{count: 4, delta: 100}},
		syncSamples: []uint32{1},
		sampleSize:  10,
		stscRuns:    []stscRun{{firstChunk: 1, samplesPerChunk: 4}},
		chunkOffs:   []uint32{100},
		moovLast:    true,
		mdatFiller:  128,
	})
	corrupt := append([]byte(nil), data...)
	// Inflate the mdat box size so the walk points past EOF.
	at := bytes.Index(corrupt, []byte("mdat"))
	if at < 4 {
		t.Fatal("fixture has no mdat")
	}
	binary.BigEndian.PutUint32(corrupt[at-4:at], 0x7FFFFFF0)

	src := newCountingSource("test:oversized", corrupt)
	res, err := Analyze(context.Background(), src, smallOpts())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !res.Index.Empty() {
		t.Errorf("index = %+v, want none from an out-of-range declaration", res.Index.Entries)
	}
	if src.read > int64(len(corrupt)) {
		t.Errorf("read %d bytes from a %d-byte source", src.read, len(corrupt))
	}
}

// ---- fuzz targets (DG-03) -------------------------------------------------

// FuzzParseMP4Moov asserts the moov parser never panics and never emits a
// negative or unordered seek point, whatever the bytes.
func FuzzParseMP4Moov(f *testing.F) {
	good, _ := standardMP4(false)
	if at := bytes.Index(good, []byte("moov")); at > 4 {
		if b, ok := parseBoxHeader(good, at-4, 0); ok {
			f.Add(good[at-4+int(b.hdrLen):at-4+int(b.hdrLen)+int(b.dataLen)], 512)
		}
	}
	f.Add([]byte{}, 1)
	f.Add([]byte("moov"), 8)
	f.Add(bytes.Repeat([]byte{0xFF}, 32), 64)

	f.Fuzz(func(t *testing.T, moov []byte, max int) {
		if max <= 0 {
			max = 1
		}
		if max > 4096 {
			max = 4096
		}
		entries, err := parseMP4Moov(moov, max)
		if err != nil {
			if entries != nil {
				t.Fatalf("error result carried %d entries", len(entries))
			}
			return
		}
		for _, e := range entries {
			if e.TimeMS < 0 || e.Offset < 0 {
				t.Fatalf("negative seek point %+v", e)
			}
		}
		idx := finalizeIndex(IndexSourceMP4Moov, entries, max)
		assertIndexInvariants(t, idx, max)
	})
}

// FuzzParseMatroskaHead asserts the leading-window walk never panics and never
// reports a Cues pointer that precedes the Segment it is relative to.
func FuzzParseMatroskaHead(f *testing.F) {
	good, _ := buildMKV(mkvOptions{points: []cuePoint{{0, 10}, {1000, 20}}})
	f.Add(good)
	padded, _ := buildMKV(mkvOptions{points: []cuePoint{{0, 10}}, padding: 64})
	f.Add(padded)
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3})
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0x00}, 32))

	f.Fuzz(func(t *testing.T, head []byte) {
		mh, err := parseMatroskaHead(head)
		if err != nil {
			return
		}
		if mh.segStart < 0 {
			t.Fatalf("negative segment start %d", mh.segStart)
		}
		if mh.cuesAbs >= 0 && mh.cuesAbs < mh.segStart {
			t.Fatalf("cues pointer %d precedes segment start %d", mh.cuesAbs, mh.segStart)
		}
		if mh.scaleNS == 0 {
			t.Fatal("zero timecode scale would divide by zero downstream")
		}
	})
}

// FuzzParseMatroskaCues asserts the Cues parser never panics and never emits a
// seek point outside the source's own coordinate space.
func FuzzParseMatroskaCues(f *testing.F) {
	f.Add(buildCues([]cuePoint{{0, 100}, {5000, 40000}}), int64(0), uint64(defaultTimecodeScale))
	f.Add(buildCues(nil), int64(16), uint64(1))
	f.Add([]byte{}, int64(0), uint64(0))
	f.Add(bytes.Repeat([]byte{0xBB}, 24), int64(1<<40), uint64(1<<40))

	f.Fuzz(func(t *testing.T, cues []byte, segStart int64, scale uint64) {
		if segStart < 0 {
			segStart = -segStart
		}
		entries := parseMatroskaCues(cues, segStart, scale)
		for _, e := range entries {
			if e.TimeMS < 0 || e.Offset < segStart {
				t.Fatalf("seek point %+v outside the segment starting at %d", e, segStart)
			}
		}
		assertIndexInvariants(t, finalizeIndex(IndexSourceMatroskaCues, entries, 256), 256)
	})
}

// FuzzAnalyzeSource drives the whole bounded surface over arbitrary bytes,
// asserting the byte budget is never exceeded and no result is fabricated.
func FuzzAnalyzeSource(f *testing.F) {
	mp4Data, _ := standardMP4(true)
	mkvData, _ := buildMKV(mkvOptions{points: []cuePoint{{0, 10}}, padding: 128})
	f.Add(mp4Data, int64(128), int64(1024))
	f.Add(mkvData, int64(64), int64(512))
	f.Add([]byte("not a container"), int64(16), int64(16))

	f.Fuzz(func(t *testing.T, data []byte, headBytes, indexBytes int64) {
		if len(data) == 0 {
			return
		}
		if headBytes <= 0 {
			headBytes = 1
		}
		if headBytes > 1<<16 {
			headBytes = 1 << 16
		}
		if indexBytes < 0 {
			indexBytes = 0
		}
		if indexBytes > 1<<16 {
			indexBytes = 1 << 16
		}
		src := newCountingSource("fuzz:src", data)
		opts := Options{
			HeadBytes:       headBytes,
			IndexBytes:      indexBytes,
			TailBytes:       headBytes,
			MaxIndexEntries: 64,
			SkipProbe:       true,
		}
		res, err := Analyze(context.Background(), src, opts)
		if err != nil {
			return
		}
		limit := headBytes + indexBytes + headBytes
		if src.read > limit {
			t.Fatalf("read %d bytes against a %d-byte budget", src.read, limit)
		}
		if res.Budget.BytesRead != src.read {
			t.Fatalf("budget accounting %d != source-observed %d", res.Budget.BytesRead, src.read)
		}
		assertIndexInvariants(t, res.Index, 64)
	})
}

func assertIndexInvariants(t *testing.T, idx Index, max int) {
	t.Helper()
	if len(idx.Entries) > max {
		t.Fatalf("index holds %d entries, above the %d cap", len(idx.Entries), max)
	}
	for i, e := range idx.Entries {
		if e.TimeMS < 0 || e.Offset < 0 {
			t.Fatalf("entry %d negative: %+v", i, e)
		}
		if i > 0 && (e.TimeMS <= idx.Entries[i-1].TimeMS || e.Offset <= idx.Entries[i-1].Offset) {
			t.Fatalf("entry %d not strictly increasing: %+v then %+v", i, idx.Entries[i-1], e)
		}
	}
	if len(idx.Entries) > 0 && idx.Source == "" {
		t.Fatal("populated index carries no source label")
	}
	if idx.Complete && idx.Declared != len(idx.Entries) {
		t.Fatalf("complete index declares %d but holds %d", idx.Declared, len(idx.Entries))
	}
}

// TestIndexMarksEstimatedOffsetSpace pins the NNTP-lane property surfaced by
// CG-01: a source that does not report an exact size yields an index whose
// offsets are approximate, and the artifact must say so.
func TestIndexMarksEstimatedOffsetSpace(t *testing.T) {
	data, _ := standardMP4(false)

	exact, err := Analyze(context.Background(), bytesource.NewMemSource("exact", data), Options{SkipProbe: true})
	if err != nil {
		t.Fatalf("Analyze exact: %v", err)
	}
	if exact.Index.Empty() {
		t.Fatal("exact-size case produced no index")
	}
	if exact.Index.Estimated {
		t.Error("exact-size source marked estimated")
	}

	inexact := &failingSource{data: data, failAt: int64(len(data)) + 1, err: nil, exact: false}
	got, err := Analyze(context.Background(), inexact, Options{SkipProbe: true})
	if err != nil {
		t.Fatalf("Analyze inexact: %v", err)
	}
	if got.Index.Empty() {
		t.Fatal("inexact-size case produced no index")
	}
	if !got.Index.Estimated {
		t.Error("inexact-size source not marked estimated")
	}
	if !reflect.DeepEqual(got.Index.Entries, exact.Index.Entries) {
		t.Errorf("entries differ across size exactness: %+v vs %+v", got.Index.Entries, exact.Index.Entries)
	}
}

// TestAnalyzeIsDeterministic re-runs the same analysis repeatedly and asserts
// byte-identical results: media truth must not vary run to run.
func TestAnalyzeIsDeterministic(t *testing.T) {
	data, _ := standardMP4(true)
	opts := smallOpts()
	opts.SkipProbe = false
	opts.Probe = fakeProbe(nil)
	opts.TempDir = t.TempDir()

	first, err := Analyze(context.Background(), bytesource.NewMemSource("det", data), opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	want, err := first.Compact()
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	for i := 0; i < 10; i++ {
		got, err := Analyze(context.Background(), bytesource.NewMemSource("det", data), opts)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		// Elapsed legitimately varies; compare everything else.
		got.Budget.Elapsed = first.Budget.Elapsed
		blob, err := got.Compact()
		if err != nil {
			t.Fatalf("run %d compact: %v", i, err)
		}
		if !bytes.Equal(blob, want) {
			t.Fatalf("run %d differs:\n got %s\nwant %s", i, blob, want)
		}
	}
}
