package archive

import (
	stdzip "archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

const maxNestedMemberCandidates = 8

// ErrNoVideoEntries means the ZIP parsed successfully but contains no member
// currently eligible for video streaming.
var (
	ErrNoVideoEntries    = errors.New("zip: no video entries")
	ErrNoSubtitleEntries = errors.New("zip: no subtitle entries")
)

// ZIPEntry describes one video member in a ZIP central directory.
type ZIPEntry struct {
	Filename       string
	LocalHeaderOff int64
	DataOff        int64
	CompressedSize int64
	Uncompressed   int64
	Method         uint16
}

// ParseZIPVideoEntries returns video members sorted by filename, with their
// exact stored-data offsets resolved from local file headers.
func ParseZIPVideoEntries(ctx context.Context, src bytesource.ByteSource) ([]ZIPEntry, int64, error) {
	return parseZIPEntries(ctx, src, isVideo, ErrNoVideoEntries)
}

// ParseZIPSubtitleEntries returns safe subtitle members. It shares the exact
// bounded central-directory and local-header parser used for video members.
func ParseZIPSubtitleEntries(ctx context.Context, src bytesource.ByteSource) ([]ZIPEntry, int64, error) {
	return parseZIPEntries(ctx, src, isSubtitle, ErrNoSubtitleEntries)
}

// ReadZIPSubtitle reads one already-validated stored or deflated subtitle
// entry through the shared ByteSource. It never allocates or emits more than
// maxBytes and refuses contradictory metadata.
func ReadZIPSubtitle(ctx context.Context, src bytesource.ByteSource, entry ZIPEntry, maxBytes int) ([]byte, error) {
	if src == nil || maxBytes <= 0 || entry.Uncompressed <= 0 || entry.Uncompressed > int64(maxBytes) ||
		entry.CompressedSize < 0 || entry.CompressedSize > int64(maxBytes) ||
		entry.DataOff < 0 || entry.DataOff > src.Size() || entry.CompressedSize > src.Size()-entry.DataOff {
		return nil, fmt.Errorf("zip: invalid subtitle entry bounds")
	}
	compressed := make([]byte, int(entry.CompressedSize))
	n, err := src.ReadAt(ctx, compressed, entry.DataOff)
	if n != len(compressed) || err != nil && !errors.Is(err, io.EOF) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("zip: read subtitle payload: %w", err)
	}
	var data []byte
	switch entry.Method {
	case stdzip.Store:
		data = compressed
	case stdzip.Deflate:
		reader := flate.NewReader(bytes.NewReader(compressed))
		data, err = io.ReadAll(io.LimitReader(reader, int64(maxBytes)+1))
		closeErr := reader.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, fmt.Errorf("zip: decode subtitle payload: %w", err)
		}
	default:
		return nil, fmt.Errorf("zip: unsupported subtitle method %d", entry.Method)
	}
	if len(data) > maxBytes || int64(len(data)) != entry.Uncompressed {
		return nil, fmt.Errorf("zip: subtitle size mismatch")
	}
	return data, nil
}

func parseZIPEntries(ctx context.Context, src bytesource.ByteSource, accept func(string) bool, noEntries error) ([]ZIPEntry, int64, error) {
	if src == nil || src.Size() < 22 {
		return nil, 0, fmt.Errorf("zip: source too short")
	}
	tailSize := min(src.Size(), int64(zipTailBytes))
	tail, err := readAt(ctx, src, src.Size()-tailSize, tailSize)
	if err != nil {
		return nil, 0, fmt.Errorf("zip: fetch EOCD tail: %w", err)
	}
	eocdPos := findEOCD(tail)
	if eocdPos < 0 {
		return nil, 0, fmt.Errorf("zip: EOCD not found in last %dKB", zipTailBytes/1024)
	}
	eocd := tail[eocdPos:]
	if len(eocd) < 22 {
		return nil, 0, fmt.Errorf("zip: EOCD record truncated (%d bytes)", len(eocd))
	}
	if binary.LittleEndian.Uint16(eocd[4:6]) != 0 || binary.LittleEndian.Uint16(eocd[6:8]) != 0 {
		return nil, 0, fmt.Errorf("zip: multi-disk archives not supported")
	}
	entryCount := int64(binary.LittleEndian.Uint16(eocd[10:12]))
	cdSize := int64(binary.LittleEndian.Uint32(eocd[12:16]))
	cdOffset := int64(binary.LittleEndian.Uint32(eocd[16:20]))
	if entryCount == 0xffff || cdSize == 0xffffffff || cdOffset == 0xffffffff {
		return nil, 0, fmt.Errorf("zip: ZIP64 not supported")
	}
	if entryCount > maxZIPCentralEntries || cdSize > maxZIPCentralDirectory {
		return nil, 0, fmt.Errorf("zip: central directory exceeds limit")
	}
	if cdOffset < 0 || cdSize > src.Size()-cdOffset {
		return nil, 0, fmt.Errorf("zip: central directory exceeds source")
	}
	localOffsets, err := zipLocalOffsets(ctx, src, cdOffset, cdSize, entryCount)
	if err != nil {
		return nil, 0, err
	}

	reader, err := stdzip.NewReader(contextReaderAt{ctx: ctx, src: src}, src.Size())
	if err != nil {
		return nil, 0, fmt.Errorf("zip: parse central directory: %w", err)
	}
	if int64(len(reader.File)) != entryCount {
		return nil, 0, fmt.Errorf("zip: central directory entry count mismatch")
	}
	if err := detectNestedZIPMember(ctx, reader.File); err != nil {
		return nil, 0, err
	}
	entries := make([]ZIPEntry, 0)
	var total int64
	for i, file := range reader.File {
		if !accept(file.Name) {
			continue
		}
		if !safeArchivePath(file.Name) {
			return nil, 0, fmt.Errorf("zip: unsafe member path")
		}
		dataOffset, err := file.DataOffset()
		if err != nil {
			return nil, 0, fmt.Errorf("zip: local header for %q: %w", file.Name, err)
		}
		if file.CompressedSize64 > maxZIP32Size || file.UncompressedSize64 > maxZIP32Size {
			return nil, 0, fmt.Errorf("zip: ZIP64 not supported")
		}
		if file.CompressedSize64 > uint64(src.Size()) || dataOffset < localOffsets[i] || dataOffset > src.Size() || file.CompressedSize64 > uint64(src.Size()-dataOffset) {
			return nil, 0, fmt.Errorf("zip: member %q data exceeds source", file.Name)
		}
		entries = append(entries, ZIPEntry{
			Filename:       file.Name,
			LocalHeaderOff: localOffsets[i],
			DataOff:        dataOffset,
			CompressedSize: int64(file.CompressedSize64),
			Uncompressed:   int64(file.UncompressedSize64),
			Method:         file.Method,
		})
		total += int64(file.UncompressedSize64)
	}
	if len(entries) == 0 {
		return nil, 0, fmt.Errorf("%w in ZIP (%d CD entries scanned)", noEntries, entryCount)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Filename < entries[j].Filename })
	return entries, total, nil
}

