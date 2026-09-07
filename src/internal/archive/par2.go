package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// PAR2 index files (Main + FileDesc + IFSC packets, no recovery slices) are small;
// bound the prefix fetch generously without ever reading a full recovery
// volume. LC-01/C0.3 bounded-preparation contract: metadata only, no
// extraction, cancellable, capped.
const maxPAR2PrefixBytes = 8 << 20

// IFSC packets carry one 16-byte MD5 plus one 4-byte CRC per source block.
// Persist only the MD5 material and reject larger indexes rather than keeping
// a truncated proof domain.
const maxPAR2IFSCBlocks = 262_144

const par2HeaderSize = 64

var par2Magic = [8]byte{'P', 'A', 'R', '2', 0x00, 'P', 'K', 'T'}

const par2TypePrefix = "PAR 2.0\x00"

// PAR2FileDesc is one recovered file identity from a PAR2 FileDesc packet.
type PAR2FileDesc struct {
	FileID  string // hex-encoded 16-byte file id, secret-free
	Hash16K string // hex-encoded MD5 of the first min(16 KiB, file size) bytes
	Name    string // recovered real filename
	Length  int64  // recovered real (decoded) file length
}

// PAR2IFSC is the compact block-hash material for one recoverable file.
// BlockMD5 is a contiguous sequence of 16-byte MD5 digests in file order.
type PAR2IFSC struct {
	FileID   string
	BlockMD5 []byte
}

// PAR2ProofIndex is the compact, URL-free IFSC material persisted on an item.
type PAR2ProofIndex struct {
	SliceSize int64           `json:"slice_size"`
	Files     []PAR2ProofFile `json:"files"`
}

// PAR2ProofFile binds one exact ParseNZB file index to its native IFSC hashes.
type PAR2ProofFile struct {
	NZBFileIndex int    `json:"nzb_file_index"`
	Length       int64  `json:"length"`
	BlockMD5     []byte `json:"block_md5"`
}

// PAR2Info is the bounded, URL-free metadata recovered from one PAR2 index
// file: real names/sizes for the files in the recovery set, in the order the
// Main packet declares them (creation order — typically archive/part order).
type PAR2Info struct {
	SliceSize     int64
	RecoverySetID string
	Files         []PAR2FileDesc // in Main packet recoverable-file order
	IFSC          []PAR2IFSC     // in Main packet recoverable-file order
	// RecoverySetIDs is the Main packet's recovery-set file id list in its
	// declared order. Reed-Solomon input-block numbering is global across
	// this exact list, so NS-4.2 cannot derive a block index without it --
	// and must abstain whenever an entry here has no matching FileDesc,
	// because the resulting numbering would silently shift.
	RecoverySetIDs []string
	nonRecovery    []string // file IDs present but outside the recovery set
}

// FileByID returns the recovered file, if any, matching a file id.
func (info PAR2Info) FileByID(id string) (PAR2FileDesc, bool) {
	for _, f := range info.Files {
		if f.FileID == id {
			return f, true
		}
	}
	return PAR2FileDesc{}, false
}

// IFSCByFileID returns a defensive copy of the block hashes for id.
func (info PAR2Info) IFSCByFileID(id string) (PAR2IFSC, bool) {
	for _, index := range info.IFSC {
		if index.FileID == id {
			index.BlockMD5 = append([]byte(nil), index.BlockMD5...)
			return index, true
		}
	}
	return PAR2IFSC{}, false
}

