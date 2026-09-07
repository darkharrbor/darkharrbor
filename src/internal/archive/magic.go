package archive

import (
	"context"
	"errors"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

const nestedMagicPrefixBytes = 16

// ErrNestedArchive reports an archive member whose payload is itself an
// archive. Nested archive streaming is intentionally unsupported.
var ErrNestedArchive = errors.New("archive: nested archive not supported")

// Format is a content-derived container or archive classification.
type Format string

const (
	FormatUnknown Format = "unknown"
	FormatRAR     Format = "rar"
	Format7z      Format = "7z"
	FormatZIP     Format = "zip"
	FormatMKV     Format = "mkv"
	FormatMP4     Format = "mp4"
	// FormatNested is reported only after inspecting an archive member.
	// Leading magic alone cannot safely prove nesting.
	FormatNested Format = "nested"
)

// ClassifyMagic classifies a bounded leading byte slice. Short, malformed,
// and unknown input always abstains.
func ClassifyMagic(head []byte) Format {
	switch {
	case len(head) >= 7 && string(head[:6]) == "Rar!\x1a\x07" && head[6] == 0:
		return FormatRAR
	case len(head) >= 8 && string(head[:6]) == "Rar!\x1a\x07" && head[6] == 1 && head[7] == 0:
		return FormatRAR
	case len(head) >= 6 && string(head[:6]) == "7z\xbc\xaf\x27\x1c":
		return Format7z
	case len(head) >= 4 && string(head[:4]) == "PK\x03\x04":
		return FormatZIP
	case len(head) >= 4 && string(head[:4]) == "\x1a\x45\xdf\xa3":
		return FormatMKV
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		return FormatMP4
	default:
		return FormatUnknown
	}
}

// DetectNestedArchive inspects only a bounded member prefix. Short or unknown
// payloads safely abstain; malformed spans fail closed.
func DetectNestedArchive(ctx context.Context, src bytesource.ByteSource, offset, size int64) (bool, error) {
	if size <= 0 {
		return false, nil
	}
	if size > nestedMagicPrefixBytes {
		size = nestedMagicPrefixBytes
	}
	head, err := readAt(ctx, src, offset, size)
	if err != nil {
		return false, err
	}
	return isArchiveFormat(ClassifyMagic(head)), nil
}

func isArchiveFormat(format Format) bool {
	switch format {
	case FormatRAR, Format7z, FormatZIP:
		return true
	default:
		return false
	}
}
