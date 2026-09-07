package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	maxReactiveEvents = 100_000
	reactiveEventDays = 366
)

type ReactiveMetrics struct {
	CommitsByPeriod map[string]map[string]int64
	PendingByReason map[string]int64
	OldestByReason  map[string]int64
	Identity        map[string]int64
	UndoTotal       int64
	PromotionBytes  int64
	HeadroomBytes   int64
	HeadroomKnown   bool
}

func (s *Store) CurrentReactiveMetrics(ctx context.Context) (ReactiveMetrics, error) {
	return s.ReactiveMetrics(ctx, s.now().UTC())
}

func (s *Store) ReactiveMetrics(ctx context.Context, now time.Time) (ReactiveMetrics, error) {
	if now.IsZero() {
		return ReactiveMetrics{}, fmt.Errorf("store: zero reactive metrics time")
	}
	result := ReactiveMetrics{
		CommitsByPeriod: map[string]map[string]int64{
			"24h": {"auto": 0, "supervised": 0},
			"7d":  {"auto": 0, "supervised": 0},
			"30d": {"auto": 0, "supervised": 0},
		},
		PendingByReason: map[string]int64{
			"positive_mismatch": 0, "routing_ambiguous": 0, "routing_unresolved": 0,
			"supervised_review": 0, "weak_evidence": 0,
		},
		OldestByReason: map[string]int64{
			"positive_mismatch": 0, "routing_ambiguous": 0, "routing_unresolved": 0,
			"supervised_review": 0, "weak_evidence": 0,
		},
		Identity: map[string]int64{
			"positive_mismatch": 0, "strong_evidence": 0, "weak_evidence": 0,
		},
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT mode,
		       SUM(CASE WHEN occurred_at>=? THEN 1 ELSE 0 END),
		       SUM(CASE WHEN occurred_at>=? THEN 1 ELSE 0 END),
		       SUM(CASE WHEN occurred_at>=? THEN 1 ELSE 0 END)
		FROM reactive_events WHERE kind='commit' GROUP BY mode`,
		formatTime(now.Add(-24*time.Hour)), formatTime(now.Add(-7*24*time.Hour)), formatTime(now.Add(-30*24*time.Hour)))
	if err != nil {
		return ReactiveMetrics{}, fmt.Errorf("store: reactive commit metrics: %w", err)
	}
	for rows.Next() {
		var mode string
		var day, week, month int64
		if err := rows.Scan(&mode, &day, &week, &month); err != nil {
			_ = rows.Close()
			return ReactiveMetrics{}, err
		}
		result.CommitsByPeriod["24h"][mode], result.CommitsByPeriod["7d"][mode], result.CommitsByPeriod["30d"][mode] = day, week, month
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return ReactiveMetrics{}, err
	}
	_ = rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT reason,COUNT(*),MIN(updated_at)
		FROM reactive_pending WHERE state='pending' GROUP BY reason`)
	if err != nil {
		return ReactiveMetrics{}, fmt.Errorf("store: reactive pending metrics: %w", err)
	}
	for rows.Next() {
		var reason, oldest string
		var count int64
		if err := rows.Scan(&reason, &count, &oldest); err != nil {
			_ = rows.Close()
			return ReactiveMetrics{}, err
		}
		at, err := time.Parse(time.RFC3339Nano, oldest)
		if err != nil {
			_ = rows.Close()
			return ReactiveMetrics{}, err
		}
		age := int64(now.Sub(at).Seconds())
		if age < 0 {
			age = 0
		}
		result.PendingByReason[reason], result.OldestByReason[reason] = count, age
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return ReactiveMetrics{}, err
	}
	_ = rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT outcome,COUNT(*) FROM reactive_events WHERE outcome<>'' GROUP BY outcome`)
	if err != nil {
		return ReactiveMetrics{}, fmt.Errorf("store: reactive identity metrics: %w", err)
	}
	for rows.Next() {
		var outcome string
		var count int64
		if err := rows.Scan(&outcome, &count); err != nil {
			_ = rows.Close()
			return ReactiveMetrics{}, err
		}
		result.Identity[outcome] = count
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return ReactiveMetrics{}, err
	}
	_ = rows.Close()
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reactive_events WHERE kind='undo'`).Scan(&result.UndoTotal); err != nil {
		return ReactiveMetrics{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM reactive_events WHERE kind='promotion'`).Scan(&result.PromotionBytes); err != nil {
		return ReactiveMetrics{}, err
	}
	var observed string
	err = s.db.QueryRowContext(ctx, `SELECT headroom_bytes,observed_at FROM reactive_promotion_headroom WHERE singleton=1`).Scan(&result.HeadroomBytes, &observed)
	if err == nil {
		result.HeadroomKnown = true
	} else if err != sql.ErrNoRows {
		return ReactiveMetrics{}, err
	}
	return result, nil
}

func appendReactiveEvent(ctx context.Context, tx execer, kind, mode, outcome string, bytes int64, at time.Time) error {
	if at.IsZero() || bytes < 0 {
		return fmt.Errorf("store: invalid reactive event")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO reactive_events(kind,mode,outcome,bytes,occurred_at) VALUES (?,?,?,?,?)`, kind, mode, outcome, bytes, formatTime(at)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM reactive_events WHERE id NOT IN (SELECT id FROM reactive_events ORDER BY occurred_at DESC,id DESC LIMIT ?) OR occurred_at<?`, maxReactiveEvents, formatTime(at.Add(-reactiveEventDays*24*time.Hour)))
	return err
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) RecordReactivePromotion(ctx context.Context, bytes int64, at time.Time) error {
	if bytes <= 0 {
		return fmt.Errorf("store: invalid reactive promotion bytes")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := appendReactiveEvent(ctx, tx, "promotion", "", "", bytes, at); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetReactivePromotionHeadroom(ctx context.Context, bytes int64, at time.Time) error {
	if bytes < 0 || at.IsZero() {
		return fmt.Errorf("store: invalid reactive promotion headroom")
	}
	_, err := s.execWrite(ctx, `INSERT INTO reactive_promotion_headroom(singleton,headroom_bytes,observed_at) VALUES (1,?,?) ON CONFLICT(singleton) DO UPDATE SET headroom_bytes=excluded.headroom_bytes,observed_at=excluded.observed_at`, bytes, formatTime(at))
	return err
}
