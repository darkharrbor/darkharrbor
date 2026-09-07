package nntp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
	"sync"
)

const (
	yEncScratchBytes    = 64 << 10
	yEncMaxLineBytes    = 1 << 20
	yEncMaxEncodedBytes = 80 << 20
	yEncMaxDecodedBytes = 64 << 20
)

var (
	errYEncUndrained = errors.New("nntp: yenc: response not fully drained")
	yEncScratchPool  = sync.Pool{New: func() any {
		b := make([]byte, yEncScratchBytes)
		return &b
	}}
)

// DecodeYEncLines decodes yEnc-encoded lines from an NNTP BODY response into
// the original binary bytes. Lines should include =ybegin, =ypart (if any),
// data lines, and =yend — all are handled correctly.
//
// yEnc encoding: each byte = (original + 42) mod 256, with escape sequences
// for special characters. The escape character is '=' (0x3D); the next byte
// is decoded as (byte - 64 - 42) mod 256.
//
// already unstuffs ".." → "." on the wire. A second strip here drops a real
// data byte from every line that legitimately starts with '.', corrupting the
// binary stream.
//
// decoded bytes and compares against pcrc32=/crc32= field. Mismatch returns
// error so the caller can log/retry rather than silently serving corrupt data.
func DecodeYEncLines(lines []string) ([]byte, error) {
	return decodeYEncLines(lines, true)
}

func decodeYEncLines(lines []string, verifyCRC bool) ([]byte, error) {
	var body bytes.Buffer
	for _, line := range lines {
		body.WriteString(line)
		body.WriteString("\r\n")
	}
	return decodeYEncReader(context.Background(), &body, verifyCRC)
}

// decodeYEncReader incrementally parses an already dot-unstuffed NNTP BODY.
// It retains only the decoded article plus one pooled scanner buffer; callers
// no longer allocate one Go string per wire line.
func decodeYEncReader(ctx context.Context, r io.Reader, verifyCRC bool) ([]byte, error) {
	return decodeYEncReaderWithTotal(ctx, r, verifyCRC, nil)
}

