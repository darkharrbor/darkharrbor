package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// HTTPBackendDetection is the persisted, URL-bound protocol classification
// for one immutable named backend (HS-3.4/D6).
type HTTPBackendDetection struct {
	BackendID       string
	BaseURL         string
	DetectorVersion int
	BackendType     string
	DetectedAt      time.Time
}

func (s *Store) GetHTTPBackendDetection(ctx context.Context, backendID string) (*HTTPBackendDetection, error) {
	var d HTTPBackendDetection
	var detectedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT backend_id, base_url, detector_version, backend_type, detected_at
		FROM http_backend_detection WHERE backend_id=?`, backendID).
		Scan(&d.BackendID, &d.BaseURL, &d.DetectorVersion, &d.BackendType, &detectedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get http backend detection: %w", err)
	}
	d.DetectedAt, err = time.Parse(time.RFC3339Nano, detectedAt)
	if err != nil {
		return nil, fmt.Errorf("parse http backend detection time: %w", err)
	}
	return &d, nil
}

func (s *Store) PutHTTPBackendDetection(ctx context.Context, d HTTPBackendDetection) error {
	if d.DetectedAt.IsZero() {
		d.DetectedAt = s.now()
	}
	_, err := s.execWrite(ctx, `
		INSERT INTO http_backend_detection
		    (backend_id, base_url, detector_version, backend_type, detected_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(backend_id) DO UPDATE SET
		    base_url=excluded.base_url,
		    detector_version=excluded.detector_version,
		    backend_type=excluded.backend_type,
		    detected_at=excluded.detected_at`,
		d.BackendID, d.BaseURL, d.DetectorVersion, d.BackendType,
		d.DetectedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("put http backend detection: %w", err)
	}
	return nil
}

func (s *Store) DeleteHTTPBackendDetection(ctx context.Context, backendID string) error {
	if _, err := s.execWrite(ctx, `DELETE FROM http_backend_detection WHERE backend_id=?`, backendID); err != nil {
		return fmt.Errorf("delete http backend detection: %w", err)
	}
	return nil
}
