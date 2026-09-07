package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"path"
	"strings"
	"unicode/utf16"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

const (
	sevenZipSignatureHeaderSize = 32
	maxSevenZipHeaderBytes      = maxZIPCentralDirectory
	maxSevenZipEntries          = maxZIPCentralEntries
)

var (
	sevenZipSignature = []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}
	sevenZipCopyID    = []byte{0x00}
	sevenZipAESID     = []byte{0x06, 0xf1, 0x07, 0x01}

	ErrSolid7z           = errors.New("archive: solid 7z not supported")
	ErrCompressed7z      = errors.New("archive: compressed 7z not supported")
	ErrEncryptedHeader7z = errors.New("archive: encrypted 7z header not supported")
)

// SevenZipEntry identifies one uncompressed member span in a Copy-coded 7z
// archive. Offsets are relative to the complete archive ByteSource.
type SevenZipEntry struct {
	Name       string
	DataOffset int64
	Size       int64
}

// ParseSevenZipCopyEntries parses bounded metadata for a non-solid 7z archive
// whose files use only the Copy coder. It never reads packed member bytes.
func ParseSevenZipCopyEntries(ctx context.Context, src bytesource.ByteSource) ([]SevenZipEntry, error) {
	if src == nil || src.Size() < sevenZipSignatureHeaderSize {
		return nil, fmt.Errorf("7z: source too short")
	}
	start, err := readAt(ctx, src, 0, sevenZipSignatureHeaderSize)
	if err != nil {
		return nil, fmt.Errorf("7z: fetch signature header: %w", err)
	}
	if !bytes.Equal(start[:len(sevenZipSignature)], sevenZipSignature) {
		return nil, fmt.Errorf("7z: invalid signature")
	}
	if start[6] != 0 {
		return nil, fmt.Errorf("7z: unsupported major version %d", start[6])
	}
	if crc32.ChecksumIEEE(start[12:]) != binary.LittleEndian.Uint32(start[8:12]) {
		return nil, fmt.Errorf("7z: start header CRC mismatch")
	}
	nextOffset := binary.LittleEndian.Uint64(start[12:20])
	nextSize := binary.LittleEndian.Uint64(start[20:28])
	if nextSize == 0 || nextSize > maxSevenZipHeaderBytes {
		return nil, fmt.Errorf("7z: next header exceeds limit")
	}
	if nextOffset > math.MaxInt64-sevenZipSignatureHeaderSize ||
		nextSize > math.MaxInt64 ||
		int64(nextOffset)+sevenZipSignatureHeaderSize > src.Size() ||
		int64(nextSize) > src.Size()-(int64(nextOffset)+sevenZipSignatureHeaderSize) {
		return nil, fmt.Errorf("7z: next header exceeds source")
	}
	header, err := readAt(ctx, src, int64(nextOffset)+sevenZipSignatureHeaderSize, int64(nextSize))
	if err != nil {
		return nil, fmt.Errorf("7z: fetch next header: %w", err)
	}
	if crc32.ChecksumIEEE(header) != binary.LittleEndian.Uint32(start[28:32]) {
		return nil, fmt.Errorf("7z: next header CRC mismatch")
	}
	return parseSevenZipHeader(ctx, header, nextOffset)
}

type sevenZipStreams struct {
	packPos   uint64
	packSizes []uint64
	folders   []sevenZipFolder
}

type sevenZipFolder struct {
	methods       [][]byte
	numIn         uint64
	numOut        uint64
	unpackSizes   []uint64
	numSubstreams uint64
	crcDefined    bool
}

type sevenZipFiles struct {
	names []string
	empty []bool
}

