package torrentmeta

import (
	"crypto/sha1" // #nosec G505 -- BitTorrent v1 infohash is defined as SHA-1; not used for any security property here.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// maxTorrentBytes bounds the raw .torrent upload this package will attempt
// to decode. handleQBitAdd's ParseMultipartForm already caps the whole
// multipart request at 8 MiB; this is an independent, slightly looser cap
// so the package is safe for any future caller with a different source.
const maxTorrentBytes = 16 << 20

// maxFileEntries bounds the number of files a single torrent may declare,
// independent of the generic maxContainerLen list bound in bencode.go, so a
// season-pack-shaped torrent has ample headroom while a pathological
// millions-of-empty-files torrent is rejected before FileEntry allocation.
const maxFileEntries = 100_000

// FileEntry is one file inside a (possibly multi-file) torrent, with its
// byte extent within the v1 concatenated piece stream pre-computed at parse
// time (grab time) — this is the "season-pack episodes pre-map to file
// indexes/extents" half of TS-0.1's scope. StartOffset/EndOffset are exact
// for v1/hybrid torrents (files are concatenated in listed order for piece
// alignment); for a pure v2-only torrent they are computed the same way for
// consistency, though v2 has no single global piece stream, so they are
// advisory rather than piece-verified in that case.
type FileEntry struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	StartOffset int64  `json:"start_offset"`
	EndOffset   int64  `json:"end_offset"`
}

// TorrentMeta is the compact, URL-free record harvested from an
// arr-uploaded .torrent file (T1). It never carries a source/CDN URL or
// content bytes — only structural metadata needed for verification (T12),
// verified naming (TS-0.3), and cross-lane splice (TS-7.x/TS-15).
type TorrentMeta struct {
	Name          string            `json:"name"`
	InfoHashV1    string            `json:"info_hash_v1,omitempty"` // hex40, empty if this torrent has no v1 "pieces" field
	InfoHashV2    string            `json:"info_hash_v2,omitempty"` // hex64, empty if not meta version 2 / no file tree
	MetaVersion   int               `json:"meta_version"`           // 1 or 2 (2 = v2/hybrid file tree present)
	PieceLength   int64             `json:"piece_length"`
	Files         []FileEntry       `json:"files"`
	PieceHashesV1 []byte            `json:"piece_hashes_v1,omitempty"` // concatenated 20-byte SHA-1 hashes; nil if absent
	V2MerkleRoots map[string]string `json:"v2_merkle_roots,omitempty"` // path -> hex SHA-256 root; nil if absent

	// PieceLayers holds BEP52 v2 per-piece hash material (TS-3.4/T12),
	// harvested verbatim from the same already-parsed info dict as
	// V2MerkleRoots -- no extra fetch. Keyed by the hex-encoded 32-byte
	// merkle root (the same string V2MerkleRoots' values use), each value
	// is the concatenation of one 32-byte SHA-256 hash per piece of that
	// file, at piece-length granularity (BEP52 "piece layers": "the layer
	// is chosen so that one hash covers piece length bytes"). A file whose
	// size does not exceed PieceLength has no entry here by design (BEP52
	// omits piece layers for such files; its root IS its only piece hash).
	// nil if absent (v1-only torrent, or a v2 torrent with no file over one
	// piece in length).
	PieceLayers map[string][]byte `json:"piece_layers,omitempty"`
}

