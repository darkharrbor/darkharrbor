package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// IdentityAudit is ID-03's persisted per-item retroactive identity-audit
// state: the most recent pass's verdicts (identitycheck.Verdict, JSON-
// encoded by the caller — this package treats it as an opaque blob to
// avoid a store<->identitycheck import cycle, the same convention
// nntp_health_audit already established for internal/nntp.DeadRegion).
// An empty '[]' is a meaningful "clean on last pass" result, distinct from
// "never audited" (no row at all, reported via GetIdentityAudit returning
// nil, nil).
type IdentityAudit struct {
	ItemID        string
	LastAuditedAt time.Time
	VerdictsJSON  string
	UpdatedAt     time.Time
}

// UpsertIdentityAudit records one ID-03 pass's result for itemID, setting
// last_audited_at to the store's injectable clock (so NextIdentityAuditItem's
// least-recently-audited selection is itself deterministically testable).
// verdictsJSON must already be the caller's bounded, secret-free
// identitycheck.Verdict encoding (DG-04 is the caller's responsibility,
// exactly mirroring UpsertNNTPHealthAudit's own division of ownership).
func (s *Store) UpsertIdentityAudit(ctx context.Context, itemID, verdictsJSON string) error {
	if itemID == "" {
		return errors.New("store: upsert identity audit: empty item id")
	}
	if verdictsJSON == "" {
		verdictsJSON = "[]"
	}
	now := formatTime(s.now())
	_, err := s.execWrite(ctx, `
        INSERT INTO identity_audit
            (item_id, last_audited_at, verdicts_json, updated_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(item_id) DO UPDATE SET
            last_audited_at = excluded.last_audited_at,
            verdicts_json   = excluded.verdicts_json,
            updated_at      = excluded.updated_at`,
		itemID, now, verdictsJSON, now)
	if err != nil {
		return fmt.Errorf("store: upsert identity audit %s: %w", itemID, err)
	}
	return nil
}

// GetIdentityAudit returns the persisted audit state for itemID, or
// (nil, nil) if the item has never been audited.
func (s *Store) GetIdentityAudit(ctx context.Context, itemID string) (*IdentityAudit, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT item_id, last_audited_at, verdicts_json, updated_at
        FROM identity_audit WHERE item_id = ?`, itemID)
	a, err := scanIdentityAudit(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get identity audit %s: %w", itemID, err)
	}
	return &a, nil
}

// NextIdentityAuditItem returns the single Ready item (any source_type —
// this row is explicitly cross-lane, unlike NextHealthAuditItem's NNTP-only
// scope) that is least-recently audited: never-audited items sort first
// (via the LEFT JOIN's NULL, which COALESCEs before any real
// last_audited_at timestamp), then ascending last_audited_at. Combined with
// a fixed per-tick cadence, this alone gives the full-library-period
// property, mirroring NextHealthAuditItem exactly. Returns (nil, nil) when
// no eligible item exists (an empty Ready library, or every item somehow
// terminal/removed between ticks).
func (s *Store) NextIdentityAuditItem(ctx context.Context) (*Item, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT `+itemColumns+` FROM items
        WHERE state = 'ready'
        ORDER BY COALESCE(
                     (SELECT last_audited_at FROM identity_audit a WHERE a.item_id = items.id),
                     ''
                 ) ASC,
                 items.id ASC
        LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("store: next identity audit item: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items, err := scanItems(rows)
	if err != nil {
		return nil, fmt.Errorf("store: next identity audit item: scan: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	return items[0], nil
}

// IdentityAuditFinding is one row of ID-03's legible possible-wrong-content
// report: a Ready item whose most recent audit pass found at least one
// verdict, joined with the item's own display name (never a raw upstream
// URL — DisplayName is the same release-title field identitycheck.Verify
// itself already consumes, DG-04-safe by construction) so the report is
// legible without a second lookup.
type IdentityAuditFinding struct {
	ItemID        string
	DisplayName   string
	LastAuditedAt time.Time
	VerdictsJSON  string
}

// ListIdentityAuditFindings returns at most limit Ready items whose most
// recent identity-audit pass found a non-empty verdict set, most-recently-
// audited first. The explicit bound keeps the report finite (DG-07),
// mirroring ListReadyHTTPItems/ListReadyNNTPItems's own established
// convention. limit<=0 returns (nil, nil), never an unbounded scan.
func (s *Store) ListIdentityAuditFindings(ctx context.Context, limit int) ([]IdentityAuditFinding, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT a.item_id, i.display_name, a.last_audited_at, a.verdicts_json
        FROM identity_audit a
        JOIN items i ON i.id = a.item_id
        WHERE i.state = 'ready' AND a.verdicts_json != '[]'
        ORDER BY a.last_audited_at DESC, a.item_id ASC
        LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list identity audit findings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []IdentityAuditFinding
	for rows.Next() {
		var f IdentityAuditFinding
		var lastAudited string
		if err := rows.Scan(&f.ItemID, &f.DisplayName, &lastAudited, &f.VerdictsJSON); err != nil {
			return nil, fmt.Errorf("store: list identity audit findings: scan: %w", err)
		}
		f.LastAuditedAt, err = time.Parse(time.RFC3339Nano, lastAudited)
		if err != nil {
			return nil, fmt.Errorf("store: list identity audit findings: parse time: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list identity audit findings: rows: %w", err)
	}
	return out, nil
}

func scanIdentityAudit(row interface{ Scan(...any) error }) (IdentityAudit, error) {
	var a IdentityAudit
	var lastAudited, updated string
	if err := row.Scan(&a.ItemID, &lastAudited, &a.VerdictsJSON, &updated); err != nil {
		return a, err
	}
	var err error
	a.LastAuditedAt, err = time.Parse(time.RFC3339Nano, lastAudited)
	if err != nil {
		return a, err
	}
	a.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return a, err
}
