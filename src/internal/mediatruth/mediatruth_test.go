package mediatruth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/probe"
)

const fakeProbeJSON = `{"streams":[
 {"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"tags":{"language":"und"}},
 {"codec_type":"audio","codec_name":"aac","tags":{"language":"eng"}},
 {"codec_type":"audio","codec_name":"ac3","tags":{"language":"jpn"}},
 {"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"}},
 {"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"und"}}
],"format":{"duration":"12.500000","bit_rate":"4000000"}}`

func fakeProbe(inspect func(path string)) ProbeFunc {
	return func(ctx context.Context, path string) (*probe.ProbeResult, error) {
		if inspect != nil {
			inspect(path)
		}
		return &probe.ProbeResult{
			JSON:         fakeProbeJSON,
			VideoCodec:   "h264",
			Width:        1920,
			Height:       1080,
			AudioCodec:   "aac",
			DurationSecs: 12.5,
			BitRate:      4000000,
		}, nil
	}
}

func smallOpts() Options {
	return Options{
		HeadBytes:       256,
		TailBytes:       1024,
		IndexBytes:      64 << 10,
		MaxIndexEntries: 512,
		SkipProbe:       true,
	}
}

// ---- index construction --------------------------------------------------

func TestAnalyzeMP4MoovAtFrontBuildsExactIndex(t *testing.T) {
	data, want := standardMP4(false)
	src := bytesource.NewMemSource("test:mp4-front", data)

	res, err := Analyze(context.Background(), src, smallOpts())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.IndexError != "" {
		t.Fatalf("IndexError = %q, want none", res.IndexError)
	}
	if res.Index.Source != IndexSourceMP4Moov {
		t.Fatalf("Index.Source = %q", res.Index.Source)
	}
	if !res.Index.Complete {
		t.Error("Index.Complete = false, want true for an uncapped index")
	}
	if !reflect.DeepEqual(res.Index.Entries, want) {
		t.Fatalf("entries = %+v, want %+v", res.Index.Entries, want)
	}
	if res.Facts.Container != ContainerMP4 {
		t.Errorf("Container = %q", res.Facts.Container)
	}
}

func TestAnalyzeMP4MoovAtEndMatchesMoovAtFront(t *testing.T) {
	front, want := standardMP4(false)
	end, _ := standardMP4(true)
	if bytes.Equal(front, end) {
		t.Fatal("fixture layouts are identical; the test proves nothing")
	}

	res, err := Analyze(context.Background(), bytesource.NewMemSource("test:mp4-end", end), smallOpts())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.IndexError != "" {
		t.Fatalf("IndexError = %q, want none", res.IndexError)
	}
	if !reflect.DeepEqual(res.Index.Entries, want) {
		t.Fatalf("moov-at-end entries = %+v, want %+v", res.Index.Entries, want)
	}
}

func TestAnalyzeMP4WideChunkOffsets(t *testing.T) {
	data := buildMP4(mp4Options{
		timescale:   1000,
		sttsRuns:    []sttsRun{{count: 10, delta: 100}},
		syncSamples: []uint32{1, 5, 9},
		sampleSize:  50,
		stscRuns:    []stscRun{{firstChunk: 1, samplesPerChunk: 5}},
		chunkOffs:   []uint32{1000, 2000},
		mdatFiller:  1024,
		wideOffsets: true,
	})
	res, err := Analyze(context.Background(), bytesource.NewMemSource("test:co64", data), smallOpts())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	_, want := standardMP4(false)
	if !reflect.DeepEqual(res.Index.Entries, want) {
		t.Fatalf("co64 entries = %+v, want %+v", res.Index.Entries, want)
	}
}

func TestAnalyzeMP4WithoutSyncTableStridesWholeTrack(t *testing.T) {
	data := buildMP4(mp4Options{
		timescale:  1000,
		sttsRuns:   []sttsRun{{count: 10, delta: 100}},
		sampleSize: 50,
		stscRuns:   []stscRun{{firstChunk: 1, samplesPerChunk: 5}},
		chunkOffs:  []uint32{1000, 2000},
		mdatFiller: 1024,
	})
	res, err := Analyze(context.Background(), bytesource.NewMemSource("test:nostss", data), smallOpts())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(res.Index.Entries) != 10 {
		t.Fatalf("entries = %d, want 10 (every sample seekable)", len(res.Index.Entries))
	}
	if got := res.Index.Entries[0]; got != (Entry{TimeMS: 0, Offset: 1000}) {
		t.Errorf("first entry = %+v", got)
	}
}