func isSubtitle(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".srt", ".ass", ".idx", ".sub":
		return true
	default:
		return false
	}
}

func detectNestedZIPMember(ctx context.Context, files []*stdzip.File) error {
	candidates := make([]*stdzip.File, 0, maxNestedMemberCandidates)
	for _, file := range files {
		if file.FileInfo().IsDir() || file.UncompressedSize64 == 0 {
			continue
		}
		insert := sort.Search(len(candidates), func(i int) bool {
			if candidates[i].UncompressedSize64 != file.UncompressedSize64 {
				return candidates[i].UncompressedSize64 < file.UncompressedSize64
			}
			return candidates[i].Name > file.Name
		})
		if insert >= maxNestedMemberCandidates {
			continue
		}
		candidates = append(candidates, nil)
		copy(candidates[insert+1:], candidates[insert:])
		candidates[insert] = file
		if len(candidates) > maxNestedMemberCandidates {
			candidates = candidates[:maxNestedMemberCandidates]
		}
	}

	for _, file := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeArchivePath(file.Name) {
			return fmt.Errorf("zip: unsafe member path")
		}
		member, err := file.Open()
		if err != nil {
			return fmt.Errorf("zip: inspect member payload: %w", err)
		}
		var head [nestedMagicPrefixBytes]byte
		n, readErr := io.ReadFull(member, head[:])
		_ = member.Close()
		if err := ctx.Err(); err != nil {
			return err
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return fmt.Errorf("zip: inspect member payload: %w", readErr)
		}
		if isArchiveFormat(ClassifyMagic(head[:n])) {
			return fmt.Errorf("%w: ZIP member", ErrNestedArchive)
		}
	}
	return nil
}

func zipLocalOffsets(ctx context.Context, src bytesource.ByteSource, offset, size, count int64) ([]int64, error) {
	directory, err := readAt(ctx, src, offset, size)
	if err != nil {
		return nil, fmt.Errorf("zip: fetch central directory: %w", err)
	}
	offsets := make([]int64, 0, count)
	pos := 0
	for i := int64(0); i < count; i++ {
		if pos+46 > len(directory) || binary.LittleEndian.Uint32(directory[pos:]) != 0x02014b50 {
			return nil, fmt.Errorf("zip: central directory entry %d truncated or invalid", i)
		}
		nameLen := int(binary.LittleEndian.Uint16(directory[pos+28:]))
		extraLen := int(binary.LittleEndian.Uint16(directory[pos+30:]))
		commentLen := int(binary.LittleEndian.Uint16(directory[pos+32:]))
		next := pos + 46 + nameLen + extraLen + commentLen
		if next < pos || next > len(directory) {
			return nil, fmt.Errorf("zip: central directory entry %d fields truncated", i)
		}
		localOffset := binary.LittleEndian.Uint32(directory[pos+42:])
		if localOffset == 0xffffffff {
			return nil, fmt.Errorf("zip: ZIP64 not supported")
		}
		offsets = append(offsets, int64(localOffset))
		pos = next
	}
	return offsets, nil
}

type contextReaderAt struct {
	ctx context.Context
	src bytesource.ByteSource
}

func (r contextReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.src.ReadAt(r.ctx, p, off)
	if err == nil && n != len(p) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func findEOCD(data []byte) int {
	for i := len(data) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(data[i:]) == 0x06054b50 {
			return i
		}
	}
	return -1
}

func isVideo(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv", ".mp4", ".avi", ".m4v", ".mov", ".ts", ".m2ts", ".webm":
		return true
	default:
		return false
	}
}

func safeArchivePath(name string) bool {
	clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") && !strings.HasPrefix(clean, "/")
}
