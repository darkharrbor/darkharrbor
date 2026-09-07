package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

var (
	rar3Sig = []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x00}
	rar5Sig = []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x01, 0x00}
)

var (
	ErrCompressedRAR          = errors.New("archive: compressed RAR not supported")
	ErrCompressedEncryptedRAR = errors.New("archive: compressed and encrypted RAR not supported")
	ErrRARPasswordRequired    = errors.New("archive: RAR password required")
	ErrRARBadPassword         = errors.New("archive: incorrect RAR password")
)

// RAREncryption is the password-free encryption metadata stored in a RAR file
// header. Version is 3 or 5; the byte slices are bounded by their format sizes.
type RAREncryption struct {
	Version       int    `json:"version"`
	Salt          []byte `json:"salt,omitempty"`
	IV            []byte `json:"iv,omitempty"`
	KDFLog2       uint8  `json:"kdf_log2,omitempty"`
	PasswordCheck []byte `json:"password_check,omitempty"`
}

// StoredRARMember describes the first store-mode member in one RAR volume.
// DataOffset and DataSize identify only logical payload bytes. PackedSize is
// the stored ciphertext span and equals DataSize for unencrypted members.
type StoredRARMember struct {
	Name       string
	DataOffset int64
	DataSize   int64
	PackedSize int64
	Encryption *RAREncryption
}

// StoredRARPart is the URL-free persisted description of one provider-hosted
// RAR volume.
type StoredRARPart struct {
	FileID      string `json:"file_id"`
	ArchiveSize int64  `json:"archive_size"`
	DataOffset  int64  `json:"data_offset"`
	DataSize    int64  `json:"data_size"`
}

// StoredRARManifest maps a logical media member onto ordered RAR volumes.
type StoredRARManifest struct {
	MemberName string          `json:"member_name"`
	TotalSize  int64           `json:"total_size"`
	Parts      []StoredRARPart `json:"parts"`
}

// ParseRARDataOffset returns the byte offset at which the first stored member's
// data begins. Compressed RAR3 and RAR5 members are rejected.
func ParseRARDataOffset(ctx context.Context, src bytesource.ByteSource) (int64, error) {
	member, err := ParseStoredRARMember(ctx, src)
	if err != nil {
		return 0, err
	}
	return member.DataOffset, nil
}

// ParseStoredRARMember returns the bounded payload span for the first
// store-mode member in a RAR3 or RAR5 volume.
func ParseStoredRARMember(ctx context.Context, src bytesource.ByteSource) (StoredRARMember, error) {
	data, err := readPrefix(ctx, src, maxRARHeaderBytes)
	if err != nil {
		return StoredRARMember{}, err
	}
	switch {
	case len(data) >= len(rar3Sig) && bytes.Equal(data[:len(rar3Sig)], rar3Sig):
		return parseRAR3StoredMember(data)
	case len(data) >= len(rar5Sig) && bytes.Equal(data[:len(rar5Sig)], rar5Sig):
		return parseRAR5StoredMember(data)
	default:
		return StoredRARMember{}, fmt.Errorf("rar: unrecognised RAR signature")
	}
}

