package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// ErrCompressedMember means the caller-provided archive member is encoded
// with a compression method other than stored/none. Only stored members can
// be exposed as random-access ByteSource reads without full extraction; a
// compressed member must fail closed rather than be silently downloaded and
// decompressed in full (HR5.1 scope: random ByteSource reads only).
var ErrCompressedMember = errors.New("archive: compressed archive member not supported")

// NewArchiveMemberByteSource exposes one stored member's payload span of an
// underlying whole-archive ByteSource as its own logical ByteSource. Reads
// against the returned source at logical offset X are served by reading the
// underlying archive source at dataOffset+X: no archive bytes are downloaded
// or buffered beyond what a caller actually requests, and the archive is
// never fully extracted. The underlying source is not copied; callers own
// its lifecycle.
func NewArchiveMemberByteSource(key string, src bytesource.ByteSource, dataOffset, size int64) (bytesource.ByteSource, error) {
	if key == "" || strings.Contains(key, "://") {
		return nil, fmt.Errorf("archive: invalid member source key")
	}
	if src == nil {
		return nil, fmt.Errorf("archive: nil byte source")
	}
	if dataOffset < 0 || size <= 0 || dataOffset > src.Size() || size > src.Size()-dataOffset {
		return nil, fmt.Errorf("archive: invalid member span offset=%d size=%d source_size=%d", dataOffset, size, src.Size())
	}
	return &archiveMemberSource{key: key, src: src, offset: dataOffset, size: size}, nil
}

type archiveMemberSource struct {
	key    string
	src    bytesource.ByteSource
	offset int64
	size   int64
}

func (s *archiveMemberSource) Key() string { return s.key }
func (s *archiveMemberSource) Size() int64 { return s.size }

func (s *archiveMemberSource) Caps() bytesource.Capabilities {
	// The member span is only ever exposed for stored (uncompressed)
	// members, so the logical member size exactly matches the underlying
	// byte range read: ExactSize is true regardless of the wrapped source's
	// own capability hints. Range support and tail cost are inherited from
	// the wrapped source since every logical member offset maps directly to
	// one wrapped-source offset.
	underlying := s.src.Caps()
	return bytesource.Capabilities{
		RangeSupport: underlying.RangeSupport,
		ExactSize:    true,
		TailCost:     underlying.TailCost,
	}
}

// ReadAt maps a logical member read onto the wrapped whole-archive source at
// offset+off. It follows the same io.ReaderAt/ByteSource contract as every
// other adapter in this package (see storedRARSource): a read that must be
// truncated because it reaches the member's own logical end returns io.EOF
// alongside the bytes it did get, and a short read from the wrapped source
// itself (fewer bytes than requested, no error) is turned into
// io.ErrUnexpectedEOF rather than silently under-filling the caller's
// buffer.
func (s *archiveMemberSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, bytesource.ErrNegativeOffset
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off >= s.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	truncated := false
	if remaining := s.size - off; want > remaining {
		want = remaining
		truncated = true
	}
	n, err := s.src.ReadAt(ctx, p[:want], s.offset+off)
	if err != nil && !(errors.Is(err, io.EOF) && int64(n) == want) {
		return n, err
	}
	if int64(n) < want {
		return n, io.ErrUnexpectedEOF
	}
	if truncated {
		return n, io.EOF
	}
	return n, nil
}
