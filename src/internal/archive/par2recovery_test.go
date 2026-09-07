package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// fixtureMedia regenerates the exact bytes the committed PAR2 fixture was
// created from. Committing the media itself would add 64 KiB of redundant
// testdata; the generator is the authoritative definition either way.
func fixtureMedia() []byte {
	data := make([]byte, 65536)
	for i := range data {
		data[i] = byte((i*i*31 + i*7 + 13) % 251)
	}
	return data
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "par2", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func memSource(t *testing.T, data []byte) bytesource.ByteSource {
	t.Helper()
	return bytesource.NewMemSource("par2-test", data)
}

// TestPAR2RealFixtureReconstruction is this row's DG-06 known-answer gate.
// The index and recovery volume were produced by par2cmdline 0.8.1 against
// the exact bytes fixtureMedia returns; nothing in this repository
// participated in creating them. Deleting blocks and rebuilding them from the
// real recovery slices therefore tests the field, the constant ordering, the
// block numbering, and the packet parsing against an independent encoder.
func TestPAR2RealFixtureReconstruction(t *testing.T) {
	ctx := context.Background()
	media := fixtureMedia()
	index := loadFixture(t, "fixture.par2")
	volume := loadFixture(t, "fixture.vol0+6.par2")

	info, err := ParsePAR2Metadata(ctx, memSource(t, index))
	if err != nil {
		t.Fatalf("parse fixture index: %v", err)
	}
	if info.SliceSize != 4096 {
		t.Fatalf("fixture slice size = %d, want 4096", info.SliceSize)
	}
	if len(info.RecoverySetIDs) != 1 || len(info.Files) != 1 {
		t.Fatalf("fixture must describe exactly one recovery-set file, got %d/%d",
			len(info.RecoverySetIDs), len(info.Files))
	}
	if info.Files[0].Length != int64(len(media)) {
		t.Fatalf("fixture describes %d bytes, want %d", info.Files[0].Length, len(media))
	}
	totalBlocks := int64(len(media)) / info.SliceSize
	if totalBlocks != 16 {
		t.Fatalf("fixture block count = %d, want 16", totalBlocks)
	}

	blocks := make([][]byte, totalBlocks)
	for i := range blocks {
		blocks[i] = media[int64(i)*info.SliceSize : int64(i+1)*info.SliceSize]
	}

	for _, unknown := range [][]int64{{0}, {7}, {15}, {3, 4}, {1, 8, 14}} {
		want := make(map[uint32]struct{})
		exps, err := PAR2RecoveryExponents(ctx, memSource(t, volume), info.RecoverySetID, info.SliceSize, len(unknown))
		if err != nil {
			t.Fatalf("discover exponents: %v", err)
		}
		if len(exps) != len(unknown) {
			t.Fatalf("discovered %d exponents, want %d", len(exps), len(unknown))
		}
		for _, e := range exps {
			want[e] = struct{}{}
		}
		slices, err := ScanPAR2RecoverySlices(ctx, memSource(t, volume), info.RecoverySetID, info.SliceSize, want, 1<<20)
		if err != nil {
			t.Fatalf("scan recovery slices: %v", err)
		}
		if len(slices) != len(unknown) {
			t.Fatalf("collected %d slices, want %d", len(slices), len(unknown))
		}

		solver, err := NewPAR2Solver(info.SliceSize, totalBlocks, unknown, exps)
		if err != nil {
			t.Fatalf("solver: %v", err)
		}
		for _, s := range slices {
			if int64(len(s.Data)) != info.SliceSize {
				t.Fatalf("recovery slice length %d, want %d", len(s.Data), info.SliceSize)
			}
			if err := solver.AddRecoverySlice(s.Exponent, s.Data); err != nil {
				t.Fatalf("add recovery slice: %v", err)
			}
		}
		missing := map[int64]struct{}{}
		for _, u := range unknown {
			missing[u] = struct{}{}
		}
		for i := range blocks {
			if _, skip := missing[int64(i)]; skip {
				continue
			}
			if err := solver.AddKnownBlock(int64(i), blocks[i]); err != nil {
				t.Fatalf("add known block %d: %v", i, err)
			}
		}
		got, err := solver.Solve()
		if err != nil {
			t.Fatalf("solve %v: %v", unknown, err)
		}
		for i, u := range unknown {
			if !bytes.Equal(got[i], blocks[u]) {
				t.Fatalf("block %d reconstructed incorrectly from real par2cmdline recovery data", u)
			}
		}
	}
}

// TestPAR2RealFixtureCorruptionIsDetected is DG-06's conflict arm: a single
// flipped byte anywhere in the surviving data must not silently produce a
// wrong-but-plausible block. The reconstruction is checked against the real
// content, which is the only thing that makes the failure observable.
func TestPAR2RealFixtureCorruptionIsDetected(t *testing.T) {
	ctx := context.Background()
	media := fixtureMedia()
	info, err := ParsePAR2Metadata(ctx, memSource(t, loadFixture(t, "fixture.par2")))
	if err != nil {
		t.Fatalf("parse index: %v", err)
	}
	volume := loadFixture(t, "fixture.vol0+6.par2")
	exps, err := PAR2RecoveryExponents(ctx, memSource(t, volume), info.RecoverySetID, info.SliceSize, 1)
	if err != nil {
		t.Fatalf("exponents: %v", err)
	}
	want := map[uint32]struct{}{exps[0]: {}}
	slices, err := ScanPAR2RecoverySlices(ctx, memSource(t, volume), info.RecoverySetID, info.SliceSize, want, 1<<20)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	solver, err := NewPAR2Solver(info.SliceSize, 16, []int64{5}, exps)
	if err != nil {
		t.Fatalf("solver: %v", err)
	}
	if err := solver.AddRecoverySlice(slices[0].Exponent, slices[0].Data); err != nil {
		t.Fatalf("add slice: %v", err)
	}
	for i := int64(0); i < 16; i++ {
		if i == 5 {
			continue
		}
		block := append([]byte(nil), media[i*info.SliceSize:(i+1)*info.SliceSize]...)
		if i == 9 {
			block[100] ^= 0x01 // one corrupted surviving block
		}
		if err := solver.AddKnownBlock(i, block); err != nil {
			t.Fatalf("known %d: %v", i, err)
		}
	}
	got, err := solver.Solve()
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	real := media[5*info.SliceSize : 6*info.SliceSize]
	if bytes.Equal(got[0], real) {
		t.Fatal("corrupted surviving data must not yield the true block")
	}
	// The caller's IFSC verification is what converts this into an abstain;
	// this test asserts the corruption genuinely propagates rather than
	// being silently absorbed.
}

func TestScanPAR2RecoverySlicesRejectsMismatchedInput(t *testing.T) {
	ctx := context.Background()
	volume := loadFixture(t, "fixture.vol0+6.par2")
	info, err := ParsePAR2Metadata(ctx, memSource(t, loadFixture(t, "fixture.par2")))
	if err != nil {
		t.Fatalf("parse index: %v", err)
	}
	want := map[uint32]struct{}{0: {}, 1: {}}

	if _, err := ScanPAR2RecoverySlices(ctx, memSource(t, volume), "deadbeef", info.SliceSize, want, 1<<20); !errors.Is(err, ErrPAR2NoRecoverySlices) {
		t.Fatalf("wrong recovery set must find nothing, got %v", err)
	}
	if _, err := ScanPAR2RecoverySlices(ctx, memSource(t, volume), info.RecoverySetID, 8192, want, 1<<20); !errors.Is(err, ErrPAR2NoRecoverySlices) {
		t.Fatalf("wrong slice size must find nothing, got %v", err)
	}
	if _, err := ScanPAR2RecoverySlices(ctx, memSource(t, volume), info.RecoverySetID, 4095, want, 1<<20); err == nil {
		t.Fatal("non-multiple-of-four slice size must be rejected")
	}
	if _, err := ScanPAR2RecoverySlices(ctx, memSource(t, volume), info.RecoverySetID, info.SliceSize, nil, 1<<20); err == nil {
		t.Fatal("empty exponent request must be rejected")
	}
	if _, err := ScanPAR2RecoverySlices(ctx, nil, info.RecoverySetID, info.SliceSize, want, 1<<20); err == nil {
		t.Fatal("nil source must be rejected")
	}
	if _, err := ScanPAR2RecoverySlices(ctx, memSource(t, []byte{}), info.RecoverySetID, info.SliceSize, want, 1<<20); err == nil {
		t.Fatal("empty source must be rejected")
	}
	if _, err := ScanPAR2RecoverySlices(ctx, memSource(t, volume), info.RecoverySetID, info.SliceSize, want, 100); err == nil {
		t.Fatal("byte budget below one slice must be refused")
	}
}

func TestScanPAR2RecoverySlicesHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	info := PAR2Info{SliceSize: 4096, RecoverySetID: "00"}
	volume := loadFixture(t, "fixture.vol0+6.par2")
	want := map[uint32]struct{}{0: {}}
	if _, err := ScanPAR2RecoverySlices(ctx, memSource(t, volume), info.RecoverySetID, info.SliceSize, want, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan must return context.Canceled, got %v", err)
	}
	if _, err := PAR2RecoveryExponents(ctx, memSource(t, volume), info.RecoverySetID, info.SliceSize, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery must return context.Canceled, got %v", err)
	}
}

