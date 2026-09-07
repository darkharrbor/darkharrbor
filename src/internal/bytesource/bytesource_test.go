package bytesource_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/bytesource/bytesourcetest"
)

// pattern returns a deterministic, non-repeating-enough byte pattern of length n.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + 7) % 251)
	}
	return b
}

// chunkedOver splits data into chunks of the given declared sizes (which must
// sum to len(data)) and returns an exact-size chunked ByteSource plus a hit
// counter for each chunk fetch.
func chunkedOver(t *testing.T, data []byte, sizes []int64, exact bool) (bytesource.ByteSource, *[]int) {
	t.Helper()
	var chunks []bytesource.Chunk
	starts := make([]int64, len(sizes)+1)
	var acc int64
	for i, s := range sizes {
		chunks = append(chunks, bytesource.Chunk{Index: i, Key: "c" + itoa(i), DeclaredSize: s})
		starts[i] = acc
		acc += s
	}
	starts[len(sizes)] = acc
	if acc != int64(len(data)) {
		t.Fatalf("chunk sizes sum %d != data %d", acc, len(data))
	}
	hits := make([]int, len(sizes))
	fetch := func(ctx context.Context, c bytesource.Chunk) ([]byte, error) {
		hits[c.Index]++
		return append([]byte(nil), data[starts[c.Index]:starts[c.Index+1]]...), nil
	}
	caps := bytesource.Capabilities{RangeSupport: true, ExactSize: exact, TailCost: bytesource.TailCostSegmented, Alignment: 0}
	bs, err := bytesource.NewChunked("k", chunks, starts, acc, caps, fetch)
	if err != nil {
		t.Fatalf("NewChunked: %v", err)
	}
	return bs, &hits
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestMemSource_Conformance(t *testing.T) {
	data := pattern(20000)
	bytesourcetest.RunConformance(t, func() (bytesource.ByteSource, []byte) {
		return bytesource.NewMemSource("mem", data), data
	})
}

func TestChunked_Conformance(t *testing.T) {
	data := pattern(20000)
	sizes := []int64{1, 4096, 4096, 4096, 4096, 3615} // sums to 20000
	bytesourcetest.RunConformance(t, func() (bytesource.ByteSource, []byte) {
		bs, _ := chunkedOver(t, data, sizes, true)
		return bs, data
	})
}

func TestChunked_SingleChunk(t *testing.T) {
	data := pattern(5000)
	bytesourcetest.RunConformance(t, func() (bytesource.ByteSource, []byte) {
		bs, _ := chunkedOver(t, data, []int64{5000}, true)
		return bs, data
	})
}

func TestChunked_Empty(t *testing.T) {
	bs, err := bytesource.NewChunked("e", nil, []int64{0}, 0, bytesource.Capabilities{}, func(context.Context, bytesource.Chunk) ([]byte, error) { return nil, nil })
	if err != nil {
		t.Fatalf("NewChunked empty: %v", err)
	}
	if _, err := bs.ReadAt(context.Background(), make([]byte, 4), 0); !errors.Is(err, io.EOF) {
		t.Fatalf("empty ReadAt: want io.EOF, got %v", err)
	}
}

// TestChunked_DecodedOffsets_Conformance proves precise random access over a
// non-exact source when the offset table holds recorded DECODED offsets: the
// chunked source is then byte-identical to the decoded concatenation.
func TestChunked_DecodedOffsets_Conformance(t *testing.T) {
	decoded := pattern(16000)
	// Uneven decoded chunk sizes (as yEnc-decoded segments would be).
	sizes := []int64{1, 3333, 4096, 4095, 4475}
	bytesourcetest.RunConformance(t, func() (bytesource.ByteSource, []byte) {
		bs, _ := chunkedOver(t, decoded, sizes, false)
		return bs, decoded
	})
}

// TestChunked_NoFabricationOnDeclaredShortfall verifies the declared-space /
// decoded-space split with a real declared-vs-decoded gap: chunk 0 is declared
// 100 bytes but decodes to only 60. A full read emits exactly the concatenated
// real decoded bytes (60+100) and stops with io.EOF — the declared gap is
// skipped, never zero-filled or fabricated.
func TestChunked_NoFabricationOnDeclaredShortfall(t *testing.T) {
	decoded0 := pattern(60)
	decoded1 := pattern(100)
	chunks := []bytesource.Chunk{{Index: 0, Key: "a", DeclaredSize: 100}, {Index: 1, Key: "b", DeclaredSize: 100}}
	starts := []int64{0, 100, 200} // DECLARED offsets; decoded lengths are shorter
	fetch := func(ctx context.Context, c bytesource.Chunk) ([]byte, error) {
		if c.Index == 0 {
			return decoded0, nil
		}
		return decoded1, nil
	}
	bs, err := bytesource.NewChunked("s", chunks, starts, 200, bytesource.Capabilities{RangeSupport: true, ExactSize: false}, fetch)
	if err != nil {
		t.Fatalf("NewChunked: %v", err)
	}
	p := make([]byte, 200)
	n, rerr := bs.ReadAt(context.Background(), p, 0)
	want := append(append([]byte(nil), decoded0...), decoded1...)
	if n != len(want) || !bytes.Equal(p[:n], want) {
		t.Fatalf("declared-shortfall read: n=%d want %d identical real bytes", n, len(want))
	}
	if !errors.Is(rerr, io.EOF) {
		t.Fatalf("declared-shortfall read: want io.EOF, got %v", rerr)
	}
}

func TestChunked_FetchError(t *testing.T) {
	boom := errors.New("boom")
	chunks := []bytesource.Chunk{{Index: 0, Key: "a", DeclaredSize: 10}}
	bs, _ := bytesource.NewChunked("f", chunks, []int64{0, 10}, 10, bytesource.Capabilities{}, func(context.Context, bytesource.Chunk) ([]byte, error) { return nil, boom })
	if _, err := bs.ReadAt(context.Background(), make([]byte, 4), 0); !errors.Is(err, boom) {
		t.Fatalf("fetch error not propagated: %v", err)
	}
}

func TestNewChunked_Validation(t *testing.T) {
	f := func(context.Context, bytesource.Chunk) ([]byte, error) { return nil, nil }
	if _, err := bytesource.NewChunked("k", []bytesource.Chunk{{}}, []int64{0}, 0, bytesource.Capabilities{}, f); err == nil {
		t.Fatalf("want error for mismatched starts length")
	}
	if _, err := bytesource.NewChunked("k", nil, []int64{0}, 0, bytesource.Capabilities{}, nil); err == nil {
		t.Fatalf("want error for nil fetch")
	}
	if _, err := bytesource.NewChunked("k", []bytesource.Chunk{{}, {}}, []int64{0, 10, 5}, 5, bytesource.Capabilities{}, f); err == nil {
		t.Fatalf("want error for decreasing starts")
	}
}

// FuzzReadAt drives random (offset, length) reads against a chunked source and
// asserts byte-identity with the reference buffer plus no panic. The chunk
// layout is derived from the fuzz input so boundaries vary.
func FuzzReadAt(f *testing.F) {
	f.Add(uint16(20000), uint8(5), int64(0), uint16(4096))
	f.Add(uint16(1), uint8(1), int64(0), uint16(1))
	f.Add(uint16(9999), uint8(17), int64(5000), uint16(1))
	f.Fuzz(func(t *testing.T, total uint16, nChunks uint8, off int64, ln uint16) {
		data := pattern(int(total))
		nc := int(nChunks)
		if nc < 1 {
			nc = 1
		}
		// Build ascending chunk boundaries covering [0,total).
		sizes := make([]int64, 0, nc)
		base := len(data) / nc
		rem := len(data) % nc
		for i := 0; i < nc; i++ {
			s := int64(base)
			if i == nc-1 {
				s += int64(rem)
			}
			sizes = append(sizes, s)
		}
		// Drop trailing zero-size chunks (when total < nChunks) except keep one.
		trimmed := sizes[:0]
		var acc int64
		for _, s := range sizes {
			if s == 0 {
				continue
			}
			trimmed = append(trimmed, s)
			acc += s
		}
		if acc != int64(len(data)) {
			// total==0 case: single zero chunk.
			trimmed = []int64{0}
		}
		bs, _ := chunkedOver(t, data, trimmed, true)
		p := make([]byte, int(ln))
		if off < 0 {
			off = -off
		}
		n, err := bs.ReadAt(context.Background(), p, off)
		// Reference expectation.
		if off >= int64(len(data)) {
			if n != 0 || !errors.Is(err, io.EOF) {
				if !(len(p) == 0 && n == 0 && err == nil) {
					t.Fatalf("past-EOF off=%d n=%d err=%v", off, n, err)
				}
			}
			return
		}
		exp := data[off:]
		if len(exp) > len(p) {
			exp = exp[:len(p)]
		}
		if !bytes.Equal(p[:n], exp) {
			t.Fatalf("content mismatch off=%d ln=%d n=%d", off, ln, n)
		}
		if n < len(p) && !errors.Is(err, io.EOF) {
			t.Fatalf("short read without EOF off=%d ln=%d n=%d err=%v", off, ln, n, err)
		}
	})
}
