package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultErrorJournalMaxEntries is used when the caller passes a
// non-positive maxEntries (config absent/invalid falls back here per the
// standing "unknown/removed config keys warn and never fail startup"
// convention -- this is the equivalent abstain-to-default for a bound).
const DefaultErrorJournalMaxEntries = 20

// AppendErrorJournal appends one bounded, sanitized SF-01 outcome event
// (OBS-01) to itemID's persisted last-N ring, oldest dropped first once the
// ring exceeds maxEntries. It is a targeted json_set of only
// metadata_json.$.error_journal (never a full-item overwrite), mirroring
// IncrementNNTPZeroFill's single-field-update convention so concurrent
// writers of unrelated metadata fields are never clobbered.
//
// Malformed/absent identity (empty itemID, empty entry.Class) safely
// abstains (no-op, nil error) rather than guessing or persisting a
// meaningless row. A non-existent itemID also abstains (nil error): the
// item may have been removed between the triggering event and this call,
// and reporting is always best-effort, never a cause of caller failure
// (mirrors reportFinalRung's own "reporter failure never masks the
// original stream error" contract).
func (s *Store) AppendErrorJournal(ctx context.Context, itemID string, entry ErrorJournalEntry, maxEntries int) error {
	itemID = strings.TrimSpace(itemID)
	entry.Class = strings.TrimSpace(entry.Class)
	entry.Rung = strings.TrimSpace(entry.Rung)
	entry.Provider = strings.TrimSpace(entry.Provider)
	if itemID == "" || entry.Class == "" {
		return nil
	}
	if maxEntries < 1 {
		maxEntries = DefaultErrorJournalMaxEntries
	}
	if entry.At.IsZero() {
		entry.At = s.now()
	}

	_, err := retrySQLiteBusy(ctx, func() (struct{}, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return struct{}{}, fmt.Errorf("begin error journal tx: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()

		var existing sql.NullString
		row := tx.QueryRowContext(ctx,
			`SELECT json_extract(metadata_json, '$.error_journal') FROM items WHERE id = ?`, itemID)
		if scanErr := row.Scan(&existing); scanErr != nil {
			if scanErr == sql.ErrNoRows {
				return struct{}{}, nil // item gone: best-effort abstain
			}
			return struct{}{}, scanErr
		}

		var journal []json.RawMessage
		if existing.Valid && existing.String != "" {
			// A corrupt/unexpected existing value fails closed by resetting
			// the ring rather than guessing at repair -- the same
			// abstain-not-guess posture used throughout this row's own
			// contract for malformed input.
			_ = json.Unmarshal([]byte(existing.String), &journal)
		}

		entryJSON, err := json.Marshal(entry)
		if err != nil {
			return struct{}{}, fmt.Errorf("marshal error journal entry: %w", err)
		}
		journal = append(journal, entryJSON)
		if len(journal) > maxEntries {
			journal = journal[len(journal)-maxEntries:]
		}
		out, err := json.Marshal(journal)
		if err != nil {
			return struct{}{}, fmt.Errorf("marshal error journal: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE items
			SET metadata_json = json_set(
					COALESCE(NULLIF(metadata_json, ''), '{}'),
					'$.error_journal',
					json(?)
				)
			WHERE id = ?`,
			string(out), itemID,
		)
		if err != nil {
			return struct{}{}, err
		}
		if n, rerr := res.RowsAffected(); rerr == nil && n == 0 {
			return struct{}{}, nil // item removed concurrently: abstain
		}
		if err := tx.Commit(); err != nil {
			return struct{}{}, err
		}
		committed = true
		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("append error journal for %s: %w", itemID, err)
	}
	return nil
}

// GetErrorJournal returns itemID's persisted last-N SF-01 event ring
// (oldest first), or an empty slice if the item has no journal entries yet
// or does not exist. Read-only; never mutates state.
func (s *Store) GetErrorJournal(ctx context.Context, itemID string) ([]ErrorJournalEntry, error) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil, fmt.Errorf("get error journal: empty item id")
	}
	var raw sql.NullString
	row := s.db.QueryRowContext(ctx,
		`SELECT json_extract(metadata_json, '$.error_journal') FROM items WHERE id = ?`, itemID)
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("item %s not found", itemID)
		}
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return []ErrorJournalEntry{}, nil
	}
	var entries []ErrorJournalEntry
	if err := json.Unmarshal([]byte(raw.String), &entries); err != nil {
		// Corrupt persisted state fails closed: report empty rather than a
		// guessed/partial repair of unknown-shaped data.
		return []ErrorJournalEntry{}, nil
	}
	return entries, nil
}

// ErrorJournalAggregate is the bounded, DB-wide summary OBS-01 exposes
// through /metrics: how many persisted journal entries exist in total, and
// how many per outcome.Class. Computed via one bounded aggregate query over
// the existing items table (same query-shape class as TS-0.5's consistency
// sweep), never a per-item scan in application code.
type ErrorJournalAggregate struct {
	Total   int64
	ByClass map[string]int64
}

// ErrorJournalAggregate computes the current DB-wide error-journal totals.
// Items with no journal entries contribute zero rows via json_each's normal
// empty-array behavior; malformed metadata_json.$.error_journal on any one
// item is skipped for that item only (COALESCE guards NULL, and a
// non-array value simply yields no json_each rows), never aborting the
// whole aggregate.
func (s *Store) ErrorJournalAggregateStats(ctx context.Context) (ErrorJournalAggregate, error) {
	agg := ErrorJournalAggregate{ByClass: make(map[string]int64)}
	rows, err := s.db.QueryContext(ctx, `
		SELECT json_extract(je.value, '$.class') AS class, COUNT(*)
		FROM items, json_each(
			COALESCE(json_extract(items.metadata_json, '$.error_journal'), '[]')
		) AS je
		GROUP BY class`)
	if err != nil {
		return agg, err
	}
	defer rows.Close()
	for rows.Next() {
		var class sql.NullString
		var count int64
		if err := rows.Scan(&class, &count); err != nil {
			return agg, err
		}
		agg.Total += count
		if class.Valid && class.String != "" {
			agg.ByClass[class.String] = count
		}
	}
	return agg, rows.Err()
}