// Parse decodes a raw .torrent file and extracts a compact TorrentMeta.
// Absence of v1 or v2 hash material is non-fatal per T1 ("for magnet-only
// adds ... absence is non-fatal") — but at least one of the two must be
// present, or the torrent has no verifiable identity at all and is rejected.
func Parse(data []byte) (*TorrentMeta, error) {
	if len(data) == 0 {
		return nil, errors.New("torrentmeta: empty input")
	}
	if len(data) > maxTorrentBytes {
		return nil, fmt.Errorf("torrentmeta: input exceeds %d bytes", maxTorrentBytes)
	}

	root, err := decodeTop(data)
	if err != nil {
		return nil, fmt.Errorf("torrentmeta: decode: %w", err)
	}
	if root.kind != kDict {
		return nil, errors.New("torrentmeta: root is not a dict")
	}

	infoVal, ok := root.dict["info"]
	if !ok || infoVal.kind != kDict {
		return nil, errors.New("torrentmeta: missing info dict")
	}
	// The raw, verbatim bencoded span of the info dict is what both v1 and
	// v2 infohashes are computed over. It must be the exact original bytes,
	// never a re-encoding — re-encoding could silently change key order or
	// integer representation and produce a hash mismatching every tracker
	// and provider's own infohash for this torrent.
	rawInfo := data[infoVal.start:infoVal.end]

	meta := &TorrentMeta{}

	if nameVal, ok := infoVal.dict["name"]; ok {
		if nameVal.kind != kBytes {
			return nil, errors.New("torrentmeta: name field is not a string")
		}
		name, err := sanitizeSegment(string(nameVal.b))
		if err != nil {
			return nil, fmt.Errorf("torrentmeta: name: %w", err)
		}
		meta.Name = name
	}

	pieceLenVal, ok := infoVal.dict["piece length"]
	if !ok || pieceLenVal.kind != kInt || pieceLenVal.i <= 0 {
		return nil, errors.New("torrentmeta: missing/invalid piece length")
	}
	meta.PieceLength = pieceLenVal.i

	metaVersion := 1
	if mv, ok := infoVal.dict["meta version"]; ok {
		if mv.kind != kInt {
			return nil, errors.New("torrentmeta: meta version is not an integer")
		}
		metaVersion = int(mv.i)
	}

	if piecesVal, ok := infoVal.dict["pieces"]; ok {
		if piecesVal.kind != kBytes || len(piecesVal.b) == 0 || len(piecesVal.b)%20 != 0 {
			return nil, errors.New("torrentmeta: malformed pieces field (not a multiple of 20 bytes)")
		}
		meta.PieceHashesV1 = append([]byte(nil), piecesVal.b...)
		sum := sha1.Sum(rawInfo)
		meta.InfoHashV1 = hex.EncodeToString(sum[:])
	}

	fileTreeVal, hasFileTree := infoVal.dict["file tree"]
	if hasFileTree && fileTreeVal.kind != kDict {
		return nil, errors.New("torrentmeta: file tree is not a dict")
	}
	if metaVersion == 2 || hasFileTree {
		sum := sha256.Sum256(rawInfo)
		meta.InfoHashV2 = hex.EncodeToString(sum[:])
		meta.MetaVersion = 2
	} else {
		meta.MetaVersion = 1
	}

	// Per BEP52, file tree paths are self-contained and must NOT be
	// re-prefixed with info.name (that field is a v1-only directory-naming
	// convention for the "files" list, handled separately in
	// parseV1FilesList). Re-prepending it here would double the path for
	// single-file v2/hybrid torrents and desynchronize V2MerkleRoots keys
	// from the Files list keys.
	switch {
	case fieldPresent(infoVal, "files"):
		filesVal := infoVal.dict["files"]
		if filesVal.kind != kList {
			return nil, errors.New("torrentmeta: files field is not a list")
		}
		if len(filesVal.list) > maxFileEntries {
			return nil, fmt.Errorf("torrentmeta: too many files (%d)", len(filesVal.list))
		}
		files, err := parseV1FilesList(filesVal, meta.Name)
		if err != nil {
			return nil, err
		}
		meta.Files = files

	case fieldPresent(infoVal, "length"):
		lengthVal := infoVal.dict["length"]
		if lengthVal.kind != kInt || lengthVal.i < 0 {
			return nil, errors.New("torrentmeta: invalid length field")
		}
		if meta.Name == "" {
			return nil, errors.New("torrentmeta: single-file torrent missing name")
		}
		meta.Files = []FileEntry{{Path: meta.Name, Size: lengthVal.i, StartOffset: 0, EndOffset: lengthVal.i}}

	case hasFileTree:
		leaves, err := walkFileTreeLeaves(fileTreeVal, nil)
		if err != nil {
			return nil, err
		}
		if len(leaves) > maxFileEntries {
			return nil, fmt.Errorf("torrentmeta: too many files (%d)", len(leaves))
		}
		var offset int64
		files := make([]FileEntry, 0, len(leaves))
		for _, l := range leaves {
			files = append(files, FileEntry{Path: l.path, Size: l.size, StartOffset: offset, EndOffset: offset + l.size})
			offset += l.size
		}
		meta.Files = files

	default:
		return nil, errors.New("torrentmeta: info dict has no files/length/file tree")
	}

	if hasFileTree {
		leaves, err := walkFileTreeLeaves(fileTreeVal, nil)
		if err != nil {
			return nil, err
		}
		roots := make(map[string]string, len(leaves))
		for _, l := range leaves {
			if l.root != "" {
				roots[l.path] = l.root
			}
		}
		if len(roots) > 0 {
			meta.V2MerkleRoots = roots
		}
	}

	// TS-3.4/T12: "piece layers" is a flat top-level info-dict field (BEP52),
	// present only for v2/hybrid torrents that have at least one file larger
	// than one piece. It sits alongside "file tree" in the same already-
	// decoded/bounded bencode structure -- no extra fetch, no extra trust
	// boundary. Absence is always non-fatal (T1's "absence is non-fatal"
	// rule): a torrent with no piece layers simply has no v2 piece-level
	// verification material, and TS-3.4's own verification path abstains.
	if plVal, ok := infoVal.dict["piece layers"]; ok {
		if plVal.kind != kDict {
			return nil, errors.New("torrentmeta: piece layers is not a dict")
		}
		layers := make(map[string][]byte, len(plVal.dict))
		for k, v := range plVal.dict {
			if len(k) != 32 {
				return nil, errors.New("torrentmeta: piece layers key is not a 32-byte SHA-256 root")
			}
			if v.kind != kBytes || len(v.b) == 0 || len(v.b)%32 != 0 {
				return nil, errors.New("torrentmeta: malformed piece layers value (not a non-empty multiple of 32 bytes)")
			}
			layers[hex.EncodeToString([]byte(k))] = append([]byte(nil), v.b...)
		}
		if len(layers) > 0 {
			meta.PieceLayers = layers
		}
	}

	if meta.InfoHashV1 == "" && meta.InfoHashV2 == "" {
		return nil, errors.New("torrentmeta: no v1 pieces or v2 file tree present; cannot compute an infohash")
	}
	if len(meta.Files) == 0 {
		return nil, errors.New("torrentmeta: no files extracted")
	}

	return meta, nil
}

