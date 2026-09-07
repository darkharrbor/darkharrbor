package nntp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// NS-4.2: bounded PAR2 recovery-slice planning and GF(2^16) reconstruction,
// consumed as ladder rung 4 once the provider rungs have exhausted for one
// segment.
//
// The shape of the problem is dictated by Reed-Solomon, not by preference:
// every recovery slice is a linear combination of *every* input block in the
// recovery set, so isolating the unknown blocks requires folding out every
// surviving block first. That is why this row's contract is a per-repair byte
// and wallclock budget rather than a cheap targeted fetch -- a repair is
// roughly "read the recovery set once", and the budget decides whether that
// is worth doing behind a live stream or whether the pre-existing final-rung
// error should stand unchanged.
//
// Every failure mode here abstains: the caller receives (nil, err) and returns
// the ORIGINAL rung-1..3 error, so no repair path can ever convert a legible
// provider verdict into a different one, and no unverified byte is served.

// ErrPAR2RepairAbstain is the sentinel wrapping every condition under which
// stream-time repair declines to produce bytes.
var ErrPAR2RepairAbstain = errors.New("par2repair: abstained")

const (
	// maxRepairRecoveryVolumes bounds how many recovery volumes the planner
	// will open while discovering exponents.
	maxRepairRecoveryVolumes = 32
	// repairBlockPadding is the zero filler fed in place of the damaged
	// segment. It only ever lands inside blocks already declared unknown.
	repairSegmentChunk = 1 << 20
)

// PAR2RepairBudget bounds one reconstruction.
type PAR2RepairBudget struct {
	// MaxBytes is the ceiling on decoded payload read for this repair,
	// counting the measure pass, the accumulate pass, and recovery slices.
	// Zero or negative disables repair entirely.
	MaxBytes int64
	// MaxDuration is the wallclock ceiling on the injected clock. Zero or
	// negative disables repair entirely.
	MaxDuration time.Duration
}

// Enabled reports whether both halves of the budget permit a repair.
func (b PAR2RepairBudget) Enabled() bool { return b.MaxBytes > 0 && b.MaxDuration > 0 }

// PAR2GeometryFile is one recovery-set member's global input-block span.
type PAR2GeometryFile struct {
	FileID       string
	Length       int64
	NZBFileIndex int
	FirstBlock   int64
	Blocks       int64
}

// PAR2Geometry is the recovery set's exact input-block numbering. PAR2
// numbers input blocks globally, in the Main packet's declared recovery-set
// order, so a per-file view is not sufficient to name a block.
type PAR2Geometry struct {
	SliceSize     int64
	RecoverySetID string
	Files         []PAR2GeometryFile
	total         int64
}

// TotalBlocks returns the recovery set's global input-block count.
func (g PAR2Geometry) TotalBlocks() int64 { return g.total }

// PayloadBytes returns the recovery set's total declared decoded length.
func (g PAR2Geometry) PayloadBytes() int64 {
	var sum int64
	for _, f := range g.Files {
		sum += f.Length
	}
	return sum
}

// FileByNZBIndex returns the geometry entry bound to one NZB file index.
func (g PAR2Geometry) FileByNZBIndex(nzbFileIndex int) (PAR2GeometryFile, bool) {
	for _, f := range g.Files {
		if f.NZBFileIndex == nzbFileIndex {
			return f, true
		}
	}
	return PAR2GeometryFile{}, false
}

