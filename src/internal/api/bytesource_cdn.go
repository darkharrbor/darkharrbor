package api

import (
	"context"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

// NewCDNByteSource exposes a debrid CDN object as a bytesource.ByteSource
// (TS-1.1; torrent-plan T2, debrid-CDN adapter).
//
// It is a BEHAVIOR-IDENTICAL wrapper over the existing windowed ranged-GET
// path (cdnChunkSource.Fetch), inheriting its expiry/reresolve/416 handling,
// per-node worker discipline, and resolve-cache invalidation. No live playback
// behavior changes; the constructor presents those bytes through the
// random-access substrate consumed by the TS-1.2 archive reader and other
// shared services. TS-1.1 proved it byte-identical to the live path via CG-02.
//
// ExactSize is true (CDN byte offsets are exact); the offset table is
// window-aligned and a tail read costs one range GET (TailCostCheap).
func NewCDNByteSource(key, cdnURL string, total, windowBytes int64, contentType string, reresolve cdnReResolver, invalidate func()) (bytesource.ByteSource, error) {
	return newCDNByteSource(key, cdnURL, total, windowBytes, contentType, reresolve, invalidate, nil, 0, "")
}

func newCDNByteSource(key, cdnURL string, total, windowBytes int64, contentType string, reresolve cdnReResolver, invalidate func(), gov *accountgov.Governor, priority accountgov.Priority, session string) (bytesource.ByteSource, error) {
	if windowBytes <= 0 {
		windowBytes = 16 * 1024 * 1024
	}
	src := makeCDNSourceFromCache(cdnURL, windowBytes, total, contentType, reresolve)
	src.key = key
	src.invalidateResolve = invalidate
	src.gov = gov
	src.govOp = TorrentCDNGovOp
	src.priority = priority
	src.session = session

	return byteSourceFromCDNChunks(key, src, windowBytes)
}

func byteSourceFromCDNChunks(key string, src *cdnChunkSource, windowBytes int64) (bytesource.ByteSource, error) {
	refs := src.Chunks()
	chunks := make([]bytesource.Chunk, len(refs))
	starts := make([]int64, len(refs)+1)
	var acc int64
	for i, r := range refs {
		chunks[i] = bytesource.Chunk{Index: r.Index, Key: r.Key, DeclaredSize: r.Size}
		starts[i] = acc
		acc += r.Size
	}
	starts[len(refs)] = acc

	fetch := func(fctx context.Context, c bytesource.Chunk) ([]byte, error) {
		return src.Fetch(fctx, rangecache.ChunkRef{Index: c.Index, Key: c.Key, Size: c.DeclaredSize}, rangecache.FetchDemand)
	}
	caps := bytesource.Capabilities{
		RangeSupport: true,
		ExactSize:    true,
		TailCost:     bytesource.TailCostCheap,
		Alignment:    windowBytes,
	}
	return bytesource.NewChunked(key, chunks, starts, src.total, caps, fetch)
}

// NewProbedCDNByteSource verifies range support before exposing a governed
// ByteSource. Range-ignoring origins return rangeCapable=false and must not be
// used for archive payload mapping.
func NewProbedCDNByteSource(ctx context.Context, key, cdnURL string, windowBytes int64, reresolve func(context.Context) (string, error), invalidate func(), gov *accountgov.Governor, priority accountgov.Priority, session string) (src bytesource.ByteSource, rangeCapable bool, err error) {
	if windowBytes <= 0 {
		windowBytes = 16 * 1024 * 1024
	}
	var lease *accountgov.Lease
	if gov != nil {
		lease, err = gov.Acquire(ctx, TorrentCDNGovOp, priority, session)
		if err != nil {
			return nil, false, err
		}
		defer lease.Release()
	}
	cdn, _, _, rangeCapable, err := initCDNSource(ctx, cdnURL, windowBytes, cdnReResolver(reresolve))
	if err != nil || !rangeCapable {
		return nil, rangeCapable, err
	}
	cdn.key = key
	cdn.invalidateResolve = invalidate
	cdn.gov = gov
	cdn.govOp = TorrentCDNGovOp
	cdn.priority = priority
	cdn.session = session
	src, err = byteSourceFromCDNChunks(key, cdn, windowBytes)
	return src, rangeCapable, err
}
