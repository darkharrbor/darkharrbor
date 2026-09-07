package nntp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/ladder"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

func TestNS43DirectNNTPAggregateMatrix(t *testing.T) {
	t.Run("verified repair coalesces and replays from cache", func(t *testing.T) {
		server := startFakeArticleServer(t, map[string]string{})
		rc := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir()}, discardLogger())
		t.Cleanup(rc.Close)
		sc := &SegmentCache{demandPool: newTestPool(t, server, 1), rc: rc, log: discardLogger()}
		req, reader, _, segments := fixtureRequest(t, 3)
		var repairs atomic.Int64
		src := &nntpChunkSource{
			sc: sc, segments: []NZBSegment{{Number: 1, Bytes: int64(len(segments[3])), MessageID: "missing-ns43"}},
			fileKey: "ns43", repair: func(ctx context.Context, segment int) ([]byte, error) {
				if segment != 0 {
					t.Fatalf("segment=%d, want 0", segment)
				}
				repairs.Add(1)
				result, err := RepairSegment(ctx, reader, req)
				return result.Data, err
			},
		}
		ref := rangecache.ChunkRef{Index: 0, Key: "missing-ns43", Size: int64(len(segments[3]))}
		const readers = 8
		got := make([][]byte, readers)
		errs := make([]error, readers)
		var wg sync.WaitGroup
		for i := range readers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				got[i], errs[i] = rc.GetChunk(context.Background(), src, rangecache.ModeDisk, ref)
			}(i)
		}
		wg.Wait()
		for i := range got {
			if errs[i] != nil || !bytes.Equal(got[i], segments[3]) {
				t.Fatalf("reader %d: bytes=%d err=%v", i, len(got[i]), errs[i])
			}
		}
		if repairs.Load() != 1 || server.bodyCount() != 1 {
			t.Fatalf("repairs=%d BODY=%d, want 1/1", repairs.Load(), server.bodyCount())
		}
		replay, err := rc.GetChunk(context.Background(), src, rangecache.ModeDisk, ref)
		if err != nil || !bytes.Equal(replay, segments[3]) || repairs.Load() != 1 || server.bodyCount() != 1 {
			t.Fatalf("replay bytes=%d err=%v repairs=%d BODY=%d", len(replay), err, repairs.Load(), server.bodyCount())
		}
	})

	t.Run("budget exhaustion preserves provider verdict", func(t *testing.T) {
		server := startFakeArticleServer(t, map[string]string{})
		sc := &SegmentCache{demandPool: newTestPool(t, server, 1), log: discardLogger()}
		req, reader, _, segments := fixtureRequest(t, 2)
		req.Budget.MaxBytes = 1
		var repairErr error
		src := &nntpChunkSource{
			sc: sc, segments: []NZBSegment{{Number: 1, Bytes: int64(len(segments[2])), MessageID: "missing-budget-ns43"}},
			repair: func(ctx context.Context, _ int) ([]byte, error) {
				result, err := RepairSegment(ctx, reader, req)
				repairErr = err
				return result.Data, err
			},
		}
		data, err := src.Fetch(context.Background(), rangecache.ChunkRef{
			Index: 0, Key: "missing-budget-ns43", Size: int64(len(segments[2])),
		}, rangecache.FetchDemand)
		if len(data) != 0 || !errors.Is(err, ErrArticleMissing) || !errors.Is(err, ladder.ErrExhausted) {
			t.Fatalf("data=%d error=%v", len(data), err)
		}
		if !errors.Is(repairErr, ErrPAR2RepairAbstain) || !strings.Contains(repairErr.Error(), "budget") {
			t.Fatalf("repair error=%v", repairErr)
		}
	})

	t.Run("cancellation never enters repair", func(t *testing.T) {
		server := startFakeArticleServer(t, map[string]string{})
		sc := &SegmentCache{demandPool: newTestPool(t, server, 1), log: discardLogger()}
		var repairs atomic.Int64
		src := &nntpChunkSource{
			sc: sc, segments: []NZBSegment{{Number: 1, Bytes: 3, MessageID: "missing-cancel-ns43"}},
			repair: func(context.Context, int) ([]byte, error) {
				repairs.Add(1)
				return nil, nil
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := src.Fetch(ctx, rangecache.ChunkRef{Index: 0, Key: "missing-cancel-ns43", Size: 3}, rangecache.FetchDemand)
		if !errors.Is(err, context.Canceled) || repairs.Load() != 0 {
			t.Fatalf("error=%v repairs=%d", err, repairs.Load())
		}
	})
}