func parseSevenZipHeader(ctx context.Context, data []byte, packedLimit uint64) ([]SevenZipEntry, error) {
	r := sevenZipReader{ctx: ctx, data: data}
	kind, err := r.byte()
	if err != nil {
		return nil, err
	}
	if kind == 0x17 {
		streams, err := r.streamsInfo()
		if err != nil {
			return nil, err
		}
		if err := validateSevenZipCoders(streams.folders, true); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: encoded next header", ErrCompressed7z)
	}
	if kind != 0x01 {
		return nil, fmt.Errorf("7z: expected header, got 0x%02x", kind)
	}

	var streams *sevenZipStreams
	var files *sevenZipFiles
	for {
		id, err := r.byte()
		if err != nil {
			return nil, err
		}
		switch id {
		case 0x00:
			if r.remaining() != 0 {
				return nil, fmt.Errorf("7z: trailing next-header bytes")
			}
			return buildSevenZipEntries(streams, files, packedLimit)
		case 0x02:
			if err := r.archiveProperties(); err != nil {
				return nil, err
			}
		case 0x03:
			return nil, fmt.Errorf("7z: additional streams not supported")
		case 0x04:
			if streams != nil {
				return nil, fmt.Errorf("7z: duplicate main streams info")
			}
			parsed, err := r.streamsInfoBody()
			if err != nil {
				return nil, err
			}
			streams = &parsed
		case 0x05:
			if files != nil {
				return nil, fmt.Errorf("7z: duplicate files info")
			}
			parsed, err := r.filesInfo()
			if err != nil {
				return nil, err
			}
			files = &parsed
		default:
			return nil, fmt.Errorf("7z: unexpected header property 0x%02x", id)
		}
	}
}

func buildSevenZipEntries(streams *sevenZipStreams, files *sevenZipFiles, packedLimit uint64) ([]SevenZipEntry, error) {
	if streams == nil || files == nil || len(files.names) == 0 {
		return nil, fmt.Errorf("7z: incomplete header")
	}
	if err := validateSevenZipCoders(streams.folders, false); err != nil {
		return nil, err
	}
	nonEmpty := 0
	for _, empty := range files.empty {
		if !empty {
			nonEmpty++
		}
	}
	if nonEmpty != len(streams.folders) || len(streams.packSizes) != len(streams.folders) {
		return nil, fmt.Errorf("%w: file/folder layout", ErrSolid7z)
	}
	if streams.packPos > packedLimit {
		return nil, fmt.Errorf("7z: packed streams exceed next-header offset")
	}
	packedSize := uint64(0)
	for _, size := range streams.packSizes {
		if size > packedLimit-streams.packPos-packedSize {
			return nil, fmt.Errorf("7z: packed streams exceed next-header offset")
		}
		packedSize += size
	}

	packOffset := uint64(sevenZipSignatureHeaderSize)
	if streams.packPos > math.MaxUint64-packOffset {
		return nil, fmt.Errorf("7z: pack offset overflow")
	}
	packOffset += streams.packPos
	entries := make([]SevenZipEntry, 0, nonEmpty)
	folderIndex := 0
	for i, name := range files.names {
		if files.empty[i] {
			continue
		}
		folder := streams.folders[folderIndex]
		packSize := streams.packSizes[folderIndex]
		if len(folder.unpackSizes) != 1 || folder.unpackSizes[0] != packSize || packSize == 0 {
			return nil, fmt.Errorf("7z: contradictory Copy stream sizes")
		}
		clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
			return nil, fmt.Errorf("7z: unsafe member path")
		}
		if packOffset > math.MaxInt64 || packSize > math.MaxInt64 || packSize > math.MaxUint64-packOffset {
			return nil, fmt.Errorf("7z: member span overflow")
		}
		entries = append(entries, SevenZipEntry{Name: clean, DataOffset: int64(packOffset), Size: int64(packSize)})
		packOffset += packSize
		folderIndex++
	}
	return entries, nil
}

func validateSevenZipCoders(folders []sevenZipFolder, header bool) error {
	if len(folders) == 0 || len(folders) > maxSevenZipEntries {
		return fmt.Errorf("7z: invalid folder count")
	}
	for _, folder := range folders {
		for _, method := range folder.methods {
			if bytes.Equal(method, sevenZipAESID) {
				if header {
					return ErrEncryptedHeader7z
				}
				return ErrCompressed7z
			}
		}
		if len(folder.methods) != 1 || !bytes.Equal(folder.methods[0], sevenZipCopyID) ||
			folder.numIn != 1 || folder.numOut != 1 {
			return ErrCompressed7z
		}
		if folder.numSubstreams != 1 {
			return ErrSolid7z
		}
	}
	return nil
}

