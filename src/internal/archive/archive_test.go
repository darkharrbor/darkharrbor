package archive

import (
	stdzip "archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

func rar3File(method byte, payload []byte) []byte {
	const name = "movie.mkv"
	headSize := 32 + len(name)
	header := make([]byte, len(rar3Sig)+headSize)
	copy(header, rar3Sig)
	offset := len(rar3Sig)
	header[offset+2] = 0x74
	binary.LittleEndian.PutUint16(header[offset+5:], uint16(headSize))
	binary.LittleEndian.PutUint32(header[offset+7:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(header[offset+11:], uint32(len(payload)))
	header[offset+25] = method
	binary.LittleEndian.PutUint16(header[offset+26:], uint16(len(name)))
	copy(header[offset+32:], name)
	return append(header, payload...)
}

func rar5File(method uint64, payload []byte) []byte {
	compression := []byte{byte(method << 7)}
	if method > 0 {
		compression = []byte{0x80, byte(method)}
	}
	name := []byte("movie.mkv")
	body := append([]byte{2, 2, byte(len(payload)), 0, byte(len(payload)), 0}, compression...)
	body = append(body, 0, byte(len(name)))
	body = append(body, name...)
	data := append(append([]byte{}, rar5Sig...), 0, 0, 0, 0, byte(len(body)))
	data = append(data, body...)
	return append(data, payload...)
}

func rar3EncryptedFile(method byte) []byte {
	const (
		name = "movie.mkv"
		size = 16
	)
	headSize := 32 + len(name) + 8
	data := make([]byte, len(rar3Sig)+headSize+size)
	copy(data, rar3Sig)
	offset := len(rar3Sig)
	data[offset+2] = 0x74
	binary.LittleEndian.PutUint16(data[offset+3:], 0x0404)
	binary.LittleEndian.PutUint16(data[offset+5:], uint16(headSize))
	binary.LittleEndian.PutUint32(data[offset+7:], size)
	binary.LittleEndian.PutUint32(data[offset+11:], 5)
	data[offset+25] = method
	binary.LittleEndian.PutUint16(data[offset+26:], uint16(len(name)))
	copy(data[offset+32:], name)
	copy(data[offset+32+len(name):], []byte("12345678"))
	return data
}

func TestParseRARDataOffset(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		data    []byte
		want    int64
		wantErr string
	}{
		{name: "rar3 stored", data: rar3File(0x30, []byte("video")), want: int64(len(rar3File(0x30, nil)))},
		{name: "rar3 compressed", data: rar3File(0x33, nil), wantErr: "compressed RAR not supported"},
		{name: "rar3 encrypted stored", data: rar3EncryptedFile(0x30), want: int64(len(rar3EncryptedFile(0x30)) - 16)},
		{name: "rar3 compressed encrypted", data: rar3EncryptedFile(0x33), wantErr: "compressed and encrypted RAR not supported"},
		{name: "rar5 stored", data: rar5File(0, []byte("video")), want: int64(len(rar5File(0, nil)))},
		{name: "rar5 compressed", data: rar5File(1, nil), wantErr: "compressed RAR not supported"},
		{name: "truncated", data: rar3File(0, nil)[:20], wantErr: "truncated file header"},
		{name: "unknown", data: []byte("not an archive"), wantErr: "unrecognised RAR signature"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseRARDataOffset(context.Background(), bytesource.NewMemSource("fixture", test.data))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("offset = %d, want %d", got, test.want)
			}
		})
	}
}

func TestParseStoredRARMember(t *testing.T) {
	t.Parallel()
	for _, data := range [][]byte{rar3File(0x30, []byte("video")), rar5File(0, []byte("video"))} {
		member, err := ParseStoredRARMember(context.Background(), bytesource.NewMemSource("fixture", data))
		if err != nil {
			t.Fatal(err)
		}
		if member.Name != "movie.mkv" || member.DataSize != 5 || string(data[member.DataOffset:member.DataOffset+member.DataSize]) != "video" {
			t.Fatalf("member = %+v", member)
		}
	}
	encrypted, err := ParseStoredRARMember(context.Background(), bytesource.NewMemSource("fixture", rar3EncryptedFile(0x30)))
	if err != nil {
		t.Fatal(err)
	}
	if encrypted.Encryption == nil || encrypted.Encryption.Version != 3 || string(encrypted.Encryption.Salt) != "12345678" ||
		encrypted.DataSize != 5 || encrypted.PackedSize != 16 {
		t.Fatalf("encrypted member = %+v", encrypted)
	}
	traversal := rar3File(0x30, []byte("video"))
	const nameStart = 7 + 32
	binary.LittleEndian.PutUint16(traversal[7+26:], uint16(len("../x.mkv")))
	copy(traversal[nameStart:], "../x.mkv")
	if _, err := ParseStoredRARMember(context.Background(), bytesource.NewMemSource("fixture", traversal)); err == nil || !strings.Contains(err.Error(), "unsafe member path") {
		t.Fatalf("traversal error = %v", err)
	}
}