func TestAnalyzeMatroskaCuesInsideHead(t *testing.T) {
	points := []cuePoint{{0, 100}, {5000, 40000}, {10000, 90000}}
	data, segStart := buildMKV(mkvOptions{points: points})

	opts := smallOpts()
	opts.HeadBytes = 4096
	res, err := Analyze(context.Background(), bytesource.NewMemSource("test:mkv-head", data), opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.IndexError != "" {
		t.Fatalf("IndexError = %q", res.IndexError)
	}
	if res.Index.Source != IndexSourceMatroskaCues {
		t.Fatalf("Index.Source = %q", res.Index.Source)
	}
	for i, p := range points {
		want := Entry{TimeMS: int64(p.timeMS), Offset: segStart + int64(p.clusterPos)}
		if res.Index.Entries[i] != want {
			t.Errorf("entry %d = %+v, want %+v", i, res.Index.Entries[i], want)
		}
	}
}

func TestAnalyzeMatroskaCuesFollowsSeekHeadPointer(t *testing.T) {
	points := []cuePoint{{0, 100}, {5000, 40000}, {10000, 90000}}
	// Cues sits past the leading window, reachable only through SeekHead.
	padded, segStart := buildMKV(mkvOptions{points: points, padding: 8192})

	opts := smallOpts()
	opts.HeadBytes = 256

	src := newCountingSource("test:mkv-seekhead", padded)
	res, err := Analyze(context.Background(), src, opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.IndexError != "" {
		t.Fatalf("IndexError = %q, want the SeekHead pointer to resolve", res.IndexError)
	}
	// Cluster positions are Segment-relative, so the expectation is built
	// from this file's own Segment offset rather than another fixture's.
	want := make([]Entry, len(points))
	for i, p := range points {
		want[i] = Entry{TimeMS: int64(p.timeMS), Offset: segStart + int64(p.clusterPos)}
	}
	if !reflect.DeepEqual(res.Index.Entries, want) {
		t.Fatalf("pointer-resolved entries = %+v, want %+v", res.Index.Entries, want)
	}
	// The pointer path must not stream the padding: head plus one targeted
	// read, never the whole file.
	if src.read >= int64(len(padded)) {
		t.Errorf("read %d bytes of a %d-byte source; the targeted read was not bounded", src.read, len(padded))
	}
}

func TestAnalyzeMatroskaTailScanWithoutSeekHead(t *testing.T) {
	points := []cuePoint{{0, 100}, {5000, 40000}}
	data, segStart := buildMKV(mkvOptions{points: points, omitSeekHead: true, padding: 2048})

	opts := smallOpts()
	opts.HeadBytes = 256
	opts.TailBytes = 2048
	res, err := Analyze(context.Background(), bytesource.NewMemSource("test:mkv-tail", data), opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.IndexError != "" {
		t.Fatalf("IndexError = %q, want the bounded tail scan to find Cues", res.IndexError)
	}
	want := Entry{TimeMS: 0, Offset: segStart + 100}
	if res.Index.Entries[0] != want {
		t.Errorf("first entry = %+v, want %+v", res.Index.Entries[0], want)
	}
}

func TestAnalyzeMatroskaWithoutCuesReportsReasonAndFabricatesNothing(t *testing.T) {
	data, _ := buildMKV(mkvOptions{omitCues: true, omitSeekHead: true, padding: 512})

	opts := smallOpts()
	opts.HeadBytes = 128
	res, err := Analyze(context.Background(), bytesource.NewMemSource("test:mkv-nocues", data), opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !res.Index.Empty() {
		t.Fatalf("index = %+v, want empty", res.Index.Entries)
	}
	if res.IndexError != reasonNoIndexStructure {
		t.Errorf("IndexError = %q, want %q", res.IndexError, reasonNoIndexStructure)
	}
	if res.Facts.Container != ContainerMatroska {
		t.Errorf("Container = %q, want the container still identified", res.Facts.Container)
	}
}

// TestZeroOptionsEnableIndexReads pins a live-gate finding: Options{} must
// mean "bounded defaults", not "facts only". The inverse convention silently
// produced an empty seek index for every source whose directory lives outside
// the leading window -- which is most of them.
func TestZeroOptionsEnableIndexReads(t *testing.T) {
	got := Options{}.resolved()
	if got.TailBytes != DefaultTailBytes {
		t.Errorf("zero TailBytes resolved to %d, want %d", got.TailBytes, DefaultTailBytes)
	}
	if got.IndexBytes != DefaultIndexBytes {
		t.Errorf("zero IndexBytes resolved to %d, want %d", got.IndexBytes, DefaultIndexBytes)
	}
	if off := (Options{TailBytes: -1, IndexBytes: -1}).resolved(); off.TailBytes != 0 || off.IndexBytes != 0 {
		t.Errorf("negative did not disable: tail=%d index=%d", off.TailBytes, off.IndexBytes)
	}

	// End to end: a moov-at-end source must yield an index under Options{}.
	data, want := standardMP4(true)
	res, err := Analyze(context.Background(), bytesource.NewMemSource("zero-opts", data), Options{SkipProbe: true})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !reflect.DeepEqual(res.Index.Entries, want) {
		t.Fatalf("Options{} entries = %+v, want %+v", res.Index.Entries, want)
	}
}

// ---- lane neutrality ------------------------------------------------------

// TestAnalyzeIdenticalAcrossSourceShapes is the deterministic half of the
// row's lane-neutrality claim: the same bytes exposed through a flat
// in-memory source and through the chunked assembly path (the shape an NNTP
// segment source carries, at several chunk granularities) must produce a
// byte-identical result. The real per-lane proofs are CG-01/CG-02/CG-03.
func TestAnalyzeIdenticalAcrossSourceShapes(t *testing.T) {
	mp4Data, _ := standardMP4(true)
	mkvData, _ := buildMKV(mkvOptions{points: []cuePoint{{0, 100}, {5000, 40000}}, padding: 4096})

	for _, tc := range []struct {
		name string
		data []byte
	}{{"mp4_moov_at_end", mp4Data}, {"matroska_cues_via_seekhead", mkvData}} {
		t.Run(tc.name, func(t *testing.T) {
			opts := smallOpts()
			opts.SkipProbe = false
			opts.Probe = fakeProbe(nil)

			flat, err := Analyze(context.Background(), bytesource.NewMemSource("lane:flat", tc.data), opts)
			if err != nil {
				t.Fatalf("flat Analyze: %v", err)
			}
			if flat.Index.Empty() {
				t.Fatal("flat analysis produced no index; the case proves nothing")
			}

			for _, chunk := range []int64{7, 64, 512, 4096} {
				src, err := chunkedOver("lane:chunked", tc.data, chunk)
				if err != nil {
					t.Fatalf("chunked source: %v", err)
				}
				got, err := Analyze(context.Background(), src, opts)
				if err != nil {
					t.Fatalf("chunked(%d) Analyze: %v", chunk, err)
				}
				if !reflect.DeepEqual(got.Index, flat.Index) {
					t.Errorf("chunk %d: index = %+v, want %+v", chunk, got.Index, flat.Index)
				}
				if !reflect.DeepEqual(got.Facts, flat.Facts) {
					t.Errorf("chunk %d: facts = %+v, want %+v", chunk, got.Facts, flat.Facts)
				}
				if got.IndexError != flat.IndexError || got.FactsError != flat.FactsError {
					t.Errorf("chunk %d: reasons = %q/%q, want %q/%q",
						chunk, got.IndexError, got.FactsError, flat.IndexError, flat.FactsError)
				}
			}
		})
	}
}

// ---- bounds, failure, cancellation (DG-05, DG-07, DG-09) ------------------

func TestAnalyzeRejectsUnsupportedContainer(t *testing.T) {
	src := bytesource.NewMemSource("test:junk", bytes.Repeat([]byte{0x7F}, 4096))
	if _, err := Analyze(context.Background(), src, smallOpts()); !errors.Is(err, ErrUnsupportedContainer) {
		t.Fatalf("err = %v, want ErrUnsupportedContainer", err)
	}
}

func TestAnalyzeRejectsEmptyAndNilSource(t *testing.T) {
	if _, err := Analyze(context.Background(), nil, smallOpts()); !errors.Is(err, ErrNoSource) {
		t.Fatalf("nil source err = %v, want ErrNoSource", err)
	}
	src := bytesource.NewMemSource("test:empty", nil)
	if _, err := Analyze(context.Background(), src, smallOpts()); !errors.Is(err, ErrTruncatedSource) {
		t.Fatalf("empty source err = %v, want ErrTruncatedSource", err)
	}
}

func TestAnalyzeNeverExceedsItsByteBudget(t *testing.T) {
	data, _ := standardMP4(true)
	src := newCountingSource("test:budget", data)

	opts := smallOpts()
	opts.HeadBytes = 128
	opts.IndexBytes = 1024
	opts.TailBytes = -1 // explicitly disabled

	res, err := Analyze(context.Background(), src, opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	// TailBytes is explicitly disabled, so it contributes nothing.
	limit := opts.HeadBytes + opts.IndexBytes
	if res.Budget.BytesLimit != limit {
		t.Errorf("BytesLimit = %d, want %d", res.Budget.BytesLimit, limit)
	}
	if src.read > limit {
		t.Errorf("source served %d bytes, above the %d-byte budget", src.read, limit)
	}
	if res.Budget.BytesRead != src.read {
		t.Errorf("Budget.BytesRead = %d, source counted %d", res.Budget.BytesRead, src.read)
	}
}

func TestAnalyzeReportsBudgetExhaustionRatherThanReadingMore(t *testing.T) {
	data, _ := standardMP4(true)
	src := newCountingSource("test:tiny-budget", data)

	opts := smallOpts()
	opts.HeadBytes = 64
	opts.IndexBytes = -1 // explicitly disabled
	opts.TailBytes = -1

	res, err := Analyze(context.Background(), src, opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !res.Index.Empty() {
		t.Fatalf("index = %+v, want empty under an exhausted budget", res.Index.Entries)
	}
	if res.IndexError != reasonIndexBudget && res.IndexError != reasonNoIndexStructure {
		t.Errorf("IndexError = %q, want a bound reason", res.IndexError)
	}
	if src.read > opts.HeadBytes {
		t.Errorf("read %d bytes past a %d-byte budget", src.read, opts.HeadBytes)
	}
}

func TestAnalyzeSourceFailureYieldsReasonNotFabrication(t *testing.T) {
	data, _ := standardMP4(true)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"read_failure", errors.New("upstream refused"), reasonSourceRead},
		{"representation_mutated", errSourceMutated, reasonSourceMutated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &failingSource{data: data, failAt: 200, err: tc.err, exact: true}
			opts := smallOpts()
			opts.HeadBytes = 128

			res, err := Analyze(context.Background(), src, opts)
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if !res.Index.Empty() {
				t.Fatalf("index = %+v, want empty after a source failure", res.Index.Entries)
			}
			if res.IndexError != tc.want {
				t.Errorf("IndexError = %q, want %q", res.IndexError, tc.want)
			}
		})
	}
}

func TestAnalyzeHonorsCancellation(t *testing.T) {
	data, _ := standardMP4(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Analyze(ctx, bytesource.NewMemSource("test:cancel", data), smallOpts()); err == nil {
		t.Fatal("Analyze returned nil error for a cancelled context")
	}
}

func TestAnalyzeSpawnsNoGoroutines(t *testing.T) {
	data, _ := standardMP4(true)
	opts := smallOpts()
	opts.SkipProbe = false
	opts.Probe = fakeProbe(nil)

	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 25; i++ {
		if _, err := Analyze(context.Background(), bytesource.NewMemSource("test:leak", data), opts); err != nil {
			t.Fatalf("Analyze: %v", err)
		}
	}
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines %d -> %d; analysis must spawn none", before, after)
	}
}

// ---- probe surface (DG-09 temp discipline, truthful offsets) --------------

func TestProbeTempFileCarriesTrueOffsetsAndIsRemoved(t *testing.T) {
	data, _ := standardMP4(true)
	moovAt := bytes.Index(data, []byte("moov"))
	if moovAt < 4 {
		t.Fatal("fixture has no moov box")
	}

	dir := t.TempDir()
	var seenPath string
	opts := smallOpts()
	opts.HeadBytes = 128
	opts.SkipProbe = false
	opts.TempDir = dir
	opts.Probe = fakeProbe(func(path string) {
		seenPath = path
		st, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat temp: %v", err)
			return
		}
		if st.Size() != int64(len(data)) {
			t.Errorf("temp apparent size = %d, want the source size %d", st.Size(), len(data))
		}
		f, err := os.Open(path)
		if err != nil {
			t.Errorf("open temp: %v", err)
			return
		}
		defer f.Close()
		got := make([]byte, 4)
		if _, err := f.ReadAt(got, int64(moovAt)); err != nil {
			t.Errorf("read temp at moov offset: %v", err)
			return
		}
		if string(got) != "moov" {
			t.Errorf("temp bytes at %d = %q, want the moov box at its true offset", moovAt, got)
		}
	})

	res, err := Analyze(context.Background(), bytesource.NewMemSource("test:temp", data), opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.FactsError != "" {
		t.Fatalf("FactsError = %q", res.FactsError)
	}
	if seenPath == "" {
		t.Fatal("prober was never invoked")
	}
	if _, err := os.Stat(seenPath); !os.IsNotExist(err) {
		t.Errorf("temp file %s survived analysis (err=%v)", seenPath, err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "darkharrbor-mediatruth-*"))
	if len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// TestProbeTempIsSparseAtTrueSizeEvenWithoutTail pins the fix for a defect the
// real IA gate exposed: a head-only probe over a contiguous temp file makes a
// prober derive bitrate from the probe window rather than the media.
func TestProbeTempIsSparseAtTrueSizeEvenWithoutTail(t *testing.T) {
	data, _ := standardMP4(false) // moov at front: no tail region is fetched
	var apparent int64
	var blocks int64
	opts := smallOpts()
	opts.SkipProbe = false
	opts.TempDir = t.TempDir()
	opts.Probe = fakeProbe(func(path string) {
		st, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat temp: %v", err)
			return
		}
		apparent = st.Size()
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			blocks = sys.Blocks
		}
	})

	res, err := Analyze(context.Background(), bytesource.NewMemSource("test:sparse", data), opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if apparent != int64(len(data)) {
		t.Errorf("temp apparent size = %d, want the true source size %d", apparent, len(data))
	}
	if blocks > 0 && blocks*512 > int64(len(data))+(64<<10) {
		t.Errorf("temp allocated %d bytes for a %d-byte sparse file", blocks*512, len(data))
	}
	if res.Facts.SizeBytes != int64(len(data)) {
		t.Errorf("Facts.SizeBytes = %d, want %d", res.Facts.SizeBytes, len(data))
	}
}

func TestProbeFailureLeavesFactsAbsentWithReason(t *testing.T) {
	data, _ := standardMP4(false)
	opts := smallOpts()
	opts.SkipProbe = false
	opts.TempDir = t.TempDir()
	opts.Probe = func(ctx context.Context, path string) (*probe.ProbeResult, error) {
		return nil, errors.New("ffprobe exited 1")
	}

	res, err := Analyze(context.Background(), bytesource.NewMemSource("test:probefail", data), opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.FactsError != reasonProbeFailed {
		t.Errorf("FactsError = %q, want %q", res.FactsError, reasonProbeFailed)
	}
	if res.Facts.VideoCodec != "" || res.Facts.DurationMS != 0 {
		t.Errorf("facts = %+v, want nothing fabricated", res.Facts)
	}
	if res.Index.Empty() {
		t.Error("a probe failure must not discard a successfully built index")
	}
	left, _ := filepath.Glob(filepath.Join(opts.TempDir, "darkharrbor-mediatruth-*"))
	if len(left) != 0 {
		t.Errorf("temp files left behind after probe failure: %v", left)
	}
}

// TestProbeFailedPredicateDistinguishesRealVerdictFromLocalPrepError pins the
// TS-3.3 split: a real prober-ran-and-rejected verdict must report
// ProbeFailed()==true (repair-eligible broken-media evidence), while a local
// DH-host temp-file preparation error -- disk full, permissions, anything
// that never reached the prober at all -- must report ProbeFailed()==false.
// Collapsing these was a real correctness hazard: routing (b) into repair
// would re-add/eventually blacklist a perfectly good item over a host-local
// IO problem that has nothing to do with the torrent's own bytes.
func TestProbeFailedPredicateDistinguishesRealVerdictFromLocalPrepError(t *testing.T) {
	data, _ := standardMP4(false)

	t.Run("real prober verdict is repair-eligible", func(t *testing.T) {
		opts := smallOpts()
		opts.SkipProbe = false
		opts.Probe = func(ctx context.Context, path string) (*probe.ProbeResult, error) {
			return nil, errors.New("ffprobe exited 1")
		}
		res, err := Analyze(context.Background(), bytesource.NewMemSource("test:realfail", data), opts)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		if res.FactsError != reasonProbeFailed {
			t.Fatalf("FactsError = %q, want %q", res.FactsError, reasonProbeFailed)
		}
		if !res.ProbeFailed() {
			t.Error("ProbeFailed() = false, want true for a real prober rejection")
		}
	})

	t.Run("local prep error is not repair-eligible", func(t *testing.T) {
		unwritable := filepath.Join(t.TempDir(), "no-such-dir", "deeper")
		opts := smallOpts()
		opts.SkipProbe = false
		opts.TempDir = unwritable // does not exist -> os.CreateTemp fails
		opts.Probe = func(ctx context.Context, path string) (*probe.ProbeResult, error) {
			t.Fatal("prober must never run when temp-file prep itself failed")
			return nil, nil
		}
		res, err := Analyze(context.Background(), bytesource.NewMemSource("test:prepfail", data), opts)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		if res.FactsError != reasonProbePrepFailed {
			t.Fatalf("FactsError = %q, want %q", res.FactsError, reasonProbePrepFailed)
		}
		if res.ProbeFailed() {
			t.Error("ProbeFailed() = true, want false for a local prep-only error")
		}
	})

	t.Run("nil result is not repair-eligible", func(t *testing.T) {
		var nilRes *Result
		if nilRes.ProbeFailed() {
			t.Error("ProbeFailed() = true on a nil Result, want false")
		}
	})

	t.Run("unavailable prober is not repair-eligible", func(t *testing.T) {
		opts := smallOpts()
		opts.SkipProbe = true
		res, err := Analyze(context.Background(), bytesource.NewMemSource("test:skip", data), opts)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		if res.FactsError != reasonProbeUnavailable {
			t.Fatalf("FactsError = %q, want %q", res.FactsError, reasonProbeUnavailable)
		}
		if res.ProbeFailed() {
			t.Error("ProbeFailed() = true, want false when no prober is configured at all")
		}
	})
}

func TestFactsFromProbeCollectsLanguagesAndDropsUndefined(t *testing.T) {
	f := factsFromProbe(&probe.ProbeResult{
		JSON:         fakeProbeJSON,
		VideoCodec:   "h264",
		Width:        1920,
		Height:       1080,
		AudioCodec:   "aac",
		DurationSecs: 12.5,
		BitRate:      4000000,
	})
	if !reflect.DeepEqual(f.AudioLangs, []string{"eng", "jpn"}) {
		t.Errorf("AudioLangs = %v", f.AudioLangs)
	}
	if !reflect.DeepEqual(f.SubtitleLangs, []string{"eng"}) {
		t.Errorf("SubtitleLangs = %v, want undefined tags dropped", f.SubtitleLangs)
	}
	if f.DurationMS != 12500 {
		t.Errorf("DurationMS = %d, want 12500", f.DurationMS)
	}
	if f.Empty() {
		t.Error("Empty() = true for a populated fact set")
	}
}

// ---- index shaping --------------------------------------------------------

func TestFinalizeIndexCapsEvenlyAndKeepsEdges(t *testing.T) {
	raw := make([]Entry, 5000)
	for i := range raw {
		raw[i] = Entry{TimeMS: int64(i) * 40, Offset: int64(i)*1000 + 7}
	}
	idx := finalizeIndex(IndexSourceMP4Moov, raw, 100)
	if idx.Complete {
		t.Error("Complete = true for a downsampled index")
	}
	if idx.Declared != 5000 {
		t.Errorf("Declared = %d, want 5000", idx.Declared)
	}
	if len(idx.Entries) > 100 {
		t.Fatalf("entries = %d, above the cap", len(idx.Entries))
	}
	if idx.Entries[0] != (Entry{TimeMS: 0, Offset: 7}) {
		t.Errorf("first entry = %+v, want the true first seek point", idx.Entries[0])
	}
	last := idx.Entries[len(idx.Entries)-1]
	if last != (Entry{TimeMS: 4999 * 40, Offset: 4999*1000 + 7}) {
		t.Errorf("last entry = %+v, want the true last seek point", last)
	}
	for i := 1; i < len(idx.Entries); i++ {
		if idx.Entries[i].TimeMS <= idx.Entries[i-1].TimeMS || idx.Entries[i].Offset <= idx.Entries[i-1].Offset {
			t.Fatalf("entry %d not strictly increasing: %+v then %+v", i, idx.Entries[i-1], idx.Entries[i])
		}
	}
}

func TestFinalizeIndexDropsDuplicatesAndNegatives(t *testing.T) {
	idx := finalizeIndex(IndexSourceMatroskaCues, []Entry{
		{TimeMS: 100, Offset: 10},
		{TimeMS: 100, Offset: 20},
		{TimeMS: -5, Offset: 30},
		{TimeMS: 200, Offset: -1},
		{TimeMS: 300, Offset: 40},
	}, 100)
	want := []Entry{{TimeMS: 100, Offset: 10}, {TimeMS: 300, Offset: 40}}
	if !reflect.DeepEqual(idx.Entries, want) {
		t.Errorf("entries = %+v, want %+v", idx.Entries, want)
	}
}

func TestIndexSeekPoint(t *testing.T) {
	idx := Index{Entries: []Entry{{0, 100}, {5000, 900}, {10000, 4000}}}
	for _, tc := range []struct {
		ask   int64
		want  Entry
		found bool
	}{
		{-1, Entry{}, false},
		{0, Entry{0, 100}, true},
		{4999, Entry{0, 100}, true},
		{5000, Entry{5000, 900}, true},
		{99999, Entry{10000, 4000}, true},
	} {
		got, ok := idx.SeekPoint(tc.ask)
		if ok != tc.found || got != tc.want {
			t.Errorf("SeekPoint(%d) = %+v,%v want %+v,%v", tc.ask, got, ok, tc.want, tc.found)
		}
	}
	if _, ok := (Index{}).SeekPoint(0); ok {
		t.Error("empty index returned a seek point")
	}
}

// ---- redaction (DG-04) ----------------------------------------------------

func TestCompactEncodingCarriesOnlyVocabularyReasons(t *testing.T) {
	data, _ := standardMP4(true)
	src := &failingSource{data: data, failAt: 200, err: errors.New("GET https://cdn.example/file?token=SECRET failed"), exact: true}
	opts := smallOpts()
	opts.HeadBytes = 128

	res, err := Analyze(context.Background(), src, opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	blob, err := res.Compact()
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	for _, banned := range []string{"http", "token", "SECRET", "cdn.example", "Authorization"} {
		if strings.Contains(strings.ToLower(string(blob)), strings.ToLower(banned)) {
			t.Fatalf("compact encoding leaked %q: %s", banned, blob)
		}
	}
	vocab := map[string]bool{
		"": true, reasonNoIndexStructure: true, reasonIndexUnparseable: true,
		reasonIndexBudget: true, reasonIndexTruncated: true, reasonProbeFailed: true,
		reasonProbeUnavailable: true, reasonSourceRead: true, reasonSourceMutated: true,
		reasonProbePrepFailed: true,
	}
	if !vocab[res.IndexError] || !vocab[res.FactsError] {
		t.Errorf("reasons outside the fixed vocabulary: %q / %q", res.IndexError, res.FactsError)
	}
	var round Result
	if err := json.Unmarshal(blob, &round); err != nil {
		t.Fatalf("compact encoding does not round-trip: %v", err)
	}
}
