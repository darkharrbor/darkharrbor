package nntp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/ladder"
)

// NS-4.2 rung 4: the live wiring between the segment fetch ladder and the
// bounded PAR2 reconstruction planner.
//
// Placement is deliberate. Rung 4 hangs off nntpChunkSource.Fetch, the single
// point where a demand segment fetch has already walked every provider rung
// and come back empty. Repaired bytes are returned as that chunk's payload, so
// the shared rangecache stores them under the existing message-ID payload key
// -- the N2 key the frozen plan requires -- with no second cache, no new
// persistence, and no change to how any consumer reads a segment.

// PAR2ProofLookup returns an item's persisted NS-4.1 IFSC proof index, or nil
// when the item has none. It is supplied by the composition root so this
// package never imports the store.
type PAR2ProofLookup func(ctx context.Context, itemID string) *archiveparser.PAR2ProofIndex

// SetPAR2Repair installs the rung-4 dependencies. A nil lookup or a disabled
// budget leaves the provider byte-identical to its pre-NS-4.2 behavior.
func (p *NNTPProvider) SetPAR2Repair(lookup PAR2ProofLookup, budget PAR2RepairBudget) {
	if p == nil {
		return
	}
	p.proofLookup = lookup
	p.repairBudget = budget
}

// SetRepairClock overrides the repair wallclock for deterministic tests.
func (p *NNTPProvider) SetRepairClock(now func() time.Time) {
	if p == nil || now == nil {
		return
	}
	p.repairNow = now
}

func (p *NNTPProvider) repairClock() func() time.Time {
	if p != nil && p.repairNow != nil {
		return p.repairNow
	}
	return time.Now
}

// par2RepairSession is one stream's repair capability. It resolves the
// recovery-set geometry at most once, serialises repairs so two readers
// cannot both pay for the same reconstruction, and abstains on anything it
// cannot establish exactly.
type par2RepairSession struct {
	provider   *NNTPProvider
	itemID     string
	contentKey string
	nzb        NZB
	targetIdx  int
	proofIndex *archiveparser.PAR2ProofIndex
	budget     PAR2RepairBudget
	now        func() time.Time
	log        *slog.Logger

	slot chan struct{}

	mu       sync.Mutex
	resolved bool
	geo      PAR2Geometry
	proof    PAR2ProofDomain
	geoErr   error
}

// newPAR2RepairSession builds a repair session for one streamed NZB file, or
// returns nil when any precondition for repair is absent. A nil session makes
// every rung-4 call a no-op.
func (p *NNTPProvider) newPAR2RepairSession(ctx context.Context, itemID, contentKey string, nzbData []byte, streamed NZBFile) *par2RepairSession {
	if p == nil || p.cache == nil || p.proofLookup == nil || itemID == "" {
		return nil
	}
	if !p.repairBudget.Enabled() {
		return nil
	}
	proofIndex := p.proofLookup(ctx, itemID)
	if proofIndex == nil || len(proofIndex.Files) == 0 {
		return nil
	}
	nzb, err := ParseNZB(nzbData)
	if err != nil || len(nzb.Files) == 0 {
		return nil
	}
	targetIdx, ok := locateNZBFileIndex(nzb.Files, streamed)
	if !ok {
		return nil
	}
	proof, ok := NewPAR2ProofDomain(proofIndex, targetIdx)
	if !ok {
		return nil
	}
	return &par2RepairSession{
		provider:   p,
		itemID:     itemID,
		contentKey: contentKey,
		nzb:        nzb,
		targetIdx:  targetIdx,
		proofIndex: proofIndex,
		budget:     p.repairBudget,
		now:        p.repairClock(),
		log:        p.log,
		slot:       make(chan struct{}, 1),
		proof:      proof,
	}
}

// locateNZBFileIndex maps a streamed file (taken from the video-filtered list)
// back to its stable ParseNZB index, which is the index the persisted proof
// material is keyed by. An ambiguous or absent match abstains: guessing here
// would bind proofs to the wrong file.
func locateNZBFileIndex(files []NZBFile, streamed NZBFile) (int, bool) {
	if len(streamed.Segments) == 0 {
		return -1, false
	}
	first := streamed.Segments[0].MessageID
	if first == "" {
		return -1, false
	}
	found := -1
	for i, f := range files {
		if len(f.Segments) != len(streamed.Segments) || f.Segments[0].MessageID != first {
			continue
		}
		if found >= 0 {
			return -1, false
		}
		found = i
	}
	if found < 0 {
		return -1, false
	}
	return found, true
}

// eligibleForRepair reports whether a failed segment fetch is the kind of
// failure PAR2 can address: the article is genuinely unavailable after every
// provider rung. Cancellation, protocol errors, and account conditions are
// not repairable and must surface unchanged.
func eligibleForRepair(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ladder.ErrAccountLevel) {
		return false
	}
	return errors.Is(err, ErrArticleMissing) ||
		errors.Is(err, ErrDeadPost) ||
		errors.Is(err, ladder.ErrExhausted)
}