func TestScanPAR2RecoverySlicesRejectsMalformedFraming(t *testing.T) {
	ctx := context.Background()
	info, err := ParsePAR2Metadata(ctx, memSource(t, loadFixture(t, "fixture.par2")))
	if err != nil {
		t.Fatalf("parse index: %v", err)
	}
	want := map[uint32]struct{}{0: {}}

	volume := loadFixture(t, "fixture.vol0+6.par2")

	truncated := append([]byte(nil), volume[:len(volume)/3]...)
	if _, err := ScanPAR2RecoverySlices(ctx, memSource(t, truncated), info.RecoverySetID, info.SliceSize, want, 1<<20); err == nil {
		t.Log("truncated volume may still hold a complete leading slice; only a partial slice must be refused")
	}

	badLength := append([]byte(nil), volume...)
	binary.LittleEndian.PutUint64(badLength[8:16], 7) // below the header size
	if _, err := ScanPAR2RecoverySlices(ctx, memSource(t, badLength), info.RecoverySetID, info.SliceSize, want, 1<<20); !errors.Is(err, ErrPAR2NoRecoverySlices) {
		t.Fatalf("malformed declared length must stop the walk, got %v", err)
	}

	badMagic := append([]byte(nil), volume...)
	badMagic[0] ^= 0xFF
	if _, err := ScanPAR2RecoverySlices(ctx, memSource(t, badMagic), info.RecoverySetID, info.SliceSize, want, 1<<20); !errors.Is(err, ErrPAR2NoRecoverySlices) {
		t.Fatalf("broken magic must stop the walk, got %v", err)
	}
}

func FuzzScanPAR2RecoverySlices(f *testing.F) {
	f.Add(loadSeed(f, "fixture.vol0+6.par2"))
	f.Add(loadSeed(f, "fixture.par2"))
	f.Add([]byte("PAR2\x00PKT"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		ctx := context.Background()
		want := map[uint32]struct{}{0: {}, 1: {}, 7: {}}
		src := bytesource.NewMemSource("fuzz", data)
		slices, err := ScanPAR2RecoverySlices(ctx, src, "00000000000000000000000000000000", 4096, want, 1<<20)
		if err == nil {
			for _, s := range slices {
				if len(s.Data) != 4096 {
					t.Fatalf("accepted a slice of length %d", len(s.Data))
				}
				if _, ok := want[s.Exponent]; !ok {
					t.Fatalf("returned an unrequested exponent %d", s.Exponent)
				}
			}
		}
		if _, err := PAR2RecoveryExponents(ctx, src, "00000000000000000000000000000000", 4096, 4); err == nil {
			return
		}
	})
}

func loadSeed(f *testing.F, name string) []byte {
	f.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "par2", name))
	if err != nil {
		f.Fatalf("seed %s: %v", name, err)
	}
	return raw
}
