package nntp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// The PAR2 fixture is owned by internal/archive: it was produced by
// par2cmdline 0.8.1 and is referenced here rather than duplicated so both
// packages verify against exactly the same independent encoder output.
const par2FixtureDir = "../archive/testdata/par2"

func fixtureMedia() []byte {
	data := make([]byte, 65536)
	for i := range data {
		data[i] = byte((i*i*31 + i*7 + 13) % 251)
	}
	return data
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(par2FixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func fixtureInfo(t *testing.T) archiveparser.PAR2Info {
	t.Helper()
	info, err := archiveparser.ParsePAR2Metadata(context.Background(),
		bytesource.NewMemSource("idx", readFixture(t, "fixture.par2")))
	if err != nil {
		t.Fatalf("parse fixture index: %v", err)
	}
	return info
}

func fixtureMatches(info archiveparser.PAR2Info, nzbIdx int) []PAR2FileMatch {
	return []PAR2FileMatch{{NZBFileIdx: nzbIdx, Desc: info.Files[0]}}
}

func fixtureProof(t *testing.T, info archiveparser.PAR2Info, nzbIdx int) PAR2ProofDomain {
	t.Helper()
	index, err := BuildPAR2ProofIndex(info, fixtureMatches(info, nzbIdx))
	if err != nil || index == nil {
		t.Fatalf("build proof index: %v", err)
	}
	proof, ok := NewPAR2ProofDomain(index, nzbIdx)
	if !ok {
		t.Fatal("proof domain unavailable for the fixture file")
	}
	return proof
}

// splitSegments cuts data into count segments of deliberately non-slice-aligned
// length, matching how real yEnc articles fall across PAR2 block boundaries.
func splitSegments(data []byte, count int) [][]byte {
	out := make([][]byte, 0, count)
	per := len(data) / count
	for i := 0; i < count; i++ {
		start := i * per
		end := start + per
		if i == count-1 {
			end = len(data)
		}
		out = append(out, append([]byte(nil), data[start:end]...))
	}
	return out
}

type fixtureReader struct {
	segments    map[int][][]byte
	volumes     []int
	volumeData  map[int][]byte
	failRead    map[[2]int]error
	reads       int
	noVolumes   bool
	blockReads  bool
	volumeOpens int
}

func newFixtureReader(t *testing.T, mediaSegments [][]byte, mediaIdx, volumeIdx int) *fixtureReader {
	t.Helper()
	return &fixtureReader{
		segments:   map[int][][]byte{mediaIdx: mediaSegments},
		volumes:    []int{volumeIdx},
		volumeData: map[int][]byte{volumeIdx: readFixture(t, "fixture.vol0+6.par2")},
		failRead:   map[[2]int]error{},
	}
}

func (r *fixtureReader) SegmentCount(idx int) (int, bool) {
	segs, ok := r.segments[idx]
	if !ok || len(segs) == 0 {
		return 0, false
	}
	return len(segs), true
}

func (r *fixtureReader) ReadSegment(ctx context.Context, idx, seg int) ([]byte, error) {
	if r.blockReads {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err, bad := r.failRead[[2]int{idx, seg}]; bad {
		return nil, err
	}
	segs, ok := r.segments[idx]
	if !ok || seg < 0 || seg >= len(segs) {
		return nil, errors.New("no such segment")
	}
	r.reads++
	return append([]byte(nil), segs[seg]...), nil
}

func (r *fixtureReader) RecoveryVolumeIndexes() []int {
	if r.noVolumes {
		return nil
	}
	return r.volumes
}

func (r *fixtureReader) RecoverySource(ctx context.Context, idx int) (bytesource.ByteSource, error) {
	r.volumeOpens++
	data, ok := r.volumeData[idx]
	if !ok {
		return nil, errors.New("no such volume")
	}
	return bytesource.NewMemSource("vol", data), nil
}

func fixedClock() func() time.Time {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return base }
}

func fixtureRequest(t *testing.T, segIdx int) (PAR2RepairRequest, *fixtureReader, []byte, [][]byte) {
	t.Helper()
	media := fixtureMedia()
	info := fixtureInfo(t)
	geo, err := BuildPAR2Geometry(info, fixtureMatches(info, 0))
	if err != nil {
		t.Fatalf("geometry: %v", err)
	}
	segs := splitSegments(media, 7)
	reader := newFixtureReader(t, segs, 0, 1)
	req := PAR2RepairRequest{
		Geometry:     geo,
		Proof:        fixtureProof(t, info, 0),
		NZBFileIndex: 0,
		SegIndex:     segIdx,
		Budget:       PAR2RepairBudget{MaxBytes: 1 << 20, MaxDuration: time.Minute},
		Now:          fixedClock(),
	}
	return req, reader, media, segs
}

func TestBuildPAR2GeometryFromRealFixture(t *testing.T) {
	info := fixtureInfo(t)
	geo, err := BuildPAR2Geometry(info, fixtureMatches(info, 4))
	if err != nil {
		t.Fatalf("geometry: %v", err)
	}
	if geo.SliceSize != 4096 || geo.TotalBlocks() != 16 || geo.PayloadBytes() != 65536 {
		t.Fatalf("unexpected geometry: slice=%d blocks=%d bytes=%d",
			geo.SliceSize, geo.TotalBlocks(), geo.PayloadBytes())
	}
	f, ok := geo.FileByNZBIndex(4)
	if !ok || f.FirstBlock != 0 || f.Blocks != 16 || f.Length != 65536 {
		t.Fatalf("unexpected member: %+v ok=%t", f, ok)
	}
	if _, ok := geo.FileByNZBIndex(0); ok {
		t.Fatal("an unbound NZB index must not resolve")
	}
}

func TestBuildPAR2GeometryAbstains(t *testing.T) {
	info := fixtureInfo(t)
	cases := []struct {
		name    string
		mutate  func(archiveparser.PAR2Info) archiveparser.PAR2Info
		matches []PAR2FileMatch
	}{
		{"unmatched member", func(i archiveparser.PAR2Info) archiveparser.PAR2Info { return i }, nil},
		{"no main packet", func(i archiveparser.PAR2Info) archiveparser.PAR2Info {
			i.RecoverySetIDs = nil
			return i
		}, fixtureMatches(info, 0)},
		{"no recovery set identity", func(i archiveparser.PAR2Info) archiveparser.PAR2Info {
			i.RecoverySetID = ""
			return i
		}, fixtureMatches(info, 0)},
		{"unusable slice size", func(i archiveparser.PAR2Info) archiveparser.PAR2Info {
			i.SliceSize = 4095
			return i
		}, fixtureMatches(info, 0)},
		{"missing file description", func(i archiveparser.PAR2Info) archiveparser.PAR2Info {
			i.Files = nil
			return i
		}, fixtureMatches(info, 0)},
		{"zero length member", func(i archiveparser.PAR2Info) archiveparser.PAR2Info {
			files := append([]archiveparser.PAR2FileDesc(nil), i.Files...)
			files[0].Length = 0
			i.Files = files
			return i
		}, fixtureMatches(info, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildPAR2Geometry(tc.mutate(info), tc.matches); !errors.Is(err, ErrPAR2RepairAbstain) {
				t.Fatalf("expected abstain, got %v", err)
			}
		})
	}

	t.Run("two members claim one NZB file", func(t *testing.T) {
		dup := info
		dup.RecoverySetIDs = []string{info.Files[0].FileID, info.Files[0].FileID}
		if _, err := BuildPAR2Geometry(dup, fixtureMatches(info, 0)); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("expected abstain, got %v", err)
		}
	})

	t.Run("ambiguous identity", func(t *testing.T) {
		matches := []PAR2FileMatch{
			{NZBFileIdx: 0, Desc: info.Files[0]},
			{NZBFileIdx: 3, Desc: info.Files[0]},
		}
		if _, err := BuildPAR2Geometry(info, matches); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("expected abstain, got %v", err)
		}
	})
}

