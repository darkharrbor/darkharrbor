package nntp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

type offsetRecordCall struct {
	itemID       string
	fileIndex    int
	segIndex     int
	decodedBytes int
}

type fakeSegmentOffsetStore struct {
	mu          sync.Mutex
	recordCalls []offsetRecordCall
	recordErr   func() error
	getCalls    int
	get         func(fileIndex, segCount int, declaredSizes []int64) ([]int64, map[int]struct{}, error)
	fileTotal   int64
}

func (f *fakeSegmentOffsetStore) RecordSegmentSize(_ context.Context, itemID, _ string, fileIndex, segIndex int, decodedBytes int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCalls = append(f.recordCalls, offsetRecordCall{itemID, fileIndex, segIndex, decodedBytes})
	if f.recordErr != nil {
		return f.recordErr()
	}
	return nil
}

func (f *fakeSegmentOffsetStore) GetSegmentOffsets(_ context.Context, _, _ string, fileIndex, segCount int, declaredSizes []int64) ([]int64, map[int]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.get == nil {
		return nil, nil, nil
	}
	return f.get(fileIndex, segCount, declaredSizes)
}

func (f *fakeSegmentOffsetStore) RecordFileTotal(_ context.Context, _, _ string, _ int, decodedBytes int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fileTotal = decodedBytes
	return nil
}

func (f *fakeSegmentOffsetStore) GetFileTotal(_ context.Context, _, _ string, _ int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fileTotal, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCrossLaneFilesRequiresCompleteExactOffsets(t *testing.T) {
	nzbData := []byte(`<?xml version="1.0"?><nzb><file subject="&quot;video.mkv&quot;"><segments><segment bytes="10" number="1">fixture-one</segment><segment bytes="10" number="2">fixture-two</segment></segments></file></nzb>`)
	offsets := &fakeSegmentOffsetStore{
		fileTotal: 18,
		get: func(_ int, _ int, _ []int64) ([]int64, map[int]struct{}, error) {
			return []int64{0, 9, 18}, map[int]struct{}{0: {}, 1: {}}, nil
		},
	}
	provider := &NNTPProvider{cache: &SegmentCache{offsets: offsets, log: discardLogger()}}
	files, err := provider.CrossLaneFiles(context.Background(), "item", nzbData, 4)
	if err != nil || len(files) != 1 || files[0].Index != 0 || files[0].Size != 18 {
		t.Fatalf("exact files=%+v err=%v", files, err)
	}
	source, err := provider.CrossLaneFileSource(context.Background(), "item", nzbData, 0, 18)
	if err != nil || source.Size() != 18 || !source.Caps().RangeSupport || !source.Caps().ExactSize {
		t.Fatalf("exact source size=%d caps=%+v err=%v", source.Size(), source.Caps(), err)
	}

	offsets.get = func(_ int, _ int, _ []int64) ([]int64, map[int]struct{}, error) {
		return []int64{0, 9, 18}, map[int]struct{}{0: {}}, nil
	}
	files, err = provider.CrossLaneFiles(context.Background(), "item", nzbData, 4)
	if err != nil || len(files) != 0 {
		t.Fatalf("partial offsets were eligible: files=%+v err=%v", files, err)
	}
	if _, err := provider.CrossLaneFileSource(context.Background(), "item", nzbData, 0, 18); err == nil {
		t.Fatal("partial offsets opened an exact source")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.CrossLaneFiles(ctx, "item", nzbData, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestSegmentOffsetRecordedSetDeduplicatesAndIsolatesFiles(t *testing.T) {
	store := &fakeSegmentOffsetStore{}
	sc := &SegmentCache{offsets: store, log: discardLogger()}
	sc.seedRecorded("item", 3, map[int]struct{}{0: {}, 2: {}})
	sc.recordSegmentSize("item", "content", 3, 0, 100)
	sc.recordSegmentSize("item", "content", 3, 2, 100)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc.recordSegmentSize("item", "content", 3, 1, 99)
		}()
	}
	wg.Wait()
	sc.recordSegmentSize("item", "content", 4, 1, 98)
	sc.recordSegmentSize("other", "content", 3, 1, 97)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.recordCalls) != 3 {
		t.Fatalf("record calls = %+v, want one call for each distinct item/file/index", store.recordCalls)
	}
}

func TestSegmentOffsetEnqueueFailureCanRetry(t *testing.T) {
	attempts := 0
	store := &fakeSegmentOffsetStore{recordErr: func() error {
		attempts++
		if attempts == 1 {
			return errors.New("queue full")
		}
		return nil
	}}
	sc := &SegmentCache{offsets: store, log: discardLogger()}
	sc.recordSegmentSize("item", "content", 0, 7, 100)
	sc.recordSegmentSize("item", "content", 0, 7, 100)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.recordCalls) != 2 {
		t.Fatalf("record calls = %d, want failed enqueue plus retry", len(store.recordCalls))
	}
}

