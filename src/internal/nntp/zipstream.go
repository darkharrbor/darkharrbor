package nntp

import (
	"compress/flate"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
)

// ZIPEntry describes one video file inside a ZIP archive stored as NNTP segments.
// JSON-serialised and stored on item.ZIPManifest so StreamZIPEntry needs no
// directory-parsing NNTP calls at play time — only segment fetches.
type ZIPEntry struct {
	EntryIdx       int          `json:"entry_idx"`
	NZBFileIdx     int          `json:"nzb_file_idx"`
	Filename       string       `json:"filename"`
	LocalHeaderOff int64        `json:"local_header_off"`
	DataOff        int64        `json:"data_off"`
	CompressedSize int64        `json:"compressed_size"`
	Uncompressed   int64        `json:"uncompressed_size"`
	Method         uint16       `json:"method"` // 0=store, 8=deflate
	Segments       []NZBSegment `json:"segments"`
}

// MarshalZIPManifest serialises a manifest slice to JSON for DB storage.
func MarshalZIPManifest(entries []ZIPEntry) (string, error) {
	b, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UnmarshalZIPManifest deserialises a manifest from DB storage.
func UnmarshalZIPManifest(s string) ([]ZIPEntry, error) {
	var m []ZIPEntry
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// BuildZIPManifest scans nzbFiles for a ZIP archive, reads its central directory
// from NNTP, enumerates video entries, and returns a ZIPEntry slice sorted by
// filename plus the total uncompressed video size.
func (p *NNTPProvider) BuildZIPManifest(ctx context.Context, nzbFiles []NZBFile) ([]ZIPEntry, int64, error) {
	// Find ZIP file(s) in the NZB.
	type zipCandidate struct {
		file    NZBFile
		origIdx int
	}
	var candidates []zipCandidate
	for i, f := range nzbFiles {
		lower := strings.ToLower(f.Subject)
		if strings.Contains(lower, ".zip") && !strings.Contains(lower, ".par2") {
			candidates = append(candidates, zipCandidate{f, i})
		}
	}
	if len(candidates) == 0 {
		return nil, 0, fmt.Errorf("zip: no ZIP files found in NZB (%d files)", len(nzbFiles))
	}
	zc := candidates[0]

	src, err := p.archiveByteSource(ctx, zc.origIdx, zc.file.Segments)
	if err != nil {
		return nil, 0, fmt.Errorf("zip: byte source: %w", err)
	}
	parsed, totalUncompressed, err := archiveparser.ParseZIPVideoEntries(ctx, src)
	if err != nil {
		return nil, 0, err
	}
	entries := make([]ZIPEntry, len(parsed))
	for i, entry := range parsed {
		entries[i] = ZIPEntry{
			EntryIdx:       i,
			NZBFileIdx:     zc.origIdx,
			Filename:       entry.Filename,
			LocalHeaderOff: entry.LocalHeaderOff,
			DataOff:        entry.DataOff,
			CompressedSize: entry.CompressedSize,
			Uncompressed:   entry.Uncompressed,
			Method:         entry.Method,
			Segments:       zc.file.Segments,
		}
	}
	return entries, totalUncompressed, nil
}

// BuildZIPManifestForItem associates resolver-time ZIP directory/header fetch
// failures with the item whose preparation reached the final rung (NS-1.3).
func (p *NNTPProvider) BuildZIPManifestForItem(ctx context.Context, itemID string, nzbFiles []NZBFile) ([]ZIPEntry, int64, error) {
	return p.BuildZIPManifest(withAccountingItemID(ctx, itemID), nzbFiles)
}

// StreamZIPEntry streams one video entry from a ZIP manifest to the HTTP response.
func (p *NNTPProvider) StreamZIPEntry(ctx context.Context, manifestJSON string, entryIdx int, w http.ResponseWriter, rangeHeader, itemID, contentKey string) error {
	ctx = withAccountingItemID(ctx, itemID)
	manifest, err := UnmarshalZIPManifest(manifestJSON)
	if err != nil {
		return fmt.Errorf("zip: unmarshal manifest: %w", err)
	}
	if entryIdx < 0 || entryIdx >= len(manifest) {
		return fmt.Errorf("zip: entry %d out of range (len=%d)", entryIdx, len(manifest))
	}
	e := manifest[entryIdx]
	switch e.Method {
	case 0:
		return p.streamZIPStored(ctx, e, w, rangeHeader, itemID, contentKey)
	case 8:
		return p.streamZIPDeflated(ctx, e, w, rangeHeader, itemID, contentKey)
	default:
		return fmt.Errorf("zip: unsupported compression method %d for %q", e.Method, e.Filename)
	}
}

func (p *NNTPProvider) streamZIPStored(ctx context.Context, e ZIPEntry, w http.ResponseWriter, rangeHeader, itemID, contentKey string) error {
	total := e.CompressedSize
	start, end := parseRARRangeHeader(rangeHeader, total)
	if start > end || start >= total {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return nil
	}
	if end >= total {
		end = total - 1
	}
	w.Header().Set("Content-Type", zipMIME(e.Filename))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if rangeHeader != "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	nf := NZBFile{Segments: e.Segments}
	return p.streamNZBByteRange(ctx, nf, e.NZBFileIdx, e.DataOff+start, e.DataOff+end, w, flush, itemID, contentKey)
}

func (p *NNTPProvider) streamZIPDeflated(ctx context.Context, e ZIPEntry, w http.ResponseWriter, rangeHeader, itemID, contentKey string) error {
	total := e.Uncompressed
	start, end := parseRARRangeHeader(rangeHeader, total)
	if start > end || start >= total {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return nil
	}
	if end >= total {
		end = total - 1
	}
	w.Header().Set("Content-Type", zipMIME(e.Filename))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if rangeHeader != "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		nf := NZBFile{Segments: e.Segments}
		err := p.streamNZBByteRange(ctx, nf, e.NZBFileIdx,
			e.DataOff, e.DataOff+e.CompressedSize-1, pw, func() {}, itemID, contentKey)
		pw.CloseWithError(err)
		errCh <- err
	}()
	defer pr.Close()

	inflater := flate.NewReader(pr)
	defer inflater.Close()

	if start > 0 {
		if _, err := io.CopyN(io.Discard, inflater, start); err != nil {
			return fmt.Errorf("zip: deflate seek: %w", err)
		}
	}
	if _, err := io.CopyN(w, inflater, end-start+1); err != nil && err != io.EOF {
		return fmt.Errorf("zip: deflate stream: %w", err)
	}
	return nil
}

// streamNZBByteRange streams absStart..absEnd bytes from an NZBFile's segments.
func (p *NNTPProvider) streamNZBByteRange(ctx context.Context, f NZBFile, fileIdx int, absStart, absEnd int64, w io.Writer, flush func(), itemID, contentKey string) error {
	contentKey = SegmentOffsetKey(contentKey, f.Segments)
	offset := int64(0)
	for segIdx, seg := range f.Segments {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data, err := p.fetchRARSegment(ctx, seg, itemID, contentKey, fileIdx, segIdx)
		if err != nil {
			return fmt.Errorf("zip: seg %d: %w", seg.Number, err)
		}
		segEnd := offset + int64(len(data)) - 1
		if segEnd < absStart {
			offset += int64(len(data))
			continue
		}
		if offset > absEnd {
			break
		}
		start := int64(0)
		if absStart > offset {
			start = absStart - offset
		}
		end := int64(len(data)) - 1
		if absEnd < segEnd {
			end = absEnd - offset
		}
		if start <= end {
			if _, err := w.Write(data[start : end+1]); err != nil {
				return fmt.Errorf("zip: write: %w", err)
			}
			if flush != nil {
				flush()
			}
		}
		offset += int64(len(data))
		if offset > absEnd {
			break
		}
	}
	return nil
}

// zipMIME returns a video MIME type for a filename.
func zipMIME(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".avi":
		return "video/x-msvideo"
	case ".ts", ".m2ts":
		return "video/mp2t"
	case ".webm":
		return "video/webm"
	default:
		return "video/x-matroska"
	}
}
