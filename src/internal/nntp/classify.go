package nntp

import (
	"context"
	"errors"
	"io"
	"strings"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
)

const (
	magicCandidateLimit      = 8
	magicPrefixBytes         = 16
	magicMaxFirstSegmentSize = 4 << 20
)

// MagicClassification describes a bounded content-first NZB classification.
// Ambiguous means distinct recognized signatures were observed and callers
// must abstain rather than falling back to a filename hint.
type MagicClassification struct {
	Format    archiveparser.Format
	FileIndex int
	Inspected int
	Ambiguous bool
}

type magicPrefixFetcher func(context.Context, int, NZBFile) ([]byte, error)

// ClassifyNZBMagic inspects at most the first segment of the eight largest
// non-PAR2 files through the existing cancellable acquisition path.
func (p *NNTPProvider) ClassifyNZBMagic(ctx context.Context, itemID string, nzb NZB) (MagicClassification, error) {
	return classifyNZBMagic(ctx, nzb.Files, func(ctx context.Context, fileIndex int, file NZBFile) ([]byte, error) {
		src, err := p.archiveByteSource(withAccountingItemID(ctx, itemID), fileIndex, file.Segments[:1])
		if err != nil {
			return nil, err
		}
		size := src.Size()
		if size > magicPrefixBytes {
			size = magicPrefixBytes
		}
		buf := make([]byte, int(size))
		n, err := src.ReadAt(ctx, buf, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return buf[:n], nil
	})
}

func classifyNZBMagic(ctx context.Context, files []NZBFile, fetch magicPrefixFetcher) (MagicClassification, error) {
	result := MagicClassification{Format: archiveparser.FormatUnknown, FileIndex: -1}
	observed := archiveparser.FormatUnknown
	var lastErr error
	candidates := 0
	for fileIndex, file := range files {
		if candidates == magicCandidateLimit {
			break
		}
		if isPAR2Subject(file.Subject) || len(file.Segments) == 0 {
			continue
		}
		first := file.Segments[0]
		if first.Bytes <= 0 || first.Bytes > magicMaxFirstSegmentSize {
			continue
		}
		candidates++
		if err := ctx.Err(); err != nil {
			return result, err
		}
		head, err := fetch(ctx, fileIndex, file)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			lastErr = err
			continue
		}
		result.Inspected++
		if len(head) > magicPrefixBytes {
			head = head[:magicPrefixBytes]
		}
		format := archiveparser.ClassifyMagic(head)
		if format == archiveparser.FormatUnknown {
			continue
		}
		if observed == archiveparser.FormatUnknown {
			observed = format
			result.Format = format
			result.FileIndex = fileIndex
			continue
		}
		if format != observed {
			result.Format = archiveparser.FormatUnknown
			result.FileIndex = -1
			result.Ambiguous = true
			return result, nil
		}
	}
	if lastErr != nil {
		result.Format = archiveparser.FormatUnknown
		result.FileIndex = -1
		result.Ambiguous = false
		return result, lastErr
	}
	return result, nil
}

func isPAR2Subject(subject string) bool {
	return strings.Contains(strings.ToLower(subject), ".par2")
}
