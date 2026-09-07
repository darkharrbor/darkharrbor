package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/searchbudget"
)

func (s *Store) GetSearchBudget(ctx context.Context, key string) (searchbudget.State, bool, error) {
	var state searchbudget.State
	var suppressed sql.NullString
	err := s.db.QueryRowContext(ctx, `
        SELECT consecutive_no_results, suppressed_until
        FROM search_budgets WHERE identity_key = ?`, key).
		Scan(&state.ConsecutiveNoResults, &suppressed)
	if errors.Is(err, sql.ErrNoRows) {
		return state, false, nil
	}
	if err != nil {
		return state, false, fmt.Errorf("store: get search budget: %w", err)
	}
	if suppressed.Valid {
		state.SuppressedUntil, err = parseTime(suppressed.String)
		if err != nil {
			return searchbudget.State{}, false, fmt.Errorf("store: parse search suppression: %w", err)
		}
	}
	return state, true, nil
}

func (s *Store) RecordSearchNoResult(ctx context.Context, key string, cap int, suppress time.Duration, now time.Time) (searchbudget.State, error) {
	if key == "" || cap < 1 || suppress <= 0 {
		return searchbudget.State{}, fmt.Errorf("store: invalid search budget update")
	}
	deadline := formatTime(now.Add(suppress))
	_, err := s.execWrite(ctx, `
        INSERT INTO search_budgets (
            identity_key, consecutive_no_results, suppressed_until, updated_at
        ) VALUES (?, 1, CASE WHEN 1 >= ? THEN ? ELSE NULL END, ?)
        ON CONFLICT(identity_key) DO UPDATE SET
            consecutive_no_results = MIN(search_budgets.consecutive_no_results + 1, ?),
            suppressed_until = CASE
                WHEN search_budgets.consecutive_no_results + 1 >= ? THEN ?
                ELSE search_budgets.suppressed_until
            END,
            updated_at = excluded.updated_at`,
		key, cap, deadline, formatTime(now), cap, cap, deadline)
	if err != nil {
		return searchbudget.State{}, fmt.Errorf("store: record search no-result: %w", err)
	}
	state, _, err := s.GetSearchBudget(ctx, key)
	return state, err
}

func (s *Store) ResetSearchBudget(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	if _, err := s.execWrite(ctx, `DELETE FROM search_budgets WHERE identity_key = ?`, key); err != nil {
		return fmt.Errorf("store: reset search budget: %w", err)
	}
	return nil
}
