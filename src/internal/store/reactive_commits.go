package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrReactiveNoRecommit = errors.New("store: reactive item was undone")

type ReactiveCommit struct {
	RepresentationID string
	ItemID           string
	FileID           string
	State            string
	Mode             string
	ReviewRequired   bool
	Reason           string
	ArrName          string
	ArrKind          string
	ArrItemID        int
	OwnerCreated     bool
	PriorMonitored   *bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ArrFileIDs       []int
	Episodes         []ReactiveCommitEpisode
}

type ReactiveCommitEpisode struct {
	ID             int
	PriorMonitored bool
}

func (s *Store) BeginReactiveCommit(ctx context.Context, representationID, itemID, fileID, mode string, now time.Time) (ReactiveCommit, bool, error) {
	if representationID == "" || itemID == "" || fileID == "" || (mode != "auto" && mode != "supervised") || now.IsZero() {
		return ReactiveCommit{}, false, fmt.Errorf("store: invalid reactive commit")
	}
	res, err := s.execWrite(ctx, `
		INSERT INTO reactive_commits
		    (representation_id,item_id,file_id,state,mode,created_at,updated_at)
		VALUES (?,?,?,'committing',?,?,?) ON CONFLICT(representation_id) DO NOTHING`,
		representationID, itemID, fileID, mode, formatTime(now), formatTime(now))
	if err != nil {
		return ReactiveCommit{}, false, fmt.Errorf("store: begin reactive commit: %w", err)
	}
	created, _ := res.RowsAffected()
	record, found, err := s.GetReactiveCommit(ctx, representationID)
	if err != nil || !found {
		return ReactiveCommit{}, false, err
	}
	if record.State == "undone" || record.State == "undoing" {
		return record, false, ErrReactiveNoRecommit
	}
	if record.ItemID != itemID || record.FileID != fileID {
		return record, false, fmt.Errorf("store: conflicting reactive commit")
	}
	// Terminal records are idempotent by representation/item/file identity,
	// not by the dispatch mode that originally created them. A supervised
	// commit may legitimately outlive a stale supervised_review queue row;
	// automatic dispatch must return that completed record rather than reject
	// it solely because the current caller uses mode "auto". Keep the mode
	// check for an in-flight record, where different caller semantics are a
	// real conflict.
	if record.State == "committed" || record.State == "parked" {
		return record, false, nil
	}
	if record.Mode != mode {
		return record, false, fmt.Errorf("store: conflicting reactive commit")
	}
	return record, created == 1, nil
}