func parseRAR3StoredMember(data []byte) (StoredRARMember, error) {
	if len(data) < len(rar3Sig) {
		return StoredRARMember{}, fmt.Errorf("rar3: data too short for signature check")
	}
	if !bytes.Equal(data[:len(rar3Sig)], rar3Sig) {
		return StoredRARMember{}, fmt.Errorf("rar3: invalid signature")
	}

	offset := len(rar3Sig)
	for offset < len(data) {
		if offset+7 > len(data) {
			break
		}
		headType := data[offset+2]
		headSize := int(binary.LittleEndian.Uint16(data[offset+5 : offset+7]))
		if headSize < 7 {
			return StoredRARMember{}, fmt.Errorf("rar3: invalid block header size %d at offset %d", headSize, offset)
		}
		if headType == 0x74 {
			const (
				methodOffset = 25
				nameLenOff   = 26
				nameOff      = 32
			)
			if headSize < nameOff || offset+headSize > len(data) {
				return StoredRARMember{}, fmt.Errorf("rar3: truncated file header at offset %d", offset)
			}
			flags := binary.LittleEndian.Uint16(data[offset+3 : offset+5])
			encrypted := flags&0x0004 != 0
			method := data[offset+methodOffset]
			if method != 0x30 {
				if encrypted {
					return StoredRARMember{}, fmt.Errorf("%w (RAR3 method 0x%02x)", ErrCompressedEncryptedRAR, method)
				}
				return StoredRARMember{}, fmt.Errorf("%w (RAR3 method 0x%02x); store mode only", ErrCompressedRAR, method)
			}
			packed := uint64(binary.LittleEndian.Uint32(data[offset+7 : offset+11]))
			unpacked := uint64(binary.LittleEndian.Uint32(data[offset+11 : offset+15]))
			nameLen := int(binary.LittleEndian.Uint16(data[offset+nameLenOff : offset+nameLenOff+2]))
			nameStart := offset + nameOff
			if flags&0x0100 != 0 {
				if headSize < nameOff+8 {
					return StoredRARMember{}, fmt.Errorf("rar3: truncated large-file header at offset %d", offset)
				}
				packed |= uint64(binary.LittleEndian.Uint32(data[nameStart:nameStart+4])) << 32
				unpacked |= uint64(binary.LittleEndian.Uint32(data[nameStart+4:nameStart+8])) << 32
				nameStart += 8
			}
			nameEnd := nameStart + nameLen
			if nameLen < 0 || nameStart > offset+headSize || nameEnd > offset+headSize {
				return StoredRARMember{}, fmt.Errorf("rar3: invalid member name length %d", nameLen)
			}
			var encryption *RAREncryption
			if encrypted {
				encryption = &RAREncryption{Version: 3}
				if flags&0x0400 != 0 {
					if nameEnd+8 > offset+headSize {
						return StoredRARMember{}, fmt.Errorf("rar3: truncated encryption salt")
					}
					encryption.Salt = append([]byte(nil), data[nameEnd:nameEnd+8]...)
				}
				if packed == 0 {
					return StoredRARMember{}, fmt.Errorf("rar3: encrypted data is empty")
				}
			} else {
				unpacked = packed
			}
			return validateStoredRARMember(StoredRARMember{
				Name:       string(data[nameStart:nameEnd]),
				DataOffset: int64(offset + headSize),
				DataSize:   int64(unpacked),
				PackedSize: int64(packed),
				Encryption: encryption,
			})
		}
		if headSize > len(data)-offset {
			break
		}
		offset += headSize
	}
	return StoredRARMember{}, fmt.Errorf("rar3: no file header found in first %d bytes", len(data))
}

func readVint(data []byte, offset int) (uint64, int, error) {
	var value uint64
	var shift uint
	for bytesRead := 1; bytesRead <= 10; bytesRead++ {
		if offset >= len(data) {
			return 0, 0, fmt.Errorf("rar5: vint read out of bounds at %d", offset)
		}
		b := data[offset]
		offset++
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, bytesRead, nil
		}
		shift += 7
	}
	return 0, 0, fmt.Errorf("rar5: vint too long")
}

