package nntp

import (
	"context"
	"errors"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

// SegmentByteSource exposes one NZB file's yEnc-decoded segments as a
// bytesource.ByteSource (TS-1.1; torrent-plan T2, NNTP-segment adapter).
//
// It is a BEHAVIOR-IDENTICAL wrapper over the existing per-segment demand
// fetch (nntpChunkSource.Fetch): the same pool, retry, yEnc decode, CRC, and
// offset-recording path the streaming cache already uses. No live playback
// behavior changes; this constructor merely presents those bytes through the
// random-access substrate consumed by the TS-1.2 archive reader and HR5.2
// media truth. TS-1.1 introduced it and proved it byte-identical to the live
// path via CG-01.
//
// ExactSize is false: yEnc-decoded length diverges from the NZB-declared byte
// count. When recorded decoded segment offsets exist they are used as the
// offset table so random access is precise; otherwise declared cumulative
// offsets are a best-effort fallback carrying the same small yEnc drift as the
// cold-seek path. The constructor does not itself fetch.
func (sc *SegmentCache) SegmentByteSource(ctx context.Context, fileKey, itemID, contentKey string, fileIndex int, segments []NZBSegment) (bytesource.ByteSource, error) {
	return sc.segmentByteSource(ctx, fileKey, itemID, contentKey, fileIndex, segments, "cache")
}

func (sc *SegmentCache) segmentByteSource(ctx context.Context, fileKey, itemID, contentKey string, fileIndex int, segments []NZBSegment, op string) (bytesource.ByteSource, error) {
	contentKey = SegmentOffsetKey(contentKey, segments)
	cs := &nntpChunkSource{
		sc:         sc,
		segments:   segments,
		itemID:     itemID,
		contentKey: contentKey,
		fileIndex:  fileIndex,
		fileKey:    fileKey,
		op:         op,
	}
	fetch := func(fctx context.Context, c bytesource.Chunk) ([]byte, error) {
		return cs.Fetch(fctx, rangecache.ChunkRef{Index: c.Index, Key: c.Key, Size: c.DeclaredSize}, rangecache.FetchDemand)
	}
	declared := make([]int64, len(segments))
	for i, seg := range segments {
		declared[i] = seg.Bytes
	}
	starts := sc.segmentStarts(ctx, itemID, contentKey, fileIndex, declared)
	return newSegmentByteSource(fileKey, segments, starts, fetch)
}

func (p *NNTPProvider) archiveByteSource(ctx context.Context, fileIndex int, segments []NZBSegment) (bytesource.ByteSource, error) {
	return p.segmentFileByteSource(ctx, fileIndex, segments, "build")
}

func (p *NNTPProvider) recoveryByteSource(ctx context.Context, fileIndex int, segments []NZBSegment) (bytesource.ByteSource, error) {
	return p.segmentFileByteSource(ctx, fileIndex, segments, "recovery")
}

func (p *NNTPProvider) segmentFileByteSource(ctx context.Context, fileIndex int, segments []NZBSegment, op string) (bytesource.ByteSource, error) {
	itemID := accountingItemID(ctx)
	fileKey := "nntp-archive"
	if itemID != "" {
		fileKey += "|" + itemID
	}
	if p.cache != nil {
		return p.cache.segmentByteSource(ctx, fileKey, itemID, "", fileIndex, segments, op)
	}
	starts := make([]int64, len(segments)+1)
	for i, segment := range segments {
		starts[i+1] = starts[i] + segment.Bytes
	}
	fetch := func(fetchCtx context.Context, chunk bytesource.Chunk) ([]byte, error) {
		segment := segments[chunk.Index]
		return fetchArticleBytesWithRetryAt(fetchCtx, p.pool, p.log, op, segment.Number, segment.MessageID, segment.PostedAt, !p.skipCRC)
	}
	return newSegmentByteSource(fileKey, segments, starts, fetch)
}

// segmentStarts returns the cumulative byte-offset table (len = nSeg+1),
// preferring recorded decoded offsets and falling back to declared cumulative
// sizes when none are recorded.
func (sc *SegmentCache) segmentStarts(ctx context.Context, itemID, contentKey string, fileIndex int, declared []int64) []int64 {
	if sc.offsets != nil && itemID != "" {
		if off, _, err := sc.offsets.GetSegmentOffsets(ctx, itemID, contentKey, fileIndex, len(declared), declared); err == nil && len(off) == len(declared)+1 {
			return off
		}
	}
	starts := make([]int64, len(declared)+1)
	var acc int64
	for i, s := range declared {
		starts[i] = acc
		acc += s
	}
	starts[len(declared)] = acc
	return starts
}

// newSegmentByteSource is the deterministic core: it maps segments to chunks
// and builds the ByteSource over the supplied fetch and offset table.
func newSegmentByteSource(fileKey string, segments []NZBSegment, starts []int64, fetch bytesource.FetchFunc) (bytesource.ByteSource, error) {
	chunks := make([]bytesource.Chunk, len(segments))
	for i, seg := range segments {
		chunks[i] = bytesource.Chunk{Index: i, Key: seg.MessageID, DeclaredSize: seg.Bytes}
	}
	size := starts[len(starts)-1]
	caps := bytesource.Capabilities{
		RangeSupport: true,
		ExactSize:    false,
		TailCost:     bytesource.TailCostSegmented,
		Alignment:    0,
	}
	return bytesource.NewChunked(fileKey, chunks, starts, size, caps, fetch)
}

// CrossLaneFile is URL-free metadata for an NNTP media file whose complete
// decoded offset table and exact total are already durable.
type CrossLaneFile struct {
	Index int
	Size  int64
}

// CrossLaneFiles returns at most limit exact, randomly addressable video files.
// Cold or partially learned offsets abstain instead of guessing from NZB sizes.
func (p *NNTPProvider) CrossLaneFiles(ctx context.Context, itemID string, nzbData []byte, limit int) ([]CrossLaneFile, error) {
	if p == nil || p.cache == nil || p.cache.offsets == nil || itemID == "" || limit <= 0 {
		return nil, nil
	}
	parsed, err := ParseNZB(nzbData)
	if err != nil {
		return nil, err
	}
	files := filterNZBVideoFiles(parsed.Files)
	if len(files) > limit {
		return nil, errors.New("nntp: cross-lane file inventory exceeds limit")
	}
	out := make([]CrossLaneFile, 0, len(files))
	for i := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, size, ok, layoutErr := p.crossLaneLayout(ctx, itemID, ContentKey(nzbData), i, files[i])
		if layoutErr != nil {
			return nil, layoutErr
		}
		if ok {
			out = append(out, CrossLaneFile{Index: i, Size: size})
		}
	}
	return out, nil
}

