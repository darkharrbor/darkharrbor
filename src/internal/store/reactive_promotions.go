package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrReactiveAlreadyPromoted = errors.New("store: representation already promoted or removed")

type ReactivePromotion struct {
	RepresentationID string
	State            string
	TargetPath       string
	SizeBytes        int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (s *Store) BeginReactivePromotion(ctx context.Context, representationID, targetPath string, size int64, now time.Time) error {
	if representationID == "" || targetPath == "" || size <= 0 || now.IsZero() {
		return errors.New("store: invalid reactive promotion")
	}
	commit, found, err := s.GetReactiveCommit(ctx, representationID)
	if err != nil || !found || commit.State != "committed" {
		if err == nil {
			err = errors.New("store: promotion requires committed representation")
		}
		return err
	}
	current, found, err := s.GetReactivePromotion(ctx, representationID)
	if err != nil {
		return err
	}
	if found && (current.State == "promoted" || current.State == "removed") {
		return ErrReactiveAlreadyPromoted
	}
	res, err := s.execWrite(ctx, `INSERT INTO reactive_promotions
		(representation_id,state,target_path,size_bytes,created_at,updated_at)
		VALUES (?, 'staging', ?, ?, ?, ?)
		ON CONFLICT(representation_id) DO UPDATE SET state='staging',target_path=excluded.target_path,
		size_bytes=excluded.size_bytes,updated_at=excluded.updated_at
		WHERE reactive_promotions.state='failed'`, representationID, targetPath, size, formatTime(now), formatTime(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("store: promotion already in progress")
	}
	return nil
}

func (s *Store) GetReactivePromotion(ctx context.Context, representationID string) (ReactivePromotion, bool, error) {
	var p ReactivePromotion
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT representation_id,state,target_path,size_bytes,created_at,updated_at
		FROM reactive_promotions WHERE representation_id=?`, representationID).Scan(
		&p.RepresentationID, &p.State, &p.TargetPath, &p.SizeBytes, &created, &updated)
	if err == sql.ErrNoRows {
		return ReactivePromotion{}, false, nil
	}
	if err != nil {
		return ReactivePromotion{}, false, err
	}
	if p.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err == nil {
		p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	}
	return p, true, err
}

func (s *Store) CompleteReactivePromotion(ctx context.Context, representationID string, bytes int64, now time.Time) error {
	if bytes <= 0 || now.IsZero() {
		return errors.New("store: invalid promotion completion")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE reactive_promotions SET state='promoted',updated_at=?
		WHERE representation_id=? AND state='staging' AND size_bytes=?`, formatTime(now), representationID, bytes)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("store: promotion cannot complete")
	}
	if err := appendReactiveEvent(ctx, tx, "promotion", "", "", bytes, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailReactivePromotion(ctx context.Context, representationID string, now time.Time) error {
	if now.IsZero() {
		return errors.New("store: invalid promotion failure")
	}
	_, err := s.execWrite(ctx, `UPDATE reactive_promotions SET state='failed',updated_at=? WHERE representation_id=? AND state='staging'`, formatTime(now), representationID)
	return err
}

func (s *Store) RemoveReactivePromotion(ctx context.Context, representationID string, now time.Time) error {
	if now.IsZero() {
		return errors.New("store: invalid promotion removal")
	}
	n, err := s.execWriteAffected(ctx, `UPDATE reactive_promotions SET state='removed',updated_at=? WHERE representation_id=? AND state='promoted'`, formatTime(now), representationID)
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("store: promotion cannot be removed")
	}
	return nil
}

// RecoverReactivePromotions makes an interrupted transfer explicitly
// retryable. The next attempt owns removal of its deterministic stage file.
func (s *Store) RecoverReactivePromotions(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return errors.New("store: invalid promotion recovery")
	}
	_, err := s.execWrite(ctx, `UPDATE reactive_promotions SET state='failed',updated_at=? WHERE state='staging'`, formatTime(now))
	return err
}
