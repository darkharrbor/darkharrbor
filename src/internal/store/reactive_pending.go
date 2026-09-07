package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const MaxReactivePendingPage = 100

type ReactivePending struct {
	RepresentationID string
	ItemID           string
	Reason           string
	State            string
	ExitAction       string
	ArrName          string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type ReactiveAssignment struct {
	Kind     string
	Provider string
	Value    string
	ArrName  string
}

func (s *Store) SyncReactivePending(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return errors.New("store: zero reactive pending sync time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	queries := []string{
		`INSERT OR IGNORE INTO reactive_pending (representation_id,item_id,reason,created_at,updated_at)
		 SELECT p.representation_id,p.item_id,'supervised_review',p.created_at,?
		 FROM playback_proposals p LEFT JOIN reactive_commits c ON c.representation_id=p.representation_id
		 WHERE c.representation_id IS NULL`,
		`INSERT OR IGNORE INTO reactive_pending (representation_id,item_id,reason,created_at,updated_at)
		 SELECT c.representation_id,c.item_id,'positive_mismatch',c.created_at,?
		 FROM reactive_commits c WHERE c.state='parked'`,
		`INSERT OR IGNORE INTO reactive_pending (representation_id,item_id,reason,created_at,updated_at)
		 SELECT c.representation_id,c.item_id,'weak_evidence',c.created_at,?
		 FROM reactive_commits c WHERE c.state='committed' AND c.review_required=1`,
	}
	for _, query := range queries {
		if _, err := tx.ExecContext(ctx, query, formatTime(now)); err != nil {
			return fmt.Errorf("store: sync reactive pending: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) EnqueueReactivePending(ctx context.Context, entry ReactivePending, now time.Time) error {
	if entry.RepresentationID == "" || entry.ItemID == "" || !validPendingReason(entry.Reason) || now.IsZero() {
		return errors.New("store: invalid reactive pending entry")
	}
	_, err := s.execWrite(ctx, `INSERT INTO reactive_pending
		(representation_id,item_id,reason,state,created_at,updated_at)
		VALUES (?,?,?,'pending',?,?)
		ON CONFLICT(representation_id) DO UPDATE SET
			reason=CASE WHEN reactive_pending.state='pending' THEN excluded.reason ELSE reactive_pending.reason END,
			updated_at=CASE WHEN reactive_pending.state='pending' THEN excluded.updated_at ELSE reactive_pending.updated_at END`,
		entry.RepresentationID, entry.ItemID, entry.Reason, formatTime(now), formatTime(now))
	return err
}

func (s *Store) ListReactivePending(ctx context.Context, after string, limit int) ([]ReactivePending, error) {
	if limit <= 0 || limit > MaxReactivePendingPage || strings.ContainsAny(after, "\r\n\x00") {
		return nil, errors.New("store: invalid reactive pending page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT representation_id,item_id,reason,state,exit_action,arr_name,created_at,updated_at
		FROM reactive_pending WHERE state='pending' AND representation_id>? ORDER BY representation_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReactivePending
	for rows.Next() {
		var entry ReactivePending
		var created, updated string
		if err := rows.Scan(&entry.RepresentationID, &entry.ItemID, &entry.Reason, &entry.State, &entry.ExitAction, &entry.ArrName, &created, &updated); err != nil {
			return nil, err
		}
		entry.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err == nil {
			entry.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		}
		if err != nil {
			return nil, fmt.Errorf("store: parse reactive pending time: %w", err)
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (s *Store) GetReactivePending(ctx context.Context, representationID string) (ReactivePending, bool, error) {
	var entry ReactivePending
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT representation_id,item_id,reason,state,exit_action,arr_name,created_at,updated_at
		FROM reactive_pending WHERE representation_id=?`, representationID).Scan(
		&entry.RepresentationID, &entry.ItemID, &entry.Reason, &entry.State, &entry.ExitAction, &entry.ArrName, &created, &updated)
	if err == sql.ErrNoRows {
		return ReactivePending{}, false, nil
	}
	if err != nil {
		return ReactivePending{}, false, err
	}
	entry.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err == nil {
		entry.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	}
	return entry, true, err
}

func (s *Store) ResolveReactivePending(ctx context.Context, representationID, action, arrName string, now time.Time) error {
	entry, found, err := s.GetReactivePending(ctx, representationID)
	if err != nil || !found {
		if err == nil {
			err = errors.New("store: reactive pending entry not found")
		}
		return err
	}
	if entry.State == "resolved" {
		if entry.ExitAction == action && entry.ArrName == arrName {
			return nil
		}
		return errors.New("store: reactive pending decision conflicts")
	}
	if !validPendingAction(entry.Reason, action) || now.IsZero() || strings.ContainsAny(arrName, "\r\n\x00") || len(arrName) > 128 {
		return errors.New("store: invalid reactive pending decision")
	}
	if action == "assign" && strings.TrimSpace(arrName) == "" {
		return errors.New("store: assignment requires Arr target")
	}
	n, err := s.execWriteAffected(ctx, `UPDATE reactive_pending SET state='resolved',exit_action=?,arr_name=?,updated_at=?
		WHERE representation_id=? AND state='pending'`, action, strings.TrimSpace(arrName), formatTime(now), representationID)
	if err != nil {
		return err
	}
	if n != 1 {
		current, found, getErr := s.GetReactivePending(ctx, representationID)
		if getErr == nil && found && current.State == "resolved" && current.ExitAction == action && current.ArrName == strings.TrimSpace(arrName) {
			return nil
		}
		if getErr != nil {
			return getErr
		}
		return errors.New("store: reactive pending decision raced")
	}
	return nil
}

func (s *Store) PutReactiveAssignment(ctx context.Context, assignment ReactiveAssignment, now time.Time) error {
	assignment.Kind = strings.TrimSpace(assignment.Kind)
	assignment.Provider = strings.TrimSpace(assignment.Provider)
	assignment.Value = strings.TrimSpace(assignment.Value)
	assignment.ArrName = strings.TrimSpace(assignment.ArrName)
	if !validAssignment(assignment) || now.IsZero() {
		return errors.New("store: invalid reactive assignment")
	}
	_, err := s.execWrite(ctx, `INSERT INTO reactive_arr_assignments
		(identity_kind,identity_provider,identity_value,arr_name,created_at,updated_at)
		VALUES (?,?,?,?,?,?) ON CONFLICT(identity_kind,identity_provider,identity_value) DO UPDATE SET
		arr_name=excluded.arr_name,updated_at=excluded.updated_at`, assignment.Kind, assignment.Provider,
		assignment.Value, assignment.ArrName, formatTime(now), formatTime(now))
	return err
}

func (s *Store) GetReactiveAssignment(ctx context.Context, kind, provider, value string) (ReactiveAssignment, bool, error) {
	key := ReactiveAssignment{Kind: strings.TrimSpace(kind), Provider: strings.TrimSpace(provider), Value: strings.TrimSpace(value)}
	if !validAssignmentKey(key) {
		return ReactiveAssignment{}, false, errors.New("store: invalid reactive assignment key")
	}
	err := s.db.QueryRowContext(ctx, `SELECT arr_name FROM reactive_arr_assignments
		WHERE identity_kind=? AND identity_provider=? AND identity_value=?`, key.Kind, key.Provider, key.Value).Scan(&key.ArrName)
	if err == sql.ErrNoRows {
		return ReactiveAssignment{}, false, nil
	}
	return key, err == nil, err
}

func validPendingReason(reason string) bool {
	switch reason {
	case "supervised_review", "weak_evidence", "positive_mismatch", "routing_unresolved", "routing_ambiguous":
		return true
	}
	return false
}

func validPendingAction(reason, action string) bool {
	switch reason {
	case "supervised_review":
		return action == "approve" || action == "jellyfin_only" || action == "remove" || action == "assign"
	case "weak_evidence":
		return action == "acknowledge" || action == "undo"
	case "positive_mismatch":
		return action == "jellyfin_only" || action == "remove"
	case "routing_unresolved", "routing_ambiguous":
		return action == "assign" || action == "jellyfin_only" || action == "remove"
	}
	return false
}

func validAssignment(assignment ReactiveAssignment) bool {
	return validAssignmentKey(assignment) && assignment.ArrName != "" && len(assignment.ArrName) <= 128 && !strings.ContainsAny(assignment.ArrName, "\r\n\x00")
}

func validAssignmentKey(assignment ReactiveAssignment) bool {
	if assignment.Kind != "series" && assignment.Kind != "movie" {
		return false
	}
	if assignment.Provider != "tvdb" && assignment.Provider != "imdb" && assignment.Provider != "tmdb" {
		return false
	}
	return assignment.Value != "" && len(assignment.Value) <= 64 && !strings.ContainsAny(assignment.Value, "\r\n\x00")
}
