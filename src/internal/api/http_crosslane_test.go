package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

type failedHTTPChunkSource struct {
	fetches atomic.Int32
}

func (s *failedHTTPChunkSource) Key() string { return "failed-http-origin" }
func (s *failedHTTPChunkSource) Chunks() []rangecache.ChunkRef {
	return []rangecache.ChunkRef{{Index: 0, Key: "0", Size: 4}}
}
func (s *failedHTTPChunkSource) Fetch(context.Context, rangecache.ChunkRef, rangecache.FetchClass) ([]byte, error) {
	s.fetches.Add(1)
	return nil, errors.New("origin unavailable")
}
func (*failedHTTPChunkSource) ExactSizes() bool { return true }

func TestHR62LadderRecoversOnlyAfterOriginFailure(t *testing.T) {
	origin := &failedHTTPChunkSource{}
	src := newHTTPLadderChunkSource(origin, nil, nil, "http:test", "session")
	var recoveries atomic.Int32
	src.recover = func(ctx context.Context, start, end int64) ([]byte, error) {
		recoveries.Add(1)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if start != 0 || end != 3 {
			t.Fatalf("recovery span = %d-%d, want 0-3", start, end)
		}
		return []byte("good"), nil
	}
	got, err := src.Fetch(context.Background(), src.Chunks()[0], rangecache.FetchDemand)
	if err != nil || !bytes.Equal(got, []byte("good")) {
		t.Fatalf("Fetch = %q, %v", got, err)
	}
	if origin.fetches.Load() != 1 || recoveries.Load() != 1 {
		t.Fatalf("calls = origin:%d recovery:%d, want 1/1", origin.fetches.Load(), recoveries.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Fetch(ctx, src.Chunks()[0], rangecache.FetchDemand); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Fetch error = %v", err)
	}
	if recoveries.Load() != 1 {
		t.Fatalf("cancelled Fetch reached recovery: %d calls", recoveries.Load())
	}
}

func TestHR62MappedHTTPProofSelectionFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	valid := contentproof.Evidence{
		RepresentationID: "http:target",
		Scope:            contentproof.ScopeBlock,
		Offset:           4,
		Length:           4,
		Kind:             contentproof.KindAuthoritative,
		Algorithm:        contentproof.AlgorithmMD5,
		Digest:           bytes.Repeat([]byte{1}, 16),
		Provenance:       contentproof.ProvenancePAR2IFSC,
		ObservedAt:       now,
	}
	if got, ok := mappedHTTPProofAt([]contentproof.Evidence{valid}, "http:target", 12, 6); !ok || got.Offset != 4 || got.Length != 4 {
		t.Fatalf("valid mapped proof = %#v, %v", got, ok)
	}
	tests := []struct {
		name  string
		proof contentproof.Evidence
	}{
		{"wrong representation", func() contentproof.Evidence { p := valid; p.RepresentationID = "http:other"; return p }()},
		{"native HTTP provenance", func() contentproof.Evidence { p := valid; p.Provenance = contentproof.ProvenanceHTTPDigest; return p }()},
		{"hint", func() contentproof.Evidence { p := valid; p.Kind = contentproof.KindHint; return p }()},
		{"whole", func() contentproof.Evidence { p := valid; p.Scope = contentproof.ScopeWhole; return p }()},
		{"short", func() contentproof.Evidence { p := valid; p.Length = 0; return p }()},
		{"uncovered", func() contentproof.Evidence { p := valid; p.Offset = 8; return p }()},
		{"oversized", func() contentproof.Evidence { p := valid; p.Length = contentproof.MaxMappingProofBytes + 1; return p }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, ok := mappedHTTPProofAt([]contentproof.Evidence{test.proof}, "http:target", 12, 6); ok {
				t.Fatalf("unsafe proof selected: %#v", got)
			}
		})
	}
}

func TestHR62CanonicalAliasProofCanAuthorizeRecovery(t *testing.T) {
	proof := contentproof.Evidence{
		RepresentationID: "torrent:route",
		Scope:            contentproof.ScopeBlock,
		Offset:           0,
		Length:           4,
		Kind:             contentproof.KindAuthoritative,
		Algorithm:        contentproof.AlgorithmSHA1,
		Digest:           bytes.Repeat([]byte{1}, 20),
		Provenance:       contentproof.ProvenanceTorrentPiece,
		ObservedAt:       time.Now().UTC(),
	}
	proofs := []contentproof.Evidence{proof}
	if _, ok := mappedHTTPProofAt(proofs, "http:target", 4, 0); ok {
		t.Fatal("unrelated proof authorized recovery")
	}
	normalizeCanonicalHTTPProofs(proofs, "http:target")
	if got, ok := mappedHTTPProofAt(proofs, "http:target", 4, 0); !ok || got.Offset != 0 || got.Length != 4 {
		t.Fatalf("canonical alias proof = %#v, %v", got, ok)
	}
}

func TestHR62CrossLaneOnlySourceIsBoundedAndCancellable(t *testing.T) {
	const chunkBytes = int64(4)
	var calls atomic.Int32
	source, err := newHTTPCrossLaneSource("item", "file", 10, chunkBytes, func(ctx context.Context, start, end int64) ([]byte, error) {
		calls.Add(1)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return bytes.Repeat([]byte{byte(start)}, int(end-start+1)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.Size() != 10 || !source.Caps().RangeSupport || !source.Caps().ExactSize || source.Caps().Alignment != chunkBytes {
		t.Fatalf("unexpected source contract: size=%d caps=%+v", source.Size(), source.Caps())
	}
	got := make([]byte, 6)
	n, err := source.ReadAt(context.Background(), got, 2)
	if n != len(got) || err != nil || !bytes.Equal(got, []byte{0, 0, 4, 4, 4, 4}) {
		t.Fatalf("ReadAt = %v, %v, want bounded cross-chunk bytes", got, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("recovery calls = %d, want 2", calls.Load())
	}

	tail := make([]byte, 4)
	n, err = source.ReadAt(context.Background(), tail, 8)
	if n != 2 || !errors.Is(err, io.EOF) || !bytes.Equal(tail[:n], []byte{8, 8}) {
		t.Fatalf("tail ReadAt = %v, %d, %v", tail, n, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err = source.ReadAt(ctx, got, 0); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ReadAt = %d, %v", n, err)
	}
	if calls.Load() != 3 {
		t.Fatalf("cancelled read reached recovery: %d calls", calls.Load())
	}
}

func TestHR62CrossLaneOnlySourceFailsClosedOnUnboundedLayout(t *testing.T) {
	recover := func(context.Context, int64, int64) ([]byte, error) { return nil, nil }
	if _, err := newHTTPCrossLaneSource("item", "file", 0, 1, recover); err == nil {
		t.Fatal("zero-sized source accepted")
	}
	tooLarge := int64(contentproof.MaxLaneCandidates) + 1
	if _, err := newHTTPCrossLaneSource("item", "file", tooLarge, 1, recover); err == nil {
		t.Fatal("unbounded chunk layout accepted")
	}
}