// decodeYEncReaderWithTotal preserves decodeYEncReader behavior while
// optionally capturing the exact logical-file size carried by yEnc's
// =ybegin size= field. For multipart posts this is the decoded file size,
// unlike NZB segment byte counts, which include wire-encoding overhead.
func decodeYEncReaderWithTotal(ctx context.Context, r io.Reader, verifyCRC bool, totalOut *int64) ([]byte, error) {
	scratch := yEncScratchPool.Get().(*[]byte)
	defer yEncScratchPool.Put(scratch)

	limited := &io.LimitedReader{R: r, N: yEncMaxEncodedBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer((*scratch)[:cap(*scratch)], yEncMaxLineBytes)

	var result []byte
	seenBegin := false
	seenData := false
	seenEnd := false
	var semanticErr error
	var yendLine string
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", errYEncUndrained, err)
		}
		line := scanner.Bytes()
		if semanticErr != nil {
			continue
		}
		switch {
		case len(line) == 0:
			continue
		case bytes.HasPrefix(line, []byte("=ybegin")):
			if seenBegin || seenData || seenEnd {
				semanticErr = fmt.Errorf("nntp: yenc: contradictory begin line")
				continue
			}
			seenBegin = true
			if size, ok := yEncIntField(line, "size"); ok && totalOut != nil {
				*totalOut = int64(size)
			}
			if _, multipart := yEncIntField(line, "part"); !multipart {
				if size, ok := yEncIntField(line, "size"); ok && size <= yEncMaxDecodedBytes {
					result = make([]byte, 0, size)
				}
			}
		case bytes.HasPrefix(line, []byte("=ypart")):
			if !seenBegin || seenData || seenEnd {
				semanticErr = fmt.Errorf("nntp: yenc: misplaced part line")
				continue
			}
			begin, beginOK := yEncIntField(line, "begin")
			end, endOK := yEncIntField(line, "end")
			if beginOK && endOK && end >= begin && end-begin < yEncMaxDecodedBytes {
				result = make([]byte, 0, end-begin+1)
			}
		case bytes.HasPrefix(line, []byte("=yend")):
			if !seenBegin || !seenData || seenEnd {
				semanticErr = fmt.Errorf("nntp: yenc: misplaced end line")
				continue
			}
			seenEnd = true
			yendLine = string(line)
		default:
			if !seenBegin || seenEnd {
				semanticErr = fmt.Errorf("nntp: yenc: data outside framed body")
				continue
			}
			if line[len(line)-1] == '=' {
				semanticErr = fmt.Errorf("nntp: yenc: truncated escape")
				continue
			}
			if len(result)+len(line) > yEncMaxDecodedBytes {
				semanticErr = fmt.Errorf("nntp: yenc: decoded body exceeds %d bytes", yEncMaxDecodedBytes)
				continue
			}
			result = decodeYEncBytes(result, line)
			seenData = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: read body: %v", errYEncUndrained, err)
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("%w: encoded body exceeds %d bytes", errYEncUndrained, yEncMaxEncodedBytes)
	}
	if semanticErr != nil {
		return nil, semanticErr
	}
	if !seenBegin || !seenData || !seenEnd {
		return nil, fmt.Errorf("nntp: yenc: incomplete body")
	}
	if size, ok := yEncIntField([]byte(yendLine), "size"); bytes.Contains([]byte(yendLine), []byte("size=")) && (!ok || size != len(result)) {
		return nil, fmt.Errorf("nntp: yenc: contradictory decoded size")
	}
	if verifyCRC {
		if err := validateYEnd(yendLine, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func yEncIntField(line []byte, name string) (int, bool) {
	prefix := []byte(name + "=")
	for _, field := range bytes.Fields(line) {
		if !bytes.HasPrefix(field, prefix) {
			continue
		}
		n, err := strconv.Atoi(string(field[len(prefix):]))
		return n, err == nil && n >= 0
	}
	return 0, false
}

// validateYEnd parses the =yend line and validates the CRC32 of decoded data.
// Returns nil if no CRC field is present (some servers omit it).
func validateYEnd(line string, data []byte) error {
	var expectedCRC uint32
	found := false

	for _, prefix := range []string{" pcrc32=", " crc32="} {
		if idx := strings.Index(line, prefix); idx >= 0 {
			hexStr := line[idx+len(prefix):]
			if endIdx := strings.IndexAny(hexStr, " \t\r\n"); endIdx >= 0 {
				hexStr = hexStr[:endIdx]
			}
			if len(hexStr) == 8 {
				if crc, err := strconv.ParseUint(hexStr, 16, 32); err == nil {
					expectedCRC = uint32(crc)
					found = true
					break
				}
			}
		}
	}

	if !found {
		return nil // no CRC in =yend line; skip validation
	}

	actualCRC := crc32.ChecksumIEEE(data)
	if actualCRC != expectedCRC {
		return fmt.Errorf("nntp: yenc crc32 mismatch: expected %08x, got %08x", expectedCRC, actualCRC)
	}
	return nil
}

func decodeYEncLine(line string) []byte {
	return decodeYEncBytes(make([]byte, 0, len(line)), []byte(line))
}

func decodeYEncBytes(out, line []byte) []byte {
	i := 0
	for i < len(line) {
		b := line[i]
		if b == '=' && i+1 < len(line) {
			// Escape: next byte decoded as (next - 64 - 42) mod 256
			next := int(line[i+1])
			val := (next - 64 - 42) % 256
			if val < 0 {
				val += 256
			}
			out = append(out, byte(val)) // #nosec G115 -- val is normalized into the byte range [0,255] immediately above.
			i += 2
		} else {
			// Normal: (b - 42) mod 256
			val := (int(b) - 42) % 256
			if val < 0 {
				val += 256
			}
			out = append(out, byte(val)) // #nosec G115 -- val is normalized into the byte range [0,255] immediately above.
			i++
		}
	}
	return out
}
