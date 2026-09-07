package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NNTPHealthAudit is the NS-6.1 persisted per-item STAT health-audit
// state: the most recent pass's completeness estimate, decayed verdict,
// and bounded dead-region map (JSON-encoded []nntp.DeadRegion). Store owns
// only persistence of this shape; internal/nntp owns sampling and the
// merge/threshold logic that produces the values passed in here.
type NNTPHealthAudit struct {
	ItemID          string
	LastAuditedAt   time.Time
	SegmentsSampled int
	SegmentsPresent int
	Completeness    float64
	Decayed         bool
	// DeadRegionsJSON is the raw JSON-encoded []nntp.DeadRegion produced by
	// nntp.MergeDeadRegions. Store treats it as an opaque blob (no
	// nntp-package import here, avoiding a store<->nntp dependency this
	// row doesn't otherwise need) so callers unmarshal it themselves.
	DeadRegionsJSON string
	UpdatedAt       time.Time
}

// UpsertNNTPHealthAudit records one health-audit pass's result for itemID,
// setting last_audited_at to the store's injectable clock (so the
// least-recently-audited selection in NextHealthAuditItem is itself
// deterministically testable). deadRegionsJSON must already be bounded by
// the caller (nntp.MergeDeadRegions' maxRegions) -- this method does not
// enforce a size cap of its own.
func (s *Store) UpsertNNTPHealthAudit(ctx context.Context, itemID string, sampled, present int, completeness float64, decayed bool, deadRegionsJSON string) error {
	if itemID == "" {
		return errors.New("store: upsert nntp health audit: empty item id")
	}
	if deadRegionsJSON == "" {
		deadRegionsJSON = "[]"
	}
	now := formatTime(s.now())
	_, err := s.execWrite(ctx, `
        INSERT INTO nntp_health_audit
            (item_id, last_audited_at, segments_sampled, segments_present, completeness, decayed, dead_regions, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(item_id) DO UPDATE SET
            last_audited_at  = excluded.last_audited_at,
            segments_sampled = excluded.segments_sampled,
            segments_present = excluded.segments_present,
            completeness     = excluded.completeness,
            decayed          = excluded.decayed,
            dead_regions     = excluded.dead_regions,
            updated_at       = excluded.updated_at`,
		itemID, now, sampled, present, completeness, boolToInt(decayed), deadRegionsJSON, now)
	if err != nil {
		return fmt.Errorf("store: upsert nntp health audit %s: %w", itemID, err)
	}
	return nil
}

// GetNNTPHealthAudit returns the persisted health-audit state for itemID,
// or (nil, nil) if the item has never been audited.
func (s *Store) GetNNTPHealthAudit(ctx context.Context, itemID string) (*NNTPHealthAudit, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT item_id, last_audited_at, segments_sampled, segments_present, completeness, decayed, dead_regions, updated_at
        FROM nntp_health_audit WHERE item_id = ?`, itemID)
	a, err := scanNNTPHealthAudit(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get nntp health audit %s: %w", itemID, err)
	}
	return &a, nil
}

// NextHealthAuditItem returns the single Ready NNTP item (source_type='nzb',
// no TorBox remote/queued id -- the same lane scope as ListReadyNNTPItems)
// that is least-recently audited: never-audited items sort first (via the
// COALESCE against an empty string, which sorts before any real
// last_audited_at timestamp), then ascending last_audited_at. Combined with
// a fixed per-tick cadence, this alone gives the frozen plan's "full-library
// period" property -- no separate rotation cursor is needed. Returns
// (nil, nil) when no eligible item exists.
func (s *Store) NextHealthAuditItem(ctx context.Context) (*Item, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT `+itemColumns+` FROM items
        WHERE state = 'ready' AND source_type = 'nzb'
          AND remote_id IS NULL AND queued_id IS NULL
        ORDER BY COALESCE(
                     (SELECT last_audited_at FROM nntp_health_audit h WHERE h.item_id = items.id),
                     ''
                 ) ASC,
                 items.id ASC
        LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("store: next health audit item: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items, err := scanItems(rows)
	if err != nil {
		return nil, fmt.Errorf("store: next health audit item: scan: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	return items[0], nil
}

func scanNNTPHealthAudit(row interface{ Scan(...any) error }) (NNTPHealthAudit, error) {
	var a NNTPHealthAudit
	var lastAudited, updated string
	var decayedInt int
	if err := row.Scan(&a.ItemID, &lastAudited, &a.SegmentsSampled, &a.SegmentsPresent, &a.Completeness, &decayedInt, &a.DeadRegionsJSON, &updated); err != nil {
		return a, err
	}
	a.Decayed = decayedInt != 0
	var err error
	a.LastAuditedAt, err = time.Parse(time.RFC3339Nano, lastAudited)
	if err != nil {
		return a, err
	}
	a.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return a, err
}
