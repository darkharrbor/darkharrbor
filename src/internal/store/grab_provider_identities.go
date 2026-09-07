package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
)

const grabProviderIdentityTTL = 30 * 24 * time.Hour

// UpsertGrabProviderIdentity durably carries authoritative Arr identity from
// search/proxy time to the later qBit/SAB add request.
func (s *Store) UpsertGrabProviderIdentity(ctx context.Context, key string, identity ProviderIdentity) error {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 255 || strings.Contains(key, "://") || strings.ContainsAny(key, "\r\n\x00") {
		return fmt.Errorf("upsert grab provider identity: invalid key")
	}
	if err := mediaidentity.Validate(&identity); err != nil {
		return fmt.Errorf("upsert grab provider identity: %w", err)
	}
	body, err := json.Marshal(identity)
	if err != nil || len(body) > 256<<10 {
		return fmt.Errorf("upsert grab provider identity: invalid payload")
	}
	now := s.now()
	if _, err := s.execWrite(ctx, `
		INSERT INTO grab_provider_identities (transport_key, identity_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(transport_key) DO UPDATE SET
			identity_json=excluded.identity_json,
			updated_at=excluded.updated_at`,
		key, string(body), formatTime(now)); err != nil {
		return fmt.Errorf("upsert grab provider identity: %w", err)
	}
	if _, err := s.execWrite(ctx, `DELETE FROM grab_provider_identities WHERE updated_at < ?`,
		formatTime(now.Add(-grabProviderIdentityTTL))); err != nil {
		return fmt.Errorf("prune grab provider identities: %w", err)
	}
	return nil
}

// GetGrabProviderIdentity returns unexpired identity context for key.
func (s *Store) GetGrabProviderIdentity(ctx context.Context, key string) (*ProviderIdentity, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false, nil
	}
	var raw string
	var updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT identity_json, updated_at FROM grab_provider_identities WHERE transport_key=?`, key).
		Scan(&raw, &updated)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get grab provider identity: %w", err)
	}
	at, err := parseTime(updated)
	if err != nil || at.Before(s.now().Add(-grabProviderIdentityTTL)) {
		return nil, false, nil
	}
	var identity ProviderIdentity
	if err := json.Unmarshal([]byte(raw), &identity); err != nil {
		return nil, false, fmt.Errorf("get grab provider identity: decode: %w", err)
	}
	if err := mediaidentity.Validate(&identity); err != nil {
		return nil, false, fmt.Errorf("get grab provider identity: %w", err)
	}
	return &identity, true, nil
}