// ParsePAR2Metadata walks PAR2 packet framing from the start of src, bounded
// to maxPAR2PrefixBytes, and returns any Main, FileDesc, and IFSC packets found.
// Best-effort: a packet whose declared length would exceed the fetched
// prefix stops the walk without error — index files are small, so this only
// happens for a genuinely oversized or malformed source, and any packets
// already parsed are still returned. An error is returned only when no
// PAR2 packet is recognised at all.
func ParsePAR2Metadata(ctx context.Context, src bytesource.ByteSource) (PAR2Info, error) {
	data, err := readPAR2PrefixBestEffort(ctx, src)
	if err != nil {
		return PAR2Info{}, err
	}

	var info PAR2Info
	var mainSeen bool
	var recoverOrder []string
	fileDescs := make(map[string]PAR2FileDesc)
	fileDescSets := make(map[string]string)
	ifscPackets := make(map[string][]par2IFSCRecord)

	pos := 0
	packetsSeen := 0
	for pos+par2HeaderSize <= len(data) {
		if err := ctx.Err(); err != nil {
			return PAR2Info{}, err
		}
		if [8]byte(data[pos:pos+8]) != par2Magic {
			break
		}
		length := binary.LittleEndian.Uint64(data[pos+8 : pos+16])
		if length < par2HeaderSize || length%4 != 0 || length > uint64(len(data)-pos) {
			// Declared length is malformed or would run past what we
			// fetched; stop rather than guess at a resync point.
			break
		}
		recoverySetID := hexEncode(data[pos+32 : pos+48])
		typeField := string(data[pos+48 : pos+64])
		body := data[pos+par2HeaderSize : pos+int(length)]

		switch {
		case !mainSeen && strings.HasPrefix(typeField, par2TypePrefix) && strings.TrimRight(typeField[len(par2TypePrefix):], "\x00") == "Main":
			m, ids, perr := parsePAR2Main(body)
			if perr == nil {
				info.SliceSize = m.SliceSize
				info.RecoverySetID = recoverySetID
				recoverOrder = ids.recover
				info.RecoverySetIDs = append([]string(nil), ids.recover...)
				info.nonRecovery = ids.nonRecover
				mainSeen = true
			}
		case strings.HasPrefix(typeField, par2TypePrefix) && strings.TrimRight(typeField[len(par2TypePrefix):], "\x00") == "FileDesc":
			fd, perr := parsePAR2FileDesc(body)
			if perr == nil {
				fileDescs[fd.FileID] = fd
				fileDescSets[fd.FileID] = recoverySetID
			}
		case strings.HasPrefix(typeField, par2TypePrefix) && strings.TrimRight(typeField[len(par2TypePrefix):], "\x00") == "IFSC":
			index, perr := parsePAR2IFSC(body)
			if perr == nil {
				ifscPackets[index.FileID] = append(ifscPackets[index.FileID], par2IFSCRecord{
					recoverySetID: recoverySetID,
					index:         index,
				})
			}
		}

		pos += int(length)
		packetsSeen++
		if packetsSeen > 100_000 {
			break // pathological packet count; bounded per DG-03
		}
	}

	if !mainSeen && len(fileDescs) == 0 {
		return PAR2Info{}, fmt.Errorf("par2: no recognised packets in first %d bytes", len(data))
	}

	if len(recoverOrder) > 0 {
		info.Files = make([]PAR2FileDesc, 0, len(recoverOrder))
		for _, id := range recoverOrder {
			if fd, ok := fileDescs[id]; ok {
				info.Files = append(info.Files, fd)
			}
			if fileDescSets[id] != info.RecoverySetID {
				continue
			}
			if index, ok := unambiguousPAR2IFSC(ifscPackets[id], info.RecoverySetID); ok {
				info.IFSC = append(info.IFSC, index)
			}
		}
	} else {
		// No Main packet (or empty recovery set): fall back to FileDesc
		// packets in encounter order — still real names/sizes, just no
		// authoritative ordering signal.
		for _, fd := range fileDescs {
			info.Files = append(info.Files, fd)
		}
	}
	return info, nil
}

type par2IFSCRecord struct {
	recoverySetID string
	index         PAR2IFSC
}

func parsePAR2IFSC(body []byte) (PAR2IFSC, error) {
	if len(body) < 36 || (len(body)-16)%20 != 0 {
		return PAR2IFSC{}, fmt.Errorf("par2: invalid IFSC packet length")
	}
	blocks := (len(body) - 16) / 20
	if blocks <= 0 || blocks > maxPAR2IFSCBlocks {
		return PAR2IFSC{}, fmt.Errorf("par2: IFSC block count exceeds bound")
	}
	index := PAR2IFSC{FileID: hexEncode(body[:16]), BlockMD5: make([]byte, 0, blocks*16)}
	for off := 16; off < len(body); off += 20 {
		index.BlockMD5 = append(index.BlockMD5, body[off:off+16]...)
	}
	return index, nil
}

func unambiguousPAR2IFSC(packets []par2IFSCRecord, recoverySetID string) (PAR2IFSC, bool) {
	var selected PAR2IFSC
	for _, packet := range packets {
		if packet.recoverySetID != recoverySetID {
			continue
		}
		if selected.FileID == "" {
			selected = packet.index
			continue
		}
		if selected.FileID != packet.index.FileID || !bytes.Equal(selected.BlockMD5, packet.index.BlockMD5) {
			return PAR2IFSC{}, false
		}
	}
	if selected.FileID == "" {
		return PAR2IFSC{}, false
	}
	selected.BlockMD5 = append([]byte(nil), selected.BlockMD5...)
	return selected, true
}