func fieldPresent(dictVal value, key string) bool {
	_, ok := dictVal.dict[key]
	return ok
}

func parseV1FilesList(filesVal value, name string) ([]FileEntry, error) {
	var offset int64
	files := make([]FileEntry, 0, len(filesVal.list))
	for _, fv := range filesVal.list {
		if fv.kind != kDict {
			return nil, errors.New("torrentmeta: file entry is not a dict")
		}
		lengthVal, ok := fv.dict["length"]
		if !ok || lengthVal.kind != kInt || lengthVal.i < 0 {
			return nil, errors.New("torrentmeta: file entry missing/invalid length")
		}
		pathVal, ok := fv.dict["path"]
		if !ok || pathVal.kind != kList || len(pathVal.list) == 0 {
			return nil, errors.New("torrentmeta: file entry missing path")
		}
		segs := make([]string, 0, len(pathVal.list)+1)
		if name != "" {
			segs = append(segs, name)
		}
		for _, seg := range pathVal.list {
			if seg.kind != kBytes {
				return nil, errors.New("torrentmeta: path segment is not a string")
			}
			segs = append(segs, string(seg.b))
		}
		path, err := sanitizePathSegments(segs)
		if err != nil {
			return nil, err
		}
		files = append(files, FileEntry{Path: path, Size: lengthVal.i, StartOffset: offset, EndOffset: offset + lengthVal.i})
		offset += lengthVal.i
	}
	return files, nil
}