func TestDetectNestedArchive(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		data []byte
		want bool
	}{
		{name: "rar", data: []byte("Rar!\x1a\x07\x00payload"), want: true},
		{name: "rar5", data: []byte("Rar!\x1a\x07\x01\x00payload"), want: true},
		{name: "zip", data: []byte("PK\x03\x04payload"), want: true},
		{name: "7z", data: []byte("7z\xbc\xaf\x27\x1cpayload"), want: true},
		{name: "video", data: []byte("\x1a\x45\xdf\xa3payload")},
		{name: "short", data: []byte("PK\x03")},
		{name: "unknown", data: []byte("not-an-archive")},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := DetectNestedArchive(context.Background(), bytesource.NewMemSource("fixture", test.data), 0, int64(len(test.data)))
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("nested = %v, want %v", got, test.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DetectNestedArchive(ctx, bytesource.NewMemSource("fixture", []byte("PK\x03\x04")), 0, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, err := DetectNestedArchive(context.Background(), nil, 0, 4); err == nil {
		t.Fatal("nil source accepted")
	}
	if _, err := DetectNestedArchive(context.Background(), bytesource.NewMemSource("fixture", []byte("PK\x03\x04")), 2, 4); err == nil {
		t.Fatal("out-of-range member span accepted")
	}
}

func TestStoredRARByteSourceMapsPartsAndRanges(t *testing.T) {
	t.Parallel()
	first := rar3File(0x30, []byte("first"))
	second := rar5File(0, []byte("second"))
	firstMember, err := ParseStoredRARMember(context.Background(), bytesource.NewMemSource("first", first))
	if err != nil {
		t.Fatal(err)
	}
	secondMember, err := ParseStoredRARMember(context.Background(), bytesource.NewMemSource("second", second))
	if err != nil {
		t.Fatal(err)
	}
	manifest := StoredRARManifest{
		MemberName: "movie.mkv",
		TotalSize:  firstMember.DataSize + secondMember.DataSize,
		Parts: []StoredRARPart{
			{FileID: "1", ArchiveSize: int64(len(first)), DataOffset: firstMember.DataOffset, DataSize: firstMember.DataSize},
			{FileID: "2", ArchiveSize: int64(len(second)), DataOffset: secondMember.DataOffset, DataSize: secondMember.DataSize},
		},
	}
	src, err := NewStoredRARByteSource("torrent-rar|item", manifest, []bytesource.ByteSource{
		bytesource.NewMemSource("first", first),
		bytesource.NewMemSource("second", second),
	})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	n, err := src.ReadAt(context.Background(), buf, 3)
	if err != nil || n != len(buf) || string(buf) != "stsecon" {
		t.Fatalf("cross-part read = (%q, %d, %v)", buf, n, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.ReadAt(cancelled, buf, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	bad := manifest
	bad.Parts = append([]StoredRARPart(nil), manifest.Parts...)
	bad.Parts[0].FileID = "http://forbidden"
	if _, err := NewStoredRARByteSource("torrent-rar|item", bad, []bytesource.ByteSource{
		bytesource.NewMemSource("first", first),
		bytesource.NewMemSource("second", second),
	}); err == nil {
		t.Fatal("URL-shaped file ID accepted")
	}
}

func makeZIP(t testing.TB, files map[string]struct {
	method uint16
	data   string
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := stdzip.NewWriter(&buf)
	for name, file := range files {
		header := &stdzip.FileHeader{Name: name, Method: file.method}
		member, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write([]byte(file.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseZIPVideoEntries(t *testing.T) {
	t.Parallel()
	data := makeZIP(t, map[string]struct {
		method uint16
		data   string
	}{
		"z/movie.mkv": {method: stdzip.Store, data: "stored-video"},
		"a/clip.mp4":  {method: stdzip.Deflate, data: "deflated-video"},
		"readme.txt":  {method: stdzip.Store, data: "ignore"},
	})
	entries, total, err := ParseZIPVideoEntries(context.Background(), bytesource.NewMemSource("fixture", data))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Filename != "a/clip.mp4" || entries[1].Filename != "z/movie.mkv" {
		t.Fatalf("entries = %+v", entries)
	}
	if total != int64(len("stored-video")+len("deflated-video")) {
		t.Fatalf("total = %d", total)
	}
	stored := entries[1]
	if got := string(data[stored.DataOff : stored.DataOff+stored.CompressedSize]); got != "stored-video" {
		t.Fatalf("stored member bytes = %q", got)
	}
}

func TestZIPByteSourceParity(t *testing.T) {
	t.Parallel()
	data := makeZIP(t, map[string]struct {
		method uint16
		data   string
	}{
		"movie.mkv": {method: stdzip.Store, data: strings.Repeat("video", 100)},
		"extra.txt": {method: stdzip.Store, data: "ignore"},
	})
	memEntries, memTotal, err := ParseZIPVideoEntries(context.Background(), bytesource.NewMemSource("memory", data))
	if err != nil {
		t.Fatal(err)
	}
	var chunks []bytesource.Chunk
	starts := []int64{0}
	for off, index := 0, 0; off < len(data); index++ {
		end := min(off+37, len(data))
		chunks = append(chunks, bytesource.Chunk{Index: index, Key: "chunk", DeclaredSize: int64(end - off)})
		starts = append(starts, int64(end))
		off = end
	}
	chunked, err := bytesource.NewChunked(
		"chunked",
		chunks,
		starts,
		int64(len(data)),
		bytesource.Capabilities{RangeSupport: true, ExactSize: true},
		func(_ context.Context, chunk bytesource.Chunk) ([]byte, error) {
			start := starts[chunk.Index]
			end := starts[chunk.Index+1]
			return append([]byte(nil), data[start:end]...), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	chunkEntries, chunkTotal, err := ParseZIPVideoEntries(context.Background(), chunked)
	if err != nil {
		t.Fatal(err)
	}
	if chunkTotal != memTotal || !reflect.DeepEqual(chunkEntries, memEntries) {
		t.Fatalf("chunked = (%+v, %d), memory = (%+v, %d)", chunkEntries, chunkTotal, memEntries, memTotal)
	}
}

func TestParseZIPSubtitleEntries(t *testing.T) {
	t.Parallel()
	data := makeZIP(t, map[string]struct {
		method uint16
		data   string
	}{
		"subs/show.en.srt": {method: stdzip.Deflate, data: "subtitle"},
		"subs/show.ass":    {method: stdzip.Store, data: "dialogue"},
		"movie.mkv":        {method: stdzip.Store, data: "video"},
	})
	entries, total, err := ParseZIPSubtitleEntries(context.Background(), bytesource.NewMemSource("fixture", data))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Filename != "subs/show.ass" || entries[1].Filename != "subs/show.en.srt" || total != int64(len("subtitle")+len("dialogue")) {
		t.Fatalf("entries = %+v, total = %d", entries, total)
	}
	traversal := makeZIP(t, map[string]struct {
		method uint16
		data   string
	}{"../escape.srt": {method: stdzip.Store, data: "x"}})
	if _, _, err := ParseZIPSubtitleEntries(context.Background(), bytesource.NewMemSource("fixture", traversal)); err == nil || !strings.Contains(err.Error(), "unsafe member path") {
		t.Fatalf("traversal error = %v", err)
	}
}

func TestReadZIPSubtitleFailsClosed(t *testing.T) {
	ctx := context.Background()
	if got, err := ReadZIPSubtitle(ctx, bytesource.NewMemSource("stored", []byte("subtitle")),
		ZIPEntry{DataOff: 0, CompressedSize: 8, Uncompressed: 8, Method: stdzip.Store}, 8); err != nil || string(got) != "subtitle" {
		t.Fatalf("stored=%q err=%v", got, err)
	}
	for name, entry := range map[string]ZIPEntry{
		"oversized":   {DataOff: 0, CompressedSize: 8, Uncompressed: 8, Method: stdzip.Store},
		"short":       {DataOff: 0, CompressedSize: 9, Uncompressed: 9, Method: stdzip.Store},
		"unsupported": {DataOff: 0, CompressedSize: 1, Uncompressed: 1, Method: 99},
	} {
		maxBytes := 16
		if name == "oversized" {
			maxBytes = 7
		}
		if _, err := ReadZIPSubtitle(ctx, bytesource.NewMemSource(name, []byte("subtitle")), entry, maxBytes); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ReadZIPSubtitle(canceled, bytesource.NewMemSource("cancel", []byte("subtitle")),
		ZIPEntry{DataOff: 0, CompressedSize: 8, Uncompressed: 8, Method: stdzip.Store}, 8); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestParseZIPVideoEntriesRejectsMalformed(t *testing.T) {
	t.Parallel()
	traversal := makeZIP(t, map[string]struct {
		method uint16
		data   string
	}{"../escape.mkv": {method: stdzip.Store, data: "x"}})
	if _, _, err := ParseZIPVideoEntries(context.Background(), bytesource.NewMemSource("fixture", traversal)); err == nil || !strings.Contains(err.Error(), "unsafe member path") {
		t.Fatalf("traversal error = %v", err)
	}

	zip64 := makeZIP(t, map[string]struct {
		method uint16
		data   string
	}{"movie.mkv": {method: stdzip.Store, data: "x"}})
	eocd := findEOCD(zip64)
	binary.LittleEndian.PutUint32(zip64[eocd+16:], 0xffffffff)
	if _, _, err := ParseZIPVideoEntries(context.Background(), bytesource.NewMemSource("fixture", zip64)); err == nil || !strings.Contains(err.Error(), "ZIP64") {
		t.Fatalf("ZIP64 error = %v", err)
	}

	noVideo := makeZIP(t, map[string]struct {
		method uint16
		data   string
	}{"readme.txt": {method: stdzip.Store, data: "x"}})
	if _, _, err := ParseZIPVideoEntries(context.Background(), bytesource.NewMemSource("fixture", noVideo)); !errors.Is(err, ErrNoVideoEntries) {
		t.Fatalf("no-video error = %v", err)
	}
}

func TestParseZIPVideoEntriesRejectsNested(t *testing.T) {
	t.Parallel()
	inner := makeZIP(t, map[string]struct {
		method uint16
		data   string
	}{"movie.mkv": {method: stdzip.Store, data: "video"}})
	for _, method := range []uint16{stdzip.Store, stdzip.Deflate} {
		outer := makeZIP(t, map[string]struct {
			method uint16
			data   string
		}{
			"movie.mkv":   {method: stdzip.Store, data: "video"},
			"payload.bin": {method: method, data: string(inner)},
		})
		if _, _, err := ParseZIPVideoEntries(context.Background(), bytesource.NewMemSource("fixture", outer)); !errors.Is(err, ErrNestedArchive) {
			t.Fatalf("method %d error = %v", method, err)
		}
	}
}

func TestParseZIPVideoEntriesFailsClosedOnUnreadableCandidate(t *testing.T) {
	t.Parallel()
	outer := makeZIP(t, map[string]struct {
		method uint16
		data   string
	}{
		"movie.mkv":   {method: stdzip.Store, data: "video"},
		"payload.bin": {method: stdzip.Deflate, data: strings.Repeat("payload", 32)},
	})
	reader, err := stdzip.NewReader(bytes.NewReader(outer), int64(len(outer)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range reader.File {
		if file.Name != "payload.bin" {
			continue
		}
		offset, err := file.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		outer[offset] = 0xff // invalid DEFLATE block type; no prefix can be inspected
	}
	if _, _, err := ParseZIPVideoEntries(context.Background(), bytesource.NewMemSource("fixture", outer)); err == nil {
		t.Fatal("unreadable nested candidate accepted")
	}
}

func TestParsersHonorCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ParseRARDataOffset(ctx, bytesource.NewMemSource("fixture", rar3File(0x30, nil)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RAR cancellation error = %v", err)
	}
	zipData := makeZIP(t, map[string]struct {
		method uint16
		data   string
	}{"movie.mkv": {method: stdzip.Store, data: "x"}})
	_, _, err = ParseZIPVideoEntries(ctx, bytesource.NewMemSource("fixture", zipData))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ZIP cancellation error = %v", err)
	}
}

func FuzzParseRARDataOffset(f *testing.F) {
	f.Add(rar3File(0x30, []byte("video")))
	f.Add(rar5File(0, []byte("video")))
	if encrypted, err := os.ReadFile("testdata/ns33-rar5/archive.part1.rar"); err == nil {
		f.Add(encrypted)
	}
	f.Add([]byte("not an archive"))
	f.Add(rar3File(0x30, []byte("PK\x03\x04nested")))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseRARDataOffset(context.Background(), bytesource.NewMemSource("fuzz", data))
		_, _ = ParseStoredRARMember(context.Background(), bytesource.NewMemSource("fuzz", data))
	})
}

func FuzzParseZIPVideoEntries(f *testing.F) {
	f.Add(makeZIP(f, map[string]struct {
		method uint16
		data   string
	}{"movie.mkv": {method: stdzip.Store, data: "video"}}))
	f.Add([]byte("not a zip"))
	inner := makeZIP(f, map[string]struct {
		method uint16
		data   string
	}{"movie.mkv": {method: stdzip.Store, data: "video"}})
	f.Add(makeZIP(f, map[string]struct {
		method uint16
		data   string
	}{"payload.bin": {method: stdzip.Deflate, data: string(inner)}}))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = ParseZIPVideoEntries(context.Background(), bytesource.NewMemSource("fuzz", data))
	})
}

func FuzzParseZIPSubtitleEntries(f *testing.F) {
	f.Add(makeZIP(f, map[string]struct {
		method uint16
		data   string
	}{"show.en.srt": {method: stdzip.Deflate, data: "subtitle"}}))
	f.Add([]byte("not a zip"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = ParseZIPSubtitleEntries(context.Background(), bytesource.NewMemSource("fuzz", data))
	})
}