type sevenZipReader struct {
	ctx  context.Context
	data []byte
	pos  int
}

func (r *sevenZipReader) remaining() int { return len(r.data) - r.pos }

func (r *sevenZipReader) check() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if r.pos < 0 || r.pos > len(r.data) {
		return fmt.Errorf("7z: parser position out of bounds")
	}
	return nil
}

func (r *sevenZipReader) byte() (byte, error) {
	if err := r.check(); err != nil {
		return 0, err
	}
	if r.pos == len(r.data) {
		return 0, fmt.Errorf("7z: truncated header")
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

func (r *sevenZipReader) bytes(n uint64) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	if n > uint64(len(r.data)-r.pos) {
		return nil, fmt.Errorf("7z: truncated header")
	}
	b := r.data[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return b, nil
}

func (r *sevenZipReader) number() (uint64, error) {
	first, err := r.byte()
	if err != nil {
		return 0, err
	}
	var value uint64
	mask := byte(0x80)
	for i := 0; i < 8; i++ {
		if first&mask == 0 {
			return value | uint64(first&(mask-1))<<uint(8*i), nil
		}
		next, err := r.byte()
		if err != nil {
			return 0, err
		}
		value |= uint64(next) << uint(8*i)
		mask >>= 1
	}
	return value, nil
}

func (r *sevenZipReader) count(what string) (int, error) {
	n, err := r.number()
	if err != nil {
		return 0, fmt.Errorf("7z: read %s: %w", what, err)
	}
	if n > maxSevenZipEntries {
		return 0, fmt.Errorf("7z: %s exceeds limit", what)
	}
	return int(n), nil
}

func (r *sevenZipReader) archiveProperties() error {
	for {
		id, err := r.byte()
		if err != nil {
			return err
		}
		if id == 0 {
			return nil
		}
		size, err := r.number()
		if err != nil {
			return err
		}
		if _, err := r.bytes(size); err != nil {
			return err
		}
	}
}

func (r *sevenZipReader) streamsInfo() (sevenZipStreams, error) {
	return r.streamsInfoBody()
}

func (r *sevenZipReader) streamsInfoBody() (sevenZipStreams, error) {
	var streams sevenZipStreams
	id, err := r.byte()
	if err != nil {
		return streams, err
	}
	if id == 0x06 {
		if err := r.packInfo(&streams); err != nil {
			return streams, err
		}
		id, err = r.byte()
		if err != nil {
			return streams, err
		}
	}
	if id == 0x07 {
		if err := r.unpackInfo(&streams); err != nil {
			return streams, err
		}
		id, err = r.byte()
		if err != nil {
			return streams, err
		}
	}
	if id == 0x08 {
		if err := r.subStreamsInfo(&streams); err != nil {
			return streams, err
		}
		id, err = r.byte()
		if err != nil {
			return streams, err
		}
	}
	if id != 0 {
		return streams, fmt.Errorf("7z: unexpected streams property 0x%02x", id)
	}
	return streams, nil
}

func (r *sevenZipReader) packInfo(streams *sevenZipStreams) error {
	var err error
	if streams.packPos, err = r.number(); err != nil {
		return err
	}
	count, err := r.count("pack stream count")
	if err != nil || count == 0 {
		return fmt.Errorf("7z: invalid pack stream count")
	}
	id, err := r.byte()
	if err != nil {
		return err
	}
	if id != 0x09 {
		return fmt.Errorf("7z: pack sizes missing")
	}
	streams.packSizes = make([]uint64, count)
	for i := range streams.packSizes {
		if streams.packSizes[i], err = r.number(); err != nil {
			return err
		}
	}
	id, err = r.byte()
	if err != nil {
		return err
	}
	if id == 0x0a {
		if _, err := r.digests(count); err != nil {
			return err
		}
		id, err = r.byte()
		if err != nil {
			return err
		}
	}
	if id != 0 {
		return fmt.Errorf("7z: unexpected PackInfo property 0x%02x", id)
	}
	return nil
}

func (r *sevenZipReader) unpackInfo(streams *sevenZipStreams) error {
	id, err := r.byte()
	if err != nil {
		return err
	}
	if id != 0x0b {
		return fmt.Errorf("7z: folders missing")
	}
	count, err := r.count("folder count")
	if err != nil || count == 0 {
		return fmt.Errorf("7z: invalid folder count")
	}
	external, err := r.byte()
	if err != nil {
		return err
	}
	if external != 0 {
		return fmt.Errorf("7z: external folders not supported")
	}
	streams.folders = make([]sevenZipFolder, count)
	var totalCoders, totalIn, totalOut uint64
	for i := range streams.folders {
		folder, err := r.folder()
		if err != nil {
			return err
		}
		coders := uint64(len(folder.methods))
		if coders > maxSevenZipEntries-totalCoders ||
			folder.numIn > maxSevenZipEntries-totalIn ||
			folder.numOut > maxSevenZipEntries-totalOut {
			return fmt.Errorf("7z: aggregate coder streams exceed limit")
		}
		totalCoders += coders
		totalIn += folder.numIn
		totalOut += folder.numOut
		streams.folders[i] = folder
	}
	id, err = r.byte()
	if err != nil {
		return err
	}
	if id != 0x0c {
		return fmt.Errorf("7z: coder unpack sizes missing")
	}
	for i := range streams.folders {
		folder := &streams.folders[i]
		folder.unpackSizes = make([]uint64, int(folder.numOut))
		for j := range folder.unpackSizes {
			if folder.unpackSizes[j], err = r.number(); err != nil {
				return err
			}
		}
		folder.numSubstreams = 1
	}
	id, err = r.byte()
	if err != nil {
		return err
	}
	if id == 0x0a {
		defined, err := r.digests(count)
		if err != nil {
			return err
		}
		for i := range streams.folders {
			streams.folders[i].crcDefined = defined[i]
		}
		id, err = r.byte()
		if err != nil {
			return err
		}
	}
	if id != 0 {
		return fmt.Errorf("7z: unexpected UnpackInfo property 0x%02x", id)
	}
	return nil
}

func (r *sevenZipReader) folder() (sevenZipFolder, error) {
	var folder sevenZipFolder
	count, err := r.count("coder count")
	if err != nil || count == 0 {
		return folder, fmt.Errorf("7z: invalid coder count")
	}
	for i := 0; i < count; i++ {
		flags, err := r.byte()
		if err != nil {
			return folder, err
		}
		idSize := uint64(flags & 0x0f)
		if idSize == 0 || flags&0x80 != 0 {
			return folder, fmt.Errorf("7z: invalid coder flags")
		}
		method, err := r.bytes(idSize)
		if err != nil {
			return folder, err
		}
		folder.methods = append(folder.methods, append([]byte(nil), method...))
		numIn, numOut := uint64(1), uint64(1)
		if flags&0x10 != 0 {
			if numIn, err = r.number(); err != nil {
				return folder, err
			}
			if numOut, err = r.number(); err != nil {
				return folder, err
			}
		}
		if numIn == 0 || numOut == 0 || numIn > maxSevenZipEntries || numOut > maxSevenZipEntries ||
			folder.numIn > maxSevenZipEntries-numIn || folder.numOut > maxSevenZipEntries-numOut {
			return folder, fmt.Errorf("7z: coder streams exceed limit")
		}
		folder.numIn += numIn
		folder.numOut += numOut
		if flags&0x20 != 0 {
			propertiesSize, err := r.number()
			if err != nil {
				return folder, err
			}
			if _, err := r.bytes(propertiesSize); err != nil {
				return folder, err
			}
		}
	}
	bindPairs := folder.numOut - 1
	for i := uint64(0); i < bindPairs; i++ {
		if _, err := r.number(); err != nil {
			return folder, err
		}
		if _, err := r.number(); err != nil {
			return folder, err
		}
	}
	packedStreams := folder.numIn - bindPairs
	if packedStreams == 0 || packedStreams > maxSevenZipEntries {
		return folder, fmt.Errorf("7z: invalid packed stream count")
	}
	if packedStreams > 1 {
		for i := uint64(0); i < packedStreams; i++ {
			if _, err := r.number(); err != nil {
				return folder, err
			}
		}
	}
	return folder, nil
}

func (r *sevenZipReader) subStreamsInfo(streams *sevenZipStreams) error {
	id, err := r.byte()
	if err != nil {
		return err
	}
	if id == 0x0d {
		for i := range streams.folders {
			n, err := r.number()
			if err != nil {
				return err
			}
			streams.folders[i].numSubstreams = n
			if n != 1 {
				return ErrSolid7z
			}
		}
		id, err = r.byte()
		if err != nil {
			return err
		}
	}
	if id == 0x09 {
		id, err = r.byte()
		if err != nil {
			return err
		}
	}
	if id == 0x0a {
		count := 0
		for _, folder := range streams.folders {
			if !folder.crcDefined {
				count++
			}
		}
		if _, err := r.digests(count); err != nil {
			return err
		}
		id, err = r.byte()
		if err != nil {
			return err
		}
	}
	if id != 0 {
		return fmt.Errorf("7z: unexpected SubStreamsInfo property 0x%02x", id)
	}
	return nil
}

func (r *sevenZipReader) digests(count int) ([]bool, error) {
	defined := make([]bool, count)
	all, err := r.byte()
	if err != nil {
		return nil, err
	}
	if all != 0 {
		for i := range defined {
			defined[i] = true
		}
	} else {
		bits, err := r.bytes(uint64((count + 7) / 8))
		if err != nil {
			return nil, err
		}
		for i := range defined {
			defined[i] = bits[i/8]&(0x80>>uint(i%8)) != 0
		}
	}
	for _, present := range defined {
		if present {
			if _, err := r.bytes(4); err != nil {
				return nil, err
			}
		}
	}
	return defined, nil
}

func (r *sevenZipReader) filesInfo() (sevenZipFiles, error) {
	var files sevenZipFiles
	count, err := r.count("file count")
	if err != nil || count == 0 {
		return files, fmt.Errorf("7z: invalid file count")
	}
	files.empty = make([]bool, count)
	for {
		id, err := r.byte()
		if err != nil {
			return files, err
		}
		if id == 0 {
			break
		}
		size, err := r.number()
		if err != nil {
			return files, err
		}
		property, err := r.bytes(size)
		if err != nil {
			return files, err
		}
		switch id {
		case 0x0e:
			if len(property) != (count+7)/8 {
				return files, fmt.Errorf("7z: invalid empty-stream vector")
			}
			for i := range files.empty {
				files.empty[i] = property[i/8]&(0x80>>uint(i%8)) != 0
			}
		case 0x11:
			names, err := sevenZipNames(property, count)
			if err != nil {
				return files, err
			}
			files.names = names
		}
	}
	if len(files.names) != count {
		return files, fmt.Errorf("7z: file names missing")
	}
	return files, nil
}

func sevenZipNames(data []byte, count int) ([]string, error) {
	if len(data) < 1 || data[0] != 0 || (len(data)-1)%2 != 0 {
		return nil, fmt.Errorf("7z: invalid names property")
	}
	units := make([]uint16, (len(data)-1)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[1+i*2:])
	}
	names := make([]string, 0, count)
	start := 0
	for i, unit := range units {
		if unit != 0 {
			continue
		}
		names = append(names, string(utf16.Decode(units[start:i])))
		start = i + 1
	}
	if len(names) != count || start != len(units) {
		return nil, fmt.Errorf("7z: file-name count mismatch")
	}
	return names, nil
}
