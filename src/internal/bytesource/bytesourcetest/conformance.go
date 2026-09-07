// Package bytesourcetest provides the shared ByteSource conformance suite
// (TS-1.1). Every ByteSource adapter proves behavior-identity against the same
// battery here, so the NNTP-segment and debrid-CDN adapters — and any later
// HTTP adapter — are validated by one contract rather than divergent ad-hoc
// tests. It imports the testing package by design and must only be used from
// _test.go files.
package bytesourcetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// RunConformance exercises a ByteSource whose content must equal the bytes
// returned alongside it, across a battery of offsets, lengths, and edges,
// asserting byte-identity and correct io.ReaderAt / EOF / cancellation
// semantics. ctor returns a fresh source and the exact bytes it should yield;
// it is called more than once, so it must be side-effect free per call.
func RunConformance(t *testing.T, ctor func() (bytesource.ByteSource, []byte)) {
	t.Helper()
	bs, want := ctor()
	if bs.Caps().ExactSize && bs.Size() != int64(len(want)) {
		t.Fatalf("exact-size source: Size()=%d want %d", bs.Size(), len(want))
	}

	// Full sequential read reconstructs the content exactly.
	if got := readAll(t, bs); !bytes.Equal(got, want) {
		t.Fatalf("full read: got %d bytes, want %d (equal=%v)", len(got), len(want), bytes.Equal(got, want))
	}

	// Sub-range battery: heads, mids, tails, odd lengths, spanning reads.
	offs := []int64{0, 1, 2, 7, 13, int64(len(want)) / 3, int64(len(want)) / 2, int64(len(want)) - 5, int64(len(want)) - 1}
	lens := []int{1, 2, 3, 5, 16, 64, 257, 1024, 4096}
	for _, off := range offs {
		if off < 0 || off >= int64(len(want)) {
			continue
		}
		for _, ln := range lens {
			p := make([]byte, ln)
			n, err := bs.ReadAt(context.Background(), p, off)
			expEnd := off + int64(ln)
			if expEnd > int64(len(want)) {
				exp := want[off:]
				if n != len(exp) || !bytes.Equal(p[:n], exp) {
					t.Fatalf("tail read off=%d ln=%d: n=%d want %d bytes", off, ln, n, len(exp))
				}
				if !errors.Is(err, io.EOF) {
					t.Fatalf("tail read off=%d ln=%d: want io.EOF, got %v", off, ln, err)
				}
			} else {
				if err != nil {
					t.Fatalf("read off=%d ln=%d: unexpected err %v", off, ln, err)
				}
				if n != ln || !bytes.Equal(p[:n], want[off:expEnd]) {
					t.Fatalf("read off=%d ln=%d: content mismatch (n=%d)", off, ln, n)
				}
			}
		}
	}

	// Negative offset.
	if _, err := bs.ReadAt(context.Background(), make([]byte, 4), -1); !errors.Is(err, bytesource.ErrNegativeOffset) {
		t.Fatalf("negative offset: want ErrNegativeOffset, got %v", err)
	}
	// Zero-length read is a no-op.
	if n, err := bs.ReadAt(context.Background(), nil, 0); n != 0 || err != nil {
		t.Fatalf("zero-length read: n=%d err=%v", n, err)
	}
	// Read at/after EOF.
	if _, err := bs.ReadAt(context.Background(), make([]byte, 4), int64(len(want))); !errors.Is(err, io.EOF) {
		t.Fatalf("read at EOF: want io.EOF, got %v", err)
	}
	// Cancelled context must not yield a successful full read.
	bs2, _ := ctor()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := bs2.ReadAt(ctx, make([]byte, 16), 0); err == nil && n == 16 {
		t.Fatalf("cancelled read returned a full result with nil error")
	}
}

func readAll(t *testing.T, bs bytesource.ByteSource) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 4096)
	var off int64
	for {
		n, err := bs.ReadAt(context.Background(), buf, off)
		out = append(out, buf[:n]...)
		off += int64(n)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("readAll at off=%d: %v", off, err)
		}
		if n == 0 {
			t.Fatalf("readAll made no progress at off=%d", off)
		}
	}
}
