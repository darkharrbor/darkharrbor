package archive

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

func sevenZipNumber(value uint64) []byte {
	for extra := 0; extra < 8; extra++ {
		limitBits := uint(7 * (extra + 1))
		if limitBits < 64 && value < uint64(1)<<limitBits {
			first := byte(0xff<<uint(8-extra)) | byte(value>>uint(8*extra))
			out := []byte{first}
			for i := 0; i < extra; i++ {
				out = append(out, byte(value>>uint(8*i)))
			}
			return out
		}
	}
	out := []byte{0xff}
	for i := 0; i < 8; i++ {
		out = append(out, byte(value>>uint(8*i)))
	}
	return out
}

func makeSevenZipFolder(method []byte) []byte {
	return append([]byte{1, byte(len(method))}, method...)
}

func makeSevenZipStreams(methods [][]byte, sizes []uint64, substreams []uint64) []byte {
	data := []byte{0x06}
	data = append(data, sevenZipNumber(0)...)
	data = append(data, sevenZipNumber(uint64(len(sizes)))...)
	data = append(data, 0x09)
	for _, size := range sizes {
		data = append(data, sevenZipNumber(size)...)
	}
	data = append(data, 0x00, 0x07, 0x0b)
	data = append(data, sevenZipNumber(uint64(len(methods)))...)
	data = append(data, 0x00)
	for _, method := range methods {
		data = append(data, makeSevenZipFolder(method)...)
	}
	data = append(data, 0x0c)
	for _, size := range sizes {
		data = append(data, sevenZipNumber(size)...)
	}
	data = append(data, 0x00)
	if substreams != nil {
		data = append(data, 0x08, 0x0d)
		for _, count := range substreams {
			data = append(data, sevenZipNumber(count)...)
		}
		data = append(data, 0x00)
	}
	return append(data, 0x00)
}

func sevenZipNamesProperty(names []string) []byte {
	property := []byte{0}
	for _, name := range names {
		for _, unit := range utf16.Encode([]rune(name)) {
			property = binary.LittleEndian.AppendUint16(property, unit)
		}
		property = append(property, 0, 0)
	}
	return append(append([]byte{0x11}, sevenZipNumber(uint64(len(property)))...), property...)
}

func sevenZipFixture(names []string, payloads [][]byte, methods [][]byte, substreams []uint64) []byte {
	var packed []byte
	sizes := make([]uint64, len(payloads))
	for i, payload := range payloads {
		packed = append(packed, payload...)
		sizes[i] = uint64(len(payload))
	}
	header := []byte{0x01, 0x04}
	header = append(header, makeSevenZipStreams(methods, sizes, substreams)...)
	header = append(header, 0x05)
	header = append(header, sevenZipNumber(uint64(len(names)))...)
	header = append(header, sevenZipNamesProperty(names)...)
	header = append(header, 0x00, 0x00)

	start := make([]byte, sevenZipSignatureHeaderSize)
	copy(start, sevenZipSignature)
	start[6], start[7] = 0, 4
	binary.LittleEndian.PutUint64(start[12:20], uint64(len(packed)))
	binary.LittleEndian.PutUint64(start[20:28], uint64(len(header)))
	binary.LittleEndian.PutUint32(start[28:32], crc32.ChecksumIEEE(header))
	binary.LittleEndian.PutUint32(start[8:12], crc32.ChecksumIEEE(start[12:32]))
	return append(append(start, packed...), header...)
}

