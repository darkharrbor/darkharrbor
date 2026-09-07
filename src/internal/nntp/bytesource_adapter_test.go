package nntp

import (
	"context"
	"strconv"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/bytesource/bytesourcetest"
)

func TestSegmentByteSource_Conformance(t *testing.T) {
	decoded := make([]byte, 12000)
	for i := range decoded {
		decoded[i] = byte((i*13 + 5) % 251)
	}
	segSizes := []int64{1, 4096, 4096, 3807} // decoded lengths; sum 12000
	segments := make([]NZBSegment, len(segSizes))
	starts := make([]int64, len(segSizes)+1)
	var acc int64
	for i, s := range segSizes {
		segments[i] = NZBSegment{Number: i + 1, Bytes: s, MessageID: "m" + strconv.Itoa(i)}
		starts[i] = acc
		acc += s
	}
	starts[len(segSizes)] = acc
	fetch := func(ctx context.Context, c bytesource.Chunk) ([]byte, error) {
		return append([]byte(nil), decoded[starts[c.Index]:starts[c.Index+1]]...), nil
	}
	bytesourcetest.RunConformance(t, func() (bytesource.ByteSource, []byte) {
		bs, err := newSegmentByteSource("fk", segments, starts, fetch)
		if err != nil {
			t.Fatalf("newSegmentByteSource: %v", err)
		}
		return bs, decoded
	})
}

func TestSegmentStarts_DeclaredFallback(t *testing.T) {
	sc := &SegmentCache{} // offsets nil → declared cumulative fallback
	declared := []int64{100, 250, 700}
	got := sc.segmentStarts(context.Background(), "", "", 0, declared)
	want := []int64{0, 100, 350, 1050}
	if len(got) != len(want) {
		t.Fatalf("starts len=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("starts[%d]=%d want %d", i, got[i], want[i])
		}
	}
}