func parseRAR5StoredMember(data []byte) (StoredRARMember, error) {
	if len(data) < len(rar5Sig) {
		return StoredRARMember{}, fmt.Errorf("rar5: data too short for signature check")
	}
	if !bytes.Equal(data[:len(rar5Sig)], rar5Sig) {
		return StoredRARMember{}, fmt.Errorf("rar5: invalid signature")
	}

	pos := len(rar5Sig)
	for pos < len(data) {
		blockStart := pos
		if pos+4 > len(data) {
			break
		}
		pos += 4
		headerSize, sizeLen, err := readVint(data, pos)
		if err != nil {
			return StoredRARMember{}, fmt.Errorf("rar5: read header_size at block %d: %w", blockStart, err)
		}
		if headerSize == 0 {
			break
		}
		pos += sizeLen
		headerStart := pos
		if headerSize > uint64(len(data)-headerStart) {
			break
		}
		headerEnd := headerStart + int(headerSize)
		readHeaderVint := func(field string) (uint64, int, error) {
			if pos >= headerEnd {
				return 0, 0, fmt.Errorf("rar5: read %s at block %d beyond header", field, blockStart)
			}
			value, n, err := readVint(data, pos)
			if err != nil || pos+n > headerEnd {
				return 0, 0, fmt.Errorf("rar5: read %s at block %d: truncated", field, blockStart)
			}
			return value, n, nil
		}
		headerType, n, err := readHeaderVint("header_type")
		if err != nil {
			return StoredRARMember{}, err
		}
		pos += n
		headerFlags, n, err := readHeaderVint("header_flags")
		if err != nil {
			return StoredRARMember{}, err
		}
		pos += n
		hasExtra := headerFlags&0x0001 != 0
		hasData := headerFlags&0x0002 != 0
		var extraSize uint64
		if hasExtra {
			extraSize, n, err = readHeaderVint("extra_size")
			if err != nil {
				return StoredRARMember{}, err
			}
			pos += n
			if extraSize > uint64(headerEnd-pos) {
				return StoredRARMember{}, fmt.Errorf("rar5: extra area crosses header at block %d", blockStart)
			}
		}
		var dataSize uint64
		if hasData {
			dataSize, n, err = readHeaderVint("data_area_size")
			if err != nil {
				return StoredRARMember{}, err
			}
			pos += n
		}
		if headerType == 2 {
			fileFlags, n, err := readHeaderVint("file_flags")
			if err != nil {
				return StoredRARMember{}, err
			}
			pos += n
			if fileFlags&0x0008 != 0 {
				return StoredRARMember{}, fmt.Errorf("rar5: unknown unpacked size is not streamable")
			}
			unpackedSize, n, err := readHeaderVint("unpacked_size")
			if err != nil {
				return StoredRARMember{}, err
			}
			pos += n
			_, n, err = readHeaderVint("attributes")
			if err != nil {
				return StoredRARMember{}, err
			}
			pos += n
			if fileFlags&0x0002 != 0 {
				if pos+4 > headerEnd {
					return StoredRARMember{}, fmt.Errorf("rar5: mtime at block %d crosses header", blockStart)
				}
				pos += 4
			}
			if fileFlags&0x0004 != 0 {
				if pos+4 > headerEnd {
					return StoredRARMember{}, fmt.Errorf("rar5: file_crc32 at block %d crosses header", blockStart)
				}
				pos += 4
			}
			compressionInfo, n, err := readHeaderVint("compression_info")
			if err != nil {
				return StoredRARMember{}, err
			}
			pos += n
			method := compressionInfo >> 7 & 0x0f
			_, n, err = readHeaderVint("host_os")
			if err != nil {
				return StoredRARMember{}, err
			}
			pos += n
			nameSize, n, err := readHeaderVint("name_size")
			if err != nil {
				return StoredRARMember{}, err
			}
			pos += n
			extraStart := headerEnd - int(extraSize)
			if pos > extraStart || nameSize > uint64(extraStart-pos) {
				return StoredRARMember{}, fmt.Errorf("rar5: member name crosses base header at block %d", blockStart)
			}
			name := string(data[pos : pos+int(nameSize)])
			encryption, err := parseRAR5Encryption(data[extraStart:headerEnd])
			if err != nil {
				return StoredRARMember{}, err
			}
			if method != 0 {
				if encryption != nil {
					return StoredRARMember{}, fmt.Errorf("%w (RAR5 method %d)", ErrCompressedEncryptedRAR, method)
				}
				return StoredRARMember{}, fmt.Errorf("%w (RAR5 method %d); store mode only", ErrCompressedRAR, method)
			}
			if encryption != nil && dataSize == 0 {
				return StoredRARMember{}, fmt.Errorf("rar5: encrypted data is empty")
			}
			if encryption == nil {
				unpackedSize = dataSize
			}
			return validateStoredRARMember(StoredRARMember{
				Name:       name,
				DataOffset: int64(headerEnd),
				DataSize:   int64(unpackedSize),
				PackedSize: int64(dataSize),
				Encryption: encryption,
			})
		}
		if headerType == 5 {
			break
		}
		pos = headerEnd
		if hasData {
			if dataSize > uint64(len(data)-headerEnd) {
				break
			}
			pos += int(dataSize)
		}
	}
	return StoredRARMember{}, fmt.Errorf("rar5: no file block found in first %d bytes", len(data))
}

func parseRAR5Encryption(extra []byte) (*RAREncryption, error) {
	var encryption *RAREncryption
	for pos := 0; pos < len(extra); {
		recordSize, n, err := readVint(extra, pos)
		if err != nil || recordSize == 0 || recordSize > uint64(len(extra)-pos-n) {
			return nil, fmt.Errorf("rar5: malformed extra record")
		}
		pos += n
		recordEnd := pos + int(recordSize)
		recordType, n, err := readVint(extra[:recordEnd], pos)
		if err != nil {
			return nil, fmt.Errorf("rar5: malformed extra record type")
		}
		pos += n
		if recordType != 1 {
			pos = recordEnd
			continue
		}
		if encryption != nil {
			return nil, fmt.Errorf("rar5: duplicate encryption record")
		}
		version, n, err := readVint(extra[:recordEnd], pos)
		if err != nil || version != 0 {
			return nil, fmt.Errorf("rar5: unsupported encryption version")
		}
		pos += n
		flags, n, err := readVint(extra[:recordEnd], pos)
		if err != nil || flags&^uint64(3) != 0 {
			return nil, fmt.Errorf("rar5: unsupported encryption flags")
		}
		pos += n
		if pos >= recordEnd {
			return nil, fmt.Errorf("rar5: truncated encryption KDF")
		}
		kdfLog2 := extra[pos]
		pos++
		if kdfLog2 > 24 || recordEnd-pos < 32 {
			return nil, fmt.Errorf("rar5: invalid encryption parameters")
		}
		encryption = &RAREncryption{
			Version: 5,
			KDFLog2: kdfLog2,
			Salt:    append([]byte(nil), extra[pos:pos+16]...),
			IV:      append([]byte(nil), extra[pos+16:pos+32]...),
		}
		pos += 32
		if flags&1 != 0 {
			if recordEnd-pos < 12 {
				return nil, fmt.Errorf("rar5: truncated password check")
			}
			check := extra[pos : pos+8]
			sum := sha256.Sum256(check)
			if !bytes.Equal(extra[pos+8:pos+12], sum[:4]) {
				return nil, fmt.Errorf("rar5: invalid password-check checksum")
			}
			encryption.PasswordCheck = append([]byte(nil), check...)
			pos += 12
		}
		if pos != recordEnd {
			return nil, fmt.Errorf("rar5: trailing encryption parameters")
		}
	}
	return encryption, nil
}

