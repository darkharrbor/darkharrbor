package archive

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

func TestNewArchiveMemberByteSourceHappyPath(t *testing.T) {
	// "HEADER" (6) + "video-payload-bytes" (19, the member) + "TRAILER" (7).
	underlying := bytesource.NewMemSource("archive", []byte("HEADERvideo-payload-bytesTRAILER"))
	member, err := NewArchiveMemberByteSource("member-key", underlying, 6, 19)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := member.Size(); got != 19 {
		t.Fatalf("Size() = %d, want 19", got)
	}
	if got := member.Key(); got != "member-key" {
		t.Fatalf("Key() = %q", got)
	}
	if !member.Caps().ExactSize {
		t.Fatalf("Caps().ExactSize = false, want true")
	}

	buf := make([]byte, member.Size())
	n, err := member.ReadAt(context.Background(), buf, 0)
	if err != nil {
		t.Fatalf("ReadAt full: %v", err)
	}
	if got, want := string(buf[:n]), "video-payload-bytes"; got != want {
		t.Fatalf("ReadAt full = %q, want %q", got, want)
	}

	partial := make([]byte, 4)
	n, err = member.ReadAt(context.Background(), partial, 6)
	if err != nil {
		t.Fatalf("ReadAt partial: %v", err)
	}
	if got, want := string(partial[:n]), "payl"; got != want {
		t.Fatalf("ReadAt partial = %q, want %q", got, want)
	}
}

func TestNewArchiveMemberByteSourceTruncatedAtEnd(t *testing.T) {
	underlying := bytesource.NewMemSource("archive", []byte("0123456789"))
	member, err := NewArchiveMemberByteSource("member-key", underlying, 2, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A read whose buffer extends past the member's own logical end must
	// return exactly the member's remaining bytes plus io.EOF, never bytes
	// from beyond the member span (even though the wrapped archive source
	// itself has more data after the member).
	buf := make([]byte, 10)
	n, err := member.ReadAt(context.Background(), buf, 3)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if got, want := string(buf[:n]), "56"; got != want {
		t.Fatalf("ReadAt straddling end = %q, want %q", got, want)
	}
	// A read starting exactly at the member's end is a clean io.EOF with no
	// bytes.
	n, err = member.ReadAt(context.Background(), buf, 5)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt at end = (%d, %v), want (0, io.EOF)", n, err)
	}
}

func TestNewArchiveMemberByteSourceNegativeOffset(t *testing.T) {
	underlying := bytesource.NewMemSource("archive", []byte("0123456789"))
	member, err := NewArchiveMemberByteSource("member-key", underlying, 0, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := member.ReadAt(context.Background(), make([]byte, 1), -1); !errors.Is(err, bytesource.ErrNegativeOffset) {
		t.Fatalf("err = %v, want ErrNegativeOffset", err)
	}
}

func TestNewArchiveMemberByteSourceCancellation(t *testing.T) {
	underlying := bytesource.NewMemSource("archive", []byte("0123456789"))
	member, err := NewArchiveMemberByteSource("member-key", underlying, 0, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := member.ReadAt(ctx, make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNewArchiveMemberByteSourceInvalidSpan(t *testing.T) {
	underlying := bytesource.NewMemSource("archive", []byte("0123456789"))
	cases := []struct {
		name       string
		key        string
		src        bytesource.ByteSource
		dataOffset int64
		size       int64
	}{
		{"empty key", "", underlying, 0, 5},
		{"url-shaped key", "https://example.com", underlying, 0, 5},
		{"nil source", "k", nil, 0, 5},
		{"negative offset", "k", underlying, -1, 5},
		{"zero size", "k", underlying, 0, 0},
		{"negative size", "k", underlying, 0, -1},
		{"offset beyond source", "k", underlying, 11, 1},
		{"size exceeds source", "k", underlying, 8, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewArchiveMemberByteSource(tc.key, tc.src, tc.dataOffset, tc.size); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// shortReadSource always returns one fewer byte than requested (with a nil
// error) to prove the wrapper turns that into io.ErrUnexpectedEOF rather
// than silently under-filling the caller's buffer.
type shortReadSource struct {
	data []byte
}

func (s shortReadSource) Key() string { return "short" }
func (s shortReadSource) Size() int64 { return int64(len(s.data)) }
func (s shortReadSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: true}
}
func (s shortReadSource) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n > 1 {
		n--
	}
	return n, nil
}

func TestNewArchiveMemberByteSourceShortReadFromUnderlying(t *testing.T) {
	underlying := shortReadSource{data: []byte("0123456789")}
	member, err := NewArchiveMemberByteSource("member-key", underlying, 0, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := member.ReadAt(context.Background(), buf, 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}