func TestFileTotalCaptureIsRawDirectOnly(t *testing.T) {
	cache := &SegmentCache{raPool: &Pool{}}
	p := &NNTPProvider{cache: cache}
	segments := []NZBSegment{{Number: 1, Bytes: 100, MessageID: "one"}}

	raw := p.PrewarmFileSource("item", "content", 0, segments)
	rawSource, ok := raw.(*nntpChunkSource)
	if !ok || !rawSource.captureFileTotal {
		t.Fatalf("raw direct source captureFileTotal = %v, want true", ok && rawSource.captureFileTotal)
	}

	zip := p.PrewarmZIPSource(context.Background(), "item", "content", ZIPEntry{
		EntryIdx:   0,
		NZBFileIdx: 0,
		DataOff:    0,
		Segments:   segments,
	})
	zipSource, ok := zip.(*nntpChunkSource)
	if !ok {
		t.Fatalf("ZIP prewarm source type = %T, want *nntpChunkSource", zip)
	}
	if zipSource.captureFileTotal {
		t.Fatal("ZIP backing source must not capture yEnc file total as logical media size")
	}
}

func TestCorrectedTotalUsesOnlyExactFileTotal(t *testing.T) {
	store := &fakeSegmentOffsetStore{
		fileTotal: 2_100_000_000,
		get: func(_ int, _ int, _ []int64) ([]int64, map[int]struct{}, error) {
			return []int64{0, 1_900_000_000}, map[int]struct{}{0: {}}, nil
		},
	}
	sc := &SegmentCache{offsets: store, log: discardLogger()}
	got := sc.CorrectedTotal(context.Background(), "item", "content", 0, 1, []int64{2_200_000_000})
	if got != 2_100_000_000 {
		t.Fatalf("corrected total = %d, want exact file total", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.getCalls != 0 {
		t.Fatalf("CorrectedTotal consulted partial segment offsets %d times", store.getCalls)
	}
}

func TestSegmentOffsetKeyIsStableAcrossFileOrdering(t *testing.T) {
	a := []NZBSegment{{Number: 2, MessageID: "b"}, {Number: 1, MessageID: "a"}}
	b := []NZBSegment{{Number: 1, MessageID: "a"}, {Number: 2, MessageID: "b"}}
	if SegmentOffsetKey("nzb:x", a) != SegmentOffsetKey("nzb:x", b) {
		t.Fatal("equivalent segment sets produced different offset keys")
	}
	if SegmentOffsetKey("nzb:x", a) == SegmentOffsetKey("nzb:x", []NZBSegment{{Number: 1, MessageID: "other"}}) {
		t.Fatal("different files produced the same offset key")
	}
}

func TestNNTPPrewarmSourcesUsePlaybackIdentityAndReadaheadPool(t *testing.T) {
	demandServer := startFakeArticleServer(t, map[string]string{})
	readaheadServer := startFakeArticleServer(t, map[string]string{})
	demand := newTestPool(t, demandServer, 1)
	readahead := newTestPool(t, readaheadServer, 1)
	sc := &SegmentCache{
		demandPool: demand,
		raPool:     readahead,
		log:        discardLogger(),
		recorded:   make(map[segmentOffsetFile]map[int]struct{}),
		rarLayouts: make(map[string]rarLayoutCacheEntry),
	}
	p := &NNTPProvider{cache: sc}
	segments := []NZBSegment{
		{Number: 1, Bytes: 100, MessageID: "first@example"},
		{Number: 2, Bytes: 100, MessageID: "second@example"},
		{Number: 3, Bytes: 100, MessageID: "third@example"},
	}

	raw := p.PrewarmFileSource("item", "content", 2, segments)
	if raw == nil {
		t.Fatal("raw prewarm source is nil")
	}
	if got := raw.(rangecache.PayloadKeyer).PayloadKey(raw.Chunks()[0]); got != "nntp|first@example" {
		t.Fatalf("raw payload key = %q", got)
	}
	if _, err := raw.Fetch(context.Background(), raw.Chunks()[0], rangecache.FetchDemand); err == nil {
		t.Fatal("missing article fetch unexpectedly succeeded")
	}
	if demandServer.bodyCount() != 0 || readaheadServer.bodyCount() != 1 {
		t.Fatalf("BODY commands demand=%d readahead=%d, want 0/1", demandServer.bodyCount(), readaheadServer.bodyCount())
	}

	rar := p.PrewarmRARSource(context.Background(), "item", "content", []RARPart{{
		HeaderBytes: 10,
		Segments:    segments,
	}})
	if rar == nil || len(rar.Chunks()) != 3 {
		t.Fatalf("RAR source = %#v", rar)
	}
	if got := rar.(rangecache.PayloadKeyer).PayloadKey(rar.Chunks()[0]); got != "nntp|first@example" {
		t.Fatalf("RAR payload key = %q", got)
	}

	zip := p.PrewarmZIPSource(context.Background(), "item", "content", ZIPEntry{
		EntryIdx:   4,
		NZBFileIdx: 1,
		DataOff:    150,
		Segments:   segments,
	})
	if zip == nil {
		t.Fatal("ZIP prewarm source is nil")
	}
	zipRefs := zip.Chunks()
	if len(zipRefs) != 2 || zipRefs[0].Index != 1 || zipRefs[0].Key != "second@example" {
		t.Fatalf("ZIP refs = %+v, want suffix beginning with containing segment", zipRefs)
	}
	if rungPriorityForOp("prewarm") != accountgov.PriorityPrewarm {
		t.Fatal("prewarm ladder operation does not carry PriorityPrewarm")
	}
}

func TestNNTPPrewarmSourceUnavailableWithoutReservedPool(t *testing.T) {
	p := &NNTPProvider{cache: &SegmentCache{}}
	segments := []NZBSegment{{Number: 1, Bytes: 100, MessageID: "one@example"}}
	if got := p.PrewarmFileSource("item", "content", 0, segments); got != nil {
		t.Fatal("prewarm must fail closed when no reserved readahead pool exists")
	}
}

func TestNNTPSeekSourcesReconstructPlaybackViews(t *testing.T) {
	readaheadServer := startFakeArticleServer(t, map[string]string{})
	readahead := newTestPool(t, readaheadServer, 1)
	sc := &SegmentCache{
		raPool:     readahead,
		log:        discardLogger(),
		recorded:   make(map[segmentOffsetFile]map[int]struct{}),
		rarLayouts: make(map[string]rarLayoutCacheEntry),
	}
	p := &NNTPProvider{cache: sc}

	rawNZB := []byte(`<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><file subject="episode.mkv"><segments><segment bytes="100" number="1">part-one</segment><segment bytes="100" number="2">part-two</segment></segments></file></nzb>`)
	raw := p.SeekFileSource("item", rawNZB, 0)
	if raw == nil || raw.Key() != "nntp|item|0|2" || len(raw.Chunks()) != 2 {
		t.Fatalf("raw seek source = %#v", raw)
	}

	rarJSON, err := MarshalRARManifest([]RARPart{{
		HeaderBytes: 10,
		Segments: []NZBSegment{
			{Number: 1, Bytes: 100, MessageID: "part-one"},
			{Number: 2, Bytes: 100, MessageID: "part-two"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rar := p.SeekRARSource(context.Background(), "item", "content", rarJSON)
	if rar == nil || rar.Key() != "rar|item|2" {
		t.Fatalf("RAR seek source = %#v", rar)
	}

	zipJSON, err := MarshalZIPManifest([]ZIPEntry{{
		EntryIdx:       0,
		NZBFileIdx:     0,
		DataOff:        50,
		CompressedSize: 100,
		Uncompressed:   100,
		Method:         0,
		Segments: []NZBSegment{
			{Number: 1, Bytes: 100, MessageID: "part-one"},
			{Number: 2, Bytes: 100, MessageID: "part-two"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	zip := p.SeekZIPSource(context.Background(), "item", "content", zipJSON, 0)
	if zip == nil || zip.Key() != "zip|item|0" {
		t.Fatalf("ZIP seek source = %#v", zip)
	}
	refs := zip.Chunks()
	if len(refs) != 2 || refs[0].Size != 50 || refs[1].Size != 50 {
		t.Fatalf("ZIP logical refs = %+v", refs)
	}
	transform := zip.(rangecache.ChunkTransformer)
	if got := len(transform.TransformChunk(refs[0], make([]byte, 100))); got != 50 {
		t.Fatalf("first ZIP logical chunk bytes = %d", got)
	}
	if got := len(transform.TransformChunk(refs[1], make([]byte, 100))); got != 50 {
		t.Fatalf("last ZIP logical chunk bytes = %d", got)
	}

	deflated := strings.Replace(zipJSON, `"method":0`, `"method":8`, 1)
	if got := p.SeekZIPSource(context.Background(), "item", "content", deflated, 0); got != nil {
		t.Fatal("deflated ZIP unexpectedly exposed a random-access seek source")
	}
}

func TestRARLayoutCacheReusesOffsetsMetaAndRefs(t *testing.T) {
	store := &fakeSegmentOffsetStore{get: func(_ int, segCount int, declared []int64) ([]int64, map[int]struct{}, error) {
		offsets := make([]int64, segCount+1)
		var cumulative int64
		for i := 0; i < segCount; i++ {
			offsets[i] = cumulative
			size := declared[i]
			if i == 0 {
				size -= 5
			}
			cumulative += size
		}
		offsets[segCount] = cumulative
		return offsets, map[int]struct{}{0: {}}, nil
	}}
	sc := &SegmentCache{offsets: store, log: discardLogger()}
	manifest := []RARPart{
		{HeaderBytes: 10, Segments: []NZBSegment{
			{Number: 1, Bytes: 100, MessageID: "a"},
			{Number: 2, Bytes: 120, MessageID: "b"},
		}},
		{HeaderBytes: 20, Segments: []NZBSegment{
			{Number: 1, Bytes: 200, MessageID: "c"},
		}},
	}

	first := sc.loadRARLayout(context.Background(), "item", "content", manifest)
	second := sc.loadRARLayout(context.Background(), "item", "content", manifest)
	store.mu.Lock()
	getCalls := store.getCalls
	store.mu.Unlock()
	if getCalls != len(manifest) {
		t.Fatalf("GetSegmentOffsets calls = %d, want %d on one layout build", getCalls, len(manifest))
	}
	if !reflect.DeepEqual(first.correctedOffsets, []int64{0, 85, 205, 380}) {
		t.Fatalf("corrected offsets = %v", first.correctedOffsets)
	}
	if len(first.meta) != 3 || len(first.refs) != 3 {
		t.Fatalf("layout sizes: meta=%d refs=%d, want 3 each", len(first.meta), len(first.refs))
	}
	if got := []string{first.refs[0].Key, first.refs[1].Key, first.refs[2].Key}; !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("ref keys = %v", got)
	}
	if got := []int64{first.refs[0].Size, first.refs[1].Size, first.refs[2].Size}; !reflect.DeepEqual(got, []int64{90, 120, 180}) {
		t.Fatalf("ref sizes = %v", got)
	}
	if &first.meta[0] != &second.meta[0] || &first.refs[0] != &second.refs[0] || &first.correctedOffsets[0] != &second.correctedOffsets[0] {
		t.Fatal("layout cache did not reuse meta, refs, and corrected offsets")
	}
	if chunks := (&rarSegSource{refs: first.refs}).Chunks(); &chunks[0] != &first.refs[0] {
		t.Fatal("rarSegSource rebuilt cached refs")
	}

	// Exact recorded-index results seed the shared set; these must not enqueue.
	sc.recordSegmentSize("item", "content", 0, 0, 85)
	sc.recordSegmentSize("item", "content", 1, 0, 175)
	store.mu.Lock()
	if len(store.recordCalls) != 0 {
		t.Fatalf("seeded indexes re-enqueued: %+v", store.recordCalls)
	}
	store.mu.Unlock()

	sc.rarMu.Lock()
	expired := sc.rarLayouts["content"]
	expired.expires = time.Now().Add(-time.Second)
	sc.rarLayouts["content"] = expired
	sc.rarMu.Unlock()
	third := sc.loadRARLayout(context.Background(), "item", "content", manifest)
	store.mu.Lock()
	getCalls = store.getCalls
	store.mu.Unlock()
	if getCalls != 2*len(manifest) {
		t.Fatalf("GetSegmentOffsets calls after expiry = %d, want %d", getCalls, 2*len(manifest))
	}
	if &third.refs[0] == &first.refs[0] {
		t.Fatal("expired layout was not rebuilt")
	}
}
