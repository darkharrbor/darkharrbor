package nntp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/ladder"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

func TestPAR2RepairReadsUseBoundedDemandPool(t *testing.T) {
	demandServer := startFakeArticleServer(t, map[string]string{"good@example": "222 body follows"})
	readaheadServer := startFakeArticleServer(t, map[string]string{"good@example": "222 body follows"})
	sc := &SegmentCache{
		demandPool: newTestPool(t, demandServer, 1),
		raPool:     newTestPool(t, readaheadServer, 1),
		log:        discardLogger(),
	}
	p := &NNTPProvider{cache: sc}
	src, err := p.archiveByteSource(context.Background(), 0, []NZBSegment{{
		Number: 1, Bytes: 3, MessageID: "good@example",
	}})
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	buf := make([]byte, 3)
	if _, err := src.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ABC" {
		t.Fatalf("decoded bytes = %q", buf)
	}
	if demandServer.bodyCount() != 1 || readaheadServer.bodyCount() != 0 {
		t.Fatalf("BODY commands demand=%d readahead=%d, want 1/0",
			demandServer.bodyCount(), readaheadServer.bodyCount())
	}
}

func TestPAR2RepairUsesRecoveryPriority(t *testing.T) {
	if got := rungPriorityForOp("recovery"); got != accountgov.PriorityRecovery {
		t.Fatalf("recovery priority = %v, want %v", got, accountgov.PriorityRecovery)
	}
}

func TestPAR2RepairSlotWaitIsCancellable(t *testing.T) {
	s := &par2RepairSession{slot: make(chan struct{}, 1)}
	s.slot <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.RepairSegment(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked repair returned %v, want context cancellation", err)
	}
}

func TestPAR2Rung4AbstainPreservesProviderError(t *testing.T) {
	server := startFakeArticleServer(t, map[string]string{})
	sc := &SegmentCache{demandPool: newTestPool(t, server, 1), log: discardLogger()}
	src := &nntpChunkSource{
		sc:       sc,
		segments: []NZBSegment{{Number: 1, Bytes: 3, MessageID: "missing@example"}},
		repair: func(context.Context, int) ([]byte, error) {
			return nil, ErrPAR2RepairAbstain
		},
	}
	_, err := src.Fetch(context.Background(), rangecache.ChunkRef{
		Index: 0, Key: "missing@example", Size: 3,
	}, rangecache.FetchDemand)
	if !errors.Is(err, ErrArticleMissing) || !errors.Is(err, ladder.ErrExhausted) {
		t.Fatalf("rung-4 abstain changed provider error identity: %v", err)
	}
}

func TestPAR2Rung4ReturnsVerifiedRepairAfterProviderExhaustion(t *testing.T) {
	server := startFakeArticleServer(t, map[string]string{})
	sc := &SegmentCache{demandPool: newTestPool(t, server, 1), log: discardLogger()}
	repairs := 0
	src := &nntpChunkSource{
		sc:       sc,
		segments: []NZBSegment{{Number: 1, Bytes: 3, MessageID: "missing@example"}},
		repair: func(_ context.Context, segment int) ([]byte, error) {
			repairs++
			if segment != 0 {
				t.Fatalf("repair segment=%d, want 0", segment)
			}
			return []byte("ABC"), nil
		},
	}
	data, err := src.Fetch(context.Background(), rangecache.ChunkRef{
		Index: 0, Key: "missing@example", Size: 3,
	}, rangecache.FetchDemand)
	if err != nil || string(data) != "ABC" {
		t.Fatalf("repaired fetch data=%q err=%v", data, err)
	}
	if server.bodyCount() != 1 || repairs != 1 {
		t.Fatalf("BODY commands=%d repairs=%d, want 1/1", server.bodyCount(), repairs)
	}
}

func TestPAR2Rung4ConcurrentReadersCoalesceAndCache(t *testing.T) {
	server := startFakeArticleServer(t, map[string]string{})
	rc := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir()}, discardLogger())
	t.Cleanup(rc.Close)
	sc := &SegmentCache{demandPool: newTestPool(t, server, 1), rc: rc, log: discardLogger()}
	var repairs atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	src := &nntpChunkSource{
		sc:       sc,
		segments: []NZBSegment{{Number: 1, Bytes: 3, MessageID: "missing@example"}},
		repair: func(ctx context.Context, _ int) ([]byte, error) {
			repairs.Add(1)
			once.Do(func() { close(entered) })
			select {
			case <-release:
				return []byte("ABC"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	ref := rangecache.ChunkRef{Index: 0, Key: "missing@example", Size: 3}

	const readers = 8
	results := make([][]byte, readers)
	errs := make([]error, readers)
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = rc.GetChunk(context.Background(), src, rangecache.ModeDisk, ref)
		}(i)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("repair did not start")
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if repairs.Load() != 1 || server.bodyCount() != 1 {
		t.Fatalf("repairs=%d BODY commands=%d, want 1/1", repairs.Load(), server.bodyCount())
	}
	for i := range results {
		if errs[i] != nil || string(results[i]) != "ABC" {
			t.Fatalf("reader %d data=%q err=%v", i, results[i], errs[i])
		}
	}
	data, err := rc.GetChunk(context.Background(), src, rangecache.ModeDisk, ref)
	if err != nil || string(data) != "ABC" || repairs.Load() != 1 || server.bodyCount() != 1 {
		t.Fatalf("cached replay data=%q err=%v repairs=%d BODY=%d",
			data, err, repairs.Load(), server.bodyCount())
	}
}

func TestEligibleForPAR2Repair(t *testing.T) {
	if !eligibleForRepair(ErrArticleMissing) || !eligibleForRepair(ladder.ErrExhausted) {
		t.Fatal("article exhaustion must be repairable")
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, ladder.ErrAccountLevel} {
		if eligibleForRepair(err) {
			t.Fatalf("error %v must not enter repair", err)
		}
	}
}