// TestRepairSegmentReconstructsRealBytes is the planner's end-to-end known
// answer: a whole segment is withheld, and every byte returned must equal the
// real media the independent encoder was run against.
func TestRepairSegmentReconstructsRealBytes(t *testing.T) {
	for _, segIdx := range []int{0, 3, 6} {
		req, reader, media, segs := fixtureRequest(t, segIdx)
		res, err := RepairSegment(context.Background(), reader, req)
		if err != nil {
			t.Fatalf("segment %d: repair failed: %v", segIdx, err)
		}
		var start int
		for i := 0; i < segIdx; i++ {
			start += len(segs[i])
		}
		want := media[start : start+len(segs[segIdx])]
		if !bytes.Equal(res.Data, want) {
			t.Fatalf("segment %d reconstructed incorrectly (%d bytes)", segIdx, len(res.Data))
		}
		if res.Blocks <= 0 || res.BytesRead <= 0 {
			t.Fatalf("segment %d: implausible accounting %+v", segIdx, res)
		}
	}
}

func TestRepairSegmentRejectsCorruptedSurvivingData(t *testing.T) {
	req, reader, _, _ := fixtureRequest(t, 3)
	segs := reader.segments[0]
	segs[5][17] ^= 0x01
	if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
		t.Fatalf("corrupted surviving data must abstain, got %v", err)
	}
}