// BuildPAR2Geometry derives exact global block numbering from a parsed PAR2
// index and the exact FileDesc-to-NZB matches NS-2.2 already owns.
//
// It is deliberately strict. If any recovery-set file id lacks a FileDesc, or
// lacks an exact NZB match, or two entries claim the same NZB file, the
// numbering would silently shift and every reconstructed block would be
// wrong-but-plausible. All of those abstain instead.
func BuildPAR2Geometry(info archiveparser.PAR2Info, matches []PAR2FileMatch) (PAR2Geometry, error) {
	if info.SliceSize <= 0 || info.SliceSize%4 != 0 || info.SliceSize > archiveparser.MaxPAR2SliceBytes {
		return PAR2Geometry{}, fmt.Errorf("%w: unusable slice size", ErrPAR2RepairAbstain)
	}
	if len(info.RecoverySetIDs) == 0 {
		return PAR2Geometry{}, fmt.Errorf("%w: no Main packet recovery set", ErrPAR2RepairAbstain)
	}
	if info.RecoverySetID == "" {
		return PAR2Geometry{}, fmt.Errorf("%w: no recovery set identity", ErrPAR2RepairAbstain)
	}
	byID := make(map[string]archiveparser.PAR2FileDesc, len(info.Files))
	for _, desc := range info.Files {
		byID[desc.FileID] = desc
	}
	matchByID := make(map[string]int, len(matches))
	for _, m := range matches {
		if m.NZBFileIdx < 0 || m.Desc.FileID == "" {
			continue
		}
		if prior, dup := matchByID[m.Desc.FileID]; dup && prior != m.NZBFileIdx {
			return PAR2Geometry{}, fmt.Errorf("%w: ambiguous file identity", ErrPAR2RepairAbstain)
		}
		matchByID[m.Desc.FileID] = m.NZBFileIdx
	}

	geo := PAR2Geometry{SliceSize: info.SliceSize, RecoverySetID: info.RecoverySetID}
	usedNZB := make(map[int]struct{}, len(info.RecoverySetIDs))
	var next int64
	for _, id := range info.RecoverySetIDs {
		desc, ok := byID[id]
		if !ok || desc.Length <= 0 {
			return PAR2Geometry{}, fmt.Errorf("%w: recovery-set member has no usable description", ErrPAR2RepairAbstain)
		}
		nzbIdx, ok := matchByID[id]
		if !ok {
			return PAR2Geometry{}, fmt.Errorf("%w: recovery-set member is unmatched", ErrPAR2RepairAbstain)
		}
		if _, dup := usedNZB[nzbIdx]; dup {
			return PAR2Geometry{}, fmt.Errorf("%w: two recovery-set members claim one NZB file", ErrPAR2RepairAbstain)
		}
		usedNZB[nzbIdx] = struct{}{}
		blocks := (desc.Length-1)/info.SliceSize + 1
		if blocks <= 0 || next > archiveparser.MaxPAR2TotalBlocks-blocks {
			return PAR2Geometry{}, fmt.Errorf("%w: recovery set exceeds the field's block ceiling", ErrPAR2RepairAbstain)
		}
		geo.Files = append(geo.Files, PAR2GeometryFile{
			FileID:       id,
			Length:       desc.Length,
			NZBFileIndex: nzbIdx,
			FirstBlock:   next,
			Blocks:       blocks,
		})
		next += blocks
	}
	geo.total = next
	if geo.total <= 0 {
		return PAR2Geometry{}, fmt.Errorf("%w: empty recovery set", ErrPAR2RepairAbstain)
	}
	return geo, nil
}

// PAR2RepairReader is the bounded I/O surface a repair needs. Implementations
// own pooling, cancellation, caching, and accounting; the planner owns
// ordering, budgets, and verification.
type PAR2RepairReader interface {
	// SegmentCount reports how many segments one recovery-set NZB file has.
	SegmentCount(nzbFileIndex int) (int, bool)
	// ReadSegment returns one segment's yEnc-decoded payload.
	ReadSegment(ctx context.Context, nzbFileIndex, segIndex int) ([]byte, error)
	// RecoveryVolumeIndexes lists candidate recovery volumes, cheapest first.
	RecoveryVolumeIndexes() []int
	// RecoverySource opens bounded random access over one recovery volume.
	RecoverySource(ctx context.Context, nzbFileIndex int) (bytesource.ByteSource, error)
}

// PAR2RepairRequest describes exactly one damaged segment.
type PAR2RepairRequest struct {
	Geometry     PAR2Geometry
	Proof        PAR2ProofDomain
	NZBFileIndex int
	SegIndex     int
	Budget       PAR2RepairBudget
	Now          func() time.Time
	Log          *slog.Logger
}

