package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

// UpsertTorrentMeta durably persists the compact TorrentMeta harvested from
// an arr-uploaded .torrent file (TS-0.1/T1), keyed by the v1 infohash -- the
// same identity item.InfoHash uses everywhere else in DarkHarrbor. A v2-only
// torrent with no v1 "pieces" field has no v1 infohash to key on and is
// intentionally not persisted here; this is non-fatal per T1, and a future
// v2-primary consumer can add its own lookup path if that case matters to it.
func (s *Store) UpsertTorrentMeta(ctx context.Context, meta *torrentmeta.TorrentMeta) error {
	if meta == nil || meta.InfoHashV1 == "" {
		return nil
	}
	filesJSON, err := json.Marshal(meta.Files)
	if err != nil {
		return fmt.Errorf("marshal torrent meta files: %w", err)
	}

	var infoHashV2 any
	if meta.InfoHashV2 != "" {
		infoHashV2 = meta.InfoHashV2
	}
	var pieceHashes any
	if len(meta.PieceHashesV1) > 0 {
		pieceHashes = meta.PieceHashesV1
	}
	var rootsJSON any
	if len(meta.V2MerkleRoots) > 0 {
		b, err := json.Marshal(meta.V2MerkleRoots)
		if err != nil {
			return fmt.Errorf("marshal torrent meta v2 merkle roots: %w", err)
		}
		rootsJSON = string(b)
	}
	// TS-3.4 (T12): PieceLayers persists alongside V2MerkleRoots so a stream
	// request served in a later process lifetime can still opportunistically
	// verify v2 pieces without re-parsing the original .torrent (never
	// retained). encoding/json base64-encodes each []byte value
	// automatically; absence (v1-only torrent, or no file over one piece
	// long) leaves this column NULL, which GetTorrentMeta treats the same
	// as any other non-fatal absence.
	var layersJSON any
	if len(meta.PieceLayers) > 0 {
		b, err := json.Marshal(meta.PieceLayers)
		if err != nil {
			return fmt.Errorf("marshal torrent meta piece layers: %w", err)
		}
		layersJSON = string(b)
	}

	_, err = s.execWrite(ctx, `
		INSERT INTO torrent_meta (
			info_hash, info_hash_v2, name, piece_length, meta_version,
			files_json, piece_hashes_v1, v2_merkle_roots, piece_layers_json, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(info_hash) DO UPDATE SET
			info_hash_v2=excluded.info_hash_v2,
			name=excluded.name,
			piece_length=excluded.piece_length,
			meta_version=excluded.meta_version,
			files_json=excluded.files_json,
			piece_hashes_v1=excluded.piece_hashes_v1,
			v2_merkle_roots=excluded.v2_merkle_roots,
			piece_layers_json=excluded.piece_layers_json`,
		meta.InfoHashV1, infoHashV2, meta.Name, meta.PieceLength, meta.MetaVersion,
		string(filesJSON), pieceHashes, rootsJSON, layersJSON, formatTime(s.now()),
	)
	if err != nil {
		return fmt.Errorf("upsert torrent meta %s: %w", meta.InfoHashV1, err)
	}
	return nil
}

// GetTorrentMeta returns the persisted TorrentMeta for infoHash (v1), if any
// row exists. A miss is not an error -- absence is always non-fatal (T1).
func (s *Store) GetTorrentMeta(ctx context.Context, infoHash string) (*torrentmeta.TorrentMeta, bool, error) {
	if infoHash == "" {
		return nil, false, nil
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT info_hash, info_hash_v2, name, piece_length, meta_version, files_json, piece_hashes_v1, v2_merkle_roots, piece_layers_json
		FROM torrent_meta WHERE info_hash=?`, infoHash)

	var (
		v1, name, filesJSON string
		v2, roots, layers   sql.NullString
		pieceLength         int64
		metaVersion         int
		pieceHashes         []byte
	)
	if err := row.Scan(&v1, &v2, &name, &pieceLength, &metaVersion, &filesJSON, &pieceHashes, &roots, &layers); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get torrent meta %s: %w", infoHash, err)
	}

	meta := &torrentmeta.TorrentMeta{
		InfoHashV1:    v1,
		Name:          name,
		PieceLength:   pieceLength,
		MetaVersion:   metaVersion,
		PieceHashesV1: pieceHashes,
	}
	if v2.Valid {
		meta.InfoHashV2 = v2.String
	}
	if filesJSON != "" {
		if err := json.Unmarshal([]byte(filesJSON), &meta.Files); err != nil {
			return nil, false, fmt.Errorf("unmarshal torrent meta files %s: %w", infoHash, err)
		}
	}
	if roots.Valid && roots.String != "" {
		if err := json.Unmarshal([]byte(roots.String), &meta.V2MerkleRoots); err != nil {
			return nil, false, fmt.Errorf("unmarshal torrent meta v2 roots %s: %w", infoHash, err)
		}
	}
	if layers.Valid && layers.String != "" {
		if err := json.Unmarshal([]byte(layers.String), &meta.PieceLayers); err != nil {
			return nil, false, fmt.Errorf("unmarshal torrent meta piece layers %s: %w", infoHash, err)
		}
	}
	return meta, true, nil
}
