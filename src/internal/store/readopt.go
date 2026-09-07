package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	MaxStrmBytes         = 4 << 10
	MaxReAdoptionEntries = 1_000_000
	maxStablePartBytes   = 255
)

var ErrReAdoptionLimit = errors.New("re-adoption scan limit reached")

type StableStrmKind string

const (
	StableStrmStream StableStrmKind = "stream"
	StableStrmDAV    StableStrmKind = "dav"
)

// StableStrmRef is the URL-free identity carried by a permanent DH .strm.
type StableStrmRef struct {
	Kind     StableStrmKind
	ItemID   string
	FileID   string
	Category string
	Release  string
	Filename string
}

func (r StableStrmRef) key() string {
	if r.Kind == StableStrmStream {
		return "stream\x00" + r.ItemID + "\x00" + r.FileID
	}
	return "dav\x00" + r.Category + "\x00" + r.Release + "\x00" + r.Filename
}

func validStablePart(v string) bool {
	if v == "" || len(v) > maxStablePartBytes || v == "." || v == ".." ||
		strings.ContainsAny(v, "/\\\x00") || !utf8.ValidString(v) {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func stableParts(escapedPath string, want int) ([]string, bool) {
	parts := strings.Split(escapedPath, "/")
	if len(parts) != want+1 || parts[0] != "" {
		return nil, false
	}
	decoded := make([]string, want)
	for i := range decoded {
		v, err := url.PathUnescape(parts[i+1])
		if err != nil || !validStablePart(v) {
			return nil, false
		}
		decoded[i] = v
	}
	return decoded, true
}

func validStreamToken(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// ParseStableStrmURL strictly parses the two permanent URL forms DH emits.
// The host is intentionally not pinned so a restored library can be audited
// after a server rename; no network request is ever made from the result.
func ParseStableStrmURL(raw string) (StableStrmRef, error) {
	if raw == "" || len(raw) > MaxStrmBytes || strings.TrimSpace(raw) != raw {
		return StableStrmRef{}, errors.New("invalid stable URL length or whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Fragment != "" || u.Opaque != "" || u.ForceQuery {
		return StableStrmRef{}, errors.New("invalid stable URL")
	}
	escaped := u.EscapedPath()
	if strings.HasPrefix(escaped, "/stream/") {
		parts, ok := stableParts(escaped, 3)
		if !ok || parts[0] != "stream" {
			return StableStrmRef{}, errors.New("invalid stream path")
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return StableStrmRef{}, errors.New("invalid stream query")
		}
		for key, values := range q {
			if key != "tok" || len(values) != 1 || !validStreamToken(values[0]) {
				return StableStrmRef{}, errors.New("invalid stream query")
			}
		}
		return StableStrmRef{Kind: StableStrmStream, ItemID: parts[1], FileID: parts[2]}, nil
	}
	if strings.HasPrefix(escaped, "/dav/") {
		parts, ok := stableParts(escaped, 4)
		if !ok || parts[0] != "dav" || u.RawQuery != "" || !strings.HasSuffix(strings.ToLower(parts[3]), ".mkv") {
			return StableStrmRef{}, errors.New("invalid dav path")
		}
		return StableStrmRef{Kind: StableStrmDAV, Category: parts[1], Release: parts[2], Filename: parts[3]}, nil
	}
	return StableStrmRef{}, errors.New("unknown stable URL path")
}

func readStableStrm(root *os.Root, path string) (StableStrmRef, error) {
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return StableStrmRef{}, errors.New(".strm is not a regular file")
	}
	f, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return StableStrmRef{}, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return StableStrmRef{}, errors.New(".strm changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxStrmBytes+1))
	if err != nil {
		return StableStrmRef{}, err
	}
	if len(b) == 0 || len(b) > MaxStrmBytes {
		return StableStrmRef{}, errors.New("invalid .strm size")
	}
	raw := string(b)
	raw = strings.TrimSuffix(raw, "\r\n")
	raw = strings.TrimSuffix(raw, "\n")
	if strings.ContainsAny(raw, "\r\n") {
		return StableStrmRef{}, errors.New("multiple .strm lines")
	}
	return ParseStableStrmURL(raw)
}

type ReAdoptionFinding struct {
	Kind   string
	Path   string
	ItemID string
	Fix    string
}

type ReAdoptionResult struct {
	Entries   int
	StrmFiles int
	Verified  int
	Orphans   int
	Ghosts    int
	Malformed int
	Ambiguous int
	Partial   int
	Findings  []ReAdoptionFinding
}

type reAdoptionBlob struct {
	itemID   string
	consumed bool
	ready    bool
}

func sanitizedItemID(value string) string {
	lower := strings.ToLower(value)
	if validStablePart(value) && !strings.Contains(lower, "tok=") && !strings.Contains(lower, "authorization") {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("redacted:%x", sum[:6])
}

func sanitizedRelativePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "unavailable"
	}
	rel = filepath.ToSlash(rel)
	lower := strings.ToLower(rel)
	if strings.Contains(lower, "tok=") || strings.Contains(lower, "://") || strings.ContainsAny(rel, "\r\n\x00") {
		sum := sha256.Sum256([]byte(rel))
		return fmt.Sprintf("redacted:%x", sum[:6])
	}
	return rel
}

func walkStrmFiles(ctx context.Context, root string, maxEntries int, visit func(string, *os.Root, string)) (int, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("library root must be a real directory")
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return 0, errors.New("open confined library root")
	}
	defer func() { _ = confined.Close() }()
	entries := 0
	stack := []string{"."}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return entries, err
		}
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		f, err := confined.Open(dir)
		if err != nil {
			return entries, err
		}
		var children []os.DirEntry
		for {
			batch, readErr := f.ReadDir(256)
			children = append(children, batch...)
			entries += len(batch)
			if entries > maxEntries {
				_ = f.Close()
				return entries, ErrReAdoptionLimit
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				_ = f.Close()
				return entries, readErr
			}
			if err := ctx.Err(); err != nil {
				_ = f.Close()
				return entries, err
			}
		}
		_ = f.Close()
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for i := len(children) - 1; i >= 0; i-- {
			if err := ctx.Err(); err != nil {
				return entries, err
			}
			child := children[i]
			if child.Type()&os.ModeSymlink != 0 {
				continue
			}
			path := filepath.Join(dir, child.Name())
			if child.IsDir() {
				stack = append(stack, path)
				continue
			}
			if child.Type().IsRegular() && strings.EqualFold(filepath.Ext(child.Name()), ".strm") {
				visit(filepath.Join(root, path), confined, path)
			}
		}
	}
	return entries, nil
}