func TestRepairSegmentAbstainsWithoutProofOrGeometry(t *testing.T) {
	t.Run("no reader", func(t *testing.T) {
		req, _, _, _ := fixtureRequest(t, 1)
		if _, err := RepairSegment(context.Background(), nil, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no proof domain", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 1)
		req.Proof = PAR2ProofDomain{}
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("file outside recovery set", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 1)
		req.NZBFileIndex = 9
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("segment index out of range", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 1)
		req.SegIndex = 99
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no injected clock", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 1)
		req.Now = nil
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no recovery volumes", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 1)
		reader.noVolumes = true
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("second segment unavailable", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 1)
		reader.failRead[[2]int{0, 4}] = errors.New("dead")
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRepairSegmentEnforcesBudgets(t *testing.T) {
	t.Run("disabled budget", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 2)
		req.Budget = PAR2RepairBudget{}
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
		if reader.reads != 0 {
			t.Fatalf("a disabled budget must read nothing, saw %d reads", reader.reads)
		}
	})
	t.Run("byte budget below the projection", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 2)
		req.Budget.MaxBytes = 70000
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("byte budget below the measure pass", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 2)
		req.Budget.MaxBytes = 20000
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("recovery volume is reserved before scanning", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 2)
		reader.volumeData[1] = append(reader.volumeData[1], make([]byte, 100000)...)
		req.Budget.MaxBytes = 130000
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
		if reader.volumeOpens != 1 {
			t.Fatalf("opened %d recovery volumes, want 1", reader.volumeOpens)
		}
	})
	t.Run("wallclock budget", func(t *testing.T) {
		req, reader, _, _ := fixtureRequest(t, 2)
		base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
		calls := 0
		req.Now = func() time.Time {
			calls++
			if calls > 2 {
				return base.Add(time.Hour)
			}
			return base
		}
		req.Budget.MaxDuration = time.Second
		if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRepairSegmentHonoursCancellation(t *testing.T) {
	req, reader, _, _ := fixtureRequest(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RepairSegment(ctx, reader, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled repair must return context.Canceled, got %v", err)
	}
}

func TestRepairSegmentCancelsBlockedReadAtBudgetDeadline(t *testing.T) {
	req, reader, _, _ := fixtureRequest(t, 2)
	reader.blockReads = true
	req.Budget.MaxDuration = 20 * time.Millisecond
	started := time.Now()
	_, err := RepairSegment(context.Background(), reader, req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked repair must hit its context deadline, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked repair exceeded bounded shutdown: %s", elapsed)
	}
}

func TestRepairSegmentAbstainsWhenRecoverySlicesAreInsufficient(t *testing.T) {
	// Two segments means one damaged segment spans about half the file --
	// far more blocks than the fixture's six recovery slices can restore.
	media := fixtureMedia()
	info := fixtureInfo(t)
	geo, err := BuildPAR2Geometry(info, fixtureMatches(info, 0))
	if err != nil {
		t.Fatalf("geometry: %v", err)
	}
	reader := newFixtureReader(t, splitSegments(media, 2), 0, 1)
	req := PAR2RepairRequest{
		Geometry:     geo,
		Proof:        fixtureProof(t, info, 0),
		NZBFileIndex: 0,
		SegIndex:     0,
		Budget:       PAR2RepairBudget{MaxBytes: 1 << 20, MaxDuration: time.Minute},
		Now:          fixedClock(),
	}
	_, err = RepairSegment(context.Background(), reader, req)
	if !errors.Is(err, ErrPAR2RepairAbstain) {
		t.Fatalf("insufficient recovery data must abstain, got %v", err)
	}
}

func TestRepairSegmentAbstainsOnIndeterminateSpan(t *testing.T) {
	req, reader, _, segs := fixtureRequest(t, 2)
	// Shorten a surviving segment so the file no longer accounts for its
	// described length: the damaged span becomes unknowable.
	segs2 := reader.segments[0]
	segs2[4] = segs2[4][:len(segs2[4])-32]
	_ = segs
	if _, err := RepairSegment(context.Background(), reader, req); !errors.Is(err, ErrPAR2RepairAbstain) {
		t.Fatalf("indeterminate span must abstain, got %v", err)
	}
}
