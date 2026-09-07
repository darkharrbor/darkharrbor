// Package archive parses streamable archive metadata from a lane-neutral
// bytesource.ByteSource.
package archive

import (
	"context"
	"fmt"
	"io"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

const (
	maxRARHeaderBytes      = 1 << 20
	zipTailBytes           = 64 << 10
	maxZIPCentralDirectory = 16 << 20
	maxZIPCentralEntries   = 100_000
	maxZIP32Size           = 1<<32 - 1
)

func readAt(ctx context.Context, src bytesource.ByteSource, off, size int64) ([]byte, error) {
	if src == nil {
		return nil, fmt.Errorf("archive: nil byte source")
	}
	if off < 0 || size < 0 || off > src.Size() || size > src.Size()-off {
		return nil, fmt.Errorf("archive: invalid read offset=%d size=%d source_size=%d", off, size, src.Size())
	}
	if size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, int(size))
	n, err := src.ReadAt(ctx, buf, off)
	if n != len(buf) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("archive: short read at %d: got %d of %d: %w", off, n, len(buf), err)
	}
	if err != nil {
		return nil, fmt.Errorf("archive: read at %d: %w", off, err)
	}
	return buf, nil
}

func readPrefix(ctx context.Context, src bytesource.ByteSource, limit int64) ([]byte, error) {
	if src == nil {
		return nil, fmt.Errorf("archive: nil byte source")
	}
	size := src.Size()
	if size <= 0 {
		return nil, fmt.Errorf("archive: empty byte source")
	}
	if size > limit {
		size = limit
	}
	return readAt(ctx, src, 0, size)
}
