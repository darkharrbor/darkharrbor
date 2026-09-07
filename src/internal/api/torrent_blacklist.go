package api

import (
	"context"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

// torrentBlacklistIdentities returns the normalized submitted hash and the
// canonical real hash used for provider work. Synthetic releases therefore
// inherit blacklist policy from their underlying torrent while retaining the
// client-facing alias for compatibility.
func torrentBlacklistIdentities(ctx context.Context, st *store.Store, submittedHash string) (submitted, canonical string, err error) {
	submitted = normalizeInfoHash(submittedHash)
	canonical = submitted
	if st == nil || submitted == "" {
		return submitted, canonical, nil
	}

	mapping, err := st.GetSyntheticRelease(ctx, submitted)
	if err != nil {
		return submitted, canonical, err
	}
	if mapping != nil {
		if real := normalizeInfoHash(mapping.RealInfohash); real != "" {
			canonical = real
		}
	}
	return submitted, canonical, nil
}

// isTorrentBlacklisted checks the canonical real torrent identity and, when
// different, the submitted synthetic alias. It returns the identity that caused
// rejection so callers can log a safe normalized hash without exposing magnets.
func isTorrentBlacklisted(ctx context.Context, st *store.Store, submittedHash string) (bool, string, error) {
	submitted, canonical, err := torrentBlacklistIdentities(ctx, st, submittedHash)
	if err != nil {
		return false, "", err
	}

	identities := []string{canonical}
	if submitted != "" && !strings.EqualFold(submitted, canonical) {
		identities = append(identities, submitted)
	}
	for _, identity := range identities {
		if identity == "" || st == nil {
			continue
		}
		blacklisted, err := st.IsBlacklisted(ctx, identity)
		if err != nil {
			return false, "", err
		}
		if blacklisted {
			return true, identity, nil
		}
	}
	return false, "", nil
}