func (s *Store) loadReAdoptionDB(ctx context.Context, limit int) (map[string]bool, map[string][]reAdoptionBlob, []ReAdoptionFinding, error) {
	items := make(map[string]bool)
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM items ORDER BY id LIMIT ?", limit+1)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, nil, nil, err
		}
		items[id] = true
		if len(items) > limit {
			_ = rows.Close()
			return nil, nil, nil, ErrReAdoptionLimit
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, nil, err
	}

	blobs := make(map[string][]reAdoptionBlob)
	var findings []ReAdoptionFinding
	rows, err = s.db.QueryContext(ctx,
		"SELECT b.item_id, b.url, i.download_consumed, i.state "+
			"FROM strm_blobs b JOIN items i ON i.id=b.item_id "+
			"ORDER BY b.item_id, b.file_index LIMIT ?", limit+1)
	if err != nil {
		return nil, nil, nil, err
	}
	count := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			_ = rows.Close()
			return nil, nil, nil, err
		}
		var itemID, raw, state string
		var consumed int
		if err := rows.Scan(&itemID, &raw, &consumed, &state); err != nil {
			_ = rows.Close()
			return nil, nil, nil, err
		}
		count++
		if count > limit {
			_ = rows.Close()
			return nil, nil, nil, ErrReAdoptionLimit
		}
		ref, err := ParseStableStrmURL(raw)
		if err != nil {
			findings = append(findings, ReAdoptionFinding{Kind: "malformed_db", ItemID: sanitizedItemID(itemID), Fix: "restore_backup"})
			continue
		}
		if ref.Kind == StableStrmStream && ref.ItemID != itemID {
			findings = append(findings, ReAdoptionFinding{Kind: "contradictory_db", ItemID: sanitizedItemID(itemID), Fix: "restore_backup"})
			continue
		}
		blobs[ref.key()] = append(blobs[ref.key()], reAdoptionBlob{itemID: itemID, consumed: consumed != 0, ready: state == string(StateReady)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, nil, err
	}
	return items, blobs, findings, nil
}