// PAR2RepairResult is one successful reconstruction.
type PAR2RepairResult struct {
	// Data is the damaged segment's exact decoded payload.
	Data []byte
	// BytesRead is the decoded payload actually consumed by this repair.
	BytesRead int64
	// Blocks is the number of input blocks reconstructed.
	Blocks int
}

// RepairSegment reconstructs one damaged segment's decoded payload.
//
// Sequence: measure the file's surviving segments to learn the damaged
// segment's exact decoded span (the yEnc-declared size is not exact and the
// damaged article cannot be read for its own header), select recovery
// exponents, fold every surviving input block of the recovery set out of the
// recovery data, solve, verify every reconstructed block against its native
// IFSC hash, verify the block edges against bytes already known, and only
// then return the span.
func RepairSegment(ctx context.Context, r PAR2RepairReader, req PAR2RepairRequest) (PAR2RepairResult, error) {
	if r == nil {
		return PAR2RepairResult{}, fmt.Errorf("%w: no repair reader", ErrPAR2RepairAbstain)
	}
	if !req.Budget.Enabled() {
		return PAR2RepairResult{}, fmt.Errorf("%w: repair budget disabled", ErrPAR2RepairAbstain)
	}
	if req.Now == nil {
		return PAR2RepairResult{}, fmt.Errorf("%w: no injected clock", ErrPAR2RepairAbstain)
	}
	ctx, cancel := context.WithTimeout(ctx, req.Budget.MaxDuration)
	defer cancel()
	geo := req.Geometry
	sliceSize := geo.SliceSize
	if sliceSize <= 0 {
		return PAR2RepairResult{}, fmt.Errorf("%w: unusable geometry", ErrPAR2RepairAbstain)
	}
	target, ok := geo.FileByNZBIndex(req.NZBFileIndex)
	if !ok {
		return PAR2RepairResult{}, fmt.Errorf("%w: damaged file is outside the recovery set", ErrPAR2RepairAbstain)
	}
	if req.Proof.Length() != target.Length {
		return PAR2RepairResult{}, fmt.Errorf("%w: proof domain does not describe this file", ErrPAR2RepairAbstain)
	}
	if req.Proof.SliceSize() != sliceSize {
		return PAR2RepairResult{}, fmt.Errorf("%w: proof slice size disagrees with recovery set", ErrPAR2RepairAbstain)
	}

	deadline := req.Now().Add(req.Budget.MaxDuration)
	acct := &repairAccount{budget: req.Budget, now: req.Now, deadline: deadline}

	// Phase 1 -- measure. Shared with NS-7.2 so reconstruction and cross-lane
	// recovery can never disagree about the damaged segment's decoded span.
	damagedStart, damagedLen, err := measureDamagedSegment(ctx, r, req.NZBFileIndex, req.SegIndex, target.Length, acct)
	if err != nil {
		return PAR2RepairResult{}, err
	}
	if damagedLen > sliceSize*int64(archiveparser.MaxPAR2UnknownBlocks) {
		return PAR2RepairResult{}, fmt.Errorf("%w: damaged span is not determinate", ErrPAR2RepairAbstain)
	}
	damagedEnd := damagedStart + damagedLen
	if damagedEnd > target.Length {
		return PAR2RepairResult{}, fmt.Errorf("%w: damaged span exceeds the described file", ErrPAR2RepairAbstain)
	}

	// Unknown blocks are every block the damaged span touches: Reed-Solomon
	// operates on whole blocks, so a block holding one unknown byte is
	// entirely unknown.
	firstBlock := damagedStart / sliceSize
	lastBlock := (damagedEnd - 1) / sliceSize
	unknownCount := int(lastBlock - firstBlock + 1)
	if unknownCount <= 0 || unknownCount > archiveparser.MaxPAR2UnknownBlocks {
		return PAR2RepairResult{}, fmt.Errorf("%w: unknown block count out of bounds", ErrPAR2RepairAbstain)
	}
	unknown := make([]int64, 0, unknownCount)
	for b := firstBlock; b <= lastBlock; b++ {
		unknown = append(unknown, target.FirstBlock+b)
	}

	// Whole-recovery-set projection before any bulk read. Recovery-volume
	// sources are charged at full declared size before discovery below.
	projected := acct.spent + geo.PayloadBytes()
	if projected > req.Budget.MaxBytes {
		return PAR2RepairResult{}, fmt.Errorf("%w: projected %d bytes exceeds the %d byte repair budget",
			ErrPAR2RepairAbstain, projected, req.Budget.MaxBytes)
	}

	// Recovery slice selection.
	slices, err := collectRecoverySlices(ctx, r, geo, unknownCount, acct)
	if err != nil {
		return PAR2RepairResult{}, err
	}
	exponents := make([]uint32, 0, len(slices))
	for _, s := range slices {
		exponents = append(exponents, s.Exponent)
	}
	solver, err := archiveparser.NewPAR2Solver(sliceSize, geo.total, unknown, exponents)
	if err != nil {
		return PAR2RepairResult{}, fmt.Errorf("%w: %v", ErrPAR2RepairAbstain, err)
	}
	for _, s := range slices {
		if err := solver.AddRecoverySlice(s.Exponent, s.Data); err != nil {
			return PAR2RepairResult{}, fmt.Errorf("%w: %v", ErrPAR2RepairAbstain, err)
		}
	}

	// Phase 2 -- accumulate. Blocks are emitted in global order; unknown
	// blocks are dropped, retaining only their edges for verification.
	edges := &repairEdges{
		sliceSize:    sliceSize,
		firstBlock:   target.FirstBlock + firstBlock,
		lastBlock:    target.FirstBlock + lastBlock,
		damagedStart: damagedStart % sliceSize,
		damagedEnd:   damagedEnd - lastBlock*sliceSize,
	}
	unknownSet := make(map[int64]struct{}, unknownCount)
	for _, b := range unknown {
		unknownSet[b] = struct{}{}
	}
	for _, f := range geo.Files {
		skip := -1
		if f.NZBFileIndex == req.NZBFileIndex {
			skip = req.SegIndex
		}
		if err := accumulateFileBlocks(ctx, r, f, sliceSize, skip, damagedLen, unknownSet, solver, edges, acct); err != nil {
			return PAR2RepairResult{}, err
		}
	}

	if err := acct.check(ctx); err != nil {
		return PAR2RepairResult{}, err
	}
	solved, err := solver.Solve()
	if err != nil {
		return PAR2RepairResult{}, fmt.Errorf("%w: %v", ErrPAR2RepairAbstain, err)
	}
	if len(solved) != unknownCount {
		return PAR2RepairResult{}, fmt.Errorf("%w: solver returned the wrong block count", ErrPAR2RepairAbstain)
	}

	// Verify every reconstructed block against its own native IFSC hash, and
	// verify the two partial blocks against bytes already in hand. Either
	// check failing means the reconstruction is not the real content.
	out := make([]byte, 0, damagedLen)
	for i, block := range solved {
		fileBlock := firstBlock + int64(i)
		blockOffset := fileBlock * sliceSize
		proof, ok := req.Proof.ProofAt(blockOffset)
		if !ok {
			return PAR2RepairResult{}, fmt.Errorf("%w: no IFSC proof for a reconstructed block", ErrPAR2RepairAbstain)
		}
		if proof.Offset != blockOffset || proof.Length <= 0 || proof.Length > sliceSize {
			return PAR2RepairResult{}, fmt.Errorf("%w: IFSC proof geometry disagrees", ErrPAR2RepairAbstain)
		}
		if int64(len(block)) != sliceSize {
			return PAR2RepairResult{}, fmt.Errorf("%w: reconstructed block has the wrong length", ErrPAR2RepairAbstain)
		}
		// PAR2 zero-pads a file's final block; anything else there is noise.
		for _, b := range block[proof.Length:] {
			if b != 0 {
				return PAR2RepairResult{}, fmt.Errorf("%w: reconstructed tail padding is not zero", ErrPAR2RepairAbstain)
			}
		}
		digest, err := proof.CandidateDigest(ctx, block[:proof.Length])
		if err != nil {
			return PAR2RepairResult{}, fmt.Errorf("%w: %v", ErrPAR2RepairAbstain, err)
		}
		if !proof.DigestEquals(digest) {
			return PAR2RepairResult{}, fmt.Errorf("%w: reconstructed block failed IFSC verification", ErrPAR2RepairAbstain)
		}
		lo, hi := int64(0), proof.Length
		if fileBlock == firstBlock {
			lo = damagedStart - blockOffset
		}
		if fileBlock == lastBlock {
			hi = damagedEnd - blockOffset
		}
		if lo < 0 || hi > int64(len(block)) || lo > hi {
			return PAR2RepairResult{}, fmt.Errorf("%w: damaged span does not lie inside its blocks", ErrPAR2RepairAbstain)
		}
		out = append(out, block[lo:hi]...)
	}
	if err := edges.verify(solved); err != nil {
		return PAR2RepairResult{}, err
	}
	if int64(len(out)) != damagedLen {
		return PAR2RepairResult{}, fmt.Errorf("%w: reconstructed span length disagrees", ErrPAR2RepairAbstain)
	}
	if req.Log != nil {
		req.Log.Info("nntp: par2 rung-4 repair succeeded",
			"event", "par2_repair_succeeded",
			"blocks", unknownCount,
			"bytes_read", acct.spent,
			"span_bytes", damagedLen,
		)
	}
	return PAR2RepairResult{Data: out, BytesRead: acct.spent, Blocks: unknownCount}, nil
}

