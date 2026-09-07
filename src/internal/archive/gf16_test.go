package archive

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// encodeRecovery reproduces the PAR2 encoder side so the solver can be
// exercised against known inputs: R_e = XOR over i of (base_i ^ e) * D_i.
func encodeRecovery(t *testing.T, blocks [][]byte, sliceSize int, exponent uint32) []byte {
	t.Helper()
	constants, ok := PAR2InputConstants(len(blocks))
	if !ok {
		t.Fatalf("constants for %d blocks unavailable", len(blocks))
	}
	out := make([]byte, sliceSize)
	for i, block := range blocks {
		factor := gf16Pow(constants[i], exponent)
		if factor == 0 {
			continue
		}
		mulXorInto(out, block, factor)
	}
	return out
}

func makeBlocks(count, sliceSize int, seed byte) [][]byte {
	blocks := make([][]byte, count)
	for b := range blocks {
		buf := make([]byte, sliceSize)
		for i := range buf {
			buf[i] = byte((b*7+i*13)%251) ^ seed
		}
		blocks[b] = buf
	}
	return blocks
}

func TestGF16FieldKnownAnswers(t *testing.T) {
	gf16Init()
	if got := gf16Mul(1, 12345); got != 12345 {
		t.Fatalf("multiplicative identity broken: %d", got)
	}
	if got := gf16Mul(0, 4242); got != 0 {
		t.Fatalf("zero absorbs: %d", got)
	}
	// alog(1) is the generator, so 2*2 must be alog(2).
	if got, want := gf16Mul(2, 2), gf16ALog[2]; got != want {
		t.Fatalf("generator square = %d, want %d", got, want)
	}
	for _, v := range []uint16{1, 2, 3, 255, 4096, 65535} {
		inv, ok := gf16Inv(v)
		if !ok {
			t.Fatalf("no inverse for %d", v)
		}
		if got := gf16Mul(v, inv); got != 1 {
			t.Fatalf("%d * inv = %d, want 1", v, got)
		}
		if got := gf16Pow(v, 0); got != 1 {
			t.Fatalf("pow(%d,0) = %d, want 1", v, got)
		}
		if got := gf16Pow(v, 1); got != v {
			t.Fatalf("pow(%d,1) = %d", v, got)
		}
		if got, want := gf16Pow(v, 3), gf16Mul(gf16Mul(v, v), v); got != want {
			t.Fatalf("pow(%d,3) = %d, want %d", v, got, want)
		}
	}
	if _, ok := gf16Inv(0); ok {
		t.Fatal("zero must have no inverse")
	}
}

func TestPAR2InputConstantsMatchSpecOrdering(t *testing.T) {
	got, ok := PAR2InputConstants(4)
	if !ok {
		t.Fatal("constants unavailable")
	}
	gf16Init()
	// par2cmdline walks logbase upward, skipping any log not relatively
	// prime to 65535: 1, 2, 4, 7 (3, 5, 6 all share a factor).
	want := []uint16{gf16ALog[1], gf16ALog[2], gf16ALog[4], gf16ALog[7]}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("constant %d = %d, want %d", i, got[i], want[i])
		}
	}
	seen := map[uint16]struct{}{}
	all, ok := PAR2InputConstants(int(MaxPAR2TotalBlocks))
	if !ok {
		t.Fatal("field must admit the full block ceiling")
	}
	for _, c := range all {
		if _, dup := seen[c]; dup {
			t.Fatal("duplicate input constant")
		}
		seen[c] = struct{}{}
	}
	if _, ok := PAR2InputConstants(int(MaxPAR2TotalBlocks) + 1); ok {
		t.Fatal("field must refuse more blocks than it has constants")
	}
}

func TestPAR2SolverRoundTripSingleAndMultiBlock(t *testing.T) {
	const sliceSize = 64
	blocks := makeBlocks(9, sliceSize, 0x5a)
	for _, unknownCount := range []int{1, 2, 3} {
		unknown := make([]int64, 0, unknownCount)
		for i := 0; i < unknownCount; i++ {
			unknown = append(unknown, int64(2+i))
		}
		exponents := make([]uint32, 0, unknownCount)
		for i := 0; i < unknownCount; i++ {
			exponents = append(exponents, uint32(i))
		}
		solver, err := NewPAR2Solver(sliceSize, int64(len(blocks)), unknown, exponents)
		if err != nil {
			t.Fatalf("solver: %v", err)
		}
		for _, e := range exponents {
			if err := solver.AddRecoverySlice(e, encodeRecovery(t, blocks, sliceSize, e)); err != nil {
				t.Fatalf("add recovery: %v", err)
			}
		}
		for i, block := range blocks {
			skip := false
			for _, u := range unknown {
				if int64(i) == u {
					skip = true
				}
			}
			if skip {
				continue
			}
			if err := solver.AddKnownBlock(int64(i), block); err != nil {
				t.Fatalf("add known: %v", err)
			}
		}
		got, err := solver.Solve()
		if err != nil {
			t.Fatalf("solve: %v", err)
		}
		for i, u := range unknown {
			if !bytes.Equal(got[i], blocks[u]) {
				t.Fatalf("unknownCount=%d block %d reconstructed incorrectly", unknownCount, u)
			}
		}
	}
}