// readPAR2PrefixBestEffort reads up to maxPAR2PrefixBytes from the start of
// src, tolerating a short read. This is deliberately NOT the shared
// archive.readPrefix helper: that helper treats any short read as a hard
// error, which is correct for sources with Capabilities.ExactSize == true,
// but NNTP-segment-backed sources report ExactSize == false -- Size() is a
// best-effort declared (yEnc-encoded) byte count that can legitimately
// exceed the actual decoded length by a small margin (documented in
// nntp.archiveByteSource). PAR2 index files are exactly the case that
// exposes this: small, often single-segment, so this walker's bound
// (maxPAR2PrefixBytes) routinely exceeds Size(), meaning the full declared
// size is requested -- and a real live gate against production usenet
// traffic confirmed the decoded byte count comes in a few percent short of
// the declared one. Returning the bytes actually read (rather than
// discarding a good partial read via a hard error) still parses correctly,
// since PAR2 packets are self-describing and length-prefixed; the walker in
// ParsePAR2Metadata already stops cleanly at whatever prefix it has.
func readPAR2PrefixBestEffort(ctx context.Context, src bytesource.ByteSource) ([]byte, error) {
	if src == nil {
		return nil, fmt.Errorf("par2: nil byte source")
	}
	size := src.Size()
	if size <= 0 {
		return nil, fmt.Errorf("par2: empty byte source")
	}
	if size > maxPAR2PrefixBytes {
		size = maxPAR2PrefixBytes
	}
	buf := make([]byte, int(size))
	n, err := src.ReadAt(ctx, buf, 0)
	if n == 0 {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("par2: read prefix: %w", err)
	}
	return buf[:n], nil
}

type par2MainBody struct {
	SliceSize int64
}

type par2FileIDs struct {
	recover    []string
	nonRecover []string
}

func parsePAR2Main(body []byte) (par2MainBody, par2FileIDs, error) {
	if len(body) < 12 {
		return par2MainBody{}, par2FileIDs{}, fmt.Errorf("par2: main packet too short")
	}
	sliceSize := binary.LittleEndian.Uint64(body[0:8])
	numFiles := binary.LittleEndian.Uint32(body[8:12])
	need := 12 + int(numFiles)*16
	if numFiles > 1_000_000 || need < 0 || need > len(body) {
		return par2MainBody{}, par2FileIDs{}, fmt.Errorf("par2: main packet file count exceeds body")
	}
	ids := par2FileIDs{}
	off := 12
	for i := uint32(0); i < numFiles; i++ {
		ids.recover = append(ids.recover, hexEncode(body[off:off+16]))
		off += 16
	}
	for off+16 <= len(body) {
		ids.nonRecover = append(ids.nonRecover, hexEncode(body[off:off+16]))
		off += 16
	}
	return par2MainBody{SliceSize: int64(sliceSize)}, ids, nil
}

func parsePAR2FileDesc(body []byte) (PAR2FileDesc, error) {
	if len(body) < 56 {
		return PAR2FileDesc{}, fmt.Errorf("par2: filedesc packet too short")
	}
	fileID := hexEncode(body[0:16])
	hash16K := hexEncode(body[32:48])
	length := int64(binary.LittleEndian.Uint64(body[48:56]))
	if length < 0 {
		return PAR2FileDesc{}, fmt.Errorf("par2: filedesc negative length")
	}
	name := strings.TrimRight(string(body[56:]), "\x00")
	if name == "" {
		return PAR2FileDesc{}, fmt.Errorf("par2: filedesc empty name")
	}
	return PAR2FileDesc{FileID: fileID, Hash16K: hash16K, Name: name, Length: length}, nil
}

// IsPAR2Prefix reports whether data begins with the PAR2 packet magic. Full
// packet framing and metadata validation remain owned by ParsePAR2Metadata.
func IsPAR2Prefix(data []byte) bool {
	return len(data) >= len(par2Magic) && [8]byte(data[:8]) == par2Magic
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0x0f]
	}
	return string(out)
}

var par2VolumePattern = regexp.MustCompile(`(?i)\.vol(\d+)\+(\d+)\.par2\b`)

// PAR2RecoveryBlocksFromSubjects sums the declared recovery-block counts
// across NZB subjects naming PAR2 volume files (e.g. "release.vol012+034.par2"
// means 34 recovery blocks starting at index 12). This reads only subject
// text already present in the parsed NZB — no additional fetch — so it is
// available even when no PAR2 index file was fetched at all.
func PAR2RecoveryBlocksFromSubjects(subjects []string) int64 {
	var total int64
	for _, subject := range subjects {
		m := par2VolumePattern.FindStringSubmatch(subject)
		if m == nil {
			continue
		}
		count, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || count < 0 {
			continue
		}
		total += count
	}
	return total
}

// PAR2SourceBlockCount returns the number of source blocks implied by a
// recovered PAR2 slice size and the total decoded size of the recoverable
// file set, i.e. ceil(totalSize / sliceSize). Returns 0 when sliceSize is
// not positive (Main packet absent or degenerate).
func PAR2SourceBlockCount(info PAR2Info, sliceSize int64) int64 {
	if sliceSize <= 0 {
		return 0
	}
	var total int64
	for _, f := range info.Files {
		total += f.Length
	}
	if total <= 0 {
		return 0
	}
	return (total + sliceSize - 1) / sliceSize
}

// PAR2IsVolumeFile reports whether subject names a PAR2 recovery volume
// (e.g. "release.vol012+034.par2") rather than the small metadata index
// file (Main + FileDesc packets only, no recovery slices).
func PAR2IsVolumeFile(subject string) bool {
	return par2VolumePattern.MatchString(subject)
}