func TestParseSevenZipCopyEntries(t *testing.T) {
	t.Parallel()
	data := sevenZipFixture(
		[]string{"video/movie.mkv", "video/clip.mp4"},
		[][]byte{[]byte("first"), []byte("second")},
		[][]byte{sevenZipCopyID, sevenZipCopyID},
		nil,
	)
	entries, err := ParseSevenZipCopyEntries(context.Background(), bytesource.NewMemSource("fixture", data))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "video/movie.mkv" || entries[0].DataOffset != 32 ||
		entries[0].Size != 5 || string(data[entries[0].DataOffset:entries[0].DataOffset+entries[0].Size]) != "first" ||
		entries[1].Name != "video/clip.mp4" || entries[1].DataOffset != 37 || entries[1].Size != 6 {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestParseSevenZipRejectsUnsupportedLayoutsDistinctly(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		data []byte
		want error
	}{
		{name: "LZMA", data: sevenZipFixture([]string{"movie.mkv"}, [][]byte{[]byte("x")}, [][]byte{{0x03, 0x01, 0x01}}, nil), want: ErrCompressed7z},
		{name: "solid", data: sevenZipFixture([]string{"a.mkv", "b.mkv"}, [][]byte{[]byte("ab")}, [][]byte{sevenZipCopyID}, []uint64{2}), want: ErrSolid7z},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseSevenZipCopyEntries(context.Background(), bytesource.NewMemSource("fixture", test.data))
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}

	encoded := append([]byte{0x17}, makeSevenZipStreams([][]byte{sevenZipAESID}, []uint64{1}, nil)...)
	data := sevenZipFixture([]string{"movie.mkv"}, [][]byte{[]byte("x")}, [][]byte{sevenZipCopyID}, nil)
	nextOffset := binary.LittleEndian.Uint64(data[12:20])
	data = append(data[:sevenZipSignatureHeaderSize+nextOffset], encoded...)
	binary.LittleEndian.PutUint64(data[20:28], uint64(len(encoded)))
	binary.LittleEndian.PutUint32(data[28:32], crc32.ChecksumIEEE(encoded))
	binary.LittleEndian.PutUint32(data[8:12], crc32.ChecksumIEEE(data[12:32]))
	if _, err := ParseSevenZipCopyEntries(context.Background(), bytesource.NewMemSource("fixture", data)); !errors.Is(err, ErrEncryptedHeader7z) {
		t.Fatalf("encrypted-header error = %v", err)
	}
}

func TestParseSevenZipFailsClosed(t *testing.T) {
	t.Parallel()
	valid := sevenZipFixture([]string{"movie.mkv"}, [][]byte{[]byte("video")}, [][]byte{sevenZipCopyID}, nil)
	for _, test := range []struct {
		name string
		data []byte
		want string
	}{
		{name: "short", data: valid[:20], want: "source too short"},
		{name: "signature", data: append([]byte("bad"), valid[3:]...), want: "invalid signature"},
		{name: "unsafe path", data: sevenZipFixture([]string{"../movie.mkv"}, [][]byte{[]byte("video")}, [][]byte{sevenZipCopyID}, nil), want: "unsafe member path"},
		{name: "missing files", data: sevenZipWithoutFiles(valid), want: "incomplete header"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseSevenZipCopyEntries(context.Background(), bytesource.NewMemSource("fixture", test.data))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}

	badStartCRC := append([]byte(nil), valid...)
	badStartCRC[8] ^= 1
	if _, err := ParseSevenZipCopyEntries(context.Background(), bytesource.NewMemSource("fixture", badStartCRC)); err == nil || !strings.Contains(err.Error(), "start header CRC") {
		t.Fatalf("start CRC error = %v", err)
	}
	badHeaderCRC := append([]byte(nil), valid...)
	badHeaderCRC[len(badHeaderCRC)-1] ^= 1
	if _, err := ParseSevenZipCopyEntries(context.Background(), bytesource.NewMemSource("fixture", badHeaderCRC)); err == nil || !strings.Contains(err.Error(), "next header CRC") {
		t.Fatalf("next CRC error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ParseSevenZipCopyEntries(ctx, bytesource.NewMemSource("fixture", valid)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, err := ParseSevenZipCopyEntries(context.Background(), shortSevenZipSource{data: valid}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short-read error = %v", err)
	}
}

func sevenZipWithoutFiles(valid []byte) []byte {
	packed := []byte("video")
	header := append([]byte{0x01, 0x04}, makeSevenZipStreams([][]byte{sevenZipCopyID}, []uint64{uint64(len(packed))}, nil)...)
	header = append(header, 0x00)
	start := append([]byte(nil), valid[:sevenZipSignatureHeaderSize]...)
	binary.LittleEndian.PutUint64(start[12:20], uint64(len(packed)))
	binary.LittleEndian.PutUint64(start[20:28], uint64(len(header)))
	binary.LittleEndian.PutUint32(start[28:32], crc32.ChecksumIEEE(header))
	binary.LittleEndian.PutUint32(start[8:12], crc32.ChecksumIEEE(start[12:32]))
	return append(append(start, packed...), header...)
}

type shortSevenZipSource struct{ data []byte }

func (s shortSevenZipSource) Size() int64 { return int64(len(s.data)) }
func (s shortSevenZipSource) Key() string { return "short" }
func (s shortSevenZipSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: true}
}
func (s shortSevenZipSource) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n > 0 {
		n--
	}
	return n, nil
}

func TestSevenZipParserReadsOnlyBoundedHeaders(t *testing.T) {
	t.Parallel()
	data := sevenZipFixture(
		[]string{"movie.mkv"},
		[][]byte{[]byte(strings.Repeat("v", 4096))},
		[][]byte{sevenZipCopyID},
		nil,
	)
	src := &recordingSevenZipSource{data: data}
	if _, err := ParseSevenZipCopyEntries(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	nextOffset := int64(binary.LittleEndian.Uint64(data[12:20]))
	nextSize := int64(binary.LittleEndian.Uint64(data[20:28]))
	want := [][2]int64{{0, sevenZipSignatureHeaderSize}, {sevenZipSignatureHeaderSize + nextOffset, nextSize}}
	if len(src.reads) != len(want) {
		t.Fatalf("reads = %v, want %v", src.reads, want)
	}
	for i := range want {
		if src.reads[i] != want[i] {
			t.Fatalf("read %d = %v, want %v", i, src.reads[i], want[i])
		}
	}
}

type recordingSevenZipSource struct {
	data  []byte
	reads [][2]int64
}

func (s *recordingSevenZipSource) Size() int64 { return int64(len(s.data)) }
func (s *recordingSevenZipSource) Key() string { return "recording" }
func (s *recordingSevenZipSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: true}
}
func (s *recordingSevenZipSource) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	s.reads = append(s.reads, [2]int64{off, int64(len(p))})
	if off < 0 || off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func FuzzParseSevenZipCopyEntries(f *testing.F) {
	f.Add(sevenZipFixture([]string{"movie.mkv"}, [][]byte{[]byte("video")}, [][]byte{sevenZipCopyID}, nil))
	f.Add(sevenZipFixture([]string{"movie.mkv"}, [][]byte{[]byte("x")}, [][]byte{{0x03, 0x01, 0x01}}, nil))
	f.Add([]byte("not a 7z archive"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseSevenZipCopyEntries(context.Background(), bytesource.NewMemSource("fuzz", data))
	})
}
