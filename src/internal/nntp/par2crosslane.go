package nntp

import (
	"context"
	"errors"
	"fmt"
	"time"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
)

// ErrCrossLaneAbstain identifies every safe rung-5 no-op.
var ErrCrossLaneAbstain = errors.New("nntp: cross-lane recovery abstained")

// CrossLaneBlock is one XR-verified IFSC block. Lane is secret-free and used
// only for aggregate counters; no source identity is persisted.
type CrossLaneBlock struct {
	Data []byte
	Lane contentproof.Lane
}

// CrossLaneRecover feeds one exact IFSC proof through the shared XR owner.
type CrossLaneRecover func(ctx context.Context, itemID string, fileIndex int, fileSize int64, proof PAR2Proof) (CrossLaneBlock, error)

// SetCrossLaneRecovery installs optional rung 5. A nil callback leaves all
// existing streams byte-identical; the one-slot provider bound is cancellable.
func (p *NNTPProvider) SetCrossLaneRecovery(recover CrossLaneRecover) {
	if p == nil {
		return
	}
	p.crossLaneRecover = recover
	if recover == nil {
		p.crossLaneSlot = nil
		return
	}
	p.crossLaneSlot = make(chan struct{}, 1)
}

// RecoverSegment preserves rung order: provider rungs, PAR2 reconstruction,
// then proof-gated cross-lane recovery. One wall-clock budget covers rungs 4
// and 5 together; cancellation never advances to another rung.
func (s *par2RepairSession) RecoverSegment(ctx context.Context, segIndex int) ([]byte, error) {
	if s == nil {
		return nil, ErrPAR2RepairAbstain
	}
	ctx, cancel := context.WithTimeout(ctx, s.budget.MaxDuration)
	defer cancel()
	data, repairErr := s.RepairSegment(ctx, segIndex)
	if repairErr == nil {
		return data, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, crossErr := s.recoverCrossLaneSegment(ctx, segIndex)
	if crossErr == nil {
		return data, nil
	}
	return nil, errors.Join(repairErr, crossErr)
}

func (s *par2RepairSession) recoverCrossLaneSegment(ctx context.Context, segIndex int) ([]byte, error) {
	if s.provider == nil || s.provider.crossLaneRecover == nil || s.provider.crossLaneSlot == nil {
		return nil, ErrCrossLaneAbstain
	}
	select {
	case s.provider.crossLaneSlot <- struct{}{}:
		defer func() { <-s.provider.crossLaneSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	geo, proof, err := s.resolveGeometry(ctx)
	if err != nil {
		return nil, err
	}
	target, ok := geo.FileByNZBIndex(s.targetIdx)
	if !ok || proof.Length() != target.Length {
		return nil, fmt.Errorf("%w: IFSC domain does not describe the streamed file", ErrCrossLaneAbstain)
	}
	result, err := recoverCrossLaneSpan(
		ctx,
		&par2NZBRepairReader{session: s},
		s.itemID,
		s.targetIdx,
		segIndex,
		target.Length,
		proof,
		s.budget,
		s.now,
		s.provider.crossLaneRecover,
	)
	if err != nil {
		return nil, err
	}
	if s.log != nil {
		s.log.Info("nntp: cross-lane rung-5 recovery served a damaged segment",
			"event", "nntp_crosslane_recovery_served",
			"blocks", result.blocks,
			"torrent_blocks", result.torrentBlocks,
			"http_blocks", result.httpBlocks,
			"span_bytes", len(result.data),
			"bytes_read", result.spent,
		)
	}
	return result.data, nil
}

type crossLaneSpanResult struct {
	data          []byte
	spent         int64
	blocks        int64
	torrentBlocks int
	httpBlocks    int
}

func recoverCrossLaneSpan(
	ctx context.Context,
	reader PAR2RepairReader,
	itemID string,
	fileIndex, segIndex int,
	fileSize int64,
	proof PAR2ProofDomain,
	budget PAR2RepairBudget,
	now func() time.Time,
	recover CrossLaneRecover,
) (crossLaneSpanResult, error) {
	var result crossLaneSpanResult
	if reader == nil || recover == nil || now == nil || !budget.Enabled() ||
		proof.SliceSize() <= 0 || proof.Length() != fileSize {
		return result, ErrCrossLaneAbstain
	}
	acct := &repairAccount{budget: budget, now: now, deadline: now().Add(budget.MaxDuration)}
	start, length, err := measureDamagedSegment(ctx, reader, fileIndex, segIndex, fileSize, acct)
	if err != nil {
		return result, err
	}
	if length > proof.SliceSize()*int64(archiveparser.MaxPAR2UnknownBlocks) {
		return result, fmt.Errorf("%w: damaged span exceeds the block bound", ErrCrossLaneAbstain)
	}
	end := start + length
	firstBlock := start / proof.SliceSize()
	lastBlock := (end - 1) / proof.SliceSize()
	result.data = make([]byte, 0, length)
	for block := firstBlock; block <= lastBlock; block++ {
		if err := acct.check(ctx); err != nil {
			return crossLaneSpanResult{}, err
		}
		blockProof, ok := proof.ProofAt(block * proof.SliceSize())
		if !ok {
			return crossLaneSpanResult{}, fmt.Errorf("%w: IFSC proof is absent", ErrCrossLaneAbstain)
		}
		if blockProof.Length > acct.remaining() {
			return crossLaneSpanResult{}, fmt.Errorf("%w: alternate block exceeds the remaining byte budget", ErrPAR2RepairAbstain)
		}
		recovered, err := recover(ctx, itemID, fileIndex, fileSize, blockProof)
		if err != nil {
			if ctx.Err() != nil {
				return crossLaneSpanResult{}, ctx.Err()
			}
			return crossLaneSpanResult{}, fmt.Errorf("%w: no verified alternate block", ErrCrossLaneAbstain)
		}
		if int64(len(recovered.Data)) != blockProof.Length {
			return crossLaneSpanResult{}, fmt.Errorf("%w: alternate block length disagrees", ErrCrossLaneAbstain)
		}
		digest, err := blockProof.CandidateDigest(ctx, recovered.Data)
		if err != nil || !blockProof.DigestEquals(digest) {
			return crossLaneSpanResult{}, fmt.Errorf("%w: alternate block failed IFSC verification", ErrCrossLaneAbstain)
		}
		if err := acct.spend(int64(len(recovered.Data))); err != nil {
			return crossLaneSpanResult{}, err
		}
		lo, hi := int64(0), blockProof.Length
		if block == firstBlock {
			lo = start - blockProof.Offset
		}
		if block == lastBlock {
			hi = end - blockProof.Offset
		}
		if lo < 0 || hi < lo || hi > int64(len(recovered.Data)) {
			return crossLaneSpanResult{}, fmt.Errorf("%w: alternate block span disagrees", ErrCrossLaneAbstain)
		}
		result.data = append(result.data, recovered.Data[lo:hi]...)
		switch recovered.Lane {
		case contentproof.LaneTorrent:
			result.torrentBlocks++
		case contentproof.LaneHTTP:
			result.httpBlocks++
		}
	}
	if int64(len(result.data)) != length {
		return crossLaneSpanResult{}, fmt.Errorf("%w: recovered segment length disagrees", ErrCrossLaneAbstain)
	}
	result.spent = acct.spent
	result.blocks = lastBlock - firstBlock + 1
	return result, nil
}