func measureDamagedSegment(ctx context.Context, r PAR2RepairReader, fileIndex, segIndex int, fileLength int64, acct *repairAccount) (int64, int64, error) {
	segCount, ok := r.SegmentCount(fileIndex)
	if !ok || segCount <= 0 || segIndex < 0 || segIndex >= segCount || fileLength <= 0 || acct == nil {
		return 0, 0, fmt.Errorf("%w: damaged segment index is out of range", ErrPAR2RepairAbstain)
	}
	sizes := make([]int64, segCount)
	var known int64
	for i := 0; i < segCount; i++ {
		if i == segIndex {
			continue
		}
		if err := acct.check(ctx); err != nil {
			return 0, 0, err
		}
		data, err := r.ReadSegment(ctx, fileIndex, i)
		if err != nil {
			if ctx.Err() != nil {
				return 0, 0, ctx.Err()
			}
			return 0, 0, fmt.Errorf("%w: a second segment of the damaged file is unavailable", ErrPAR2RepairAbstain)
		}
		if err := acct.spend(int64(len(data))); err != nil {
			return 0, 0, err
		}
		sizes[i] = int64(len(data))
		known += sizes[i]
	}
	damagedLen := fileLength - known
	if damagedLen <= 0 {
		return 0, 0, fmt.Errorf("%w: damaged span is not determinate", ErrPAR2RepairAbstain)
	}
	sizes[segIndex] = damagedLen
	var damagedStart int64
	for i := 0; i < segIndex; i++ {
		damagedStart += sizes[i]
	}
	if damagedStart < 0 || damagedStart > fileLength || damagedLen > fileLength-damagedStart {
		return 0, 0, fmt.Errorf("%w: damaged span exceeds the described file", ErrPAR2RepairAbstain)
	}
	return damagedStart, damagedLen, nil
}