func TestPAR2SolverZeroPaddedFinalBlock(t *testing.T) {
	const sliceSize = 32
	blocks := makeBlocks(4, sliceSize, 0x11)
	// Model a file whose final block is short: PAR2 zero-pads it.
	for i := 20; i < sliceSize; i++ {
		blocks[3][i] = 0
	}
	solver, err := NewPAR2Solver(sliceSize, 4, []int64{1}, []uint32{5})
	if err != nil {
		t.Fatalf("solver: %v", err)
	}
	if err := solver.AddRecoverySlice(5, encodeRecovery(t, blocks, sliceSize, 5)); err != nil {
		t.Fatalf("add recovery: %v", err)
	}
	if err := solver.AddKnownBlock(0, blocks[0]); err != nil {
		t.Fatalf("known 0: %v", err)
	}
	if err := solver.AddKnownBlock(2, blocks[2]); err != nil {
		t.Fatalf("known 2: %v", err)
	}
	// The short final block is supplied without its padding, exactly as a
	// streaming caller holds it.
	if err := solver.AddKnownBlock(3, blocks[3][:20]); err != nil {
		t.Fatalf("known 3: %v", err)
	}
	got, err := solver.Solve()
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if !bytes.Equal(got[0], blocks[1]) {
		t.Fatal("short-tail recovery set reconstructed incorrectly")
	}
}

func TestPAR2SolverRejectsMalformedConstruction(t *testing.T) {
	cases := []struct {
		name      string
		sliceSize int64
		total     int64
		unknown   []int64
		exponents []uint32
	}{
		{"odd slice size", 33, 4, []int64{0}, []uint32{1}},
		{"zero slice size", 0, 4, []int64{0}, []uint32{1}},
		{"oversized slice", MaxPAR2SliceBytes + 4, 4, []int64{0}, []uint32{1}},
		{"no blocks", 32, 0, []int64{0}, []uint32{1}},
		{"too many blocks", 32, MaxPAR2TotalBlocks + 1, []int64{0}, []uint32{1}},
		{"no unknowns", 32, 4, nil, nil},
		{"count mismatch", 32, 4, []int64{0}, []uint32{1, 2}},
		{"unknown out of range", 32, 4, []int64{9}, []uint32{1}},
		{"duplicate unknown", 32, 4, []int64{1, 1}, []uint32{1, 2}},
		{"duplicate exponent", 32, 4, []int64{1, 2}, []uint32{3, 3}},
		{"too many unknowns", 32, 4096, make([]int64, MaxPAR2UnknownBlocks+1), make([]uint32, MaxPAR2UnknownBlocks+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPAR2Solver(tc.sliceSize, tc.total, tc.unknown, tc.exponents); err == nil {
				t.Fatal("expected construction to fail closed")
			}
		})
	}
}

func TestPAR2SolverRejectsIncompleteAndConflictingInput(t *testing.T) {
	const sliceSize = 32
	blocks := makeBlocks(3, sliceSize, 0x22)
	newSolver := func(t *testing.T) *PAR2Solver {
		t.Helper()
		s, err := NewPAR2Solver(sliceSize, 3, []int64{0}, []uint32{2})
		if err != nil {
			t.Fatalf("solver: %v", err)
		}
		return s
	}

	s := newSolver(t)
	if _, err := s.Solve(); err == nil {
		t.Fatal("solve without recovery data must fail")
	}

	s = newSolver(t)
	if err := s.AddRecoverySlice(2, make([]byte, sliceSize-4)); err == nil {
		t.Fatal("short recovery slice must be rejected")
	}
	if err := s.AddRecoverySlice(99, make([]byte, sliceSize)); err == nil {
		t.Fatal("undeclared exponent must be rejected")
	}
	if err := s.AddRecoverySlice(2, encodeRecovery(t, blocks, sliceSize, 2)); err != nil {
		t.Fatalf("add recovery: %v", err)
	}
	if err := s.AddRecoverySlice(2, encodeRecovery(t, blocks, sliceSize, 2)); err == nil {
		t.Fatal("duplicate recovery slice must be rejected")
	}
	if err := s.AddKnownBlock(0, blocks[0]); err == nil {
		t.Fatal("supplying a declared-unknown block as known must be rejected")
	}
	if err := s.AddKnownBlock(7, blocks[1]); err == nil {
		t.Fatal("out-of-range known block must be rejected")
	}
	if err := s.AddKnownBlock(1, make([]byte, sliceSize+1)); err == nil {
		t.Fatal("oversized known block must be rejected")
	}
}

func TestPAR2SolverDetectsSingularSystem(t *testing.T) {
	const sliceSize = 16
	// A single recovery slice with exponent 0 gives every input block the
	// coefficient 1, so two unknowns cannot be separated.
	s, err := NewPAR2Solver(sliceSize, 4, []int64{0, 1}, []uint32{0, 1})
	if err != nil {
		t.Fatalf("solver: %v", err)
	}
	// Force a degenerate matrix by zeroing the second column's coefficients.
	for r := range s.coeff {
		s.coeff[r][1] = s.coeff[r][0]
	}
	if err := s.AddRecoverySlice(0, make([]byte, sliceSize)); err != nil {
		t.Fatalf("add recovery: %v", err)
	}
	if err := s.AddRecoverySlice(1, make([]byte, sliceSize)); err != nil {
		t.Fatalf("add recovery: %v", err)
	}
	if _, err := s.Solve(); err == nil {
		t.Fatal("singular system must not produce bytes")
	}
}

func TestGF16WordOrderIsLittleEndian(t *testing.T) {
	dst := make([]byte, 4)
	src := []byte{0x34, 0x12, 0x78, 0x56}
	mulXorInto(dst, src, 1)
	if binary.LittleEndian.Uint16(dst[0:]) != 0x1234 || binary.LittleEndian.Uint16(dst[2:]) != 0x5678 {
		t.Fatalf("PAR2 words must be little-endian: %x", dst)
	}
}