// v2Leaf is one leaf of a BEP52 "file tree" walk.
type v2Leaf struct {
	path string
	size int64
	root string // hex SHA-256 pieces root; "" if the leaf omitted it (zero-length files may omit it)
}

// walkFileTreeLeaves recursively walks a BEP52 file tree dict, returning
// every leaf file in deterministic (sorted-key) order. Sorting is not merely
// a convenience: valid bencode dicts are required to have sorted keys, so
// this also normalizes any dict that violates that requirement rather than
// depending on Go's randomized map iteration for anything security- or
// offset-relevant.
func walkFileTreeLeaves(node value, prefix []string) ([]v2Leaf, error) {
	if node.kind != kDict {
		return nil, errors.New("torrentmeta: file tree node is not a dict")
	}
	keys := make([]string, 0, len(node.dict))
	for k := range node.dict {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []v2Leaf
	for _, key := range keys {
		child := node.dict[key]
		if key == "" {
			lengthVal, ok := child.dict["length"]
			if !ok || lengthVal.kind != kInt || lengthVal.i < 0 {
				return nil, errors.New("torrentmeta: file tree leaf missing/invalid length")
			}
			path, err := sanitizePathSegments(prefix)
			if err != nil {
				return nil, err
			}
			leaf := v2Leaf{path: path, size: lengthVal.i}
			if rootVal, ok := child.dict["pieces root"]; ok {
				if rootVal.kind != kBytes || len(rootVal.b) != 32 {
					return nil, errors.New("torrentmeta: invalid pieces root (expected 32 bytes)")
				}
				leaf.root = hex.EncodeToString(rootVal.b)
			}
			out = append(out, leaf)
			continue
		}
		nextPrefix := make([]string, 0, len(prefix)+1)
		nextPrefix = append(nextPrefix, prefix...)
		nextPrefix = append(nextPrefix, key)
		if _, err := sanitizeSegment(key); err != nil {
			return nil, fmt.Errorf("torrentmeta: file tree: %w", err)
		}
		children, err := walkFileTreeLeaves(child, nextPrefix)
		if err != nil {
			return nil, err
		}
		out = append(out, children...)
	}
	return out, nil
}

// sanitizeSegment rejects a single path/name component that could be used
// for traversal or that embeds a path separator (DG-03: an untrusted
// arr-uploaded .torrent must never be able to make a downstream consumer of
// TorrentMeta.Files write or reference a path outside its intended root).
func sanitizeSegment(seg string) (string, error) {
	seg = strings.TrimSpace(seg)
	switch {
	case seg == "":
		return "", errors.New("empty path segment")
	case seg == ".", seg == "..":
		return "", fmt.Errorf("invalid path segment %q", seg)
	case strings.ContainsRune(seg, '/'), strings.ContainsRune(seg, '\\'), strings.ContainsRune(seg, 0):
		return "", fmt.Errorf("invalid path segment %q", seg)
	}
	return seg, nil
}

// sanitizePathSegments validates and joins a full path's segments.
func sanitizePathSegments(segs []string) (string, error) {
	if len(segs) == 0 {
		return "", errors.New("empty file path")
	}
	clean := make([]string, 0, len(segs))
	for _, seg := range segs {
		s, err := sanitizeSegment(seg)
		if err != nil {
			return "", err
		}
		clean = append(clean, s)
	}
	return strings.Join(clean, "/"), nil
}