// collectRecoverySlices discovers exponents across the cheapest recovery
// volumes and materialises exactly as many slices as there are unknowns.
func collectRecoverySlices(
	ctx context.Context,
	r PAR2RepairReader,
	geo PAR2Geometry,
	need int,
	acct *repairAccount,
) ([]archiveparser.PAR2RecoverySlice, error) {
	volumes := r.RecoveryVolumeIndexes()
	if len(volumes) == 0 {
		return nil, fmt.Errorf("%w: no recovery volumes in this post", ErrPAR2RepairAbstain)
	}
	if len(volumes) > maxRepairRecoveryVolumes {
		volumes = volumes[:maxRepairRecoveryVolumes]
	}
	want := make(map[uint32]struct{}, need)
	perVolume := make(map[int][]uint32, len(volumes))
	sources := make(map[int]bytesource.ByteSource, len(volumes))
	for _, idx := range volumes {
		if len(want) >= need {
			break
		}
		if err := acct.check(ctx); err != nil {
			return nil, err
		}
		src, err := r.RecoverySource(ctx, idx)
		if err != nil {
			continue
		}
		// A chunked ByteSource may fetch a complete NNTP segment for a small
		// header read. Reserve the source's full declared size before the
		// first read so upstream traffic cannot escape the repair budget.
		if err := acct.spend(src.Size()); err != nil {
			return nil, err
		}
		sources[idx] = src
		exps, err := archiveparser.PAR2RecoveryExponents(ctx, src, geo.RecoverySetID, geo.SliceSize, need)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		for _, e := range exps {
			if len(want) >= need {
				break
			}
			if _, dup := want[e]; dup {
				continue
			}
			want[e] = struct{}{}
			perVolume[idx] = append(perVolume[idx], e)
		}
	}
	if len(want) < need {
		return nil, fmt.Errorf("%w: only %d of %d required recovery slices are available",
			ErrPAR2RepairAbstain, len(want), need)
	}

	out := make([]archiveparser.PAR2RecoverySlice, 0, need)
	for _, idx := range volumes {
		exps := perVolume[idx]
		if len(exps) == 0 {
			continue
		}
		if err := acct.check(ctx); err != nil {
			return nil, err
		}
		src := sources[idx]
		subset := make(map[uint32]struct{}, len(exps))
		for _, e := range exps {
			subset[e] = struct{}{}
		}
		remaining := acct.remaining()
		slices, err := archiveparser.ScanPAR2RecoverySlices(ctx, src, geo.RecoverySetID, geo.SliceSize, subset, remaining)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%w: recovery slice read failed", ErrPAR2RepairAbstain)
		}
		out = append(out, slices...)
	}
	if len(out) != need {
		return nil, fmt.Errorf("%w: recovery slice collection is incomplete", ErrPAR2RepairAbstain)
	}
	return out, nil
}

