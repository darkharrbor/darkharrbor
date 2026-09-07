package store

import (
	"context"
	"database/sql"
	"fmt"
)

// NNTPArtifacts are content-keyed archive manifests shared by equivalent NZBs.
type NNTPArtifacts struct {
	RARManifest *string
	ZIPManifest *string
}

func (s *Store) GetNNTPArtifacts(ctx context.Context, contentKey string) (NNTPArtifacts, error) {
	var rar, zip sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT rar_manifest, zip_manifest FROM nntp_artifacts WHERE content_key=?`, contentKey,
	).Scan(&rar, &zip)
	if err == sql.ErrNoRows {
		return NNTPArtifacts{}, nil
	}
	if err != nil {
		return NNTPArtifacts{}, fmt.Errorf("get nntp artifacts: %w", err)
	}
	return NNTPArtifacts{RARManifest: fromNullString(rar), ZIPManifest: fromNullString(zip)}, nil
}

// PromoteNNTPArtifacts copies an item's existing manifests into the
// content-keyed cache. Legacy item offsets are read as a fallback and are
// promoted only when observed with a stable per-file content key.
func (s *Store) PromoteNNTPArtifacts(ctx context.Context, contentKey, itemID string, rarManifest, zipManifest *string) error {
	_, err := s.execWrite(ctx, `
		INSERT INTO nntp_artifacts (content_key, rar_manifest, zip_manifest, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(content_key) DO UPDATE SET
			rar_manifest=COALESCE(excluded.rar_manifest, nntp_artifacts.rar_manifest),
			zip_manifest=COALESCE(excluded.zip_manifest, nntp_artifacts.zip_manifest),
			updated_at=excluded.updated_at`,
		contentKey, nullableString(rarManifest), nullableString(zipManifest), formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("promote nntp manifests: %w", err)
	}
	return nil
}
