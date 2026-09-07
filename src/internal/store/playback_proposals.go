package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
)

func (s *Store) CreatePlaybackProposal(ctx context.Context, proposal playbackcoverage.Proposal) (bool, error) {
	if err := playbackcoverage.ValidateRepresentationID(proposal.RepresentationID); err != nil ||
		playbackcoverage.ValidateTarget(playbackcoverage.Target{ItemID: proposal.ItemID, FileID: proposal.FileID, Kind: proposal.ExtentKind, Total: proposal.Total}) != nil ||
		proposal.Delivered <= 0 || proposal.Threshold <= 0 || proposal.Threshold > 1 ||
		math.IsNaN(proposal.Threshold) || math.IsInf(proposal.Threshold, 0) || proposal.CreatedAt.IsZero() {
		return false, fmt.Errorf("store: invalid playback proposal")
	}
	res, err := s.execWrite(ctx, `
		INSERT INTO playback_proposals
			(representation_id, item_id, file_id, extent_kind, delivered, total, threshold, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(representation_id) DO NOTHING`,
		proposal.RepresentationID, proposal.ItemID, proposal.FileID, string(proposal.ExtentKind),
		proposal.Delivered, proposal.Total, proposal.Threshold, formatTime(proposal.CreatedAt))
	if err != nil {
		return false, fmt.Errorf("store: create playback proposal: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) GetPlaybackProposal(ctx context.Context, representationID string) (playbackcoverage.Proposal, bool, error) {
	var proposal playbackcoverage.Proposal
	var kind, created string
	err := s.db.QueryRowContext(ctx, `
		SELECT representation_id, item_id, file_id, extent_kind, delivered, total, threshold, created_at
		FROM playback_proposals WHERE representation_id=?`, representationID).Scan(
		&proposal.RepresentationID, &proposal.ItemID, &proposal.FileID, &kind,
		&proposal.Delivered, &proposal.Total, &proposal.Threshold, &created)
	if err == sql.ErrNoRows {
		return playbackcoverage.Proposal{}, false, nil
	}
	if err != nil {
		return playbackcoverage.Proposal{}, false, fmt.Errorf("store: get playback proposal: %w", err)
	}
	proposal.ExtentKind = playbackcoverage.ExtentKind(kind)
	proposal.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return playbackcoverage.Proposal{}, false, fmt.Errorf("store: parse playback proposal time: %w", err)
	}
	return proposal, true, nil
}