// accumulateFileBlocks streams one recovery-set member's decoded payload,
// folding every known block out of the solver's right-hand sides. skipSeg is
// the damaged segment index for the damaged file (-1 elsewhere); its bytes are
// replaced with zero filler of the determined length, which by construction
// only ever lands inside blocks already declared unknown.
func accumulateFileBlocks(
	ctx context.Context,
	r PAR2RepairReader,
	f PAR2GeometryFile,
	sliceSize int64,
	skipSeg int,
	skipLen int64,
	unknown map[int64]struct{},
	solver *archiveparser.PAR2Solver,
	edges *repairEdges,
	acct *repairAccount,
) error {
	segCount, ok := r.SegmentCount(f.NZBFileIndex)
	if !ok || segCount <= 0 {
		return fmt.Errorf("%w: recovery-set member has no segments", ErrPAR2RepairAbstain)
	}
	buf := make([]byte, 0, int(sliceSize))
	block := f.FirstBlock
	var produced int64

	emit := func(data []byte) error {
		buf = append(buf, data...)
		for int64(len(buf)) >= sliceSize {
			if err := acct.check(ctx); err != nil {
				return err
			}
			full := buf[:sliceSize]
			if _, isUnknown := unknown[block]; isUnknown {
				edges.observe(block, full)
			} else if err := solver.AddKnownBlock(block, full); err != nil {
				return fmt.Errorf("%w: %v", ErrPAR2RepairAbstain, err)
			}
			buf = append(buf[:0], buf[sliceSize:]...)
			block++
		}
		return nil
	}

	for i := 0; i < segCount; i++ {
		if err := acct.check(ctx); err != nil {
			return err
		}
		if i == skipSeg {
			remaining := skipLen
			zeros := make([]byte, minInt64(remaining, repairSegmentChunk))
			for remaining > 0 {
				n := minInt64(remaining, int64(len(zeros)))
				if err := emit(zeros[:n]); err != nil {
					return err
				}
				produced += n
				remaining -= n
			}
			continue
		}
		data, err := r.ReadSegment(ctx, f.NZBFileIndex, i)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: recovery-set member segment unavailable", ErrPAR2RepairAbstain)
		}
		if err := acct.spend(int64(len(data))); err != nil {
			return err
		}
		if produced+int64(len(data)) > f.Length {
			return fmt.Errorf("%w: member payload exceeds its described length", ErrPAR2RepairAbstain)
		}
		produced += int64(len(data))
		if err := emit(data); err != nil {
			return err
		}
	}
	if produced != f.Length {
		return fmt.Errorf("%w: member payload is %d bytes against a described %d", ErrPAR2RepairAbstain, produced, f.Length)
	}
	if len(buf) > 0 {
		if err := acct.check(ctx); err != nil {
			return err
		}
		if _, isUnknown := unknown[block]; isUnknown {
			edges.observe(block, buf)
		} else if err := solver.AddKnownBlock(block, buf); err != nil {
			return fmt.Errorf("%w: %v", ErrPAR2RepairAbstain, err)
		}
		block++
	}
	if block != f.FirstBlock+f.Blocks {
		return fmt.Errorf("%w: member block count disagrees with its geometry", ErrPAR2RepairAbstain)
	}
	return nil
}