func validateStoredRARMember(member StoredRARMember) (StoredRARMember, error) {
	if member.DataOffset < 0 || member.DataSize <= 0 || member.PackedSize <= 0 {
		return StoredRARMember{}, fmt.Errorf("rar: invalid stored member span offset=%d size=%d", member.DataOffset, member.DataSize)
	}
	if member.Name != "" {
		clean := path.Clean(strings.ReplaceAll(member.Name, "\\", "/"))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
			return StoredRARMember{}, fmt.Errorf("rar: unsafe member path")
		}
		member.Name = clean
	}
	return member, nil
}

// NewStoredRARByteSource exposes the concatenated payload spans of a persisted
// manifest as one exact logical media ByteSource.
func NewStoredRARByteSource(key string, manifest StoredRARManifest, sources []bytesource.ByteSource) (bytesource.ByteSource, error) {
	if key == "" || strings.Contains(key, "://") {
		return nil, fmt.Errorf("rar: invalid source key")
	}
	if len(manifest.Parts) == 0 || len(sources) != len(manifest.Parts) {
		return nil, fmt.Errorf("rar: manifest/source count mismatch")
	}
	var total int64
	starts := make([]int64, len(manifest.Parts)+1)
	for i, part := range manifest.Parts {
		if sources[i] == nil || part.FileID == "" || strings.Contains(part.FileID, "://") ||
			part.ArchiveSize <= 0 || part.DataOffset < 0 || part.DataSize <= 0 ||
			part.DataOffset > part.ArchiveSize || part.DataSize > part.ArchiveSize-part.DataOffset ||
			sources[i].Size() != part.ArchiveSize {
			return nil, fmt.Errorf("rar: invalid manifest part %d", i)
		}
		if total > int64(^uint64(0)>>1)-part.DataSize {
			return nil, fmt.Errorf("rar: total size overflow")
		}
		starts[i] = total
		total += part.DataSize
	}
	starts[len(manifest.Parts)] = total
	if manifest.TotalSize != total {
		return nil, fmt.Errorf("rar: manifest total %d does not match parts %d", manifest.TotalSize, total)
	}
	return &storedRARSource{key: key, manifest: manifest, sources: sources, starts: starts}, nil
}

type storedRARSource struct {
	key      string
	manifest StoredRARManifest
	sources  []bytesource.ByteSource
	starts   []int64
}

func (s *storedRARSource) Key() string { return s.key }
func (s *storedRARSource) Size() int64 { return s.manifest.TotalSize }
func (s *storedRARSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: true, TailCost: bytesource.TailCostCheap}
}

func (s *storedRARSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, bytesource.ErrNegativeOffset
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off >= s.Size() {
		return 0, io.EOF
	}
	n := 0
	for i, part := range s.manifest.Parts {
		partStart, partEnd := s.starts[i], s.starts[i+1]
		if off >= partEnd {
			continue
		}
		local := int64(0)
		if off > partStart {
			local = off - partStart
		}
		want := min(int64(len(p)-n), partEnd-partStart-local)
		got, err := s.sources[i].ReadAt(ctx, p[n:n+int(want)], part.DataOffset+local)
		n += got
		off += int64(got)
		if err != nil && !(errors.Is(err, io.EOF) && int64(got) == want) {
			return n, err
		}
		if int64(got) != want {
			return n, io.ErrUnexpectedEOF
		}
		if n == len(p) {
			return n, nil
		}
	}
	return n, io.EOF
}
