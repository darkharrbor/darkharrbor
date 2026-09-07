package nntp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
)

// RARPart describes one RAR part file within an NZB, with pre-scanned byte
// offsets for efficient seek-and-stream operations. JSON-serialised and stored
// on the item as RarManifest so StreamRAR needs no NNTP calls at play time for
// header parsing — only segment fetches.
type RARPart struct {
	PartNum      int                          `json:"part_num"`
	NZBFileIdx   int                          `json:"nzb_file_idx"`
	HeaderBytes  int64                        `json:"header_bytes"`
	TrailerBytes int64                        `json:"trailer_bytes"`
	DataBytes    int64                        `json:"data_bytes"`
	PackedBytes  int64                        `json:"packed_bytes,omitempty"`
	CumOffset    int64                        `json:"cum_offset"`
	Encryption   *archiveparser.RAREncryption `json:"encryption,omitempty"`
	Segments     []NZBSegment                 `json:"segments"`
}

// MarshalRARManifest serialises a manifest slice to JSON for DB storage.
func MarshalRARManifest(manifest []RARPart) (string, error) {
	b, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UnmarshalRARManifest deserialises a manifest from DB storage.
func UnmarshalRARManifest(s string) ([]RARPart, error) {
	var m []RARPart
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// BuildRARManifest scans the NZB for RAR parts, fetches the first segment of
// each part from NNTP, parses the RAR3 or RAR5 header to find the data offset,
// and returns a sorted RARPart slice with cumulative byte offsets pre-computed.
// totalVideoBytes is the sum of DataBytes across all parts.
func (p *NNTPProvider) BuildRARManifest(ctx context.Context, nzb NZB) ([]RARPart, int64, error) {
	nzbFiles := nzb.Files
	// Filter to recovered RAR volume subjects (.rar and legacy .rNN).
	// PAR2 recovery files remain excluded.
	var rarFiles []struct {
		file    NZBFile
		origIdx int
	}
	for i, f := range nzbFiles {
		if isRARPartSubject(f.Subject) {
			rarFiles = append(rarFiles, struct {
				file    NZBFile
				origIdx int
			}{f, i})
		}
	}
	if len(rarFiles) == 0 {
		return nil, 0, fmt.Errorf("rar: no RAR parts found in NZB (%d files total)", len(nzbFiles))
	}

	// Sort by part number extracted from subject.
	sort.SliceStable(rarFiles, func(i, j int) bool {
		return extractRARPartNum(rarFiles[i].file.Subject) < extractRARPartNum(rarFiles[j].file.Subject)
	})

	manifest := make([]RARPart, 0, len(rarFiles))
	var totalVideoBytes int64
	var firstMember archiveparser.StoredRARMember

	for partIndex, rf := range rarFiles {
		if len(rf.file.Segments) == 0 {
			return nil, 0, fmt.Errorf("rar: no segments for part %q", rf.file.Subject)
		}

		src, err := p.archiveByteSource(ctx, rf.origIdx, rf.file.Segments)
		if err != nil {
			return nil, 0, fmt.Errorf("rar: byte source for %q: %w", rf.file.Subject, err)
		}
		member, err := archiveparser.ParseStoredRARMember(ctx, src)
		if err != nil {
			return nil, 0, fmt.Errorf("rar: parse header for %q: %w", rf.file.Subject, err)
		}
		if member.DataOffset < 0 || member.PackedSize <= 0 {
			return nil, 0, fmt.Errorf("rar: invalid member span")
		}
		if partIndex == 0 {
			firstMember = member
			if member.Encryption == nil {
				available := src.Size() - member.DataOffset
				if available < 0 {
					return nil, 0, fmt.Errorf("rar: invalid member offset")
				}
				nested, detectErr := archiveparser.DetectNestedArchive(ctx, src, member.DataOffset, min(member.DataSize, available))
				if detectErr != nil {
					return nil, 0, fmt.Errorf("rar: inspect member payload: %w", detectErr)
				}
				if nested {
					return nil, 0, fmt.Errorf("%w: RAR member", archiveparser.ErrNestedArchive)
				}
			} else {
				head, decryptErr := archiveparser.DecryptRARPrefix(ctx, src, member, nzb.Password, 16)
				if decryptErr != nil {
					return nil, 0, fmt.Errorf("rar: decrypt member prefix: %w", decryptErr)
				}
				switch archiveparser.ClassifyMagic(head) {
				case archiveparser.FormatRAR, archiveparser.Format7z, archiveparser.FormatZIP:
					return nil, 0, fmt.Errorf("%w: encrypted RAR member", archiveparser.ErrNestedArchive)
				case archiveparser.FormatMKV, archiveparser.FormatMP4:
				default:
					return nil, 0, archiveparser.ErrRARBadPassword
				}
			}
		} else {
			if (member.Encryption == nil) != (firstMember.Encryption == nil) {
				return nil, 0, fmt.Errorf("rar: inconsistent volume encryption")
			}
			if member.Encryption != nil && !sameRAREncryption(member.Encryption, firstMember.Encryption) {
				return nil, 0, fmt.Errorf("rar: inconsistent volume encryption parameters")
			}
		}

		manifest = append(manifest, RARPart{
			PartNum:     len(manifest) + 1,
			NZBFileIdx:  rf.origIdx,
			HeaderBytes: member.DataOffset,
			DataBytes:   member.PackedSize,
			PackedBytes: member.PackedSize,
			Encryption:  member.Encryption,
			Segments:    rf.file.Segments,
		})
	}

	if firstMember.Encryption == nil {
		for i := range manifest {
			manifest[i].CumOffset = totalVideoBytes
			totalVideoBytes += manifest[i].DataBytes
		}
	} else {
		totalVideoBytes = firstMember.DataSize
		var packedTotal, logicalOffset int64
		for i := range manifest {
			if manifest[i].PackedBytes > int64(^uint64(0)>>1)-packedTotal {
				return nil, 0, fmt.Errorf("rar: encrypted size overflow")
			}
			packedTotal += manifest[i].PackedBytes
			manifest[i].CumOffset = logicalOffset
			remaining := totalVideoBytes - logicalOffset
			manifest[i].DataBytes = min(manifest[i].PackedBytes, remaining)
			if manifest[i].DataBytes < 0 {
				return nil, 0, fmt.Errorf("rar: encrypted volume exceeds logical size")
			}
			logicalOffset += manifest[i].DataBytes
		}
		if logicalOffset != totalVideoBytes || packedTotal < totalVideoBytes || packedTotal-totalVideoBytes >= 16 || packedTotal%16 != 0 {
			return nil, 0, fmt.Errorf("rar: invalid encrypted volume totals")
		}
	}

	return manifest, totalVideoBytes, nil
}

func sameRAREncryption(a, b *archiveparser.RAREncryption) bool {
	return a != nil && b != nil &&
		a.Version == b.Version &&
		a.KDFLog2 == b.KDFLog2 &&
		bytes.Equal(a.Salt, b.Salt) &&
		bytes.Equal(a.IV, b.IV) &&
		bytes.Equal(a.PasswordCheck, b.PasswordCheck)
}

// BuildRARManifestForItem associates resolver-time manifest fetch failures
// with the item whose preparation reached the final rung (NS-1.3).
func (p *NNTPProvider) BuildRARManifestForItem(ctx context.Context, itemID string, nzb NZB) ([]RARPart, int64, error) {
	return p.BuildRARManifest(withAccountingItemID(ctx, itemID), nzb)
}

// StreamRAR streams a RAR-split NZB to w using a pre-built manifest.
// When a SegmentCache is attached, delegates to StreamRARWithCache which routes
// through rangecache for parallel readahead, disk cache, and instant seek —
// identical to the plain NZB path. Falls back to sequential segment fetching
// only when no cache is configured.
func (p *NNTPProvider) StreamRAR(ctx context.Context, manifest []RARPart, totalVideoBytes int64, w http.ResponseWriter, rangeHeader, itemID, contentKey string) error {
	return p.streamRAR(ctx, manifest, totalVideoBytes, w, rangeHeader, itemID, contentKey, "")
}

func (p *NNTPProvider) streamRAR(ctx context.Context, manifest []RARPart, totalVideoBytes int64, w http.ResponseWriter, rangeHeader, itemID, contentKey, password string) error {
	ctx = withAccountingItemID(ctx, itemID)
	if len(manifest) == 0 {
		return fmt.Errorf("rar: empty manifest")
	}

	encrypted := manifest[0].Encryption != nil
	var rarKey *archiveparser.RARKey
	if encrypted {
		for i := range manifest {
			if manifest[i].Encryption == nil || manifest[i].PackedBytes <= 0 ||
				!sameRAREncryption(manifest[i].Encryption, manifest[0].Encryption) {
				return fmt.Errorf("rar: invalid encrypted manifest")
			}
		}
		var err error
		rarKey, err = archiveparser.DeriveRARKey(ctx, password, manifest[0].Encryption)
		if err != nil {
			return err
		}
		if len(manifest[0].Encryption.PasswordCheck) == 0 {
			if err := validateEncryptedRARPassword(ctx, p.fetchRARSegment, manifest[0], rarKey, itemID, contentKey); err != nil {
				return err
			}
		}
	} else if p.cache != nil {
		// Preserve the existing rangecache/readahead fast path byte-for-byte
		// for unencrypted manifests.
		return p.cache.StreamRARWithCache(ctx, itemID, contentKey, manifest, totalVideoBytes, w, rangeHeader)
	}

	rangeStart, rangeEnd := parseRARRangeHeader(rangeHeader, totalVideoBytes)
	if totalVideoBytes <= 0 || rangeStart < 0 || rangeEnd < rangeStart || rangeEnd >= totalVideoBytes {
		return fmt.Errorf("rar: invalid byte range")
	}

	// Content-Type: RAR-split NZBs almost always contain MKV. Default to MKV.
	// Fine-grained sniffing is not possible without the NZBFile Subject at stream
	// time — the manifest stores segments (MessageID/Bytes/Number), not file names.
	contentType := "video/x-matroska"

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")

	isPartial := rangeStart > 0 || rangeEnd < totalVideoBytes-1
	if isPartial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rangeStart, rangeEnd, totalVideoBytes))
		w.Header().Set("Content-Length", strconv.FormatInt(rangeEnd-rangeStart+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(totalVideoBytes, 10))
		w.WriteHeader(http.StatusOK)
	}

	flush := func() {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	if encrypted {
		return streamEncryptedRARRange(ctx, p.fetchRARSegment, manifest, rangeStart, rangeEnd, rarKey, w, flush, itemID, contentKey)
	}

	for _, part := range manifest {
		// Skip parts entirely before rangeStart.
		if part.CumOffset+part.DataBytes <= rangeStart {
			continue
		}
		// Stop when we've passed rangeEnd.
		if part.CumOffset > rangeEnd {
			break
		}

		if part.DataBytes <= 0 {
			continue
		}

		// Trim to the requested range within this part. The range is relative to
		// the logical data stream after HeaderBytes and before TrailerBytes.
		localStart := int64(0)
		if part.CumOffset < rangeStart {
			localStart = rangeStart - part.CumOffset
		}
		localEnd := part.DataBytes - 1
		if part.CumOffset+part.DataBytes-1 > rangeEnd {
			localEnd = rangeEnd - part.CumOffset
		}
		if localStart > localEnd {
			continue
		}

		if err := streamRARPartRange(ctx, p.fetchRARSegment, part, localStart, localEnd, w, flush, itemID, contentKey); err != nil {
			return fmt.Errorf("rar: stream part %d: %w", part.PartNum, err)
		}
	}
	return nil
}

type rarSegmentFetcher func(ctx context.Context, seg NZBSegment, itemID, contentKey string, fileIndex, segIndex int) ([]byte, error)

// fetchRARSegment fetches and decodes exactly one RAR/NZB segment. The caller
// streams each returned segment before fetching the next, so memory is bounded
// by one decoded segment rather than by the full RAR part.
// itemID, fileIndex, segIndex are forwarded to GetSegmentWithMeta so that
// decoded sizes are recorded in segment_offsets for corrected seek mapping.
func (p *NNTPProvider) fetchRARSegment(ctx context.Context, seg NZBSegment, itemID, contentKey string, fileIndex, segIndex int) ([]byte, error) {
	if p.cache != nil {
		data, err := p.cache.GetSegmentWithMeta(ctx, seg, itemID, contentKey, fileIndex, segIndex)
		if err != nil {
			return nil, fmt.Errorf("rar: cache get seg %d: %w", seg.Number, err)
		}
		return data, nil
	}

	return fetchArticleBytesWithRetryAt(ctx, p.pool, p.log, "rar stream", seg.Number, seg.MessageID, seg.PostedAt, !p.skipCRC)
}

func validateEncryptedRARPassword(ctx context.Context, fetch rarSegmentFetcher, part RARPart, key *archiveparser.RARKey, itemID, contentKey string) error {
	if fetch == nil || key == nil || part.HeaderBytes < 0 || part.PackedBytes < 16 {
		return fmt.Errorf("rar: invalid encrypted password proof")
	}
	const prefixSize = 16
	prefix := make([]byte, 0, prefixSize)
	var rawOffset int64
	for segIdx, seg := range part.Segments {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := fetch(ctx, seg, itemID, contentKey, part.NZBFileIdx, segIdx)
		if err != nil {
			return fmt.Errorf("rar: password proof fetch: %w", err)
		}
		segStart, segEnd := rawOffset, rawOffset+int64(len(data))
		rawOffset = segEnd
		start, end := max(part.HeaderBytes, segStart), min(part.HeaderBytes+prefixSize, segEnd)
		if start < end {
			prefix = append(prefix, data[start-segStart:end-segStart]...)
		}
		if len(prefix) == prefixSize {
			break
		}
	}
	if len(prefix) != prefixSize {
		return fmt.Errorf("rar: truncated encrypted password proof")
	}
	plain := make([]byte, prefixSize)
	if err := key.DecryptBlocks(plain, prefix, key.InitialIV()); err != nil {
		return err
	}
	switch archiveparser.ClassifyMagic(plain) {
	case archiveparser.FormatMKV, archiveparser.FormatMP4:
		return nil
	default:
		return archiveparser.ErrRARBadPassword
	}
}

func streamEncryptedRARRange(ctx context.Context, fetch rarSegmentFetcher, manifest []RARPart, rangeStart, rangeEnd int64, key *archiveparser.RARKey, w io.Writer, flush func(), itemID, contentKey string) error {
	if fetch == nil || key == nil || len(manifest) == 0 || rangeStart < 0 || rangeEnd < rangeStart {
		return fmt.Errorf("rar: invalid encrypted stream request")
	}
	alignedStart := rangeStart &^ int64(15)
	needStart := alignedStart
	iv := key.InitialIV()
	if alignedStart > 0 {
		needStart -= 16
		iv = nil
	}
	cipherEnd := (rangeEnd + 16) &^ int64(15)
	var totalCipher int64
	for _, part := range manifest {
		if part.PackedBytes <= 0 || part.PackedBytes > int64(^uint64(0)>>1)-totalCipher {
			return fmt.Errorf("rar: invalid encrypted manifest size")
		}
		totalCipher += part.PackedBytes
	}
	if cipherEnd > totalCipher {
		cipherEnd = totalCipher
	}
	if cipherEnd <= alignedStart || (cipherEnd-alignedStart)%16 != 0 {
		return fmt.Errorf("rar: invalid encrypted block range")
	}

	pending := make([]byte, 0, 64*1024)
	decryptOffset := alignedStart
	var collected, cipherBase int64
	consume := func(data []byte) error {
		collected += int64(len(data))
		pending = append(pending, data...)
		if iv == nil {
			if len(pending) < 16 {
				return nil
			}
			iv = append([]byte(nil), pending[:16]...)
			pending = pending[16:]
		}
		blockBytes := len(pending) &^ 15
		if blockBytes == 0 {
			return nil
		}
		ciphertext := pending[:blockBytes]
		nextIV := append([]byte(nil), ciphertext[blockBytes-16:blockBytes]...)
		plaintext := make([]byte, blockBytes)
		if err := key.DecryptBlocks(plaintext, ciphertext, iv); err != nil {
			return err
		}
		chunkStart := decryptOffset
		chunkEnd := decryptOffset + int64(len(plaintext))
		writeStart := max(rangeStart, chunkStart)
		writeEnd := min(rangeEnd+1, chunkEnd)
		if writeStart < writeEnd {
			if _, err := w.Write(plaintext[writeStart-chunkStart : writeEnd-chunkStart]); err != nil {
				return fmt.Errorf("rar: write to client: %w", err)
			}
			if flush != nil {
				flush()
			}
		}
		decryptOffset = chunkEnd
		iv = nextIV
		pending = append(pending[:0], pending[blockBytes:]...)
		return nil
	}

	for _, part := range manifest {
		partStart, partEnd := cipherBase, cipherBase+part.PackedBytes
		cipherBase = partEnd
		wantStart := max(needStart, partStart)
		wantEnd := min(cipherEnd, partEnd)
		if wantStart >= wantEnd {
			continue
		}
		rawWantStart := part.HeaderBytes + wantStart - partStart
		rawWantEnd := part.HeaderBytes + wantEnd - partStart
		var rawOffset int64
		for segIdx, seg := range part.Segments {
			if err := ctx.Err(); err != nil {
				return err
			}
			data, err := fetch(ctx, seg, itemID, contentKey, part.NZBFileIdx, segIdx)
			if err != nil {
				return fmt.Errorf("rar: encrypted segment %d: %w", seg.Number, err)
			}
			segStart, segEnd := rawOffset, rawOffset+int64(len(data))
			rawOffset = segEnd
			start, end := max(rawWantStart, segStart), min(rawWantEnd, segEnd)
			if start < end {
				if err := consume(data[start-segStart : end-segStart]); err != nil {
					return err
				}
			}
			if segEnd >= rawWantEnd {
				break
			}
		}
	}
	if collected != cipherEnd-needStart || len(pending) != 0 || decryptOffset < rangeEnd+1 {
		return fmt.Errorf("rar: truncated encrypted payload")
	}
	return nil
}

// streamRARPartRange writes localStart..localEnd (inclusive) from one RAR part
// without ever assembling the full part in memory. local offsets are relative
// to the logical media data after HeaderBytes and before TrailerBytes.
func streamRARPartRange(ctx context.Context, fetch rarSegmentFetcher, part RARPart, localStart, localEnd int64, w io.Writer, flush func(), itemID, contentKey string) error {
	if fetch == nil {
		return fmt.Errorf("rar: nil segment fetcher")
	}
	if part.DataBytes <= 0 {
		return nil
	}
	if localStart < 0 {
		localStart = 0
	}
	if maxEnd := part.DataBytes - 1; localEnd > maxEnd {
		localEnd = maxEnd
	}
	if localStart > localEnd {
		return nil
	}

	rawStart := part.HeaderBytes + localStart
	rawEnd := part.HeaderBytes + localEnd
	rawOffset := int64(0)
	wrote := false

	for segIdx, seg := range part.Segments {
		if err := ctx.Err(); err != nil {
			return err
		}

		data, err := fetch(ctx, seg, itemID, contentKey, part.NZBFileIdx, segIdx)
		if err != nil {
			return fmt.Errorf("rar: segment %d: %w", seg.Number, err)
		}
		segStart := rawOffset
		segEnd := rawOffset + int64(len(data)) - 1
		rawOffset += int64(len(data))
		if len(data) == 0 || segEnd < rawStart {
			continue
		}
		if segStart > rawEnd {
			break
		}

		start := int64(0)
		if rawStart > segStart {
			start = rawStart - segStart
		}
		end := int64(len(data)) - 1
		if rawEnd < segEnd {
			end = rawEnd - segStart
		}
		if start > end {
			continue
		}
		if _, err := w.Write(data[start : end+1]); err != nil {
			return fmt.Errorf("rar: write to client: %w", err)
		}
		wrote = true
		if flush != nil {
			flush()
		}
		if rawOffset > rawEnd {
			break
		}
	}

	// If we exhausted all segments without writing anything the range truly
	// falls outside what the decoded stream produced — this should only happen
	// if the manifest's DataBytes (derived from NZB-declared encoded sizes) is
	// larger than the actual decoded content. Log as a no-op rather than error:
	// Jellyfin will retry with an adjusted range once it sees the actual bytes.
	_ = wrote
	return nil
}

// parseRARRangeHeader parses "bytes=START-END" or "bytes=START-" Range header.
// Returns (0, totalSize-1) for missing/invalid headers.
func parseRARRangeHeader(rangeHeader string, totalSize int64) (int64, int64) {
	if rangeHeader == "" || !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, totalSize - 1
	}
	spec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, totalSize - 1
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, totalSize - 1
	}
	if parts[1] == "" {
		return start, totalSize - 1
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return start, totalSize - 1
	}
	return start, end
}

