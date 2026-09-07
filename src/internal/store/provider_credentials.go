package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/util"
)

// sealedCredentialPrefix marks a credentials_json value that is sealed at
// rest rather than plaintext JSON. It is deliberately NOT valid JSON, so a
// sealed row can never be mistaken for an unsealed one by any consumer, and
// a plaintext row can never be mistaken for a sealed one.
const sealedCredentialPrefix = "sealed:v1:"

// SetCredentialSealKey installs the key used to seal provider credentials at
// rest (SEC-01). An EMPTY key leaves the store in its historical plaintext
// behaviour: sealed secrets are optional, and refusing to operate without
// them would brick every deployment that never adopted sealing. Callers pass
// config.Secrets.SealKey().
//
// This is separate from the sealed-secrets STORE (migrations/0010 explains
// why provider credentials are not operator-supplied static config and
// cannot be re-fetched at boot). SEC-01 does not move them into that store;
// it reuses only its KEY and its OCSEAL envelope so no new primitive or
// format is introduced.
func (s *Store) SetCredentialSealKey(key string) {
	s.credentialSealKey = util.Redacted(key)
}

// NewWithSealKey is New plus the credential seal key, so call sites cannot
// construct a Store and then FORGET to install the key — a forgotten call
// would silently write the next credential in plaintext and, worse, would
// make an already-sealed row unreadable. Pass "" where sealing is off.
func NewWithSealKey(db *sql.DB, sealKey string) *Store {
	st := New(db)
	st.SetCredentialSealKey(sealKey)
	return st
}

// credentialSealingEnabled reports whether credentials will be sealed.
func (s *Store) credentialSealingEnabled() bool {
	return s.credentialSealKey.Value() != ""
}

// GetProviderCredential returns the persisted credentials_json blob for a
// provider name (e.g. "torbox-nntp"), or ("", false, nil) if none is stored
// yet. See migrations/0010_provider_credentials.sql for why this exists —
// some provider capabilities discover credentials that cannot be safely
// re-fetched on every boot.
//
// SEC-01: a stored value may be sealed or legacy plaintext. Which one is
// determined by the marker prefix, never by attempting a decrypt and
// guessing from the failure — that would make a WRONG KEY indistinguishable
// from a plaintext row and silently hand the caller ciphertext-as-JSON. A
// sealed row that will not open FAILS CLOSED with an error naming the
// provider and never the key or the value.
//
// A legacy plaintext row is returned and then RE-SEALED IN PLACE when a key
// is available, so an existing deployment migrates on first read with no
// flag day and no operator action. A re-seal failure is logged and does not
// fail the read: the caller's credential is valid either way, and refusing
// to return it would break playback to fix a storage property.
func (s *Store) GetProviderCredential(ctx context.Context, providerName string) (string, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT credentials_json FROM provider_credentials WHERE provider_name=? LIMIT 1`, providerName)
	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get provider credential %s: %w", providerName, err)
	}

	if strings.HasPrefix(raw, sealedCredentialPrefix) {
		if !s.credentialSealingEnabled() {
			return "", false, fmt.Errorf("get provider credential %s: value is sealed at rest but no seal key is configured; set HARRBOR_SECRETS_FILE and its unlock mode", providerName)
		}
		plain, err := s.unsealCredential(raw)
		if err != nil {
			return "", false, fmt.Errorf("get provider credential %s: %w", providerName, err)
		}
		return plain, true, nil
	}

	// Legacy plaintext row.
	if s.credentialSealingEnabled() {
		if err := s.SetProviderCredential(ctx, providerName, raw); err != nil {
			slog.Warn("provider credential re-seal failed; value remains plaintext at rest",
				"provider", providerName, "error", err)
		} else {
			slog.Info("provider credential sealed at rest on first read (SEC-01)",
				"provider", providerName)
		}
	}
	return raw, true, nil
}

// SetProviderCredential upserts the credentials_json blob for a provider
// name. Callers own the JSON shape; this table is intentionally generic
// across providers. When a seal key is configured the value is sealed before
// it reaches the database, so the column is opaque to anything reading the
// file or any backup of it.
func (s *Store) SetProviderCredential(ctx context.Context, providerName, credentialsJSON string) error {
	stored := credentialsJSON
	if s.credentialSealingEnabled() {
		sealed, err := s.sealCredential(credentialsJSON)
		if err != nil {
			// FAIL CLOSED. Falling back to a plaintext write here would
			// silently defeat the whole row and leave the operator believing
			// the credential is sealed.
			return fmt.Errorf("set provider credential %s: %w", providerName, err)
		}
		stored = sealed
	}
	_, err := s.execWrite(ctx, `
        INSERT INTO provider_credentials (provider_name, credentials_json, updated_at)
        VALUES (?, ?, ?)
        ON CONFLICT(provider_name) DO UPDATE SET
            credentials_json = excluded.credentials_json,
            updated_at       = excluded.updated_at`,
		providerName, stored, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("set provider credential %s: %w", providerName, err)
	}
	return nil
}

// sealCredential wraps plaintext in the existing OCSEAL envelope and encodes
// it for a TEXT column. No new crypto: util.SealSecrets is AES-256-GCM over
// an argon2id key whose parameters travel in the header.
func (s *Store) sealCredential(plaintext string) (string, error) {
	blob, err := util.SealSecrets([]byte(plaintext), s.credentialSealKey.Value())
	if err != nil {
		return "", fmt.Errorf("seal credential: %w", err)
	}
	return sealedCredentialPrefix + base64.StdEncoding.EncodeToString(blob), nil
}

// unsealCredential reverses sealCredential. Errors never include the key or
// any plaintext.
func (s *Store) unsealCredential(stored string) (string, error) {
	blob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, sealedCredentialPrefix))
	if err != nil {
		return "", fmt.Errorf("unseal credential: value is marked sealed but is not valid base64 (corrupt row): %w", err)
	}
	plain, err := util.UnsealSecrets(blob, s.credentialSealKey.Value())
	if err != nil {
		return "", fmt.Errorf("unseal credential: %w (wrong seal key, or the row was written under a different one)", err)
	}
	return string(plain), nil
}
