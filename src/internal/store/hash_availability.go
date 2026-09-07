package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// HashAvailability is one persisted own-traffic observation of whether a
// provider currently has a given torrent hash cached (TS-5.1 / T6).
type HashAvailability struct {
	Provider   string
	InfoHash   string
	Cached     bool
	Source     string
	ObservedAt time.Time
	ExpiresAt  time.Time
	Hits       int64
}

// RecordHashAvailability upserts one own-traffic observation of whether
// provider currently has infoHash cached, as seen via source (submit-time
// CheckCached, a real stream-time capability probe, or -- once TS-3.1 lands
// -- grab-time preflight). ttl bounds how long the observation is considered
// live; a repeat observation for the same (provider, info_hash) refreshes
// observed_at/expires_at and increments hits, matching
// SetFileProbeFailure's (SF-05) established upsert-on-observe shape.
//
// Callers are expected to have already normalized provider/infoHash via
// internal/availability; this method itself does not validate shape (that
// responsibility lives with the pure package per the SF-03/internal/suppress
// split), only that neither is empty.
func (s *Store) RecordHashAvailability(ctx context.Context, providerName, infoHash string, cached bool, source string, ttl time.Duration) error {
	if providerName == "" || infoHash == "" {
		return fmt.Errorf("store: record hash availability: provider and info_hash are required")
	}
	now := s.now()
	_, err := s.execWrite(ctx, `
        INSERT INTO hash_availability (provider, info_hash, cached, source, observed_at, expires_at, hits)
        VALUES (?, ?, ?, ?, ?, ?, 0)
        ON CONFLICT(provider, info_hash) DO UPDATE SET
            cached      = excluded.cached,
            source      = excluded.source,
            observed_at = excluded.observed_at,
            expires_at  = excluded.expires_at,
            hits        = hash_availability.hits + 1`,
		providerName, infoHash, boolToInt(cached), source,
		formatTime(now), formatTime(now.Add(ttl)))
	if err != nil {
		return fmt.Errorf("store: record hash availability %s/%s: %w", providerName, infoHash, err)
	}
	return nil
}

// GetHashAvailability returns the live (unexpired) observation for
// (provider, infoHash), or found=false if none is stored or it has expired.
// An expired row is left in place (no delete-on-read here, unlike
// GetFileProbeFailure's negative cache): TS-5.2's future age/confidence
// consumption may still want to distinguish "never observed" from "observed,
// but stale," so this row's own store layer does not decide that policy --
// it only reports the live/expired boundary via the returned bool.
func (s *Store) GetHashAvailability(ctx context.Context, providerName, infoHash string) (HashAvailability, bool, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT provider, info_hash, cached, source, observed_at, expires_at, hits
        FROM hash_availability
        WHERE provider = ? AND info_hash = ?`, providerName, infoHash)

	var (
		ha          HashAvailability
		cachedInt   int
		observedRaw string
		expiresRaw  string
	)
	if err := row.Scan(&ha.Provider, &ha.InfoHash, &cachedInt, &ha.Source, &observedRaw, &expiresRaw, &ha.Hits); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HashAvailability{}, false, nil
		}
		return HashAvailability{}, false, fmt.Errorf("store: get hash availability %s/%s: %w", providerName, infoHash, err)
	}
	ha.Cached = cachedInt == 1
	observedAt, err := parseTime(observedRaw)
	if err != nil {
		return HashAvailability{}, false, fmt.Errorf("store: get hash availability %s/%s: parse observed_at: %w", providerName, infoHash, err)
	}
	expiresAt, err := parseTime(expiresRaw)
	if err != nil {
		return HashAvailability{}, false, fmt.Errorf("store: get hash availability %s/%s: parse expires_at: %w", providerName, infoHash, err)
	}
	ha.ObservedAt = observedAt
	ha.ExpiresAt = expiresAt
	if !s.now().Before(expiresAt) {
		return ha, false, nil
	}
	return ha, true, nil
}
