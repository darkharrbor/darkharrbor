// Package torrentmeta implements TS-0.1 (T1): a pure-Go bencode decoder and
// compact TorrentMeta extractor for arr-uploaded .torrent files. No
// third-party dependency is used — the format is small enough that a
// hand-written, bounded decoder is both simpler to audit and easier to keep
// inside DarkHarrbor's own security posture (DG-03: malformed/oversized
// input from an untrusted upload must never panic, hang, or exhaust memory).
package torrentmeta

import (
	"errors"
	"fmt"
	"strconv"
)

// Bounds are defense-in-depth against a malicious or corrupted .torrent
// upload. handleQBitAdd's ParseMultipartForm already caps the overall
// request at 8 MiB; these bounds apply independently so this package is
// safe to call from any future caller with its own input source.
const (
	maxDecodeDepth  = 64      // nested list/dict recursion ceiling
	maxContainerLen = 200_000 // max items in any single list or dict
)

type kind int

const (
	kInt kind = iota
	kBytes
	kList
	kDict
)

// value is one decoded bencode node. start/end bound its own exact encoded
// span in the original buffer — used to extract the raw "info" dict bytes
// verbatim for infohash hashing (the info dict must be hashed exactly as
// it appears in the original file, not re-encoded).
type value struct {
	kind  kind
	i     int64
	b     []byte
	list  []value
	dict  map[string]value
	start int
	end   int
}

type decoder struct {
	buf []byte
	pos int
}

func decodeTop(data []byte) (value, error) {
	d := &decoder{buf: data}
	v, err := d.decode(0)
	if err != nil {
		return value{}, err
	}
	return v, nil
}

func (d *decoder) decode(depth int) (value, error) {
	if depth > maxDecodeDepth {
		return value{}, errors.New("bencode: max nesting depth exceeded")
	}
	if d.pos >= len(d.buf) {
		return value{}, errors.New("bencode: unexpected end of input")
	}
	start := d.pos
	switch c := d.buf[d.pos]; {
	case c == 'i':
		return d.decodeInt(start)
	case c == 'l':
		return d.decodeList(start, depth)
	case c == 'd':
		return d.decodeDict(start, depth)
	case c >= '0' && c <= '9':
		return d.decodeBytes(start)
	default:
		return value{}, fmt.Errorf("bencode: invalid type byte %q at offset %d", c, d.pos)
	}
}

func (d *decoder) decodeInt(start int) (value, error) {
	i := d.pos + 1 // consume 'i'
	j := i
	if j < len(d.buf) && d.buf[j] == '-' {
		j++
	}
	digitsStart := j
	for j < len(d.buf) && d.buf[j] >= '0' && d.buf[j] <= '9' {
		j++
	}
	if j == digitsStart {
		return value{}, errors.New("bencode: invalid integer")
	}
	if j >= len(d.buf) || d.buf[j] != 'e' {
		return value{}, errors.New("bencode: unterminated integer")
	}
	n, err := strconv.ParseInt(string(d.buf[i:j]), 10, 64)
	if err != nil {
		return value{}, fmt.Errorf("bencode: invalid integer: %w", err)
	}
	d.pos = j + 1
	return value{kind: kInt, i: n, start: start, end: d.pos}, nil
}

func (d *decoder) decodeBytes(start int) (value, error) {
	i := d.pos
	for i < len(d.buf) && d.buf[i] >= '0' && d.buf[i] <= '9' {
		i++
	}
	if i == d.pos {
		return value{}, errors.New("bencode: expected byte string length")
	}
	lenStr := string(d.buf[d.pos:i])
	n, err := strconv.ParseInt(lenStr, 10, 63)
	if err != nil || n < 0 {
		return value{}, fmt.Errorf("bencode: invalid byte string length %q", lenStr)
	}
	if i >= len(d.buf) || d.buf[i] != ':' {
		return value{}, errors.New("bencode: expected ':' after byte string length")
	}
	i++
	if n > int64(len(d.buf)-i) {
		return value{}, errors.New("bencode: byte string length exceeds remaining input")
	}
	end := i + int(n)
	b := d.buf[i:end]
	d.pos = end
	return value{kind: kBytes, b: b, start: start, end: d.pos}, nil
}

func (d *decoder) decodeList(start, depth int) (value, error) {
	d.pos++ // consume 'l'
	var list []value
	for {
		if d.pos >= len(d.buf) {
			return value{}, errors.New("bencode: unterminated list")
		}
		if d.buf[d.pos] == 'e' {
			d.pos++
			break
		}
		v, err := d.decode(depth + 1)
		if err != nil {
			return value{}, err
		}
		list = append(list, v)
		if len(list) > maxContainerLen {
			return value{}, errors.New("bencode: list too large")
		}
	}
	return value{kind: kList, list: list, start: start, end: d.pos}, nil
}

func (d *decoder) decodeDict(start, depth int) (value, error) {
	d.pos++ // consume 'd'
	dict := make(map[string]value)
	for {
		if d.pos >= len(d.buf) {
			return value{}, errors.New("bencode: unterminated dict")
		}
		if d.buf[d.pos] == 'e' {
			d.pos++
			break
		}
		keyVal, err := d.decode(depth + 1)
		if err != nil {
			return value{}, fmt.Errorf("bencode: dict key: %w", err)
		}
		if keyVal.kind != kBytes {
			return value{}, errors.New("bencode: dict key must be a byte string")
		}
		v, err := d.decode(depth + 1)
		if err != nil {
			return value{}, err
		}
		dict[string(keyVal.b)] = v
		if len(dict) > maxContainerLen {
			return value{}, errors.New("bencode: dict too large")
		}
	}
	return value{kind: kDict, dict: dict, start: start, end: d.pos}, nil
}