// RepairSegment attempts one bounded reconstruction of a damaged segment.
// Every abstain path returns an error, and the caller then surfaces the
// original provider verdict untouched.
func (s *par2RepairSession) RepairSegment(ctx context.Context, segIndex int) ([]byte, error) {
	if s == nil {
		return nil, ErrPAR2RepairAbstain
	}
	select {
	case s.slot <- struct{}{}:
		defer func() { <-s.slot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, s.budget.MaxDuration)
	defer cancel()

	geo, proof, err := s.resolveGeometry(ctx)
	if err != nil {
		return nil, err
	}
	reader := &par2NZBRepairReader{session: s}
	started := s.now()
	res, err := RepairSegment(ctx, reader, PAR2RepairRequest{
		Geometry:     geo,
		Proof:        proof,
		NZBFileIndex: s.targetIdx,
		SegIndex:     segIndex,
		Budget:       s.budget,
		Now:          s.now,
		Log:          nil,
	})
	if err != nil {
		if s.log != nil && ctx.Err() == nil {
			s.log.Info("nntp: par2 rung-4 repair abstained",
				"event", "par2_repair_abstained",
				"segment", segIndex,
				"reason", err.Error(),
			)
		}
		return nil, err
	}
	if s.log != nil {
		s.log.Info("nntp: par2 rung-4 repair served a damaged segment",
			"event", "par2_repair_served",
			"segment", segIndex,
			"blocks", res.Blocks,
			"bytes_read", res.BytesRead,
			"span_bytes", len(res.Data),
			"elapsed_ms", s.now().Sub(started).Milliseconds(),
		)
	}
	return res.Data, nil
}

// resolveGeometry fetches and parses the small PAR2 index once per stream and
// binds it to the exact NZB files. A cancellation is not cached, so a repair
// attempted after a client disconnect does not permanently disable rung 4.
func (s *par2RepairSession) resolveGeometry(ctx context.Context) (PAR2Geometry, PAR2ProofDomain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolved {
		return s.geo, s.proof, s.geoErr
	}
	info, par2Idx, ok, err := s.provider.FetchPAR2ForItem(ctx, s.itemID, s.nzb)
	if err != nil || !ok {
		if ctx.Err() != nil {
			return PAR2Geometry{}, PAR2ProofDomain{}, ctx.Err()
		}
		s.resolved = true
		s.geoErr = fmt.Errorf("%w: no PAR2 index is reachable for this post", ErrPAR2RepairAbstain)
		return PAR2Geometry{}, PAR2ProofDomain{}, s.geoErr
	}
	matches, matchErr := s.provider.MatchPAR2FilesForItem(ctx, s.itemID, s.nzb, info, par2Idx)
	if matchErr != nil && ctx.Err() != nil {
		return PAR2Geometry{}, PAR2ProofDomain{}, ctx.Err()
	}
	geo, err := BuildPAR2Geometry(info, matches)
	s.resolved = true
	if err != nil {
		s.geoErr = err
		return PAR2Geometry{}, PAR2ProofDomain{}, err
	}
	s.geo = geo
	return s.geo, s.proof, nil
}

// par2NZBRepairReader adapts one item's NZB to the planner's I/O surface. All
// segment reads go through the existing shared segment cache, so a repair
// reuses whatever is already cached and leaves everything it touches cached
// under the same message-ID payload keys ordinary playback uses.
type par2NZBRepairReader struct {
	session *par2RepairSession
}

func (r *par2NZBRepairReader) file(nzbFileIndex int) (NZBFile, bool) {
	files := r.session.nzb.Files
	if nzbFileIndex < 0 || nzbFileIndex >= len(files) {
		return NZBFile{}, false
	}
	return files[nzbFileIndex], true
}

func (r *par2NZBRepairReader) SegmentCount(nzbFileIndex int) (int, bool) {
	file, ok := r.file(nzbFileIndex)
	if !ok || len(file.Segments) == 0 {
		return 0, false
	}
	return len(file.Segments), true
}

func (r *par2NZBRepairReader) ReadSegment(ctx context.Context, nzbFileIndex, segIndex int) ([]byte, error) {
	file, ok := r.file(nzbFileIndex)
	if !ok || segIndex < 0 || segIndex >= len(file.Segments) {
		return nil, errors.New("par2repair: segment coordinate out of range")
	}
	cache := r.session.provider.cache
	if cache == nil {
		return nil, errors.New("par2repair: no segment cache")
	}
	contentKey := SegmentOffsetKey(r.session.contentKey, file.Segments)
	data, err := cache.GetSegmentWithMeta(
		withAccountingItemID(ctx, r.session.itemID),
		file.Segments[segIndex], r.session.itemID, contentKey, nzbFileIndex, segIndex)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("par2repair: segment read produced no bytes")
	}
	return data, nil
}

func (r *par2NZBRepairReader) RecoveryVolumeIndexes() []int {
	files := r.session.nzb.Files
	idx := make([]int, 0, len(files))
	for i, f := range files {
		if len(f.Segments) == 0 || f.TotalBytes <= 0 {
			continue
		}
		if archiveparser.PAR2IsVolumeFile(f.Subject) {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return files[idx[a]].TotalBytes < files[idx[b]].TotalBytes
	})
	return idx
}

func (r *par2NZBRepairReader) RecoverySource(ctx context.Context, nzbFileIndex int) (bytesource.ByteSource, error) {
	file, ok := r.file(nzbFileIndex)
	if !ok || len(file.Segments) == 0 {
		return nil, errors.New("par2repair: recovery volume coordinate out of range")
	}
	return r.session.provider.recoveryByteSource(
		withAccountingItemID(ctx, r.session.itemID), nzbFileIndex, file.Segments)
}
