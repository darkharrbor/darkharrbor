package nntp

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
)

const (
	maxPAR2MagicCandidates    = 16
	maxPAR2MagicDeclaredBytes = 32 << 20
)

// FindPAR2File selects the best PAR2 metadata-index candidate from an NZB's
// files: prefers a non-volume ".par2" file (the small Main+FileDesc index)
// and falls back to the smallest ".par2"-named file (typically the smallest
// volume) if no bare index is present. Returns ok=false when no subject names
// a PAR2 file; FetchPAR2ForItem then performs a bounded magic-byte fallback.
func FindPAR2File(files []NZBFile) (NZBFile, int, bool) {
	bestIdx, bestVolIdx := -1, -1
	for i, f := range files {
		lower := strings.ToLower(f.Subject)
		if !strings.Contains(lower, ".par2") {
			continue
		}
		if archiveparser.PAR2IsVolumeFile(f.Subject) {
			if bestVolIdx == -1 || f.TotalBytes < files[bestVolIdx].TotalBytes {
				bestVolIdx = i
			}
			continue
		}
		if bestIdx == -1 || f.TotalBytes < files[bestIdx].TotalBytes {
			bestIdx = i
		}
	}
	if bestIdx != -1 {
		return files[bestIdx], bestIdx, true
	}
	if bestVolIdx != -1 {
		return files[bestVolIdx], bestVolIdx, true
	}
	return NZBFile{}, -1, false
}

// par2MagicCandidateIndexes returns a bounded set of the smallest non-empty
// NZB files. PAR2 metadata indexes are normally among the smallest entries;
// inspecting only this set prevents a fully-obfuscated release from making
// the resolver probe every media/archive payload.
func par2MagicCandidateIndexes(files []NZBFile) []int {
	indexes := make([]int, 0, len(files))
	for i, file := range files {
		if len(file.Segments) == 0 || file.TotalBytes <= 0 {
			continue
		}
		indexes = append(indexes, i)
	}
	sort.SliceStable(indexes, func(i, j int) bool {
		return files[indexes[i]].TotalBytes < files[indexes[j]].TotalBytes
	})

	selected := make([]int, 0, min(len(indexes), maxPAR2MagicCandidates))
	var declared int64
	for _, idx := range indexes {
		first := files[idx].Segments[0].Bytes
		if first <= 0 {
			first = files[idx].TotalBytes
		}
		first = min(first, int64(8))
		if len(selected) >= maxPAR2MagicCandidates {
			break
		}
		if first > maxPAR2MagicDeclaredBytes-declared {
			continue
		}
		selected = append(selected, idx)
		declared += first
	}
	return selected
}

func (p *NNTPProvider) readNZBFilePrefix(ctx context.Context, itemID string, fileIndex int, file NZBFile, maxBytes int64) ([]byte, error) {
	if len(file.Segments) == 0 {
		return nil, fmt.Errorf("nntp: prefix source has no segments")
	}
	src, err := p.archiveByteSource(withAccountingItemID(ctx, itemID), fileIndex, file.Segments)
	if err != nil {
		return nil, fmt.Errorf("nntp: prefix byte source: %w", err)
	}
	size := src.Size()
	if size <= 0 {
		return nil, fmt.Errorf("nntp: prefix source is empty")
	}
	if size > maxBytes {
		size = maxBytes
	}
	buf := make([]byte, int(size))
	n, readErr := src.ReadAt(ctx, buf, 0)
	if n == 0 {
		if readErr == nil {
			readErr = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("nntp: prefix read: %w", readErr)
	}
	return buf[:n], nil
}

func (p *NNTPProvider) locatePAR2File(ctx context.Context, itemID string, nzb NZB) (NZBFile, int, bool, error) {
	if file, idx, ok := FindPAR2File(nzb.Files); ok {
		return file, idx, true, nil
	}

	var readFailures int
	for _, idx := range par2MagicCandidateIndexes(nzb.Files) {
		prefix, err := p.readNZBFilePrefix(ctx, itemID, idx, nzb.Files[idx], 8)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return NZBFile{}, -1, false, ctxErr
			}
			readFailures++
			continue
		}
		if archiveparser.IsPAR2Prefix(prefix) {
			return nzb.Files[idx], idx, true, nil
		}
	}
	if readFailures > 0 {
		return NZBFile{}, -1, false, fmt.Errorf("par2: magic discovery had %d bounded read failures", readFailures)
	}
	return NZBFile{}, -1, false, nil
}

// FetchPAR2ForItem locates and parses the small PAR2 metadata index, fetching
// through the existing bounded archive ByteSource path. When subjects expose
// no extension, it inspects at most maxPAR2MagicCandidates small files and no
// more than maxPAR2MagicDeclaredBytes of declared first-segment payload.
// par2FileIndex is the stable index in ParseNZB's size-sorted file list.
func (p *NNTPProvider) FetchPAR2ForItem(ctx context.Context, itemID string, nzb NZB) (info archiveparser.PAR2Info, par2FileIndex int, ok bool, err error) {
	file, idx, ok, err := p.locatePAR2File(ctx, itemID, nzb)
	if err != nil || !ok {
		return archiveparser.PAR2Info{}, -1, ok, err
	}
	if len(file.Segments) == 0 {
		return archiveparser.PAR2Info{}, idx, true, fmt.Errorf("par2: metadata source has no segments")
	}
	src, err := p.archiveByteSource(withAccountingItemID(ctx, itemID), idx, file.Segments)
	if err != nil {
		return archiveparser.PAR2Info{}, idx, true, fmt.Errorf("par2: byte source: %w", err)
	}
	info, err = archiveparser.ParsePAR2Metadata(ctx, src)
	if err != nil {
		return archiveparser.PAR2Info{}, idx, true, fmt.Errorf("par2: parse metadata: %w", err)
	}
	return info, idx, true, nil
}
