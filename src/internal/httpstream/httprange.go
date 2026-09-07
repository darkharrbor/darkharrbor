package httpstream

import (
	"fmt"
	"strconv"
	"strings"
)

// Range math for the HTTP live byte relay (HS-1.7, D16). Pure functions —
// the H1 deterministic suite exercises these directly; the live H2 gate
// exercises the wired relay.
//
// Policy (D16):
//   - Exactly one Range is honored. Multipart ranges (bytes=a-b,c-d) serve
//     the FIRST range only; DH never emits multipart/byteranges.
//   - A syntactically valid but unsatisfiable range is 416 with
//     Content-Range: bytes */total, and is never treated as a refresh signal.
//   - DH owns the upstream Range header; Accept-Encoding is forced to identity.

// ByteRange is an inclusive byte span.
type ByteRange struct {
	Start int64
	End   int64
}

// Length returns the number of bytes in the range.
func (r ByteRange) Length() int64 { return r.End - r.Start + 1 }

// ParseRange interprets a client Range header against a known total size.
// Returns:
//   - rng == nil, satisfiable == true  → no/ignorable Range header: serve 200 full.
//   - rng != nil, satisfiable == true  → serve 206 for rng.
//   - satisfiable == false             → respond 416 (Content-Range: bytes */size).
func ParseRange(header string, size int64) (rng *ByteRange, satisfiable bool) {
	header = strings.TrimSpace(header)
	if header == "" || size <= 0 {
		return nil, true
	}
	const pfx = "bytes="
	if !strings.HasPrefix(strings.ToLower(header), pfx) {
		// Unknown unit: per RFC 9110 a server MAY ignore the header.
		return nil, true
	}
	spec := strings.TrimSpace(header[len(pfx):])
	if spec == "" {
		return nil, false
	}
	// Multipart: first range only (D16/G5).
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = strings.TrimSpace(spec[:i])
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return nil, false
	}
	startStr := strings.TrimSpace(spec[:dash])
	endStr := strings.TrimSpace(spec[dash+1:])

	if startStr == "" {
		// Suffix range: last N bytes.
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return nil, false
		}
		if n > size {
			n = size
		}
		return &ByteRange{Start: size - n, End: size - 1}, true
	}

	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return nil, false
	}
	if start >= size {
		return nil, false
	}
	end := size - 1
	if endStr != "" {
		e, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || e < start {
			return nil, false
		}
		if e < end {
			end = e
		}
	}
	return &ByteRange{Start: start, End: end}, true
}

// ContentRange renders the 206 Content-Range value for a served range.
func ContentRange(r ByteRange, total int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", r.Start, r.End, total)
}

// UnsatisfiedContentRange renders the 416 Content-Range value.
func UnsatisfiedContentRange(total int64) string {
	return fmt.Sprintf("bytes */%d", total)
}

// UpstreamRangeHeader renders the single Range header DH sends upstream.
func UpstreamRangeHeader(r ByteRange) string {
	return fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
}

// ValidateUpstream206 checks that an upstream 206 Content-Range header is
// coherent with the range DH requested. Returns the advertised total length.
// Any mismatch is ClassUpstreamMalformed (mapped to 502 — never relayed raw).
func ValidateUpstream206(contentRange string, want ByteRange) (int64, error) {
	cr := strings.TrimSpace(contentRange)
	const pfx = "bytes "
	if !strings.HasPrefix(strings.ToLower(cr), pfx) {
		return 0, NewError(ClassUpstreamMalformed, "206 without valid Content-Range unit")
	}
	rest := strings.TrimSpace(cr[len(pfx):])
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return 0, NewError(ClassUpstreamMalformed, "206 Content-Range missing total")
	}
	span := strings.TrimSpace(rest[:slash])
	totalStr := strings.TrimSpace(rest[slash+1:])
	dash := strings.IndexByte(span, '-')
	if dash < 0 {
		return 0, NewError(ClassUpstreamMalformed, "206 Content-Range missing span")
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(span[:dash]), 10, 64)
	end, err2 := strconv.ParseInt(strings.TrimSpace(span[dash+1:]), 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return 0, NewError(ClassUpstreamMalformed, "206 Content-Range span not parseable")
	}
	if start != want.Start || end != want.End {
		return 0, NewError(ClassUpstreamMalformed, "206 Content-Range does not match requested range")
	}
	var total int64
	if totalStr == "*" {
		total = 0 // unknown total is tolerated when the span matches
	} else {
		total, err1 = strconv.ParseInt(totalStr, 10, 64)
		if err1 != nil || total <= end {
			return 0, NewError(ClassUpstreamMalformed, "206 Content-Range total incoherent")
		}
	}
	return total, nil
}