// ReAdoptionScan compares a bounded library walk with existing item/blob
// authority. It never changes the database or library: missing source facts
// cannot be reconstructed from an ID-only stable URL without guessing.
func (s *Store) ReAdoptionScan(ctx context.Context, root string, maxEntries int) (ReAdoptionResult, error) {
	var res ReAdoptionResult
	if maxEntries <= 0 || maxEntries > MaxReAdoptionEntries {
		return res, errors.New("invalid re-adoption entry limit")
	}
	items, blobs, dbFindings, err := s.loadReAdoptionDB(ctx, maxEntries)
	if err != nil {
		return res, err
	}
	res.Findings = append(res.Findings, dbFindings...)
	res.Malformed += len(dbFindings)

	type scannedRef struct {
		ref   StableStrmRef
		paths []string
	}
	scanned := make(map[string]*scannedRef)
	entries, err := walkStrmFiles(ctx, root, maxEntries, func(path string, confined *os.Root, relative string) {
		res.StrmFiles++
		ref, parseErr := readStableStrm(confined, relative)
		if parseErr != nil {
			res.Malformed++
			res.Findings = append(res.Findings, ReAdoptionFinding{Kind: "malformed", Path: sanitizedRelativePath(root, path), Fix: "inspect_or_remove"})
			return
		}
		key := ref.key()
		if scanned[key] == nil {
			scanned[key] = &scannedRef{ref: ref}
		}
		scanned[key].paths = append(scanned[key].paths, sanitizedRelativePath(root, path))
	})
	res.Entries = entries
	if err != nil {
		return res, err
	}

	keys := make([]string, 0, len(scanned))
	for key := range scanned {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := scanned[key]
		if len(entry.paths) != 1 || len(blobs[key]) > 1 {
			res.Ambiguous += len(entry.paths)
			for _, path := range entry.paths {
				res.Findings = append(res.Findings, ReAdoptionFinding{Kind: "ambiguous", Path: path, ItemID: sanitizedItemID(entry.ref.ItemID), Fix: "deduplicate_or_restore_backup"})
			}
			continue
		}
		if records := blobs[key]; len(records) == 1 {
			res.Verified++
			continue
		}
		if entry.ref.Kind == StableStrmStream && items[entry.ref.ItemID] {
			res.Partial++
			res.Findings = append(res.Findings, ReAdoptionFinding{Kind: "partial", Path: entry.paths[0], ItemID: sanitizedItemID(entry.ref.ItemID), Fix: "restore_backup"})
			continue
		}
		res.Orphans++
		res.Findings = append(res.Findings, ReAdoptionFinding{Kind: "orphan", Path: entry.paths[0], ItemID: sanitizedItemID(entry.ref.ItemID), Fix: "restore_backup"})
	}

	for key, records := range blobs {
		if _, ok := scanned[key]; ok {
			continue
		}
		for _, record := range records {
			if record.ready && record.consumed {
				res.Ghosts++
				res.Findings = append(res.Findings, ReAdoptionFinding{Kind: "ghost", ItemID: sanitizedItemID(record.itemID), Fix: "restore_library_entry"})
			}
		}
	}
	sort.Slice(res.Findings, func(i, j int) bool {
		a, b := res.Findings[i], res.Findings[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ItemID != b.ItemID {
			return a.ItemID < b.ItemID
		}
		return a.Path < b.Path
	})
	return res, nil
}