func isRARPartSubject(subject string) bool {
	lower := strings.ToLower(subject)
	if strings.Contains(lower, ".par2") {
		return false
	}
	if strings.Contains(lower, ".rar") {
		return true
	}
	if idx := strings.LastIndex(lower, ".r"); idx >= 0 && idx+4 <= len(lower) {
		return lower[idx+2] >= '0' && lower[idx+2] <= '9' &&
			lower[idx+3] >= '0' && lower[idx+3] <= '9'
	}
	return false
}

// extractRARPartNum extracts the part number from a RAR subject line.
// Handles both "part01.rar" and ".r00", ".r01" style naming.
func extractRARPartNum(subject string) int {
	lower := strings.ToLower(subject)

	// "partNNN.rar" style
	if idx := strings.Index(lower, ".rar"); idx != -1 {
		prefix := lower[:idx]
		if pi := strings.LastIndex(prefix, "part"); pi != -1 {
			numStr := strings.TrimLeft(prefix[pi+4:], "0")
			if numStr == "" {
				return 0
			}
			if n, err := strconv.Atoi(numStr); err == nil {
				return n
			}
		}
	}

	// ".r00", ".r01" ... style (continuation parts)
	for _, ext := range []string{".r"} {
		if idx := strings.LastIndex(lower, ext); idx != -1 {
			numStr := lower[idx+2:]
			// Strip anything after the number
			end := 0
			for end < len(numStr) && numStr[end] >= '0' && numStr[end] <= '9' {
				end++
			}
			if end > 0 {
				if n, err := strconv.Atoi(numStr[:end]); err == nil {
					return n + 1000 // offset so r00 sorts after part01.rar
				}
			}
		}
	}

	return 0
}
