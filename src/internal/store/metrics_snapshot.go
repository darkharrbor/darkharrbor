package store

import "context"

// ItemStateCounts returns the current count of items per ItemState, via one
// bounded GROUP BY aggregate query (no per-item scan). Used only for the
// OBS-01 /metrics endpoint; never on any hot request path.
func (s *Store) ItemStateCounts(ctx context.Context) (map[ItemState]int64, error) {
	counts := make(map[ItemState]int64)
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM items GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		counts[ItemState(state)] = count
	}
	return counts, rows.Err()
}

// NNTPZeroFillTotal sums NS-1.3's persisted per-item final-rung-exhaustion
// counter across every item, via one bounded aggregate query. Used only for
// the OBS-01 /metrics endpoint.
func (s *Store) NNTPZeroFillTotal(ctx context.Context) (int64, error) {
	var total int64
	row := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(
			CAST(json_extract(metadata_json, '$.nntp_zero_fill_count') AS INTEGER)
		), 0)
		FROM items`)
	if err := row.Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}