// repairEdges retains only the known bytes that sit inside an unknown block:
// the head of the first unknown block and the tail of the last. Verifying the
// reconstruction against them catches a plausible-but-wrong solve that happens
// to satisfy a hash collision or a mis-numbered block.
type repairEdges struct {
	sliceSize    int64
	firstBlock   int64
	lastBlock    int64
	damagedStart int64 // offset within firstBlock
	damagedEnd   int64 // offset within lastBlock
	head         []byte
	tail         []byte
	haveHead     bool
	haveTail     bool
}

func (e *repairEdges) observe(block int64, data []byte) {
	if e == nil {
		return
	}
	if block == e.firstBlock && e.damagedStart > 0 && !e.haveHead {
		n := e.damagedStart
		if n > int64(len(data)) {
			n = int64(len(data))
		}
		e.head = append([]byte(nil), data[:n]...)
		e.haveHead = true
	}
	if block == e.lastBlock && !e.haveTail && e.damagedEnd < int64(len(data)) {
		e.tail = append([]byte(nil), data[e.damagedEnd:]...)
		e.haveTail = true
	}
}

func (e *repairEdges) verify(solved [][]byte) error {
	if e == nil {
		return nil
	}
	if e.haveHead && len(solved) > 0 {
		got := solved[0]
		if int64(len(got)) < int64(len(e.head)) || !bytes.Equal(got[:len(e.head)], e.head) {
			return fmt.Errorf("%w: reconstructed head disagrees with bytes already held", ErrPAR2RepairAbstain)
		}
	}
	if e.haveTail && len(solved) > 0 {
		got := solved[len(solved)-1]
		if e.damagedEnd < 0 || e.damagedEnd+int64(len(e.tail)) > int64(len(got)) {
			return fmt.Errorf("%w: reconstructed tail is out of range", ErrPAR2RepairAbstain)
		}
		if !bytes.Equal(got[e.damagedEnd:e.damagedEnd+int64(len(e.tail))], e.tail) {
			return fmt.Errorf("%w: reconstructed tail disagrees with bytes already held", ErrPAR2RepairAbstain)
		}
	}
	return nil
}

// repairAccount enforces the byte and wallclock halves of the budget on the
// injected clock. Every bulk operation checks it before spending.
type repairAccount struct {
	budget   PAR2RepairBudget
	now      func() time.Time
	deadline time.Time
	spent    int64
}

func (a *repairAccount) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !a.now().Before(a.deadline) {
		return fmt.Errorf("%w: repair exceeded its %s wallclock budget", ErrPAR2RepairAbstain, a.budget.MaxDuration)
	}
	return nil
}

func (a *repairAccount) spend(n int64) error {
	if n < 0 {
		return fmt.Errorf("%w: negative read accounting", ErrPAR2RepairAbstain)
	}
	if a.spent > a.budget.MaxBytes-n {
		return fmt.Errorf("%w: repair exceeded its %d byte budget", ErrPAR2RepairAbstain, a.budget.MaxBytes)
	}
	a.spent += n
	return nil
}

func (a *repairAccount) remaining() int64 {
	if a.spent >= a.budget.MaxBytes {
		return 0
	}
	return a.budget.MaxBytes - a.spent
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