func (s *Store) GetReactiveCommit(ctx context.Context, representationID string) (ReactiveCommit, bool, error) {
	var r ReactiveCommit
	var review, ownerCreated int
	var prior sql.NullInt64
	var created, updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT representation_id,item_id,file_id,state,mode,review_required,reason,
		       arr_name,arr_kind,arr_item_id,owner_created,prior_monitored,created_at,updated_at
		FROM reactive_commits WHERE representation_id=?`, representationID).Scan(
		&r.RepresentationID, &r.ItemID, &r.FileID, &r.State, &r.Mode, &review, &r.Reason,
		&r.ArrName, &r.ArrKind, &r.ArrItemID, &ownerCreated, &prior, &created, &updated)
	if err == sql.ErrNoRows {
		return ReactiveCommit{}, false, nil
	}
	if err != nil {
		return ReactiveCommit{}, false, fmt.Errorf("store: get reactive commit: %w", err)
	}
	r.ReviewRequired = review != 0
	r.OwnerCreated = ownerCreated != 0
	if prior.Valid {
		v := prior.Int64 != 0
		r.PriorMonitored = &v
	}
	var errParse error
	r.CreatedAt, errParse = time.Parse(time.RFC3339Nano, created)
	if errParse == nil {
		r.UpdatedAt, errParse = time.Parse(time.RFC3339Nano, updated)
	}
	if errParse != nil {
		return ReactiveCommit{}, false, fmt.Errorf("store: parse reactive commit time: %w", errParse)
	}
	files, err := s.db.QueryContext(ctx, `SELECT arr_file_id FROM reactive_commit_files WHERE representation_id=? ORDER BY arr_file_id`, representationID)
	if err != nil {
		return ReactiveCommit{}, false, err
	}
	for files.Next() {
		var id int
		if err := files.Scan(&id); err != nil {
			_ = files.Close()
			return ReactiveCommit{}, false, err
		}
		r.ArrFileIDs = append(r.ArrFileIDs, id)
	}
	err = files.Err()
	_ = files.Close()
	if err != nil {
		return ReactiveCommit{}, false, err
	}
	episodes, err := s.db.QueryContext(ctx, `SELECT arr_episode_id,prior_monitored FROM reactive_commit_episodes WHERE representation_id=? ORDER BY arr_episode_id`, representationID)
	if err != nil {
		return ReactiveCommit{}, false, err
	}
	for episodes.Next() {
		var ep ReactiveCommitEpisode
		var monitored int
		if err := episodes.Scan(&ep.ID, &monitored); err != nil {
			_ = episodes.Close()
			return ReactiveCommit{}, false, err
		}
		ep.PriorMonitored = monitored != 0
		r.Episodes = append(r.Episodes, ep)
	}
	err = episodes.Err()
	_ = episodes.Close()
	return r, true, err
}

func (s *Store) ParkReactiveCommit(ctx context.Context, representationID, reason string, now time.Time) error {
	if reason == "" || now.IsZero() {
		return fmt.Errorf("store: invalid reactive park")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE reactive_commits SET state='parked',reason=?,updated_at=? WHERE representation_id=? AND state='committing'`, reason, formatTime(now), representationID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("store: reactive commit cannot park")
	}
	if err := appendReactiveEvent(ctx, tx, "park", "", "positive_mismatch", 0, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AbortReactiveCommit(ctx context.Context, representationID string) error {
	if representationID == "" {
		return fmt.Errorf("store: invalid reactive abort")
	}
	_, err := s.execWrite(ctx, `DELETE FROM reactive_commits WHERE representation_id=? AND state='committing'`, representationID)
	return err
}

func (s *Store) CompleteReactiveCommit(ctx context.Context, r ReactiveCommit, now time.Time) error {
	if r.RepresentationID == "" || r.ArrName == "" || (r.ArrKind != "series" && r.ArrKind != "movie") || r.ArrItemID <= 0 || len(r.ArrFileIDs) == 0 || now.IsZero() {
		return fmt.Errorf("store: invalid reactive completion")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	prior := any(nil)
	if r.PriorMonitored != nil {
		if *r.PriorMonitored {
			prior = 1
		} else {
			prior = 0
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE reactive_commits SET state='committed',review_required=?,reason=?,arr_name=?,arr_kind=?,arr_item_id=?,owner_created=?,prior_monitored=?,updated_at=? WHERE representation_id=? AND state='committing'`, boolToInt(r.ReviewRequired), r.Reason, r.ArrName, r.ArrKind, r.ArrItemID, boolToInt(r.OwnerCreated), prior, formatTime(now), r.RepresentationID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("store: reactive commit cannot complete")
	}
	for _, id := range r.ArrFileIDs {
		if id <= 0 {
			return fmt.Errorf("store: invalid reactive file id")
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO reactive_commit_files(representation_id,arr_file_id) VALUES (?,?)`, r.RepresentationID, id); err != nil {
			return err
		}
	}
	for _, ep := range r.Episodes {
		if ep.ID <= 0 {
			return fmt.Errorf("store: invalid reactive episode id")
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO reactive_commit_episodes(representation_id,arr_episode_id,prior_monitored) VALUES (?,?,?)`, r.RepresentationID, ep.ID, boolToInt(ep.PriorMonitored)); err != nil {
			return err
		}
	}
	outcome := "strong_evidence"
	if r.ReviewRequired {
		outcome = "weak_evidence"
	}
	if err := appendReactiveEvent(ctx, tx, "commit", r.Mode, outcome, 0, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginReactiveUndo(ctx context.Context, representationID string, now time.Time) (ReactiveCommit, error) {
	n, err := s.execWriteAffected(ctx, `UPDATE reactive_commits SET state='undoing',updated_at=? WHERE representation_id=? AND state='committed'`, formatTime(now), representationID)
	if err != nil {
		return ReactiveCommit{}, err
	}
	if n == 0 {
		r, found, getErr := s.GetReactiveCommit(ctx, representationID)
		if getErr != nil {
			return ReactiveCommit{}, getErr
		}
		if found && r.State == "undone" {
			return r, nil
		}
		return ReactiveCommit{}, fmt.Errorf("store: reactive commit cannot undo")
	}
	r, _, err := s.GetReactiveCommit(ctx, representationID)
	return r, err
}

func (s *Store) CompleteReactiveUndo(ctx context.Context, representationID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE reactive_commits SET state='undone',updated_at=? WHERE representation_id=? AND state='undoing'`, formatTime(now), representationID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("store: reactive undo cannot complete")
	}
	if err := appendReactiveEvent(ctx, tx, "undo", "", "", 0, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteStrmBlobs(ctx context.Context, itemID string) error {
	_, err := s.execWrite(ctx, `DELETE FROM strm_blobs WHERE item_id=?`, itemID)
	return err
}