// CrossLaneFileSource revalidates and opens one exact NNTP file lazily.
func (p *NNTPProvider) CrossLaneFileSource(ctx context.Context, itemID string, nzbData []byte, fileIndex int, size int64) (bytesource.ByteSource, error) {
	if p == nil || p.cache == nil || p.cache.offsets == nil || itemID == "" || size <= 0 {
		return nil, errors.New("nntp: cross-lane source unavailable")
	}
	parsed, err := ParseNZB(nzbData)
	if err != nil {
		return nil, err
	}
	files := filterNZBVideoFiles(parsed.Files)
	if fileIndex < 0 || fileIndex >= len(files) {
		return nil, errors.New("nntp: cross-lane file index unavailable")
	}
	contentKey := ContentKey(nzbData)
	_, exact, ok, err := p.crossLaneLayout(ctx, itemID, contentKey, fileIndex, files[fileIndex])
	if err != nil {
		return nil, err
	}
	if !ok || exact != size {
		return nil, errors.New("nntp: cross-lane file is not exact")
	}
	src, err := p.cache.segmentByteSource(ctx, "nntp-crosslane|"+itemID, itemID, contentKey, fileIndex, files[fileIndex].Segments, "recovery")
	if err != nil || src.Size() != size {
		return nil, errors.New("nntp: cross-lane source geometry disagrees")
	}
	return exactSegmentSource{ByteSource: src}, nil
}

func (p *NNTPProvider) crossLaneLayout(ctx context.Context, itemID, contentKey string, fileIndex int, file NZBFile) ([]int64, int64, bool, error) {
	if len(file.Segments) == 0 {
		return nil, 0, false, nil
	}
	declared := make([]int64, len(file.Segments))
	for i := range file.Segments {
		declared[i] = file.Segments[i].Bytes
	}
	offsetKey := SegmentOffsetKey(contentKey, file.Segments)
	offsets, recorded, err := p.cache.offsets.GetSegmentOffsets(ctx, itemID, offsetKey, fileIndex, len(file.Segments), declared)
	if err != nil {
		return nil, 0, false, err
	}
	total, err := p.cache.offsets.GetFileTotal(ctx, itemID, offsetKey, fileIndex)
	if err != nil {
		return nil, 0, false, err
	}
	if len(offsets) != len(file.Segments)+1 || len(recorded) != len(file.Segments) || total <= 0 || offsets[0] != 0 || offsets[len(offsets)-1] != total {
		return nil, 0, false, nil
	}
	for i := range file.Segments {
		if _, ok := recorded[i]; !ok || offsets[i+1] <= offsets[i] {
			return nil, 0, false, nil
		}
	}
	return offsets, total, true, nil
}

type exactSegmentSource struct {
	bytesource.ByteSource
}

func (s exactSegmentSource) Caps() bytesource.Capabilities {
	caps := s.ByteSource.Caps()
	caps.ExactSize = true
	return caps
}
