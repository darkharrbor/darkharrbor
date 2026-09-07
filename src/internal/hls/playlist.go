// Package hls owns HR4.2-HR4.6's master/media playlist rewrite: parsing an
// HLS (M3U8) playlist just far enough to find every URI-bearing tag or
// line, and reproducing the playlist with those URIs replaced by opaque DH
// resource URLs (HR-D6, HR4.1's internal/hlssession graph). This package is
// the first real consumer of hlssession's shared ResourceKind vocabulary —
// it deliberately reuses that enum rather than inventing a second one
// (WORKFLOW rule 3).
//
// Scope is intentionally narrow: this package rewrites playlist TEXT
// (master variant/rendition
// URIs, and — recursively, one HTTP fetch at a time — the media/segment
// playlists those variants point to). It mints hlssession resources for
// every URI-bearing entity a real player would need (segments, byte-range
// maps, keys, subtitles), but does not itself serve their bytes; that is
// HR4.3's job. Everything not touched by the specific tags this file knows
// about (TARGETDURATION, MEDIA-SEQUENCE, PLAYLIST-TYPE, DISCONTINUITY,
// ENDLIST, VERSION, EXT-X-I-FRAME-STREAM-INF, EXT-X-SESSION-DATA, Content
// Steering tags, and any future/unknown tag) passes through byte-identical
// — an unrecognized tag is forward-compatible passthrough, never a parse
// failure, mirroring the "unknown config keys warn, never fail" standing
// convention applied here to unknown manifest content.
package hls

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/hlssession"
)

// MaxPlaylistBytes bounds how much playlist text this package will ever
// parse or rewrite (DG-03: a hostile or corrupt origin must not exhaust
// memory via an oversized "manifest"). Real VOD master/media playlists are
// small text documents; even a multi-thousand-segment media playlist stays
// far under this bound.
const MaxPlaylistBytes = 8 << 20 // 8 MiB

// MaxLivePlaylistEntries bounds one sliding live window independently of the
// session-wide resource cap. Ordinary live manifests contain tens of entries;
// this leaves ample headroom while preventing one refresh from filling the
// entire durable graph before old windows can be pruned. LL-HLS parts,
// preload hints, and rendition reports share this same bound.
const MaxLivePlaylistEntries = 1024

// Sentinel parse errors. Kept distinct (via errors.Is) from a generic
// fmt.Errorf so callers can classify a malformed/oversized playlist as a
// permanent-here failure rather than a transient one.
var (
	ErrNotPlaylist   = fmt.Errorf("hls: not a recognizable M3U8 playlist")
	ErrOversized     = fmt.Errorf("hls: playlist exceeds MaxPlaylistBytes")
	ErrTruncatedTag  = fmt.Errorf("hls: tag references a URI line that is missing")
	ErrMediaSequence = fmt.Errorf("hls: invalid media sequence")
	ErrLiveWindow    = fmt.Errorf("hls: live window exceeds MaxLivePlaylistEntries")
	ErrDeltaUpdate   = fmt.Errorf("hls: invalid delta update")
	ErrLowLatencyTag = fmt.Errorf("hls: invalid low-latency tag")
	ErrSteering      = fmt.Errorf("hls: invalid content steering tag")
)

// Entry is one URI-bearing occurrence discovered in a playlist, in document
// order. Ordinal (its position in the sequence ParseMaster/ParseMedia
// return, and RewriteMaster/RewriteMedia walk) is the stable numbering an
// hlssession Resource's Reference.Ordinal records, so a later independent
// parse of the same-shaped content can relocate the exact same entry again
// (HR-D6) without DarkHarrbor ever persisting the URI itself.
type Entry struct {
	Kind ResourceKind
	// PathwayID is the origin-declared Content Steering pathway for a
	// variant. It is validated as an opaque HLS Pathway ID before use.
	PathwayID string
	// URI is the raw, as-written URI text (absolute or relative to the
	// playlist's own fetch URL) — never persisted; only used transiently to
	// compute the current live upstream location.
	URI string
	// ByteStart/ByteEnd are an inclusive byte range when the entry carries
	// one (EXT-X-BYTERANGE segments, or an EXT-X-MAP/EXT-X-BYTERANGE
	// attribute). hlssession.NewWholeReference() otherwise.
	ByteStart int64
	ByteEnd   int64
	// MediaSequence identifies an entry within a sliding live playlist.
	// HasMediaSequence is false for VOD and master playlists, which retain
	// their established ordinal identity.
	MediaSequence    int64
	HasMediaSequence bool
	// PartIndex distinguishes Partial Segments that share one parent Media
	// Sequence Number. A preload hint uses the index of the next part so its
	// opaque identity is reused when that part is published.
	PartIndex    int
	HasPartIndex bool
	// RenditionIndex is the document order among EXT-X-RENDITION-REPORT
	// tags, independent of the moving live-edge entries before them.
	RenditionIndex    int
	HasRenditionIndex bool
	// MediaStartNS/MediaEndNS are an exclusive VOD presentation-time span.
	// Live/unbounded entries, preload hints, and non-media resources omit it.
	MediaStartNS     int64
	MediaEndNS       int64
	HasMediaTime     bool
	MediaDurationNS  int64
	HasMediaDuration bool
}

// ResourceKind is an alias for hlssession's shared vocabulary — this
// package never defines its own resource-kind enum.
type ResourceKind = hlssession.ResourceKind

func checkSize(body string) error {
	if len(body) > MaxPlaylistBytes {
		return ErrOversized
	}
	return nil
}

// IsMaster reports whether body is shaped like a master (variant) playlist.
// Per RFC 8216 a playlist document is a master or a media playlist, never
// both.
func IsMaster(body string) bool {
	return strings.Contains(body, "#EXT-X-STREAM-INF:")
}

// IsMedia reports whether body is shaped like a media (segment) playlist.
func IsMedia(body string) bool {
	return strings.Contains(body, "#EXTINF:") ||
		strings.Contains(body, "#EXT-X-PART:") ||
		strings.Contains(body, "#EXT-X-PART-INF:")
}

func splitLines(body string) []string {
	return strings.Split(body, "\n")
}

// entryFunc is invoked once per discovered URI-bearing entry, in document
// order. A non-empty returned replace substitutes the entry's URI (or URI
// attribute) with that exact text; an empty replace with a nil error leaves
// the line unchanged (used by the read-only Parse* wrappers below).
type entryFunc func(ordinal int, e Entry) (replace string, err error)

// MintFunc registers (or finds) the resource standing for entry e at the
// given ordinal and returns the opaque DH URL to substitute in its place.
type MintFunc func(ordinal int, e Entry) (string, error)

// ── attribute helpers ───────────────────────────────────────────────────

var (
	uriAttrRe       = regexp.MustCompile(`URI="([^"]*)"`)
	typeAttrRe      = regexp.MustCompile(`(?i)\bTYPE=([A-Za-z-]+)`)
	methodAttrRe    = regexp.MustCompile(`(?i)\bMETHOD=([A-Za-z0-9-]+)`)
	byterangeAttrRe = regexp.MustCompile(`(?i)\bBYTERANGE="([^"]*)"`)
	skippedAttrRe   = regexp.MustCompile(`(?i)(?:^|[:,])SKIPPED-SEGMENTS=([^,]*)`)
	preloadStartRe  = regexp.MustCompile(`(?i)(?:^|[:,])BYTERANGE-START=([^,]*)`)
	preloadLengthRe = regexp.MustCompile(`(?i)(?:^|[:,])BYTERANGE-LENGTH=([^,]*)`)
	durationAttrRe  = regexp.MustCompile(`(?i)(?:^|[:,])DURATION=([^,]*)`)
	serverURIAttrRe = regexp.MustCompile(`(?:^|[:,])SERVER-URI="([^"]*)"`)
	pathwayIDAttrRe = regexp.MustCompile(`(?:^|[:,])PATHWAY-ID="([^"]*)"`)
)

func extractURIAttr(line string) (string, bool) {
	m := uriAttrRe.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// replaceURIAttr substitutes only the quoted value of the first URI="..."
// attribute in line, leaving every other character (attribute order,
// spacing, quoting of other attributes) byte-identical.
func replaceURIAttr(line, newURI string) string {
	loc := uriAttrRe.FindStringSubmatchIndex(line)
	if loc == nil {
		return line
	}
	return line[:loc[2]] + newURI + line[loc[3]:]
}

func extractPathwayID(line string) string {
	m := pathwayIDAttrRe.FindStringSubmatch(line)
	if m == nil || !ValidPathwayID(m[1]) {
		return ""
	}
	return m[1]
}

// mediaRenditionKind maps an EXT-X-MEDIA tag's TYPE attribute to the
// resource kind its URI (if any) stands for. CLOSED-CAPTIONS renditions
// never carry a URI (in-stream only) and TYPE=VIDEO is treated the same as
// AUDIO — both are alternate renditions with their own child playlist.
func mediaRenditionKind(line string) ResourceKind {
	m := typeAttrRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	switch strings.ToUpper(m[1]) {
	case "AUDIO", "VIDEO":
		return hlssession.KindMedia
	case "SUBTITLES":
		return hlssession.KindSubtitle
	default:
		return ""
	}
}

func typeValue(line string) string {
	m := typeAttrRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}

func keyMethod(line string) string {
	m := methodAttrRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

// parseByteRangeSpec parses an EXT-X-BYTERANGE tag body or an EXT-X-MAP
// BYTERANGE attribute value: "n" or "n@o". prevEnd is the previous
// segment's byte-range end (inclusive); -1 means none is known yet. When o
// is omitted, the offset continues immediately after prevEnd (RFC 8216);
// omitting it with no known prevEnd is invalid.
func parseByteRangeSpec(spec string, prevEnd int64) (n, o int64, ok bool) {
	spec = strings.TrimSpace(spec)
	parts := strings.SplitN(spec, "@", 2)
	nv, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || nv < 0 {
		return 0, 0, false
	}
	if len(parts) == 2 {
		ov, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || ov < 0 {
			return 0, 0, false
		}
		return nv, ov, true
	}
	if prevEnd < 0 {
		return 0, 0, false
	}
	return nv, prevEnd + 1, true
}

func parseByterangeAttr(line string) (bs, be int64, ok bool) {
	m := byterangeAttrRe.FindStringSubmatch(line)
	if m == nil {
		bs, be = hlssession.NewWholeReference()
		return bs, be, false
	}
	n, o, ok := parseByteRangeSpec(m[1], -1)
	if !ok {
		bs, be = hlssession.NewWholeReference()
		return bs, be, false
	}
	return o, o + n - 1, true
}

func parseNonNegativeAttr(line string, re *regexp.Regexp) (int64, bool) {
	m := re.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(m[1]), 10, 64)
	return n, err == nil && n >= 0
}

func parsePositiveDurationNS(value string) (int64, bool) {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || seconds <= 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) ||
		seconds > float64(math.MaxInt64)/float64(time.Second) {
		return 0, false
	}
	ns := int64(math.Round(seconds * float64(time.Second)))
	return ns, ns > 0
}

func parseDurationNSAttr(line string, re *regexp.Regexp) (int64, bool) {
	m := re.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	return parsePositiveDurationNS(m[1])
}

func parseEXTINFDurationNS(line string) (int64, bool) {
	value := strings.TrimPrefix(line, "#EXTINF:")
	if value == line {
		return 0, false
	}
	value, _, _ = strings.Cut(value, ",")
	return parsePositiveDurationNS(value)
}

// ── master playlist walk ────────────────────────────────────────────────

// walkMaster is the single traversal ParseMaster and RewriteMaster both
// build on, so their ordinal numbering can never drift apart.
func walkMaster(body string, fn entryFunc) (string, error) {
	if err := checkSize(body); err != nil {
		return "", err
	}
	if !IsMaster(body) {
		return "", ErrNotPlaylist
	}
	lines := splitLines(body)
	out := make([]string, 0, len(lines))
	ordinal := 0
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			out = append(out, line)
			i++
			if i >= len(lines) || strings.TrimSpace(lines[i]) == "" {
				return "", ErrTruncatedTag
			}
			uriLine := strings.TrimRight(lines[i], "\r")
			bs, be := hlssession.NewWholeReference()
			e := Entry{
				Kind: hlssession.KindMedia, PathwayID: extractPathwayID(line),
				URI: uriLine, ByteStart: bs, ByteEnd: be,
			}
			repl, err := fn(ordinal, e)
			if err != nil {
				return "", err
			}
			ordinal++
			if repl != "" {
				out = append(out, repl)
			} else {
				out = append(out, uriLine)
			}
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			uri, ok := extractURIAttr(line)
			kind := mediaRenditionKind(line)
			if !ok || kind == "" {
				out = append(out, line)
				continue
			}
			bs, be := hlssession.NewWholeReference()
			e := Entry{Kind: kind, URI: uri, ByteStart: bs, ByteEnd: be}
			repl, err := fn(ordinal, e)
			if err != nil {
				return "", err
			}
			ordinal++
			if repl != "" {
				out = append(out, replaceURIAttr(line, repl))
			} else {
				out = append(out, line)
			}
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n") + "\n", nil
}

// ── media (segment) playlist walk ───────────────────────────────────────

func walkMedia(body string, fn entryFunc) (string, error) {
	if err := checkSize(body); err != nil {
		return "", err
	}
	if !IsMedia(body) {
		return "", ErrNotPlaylist
	}
	lines := splitLines(body)
	live := true
	lowLatency := false
	mediaSequence := int64(0)
	skippedSegments := int64(0)
	sawSkip := false
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if line == "#EXT-X-ENDLIST" {
			live = false
		}
		if strings.HasPrefix(line, "#EXT-X-PART-INF:") ||
			strings.HasPrefix(line, "#EXT-X-PART:") ||
			strings.HasPrefix(line, "#EXT-X-PRELOAD-HINT:") {
			lowLatency = true
		}
		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 {
				return "", ErrMediaSequence
			}
			mediaSequence = parsed
		}
		if strings.HasPrefix(line, "#EXT-X-SKIP:") {
			if sawSkip {
				return "", ErrDeltaUpdate
			}
			sawSkip = true
			parsed, ok := parseNonNegativeAttr(line, skippedAttrRe)
			if !ok {
				return "", ErrDeltaUpdate
			}
			skippedSegments = parsed
		}
	}
	if sawSkip {
		if !live || mediaSequence > int64(^uint64(0)>>1)-skippedSegments {
			return "", ErrDeltaUpdate
		}
		mediaSequence += skippedSegments
	}
	vodDurationNS := int64(0)
	if !live {
		valid, pending, sawSegment := true, false, false
		for _, raw := range lines {
			line := strings.TrimRight(raw, "\r")
			switch {
			case strings.HasPrefix(line, "#EXTINF:"):
				duration, ok := parseEXTINFDurationNS(line)
				if !ok || pending || vodDurationNS > math.MaxInt64-duration {
					valid = false
					continue
				}
				vodDurationNS += duration
				pending = true
			case line != "" && !strings.HasPrefix(line, "#"):
				if !pending {
					valid = false
				}
				pending = false
				sawSegment = true
			}
		}
		if !valid || pending || !sawSegment || vodDurationNS <= 0 {
			vodDurationNS = 0
		}
	}
	out := make([]string, 0, len(lines))
	ordinal := 0
	pendingBS, pendingBE := hlssession.NewWholeReference()
	havePending := false
	prevSegEnd := int64(-1)
	sequenceExhausted := false
	partIndex := 0
	prevPartEnd := int64(-1)
	renditionIndex := 0
	timelineNS := int64(0)
	partOffsetNS := int64(0)
	pendingDurationNS := int64(0)
	havePendingDuration := false
	timelineValid := !live
	addSequence := func(e Entry) Entry {
		if live {
			e.MediaSequence = mediaSequence
			e.HasMediaSequence = true
		}
		return e
	}
	visit := func(e Entry, sequence bool) (string, error) {
		if live && ordinal >= MaxLivePlaylistEntries {
			return "", ErrLiveWindow
		}
		if sequence {
			e = addSequence(e)
		}
		repl, err := fn(ordinal, e)
		if err == nil {
			ordinal++
		}
		return repl, err
	}

	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		switch {
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			uri, ok := extractURIAttr(line)
			if !ok || keyMethod(line) == "NONE" {
				out = append(out, line)
				continue
			}
			bs, be := hlssession.NewWholeReference()
			e := Entry{Kind: hlssession.KindKey, URI: uri, ByteStart: bs, ByteEnd: be}
			repl, err := visit(e, true)
			if err != nil {
				return "", err
			}
			if repl != "" {
				out = append(out, replaceURIAttr(line, repl))
			} else {
				out = append(out, line)
			}
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			uri, ok := extractURIAttr(line)
			if !ok {
				out = append(out, line)
				continue
			}
			bs, be, _ := parseByterangeAttr(line)
			e := Entry{Kind: hlssession.KindMap, URI: uri, ByteStart: bs, ByteEnd: be}
			if live && lowLatency {
				e.PartIndex = partIndex
				e.HasPartIndex = true
			}
			repl, err := visit(e, true)
			if err != nil {
				return "", err
			}
			if repl != "" {
				out = append(out, replaceURIAttr(line, repl))
			} else {
				out = append(out, line)
			}
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			spec := strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
			n, o, ok := parseByteRangeSpec(spec, prevSegEnd)
			if ok {
				pendingBS, pendingBE = o, o+n-1
				havePending = true
			}
			out = append(out, line)
		case strings.HasPrefix(line, "#EXTINF:"):
			pendingDurationNS, havePendingDuration = parseEXTINFDurationNS(line)
			if !havePendingDuration {
				timelineValid = false
			}
			out = append(out, line)
		case strings.HasPrefix(line, "#EXT-X-PART:"):
			uri, ok := extractURIAttr(line)
			durationNS, durationOK := parseDurationNSAttr(line, durationAttrRe)
			if !ok || uri == "" || !durationOK {
				return "", ErrLowLatencyTag
			}
			if m := byterangeAttrRe.FindStringSubmatch(line); m != nil {
				n, o, valid := parseByteRangeSpec(m[1], prevPartEnd)
				if !valid || n == 0 || o > int64(^uint64(0)>>1)-n {
					return "", ErrLowLatencyTag
				}
				prevPartEnd = o + n - 1
			}
			bs, be := hlssession.NewWholeReference()
			e := Entry{
				Kind: hlssession.KindSegment, URI: uri, ByteStart: bs, ByteEnd: be,
				PartIndex: partIndex, HasPartIndex: true,
			}
			if timelineValid && timelineNS <= math.MaxInt64-partOffsetNS &&
				timelineNS+partOffsetNS <= math.MaxInt64-durationNS {
				e.MediaStartNS = timelineNS + partOffsetNS
				e.MediaEndNS = e.MediaStartNS + durationNS
				e.HasMediaTime = true
				if vodDurationNS > 0 {
					e.MediaDurationNS, e.HasMediaDuration = vodDurationNS, true
				}
				partOffsetNS += durationNS
			} else if !live {
				timelineValid = false
			}
			repl, err := visit(e, true)
			if err != nil {
				return "", err
			}
			partIndex++
			if repl != "" {
				out = append(out, replaceURIAttr(line, repl))
			} else {
				out = append(out, line)
			}
		case strings.HasPrefix(line, "#EXT-X-PRELOAD-HINT:"):
			uri, ok := extractURIAttr(line)
			var kind ResourceKind
			switch strings.ToUpper(strings.TrimSpace(typeValue(line))) {
			case "PART":
				kind = hlssession.KindSegment
			case "MAP":
				kind = hlssession.KindMap
			default:
				return "", ErrLowLatencyTag
			}
			if !ok || uri == "" {
				return "", ErrLowLatencyTag
			}
			start, hasStart := parseNonNegativeAttr(line, preloadStartRe)
			length, hasLength := parseNonNegativeAttr(line, preloadLengthRe)
			if (preloadStartRe.MatchString(line) && !hasStart) ||
				(preloadLengthRe.MatchString(line) && (!hasLength || length == 0)) ||
				(hasLength && start > int64(^uint64(0)>>1)-length) {
				return "", ErrLowLatencyTag
			}
			bs, be := hlssession.NewWholeReference()
			e := Entry{Kind: kind, URI: uri, ByteStart: bs, ByteEnd: be}
			e.PartIndex = partIndex
			e.HasPartIndex = true
			repl, err := visit(e, true)
			if err != nil {
				return "", err
			}
			if repl != "" {
				out = append(out, replaceURIAttr(line, repl))
			} else {
				out = append(out, line)
			}
		case strings.HasPrefix(line, "#EXT-X-RENDITION-REPORT:"):
			uri, ok := extractURIAttr(line)
			if !ok || uri == "" {
				return "", ErrLowLatencyTag
			}
			bs, be := hlssession.NewWholeReference()
			e := Entry{
				Kind: hlssession.KindMedia, URI: uri, ByteStart: bs, ByteEnd: be,
				RenditionIndex: renditionIndex, HasRenditionIndex: true,
			}
			repl, err := visit(e, false)
			if err != nil {
				return "", err
			}
			renditionIndex++
			if repl != "" {
				out = append(out, replaceURIAttr(line, repl))
			} else {
				out = append(out, line)
			}
		case strings.HasPrefix(line, "#"):
			out = append(out, line)
		case strings.TrimSpace(line) == "":
			out = append(out, line)
		default:
			if live && sequenceExhausted {
				return "", ErrMediaSequence
			}
			bs, be := hlssession.NewWholeReference()
			if havePending {
				bs, be = pendingBS, pendingBE
				prevSegEnd = be
				havePending = false
			}
			e := Entry{Kind: hlssession.KindSegment, URI: line, ByteStart: bs, ByteEnd: be}
			if timelineValid && havePendingDuration && partOffsetNS <= pendingDurationNS &&
				timelineNS <= math.MaxInt64-pendingDurationNS {
				e.MediaStartNS = timelineNS
				e.MediaEndNS = timelineNS + pendingDurationNS
				e.HasMediaTime = true
				if vodDurationNS > 0 {
					e.MediaDurationNS, e.HasMediaDuration = vodDurationNS, true
				}
				timelineNS = e.MediaEndNS
			} else if !live {
				timelineValid = false
			}
			pendingDurationNS = 0
			havePendingDuration = false
			partOffsetNS = 0
			repl, err := visit(e, true)
			if err != nil {
				return "", err
			}
			if live {
				if mediaSequence == int64(^uint64(0)>>1) {
					sequenceExhausted = true
				} else {
					mediaSequence++
				}
				partIndex = 0
				prevPartEnd = -1
			}
			if repl != "" {
				out = append(out, repl)
			} else {
				out = append(out, line)
			}
		}
	}
	return strings.Join(out, "\n") + "\n", nil
}

// ContentSteering is the bounded, transient steering coordinate carried by
// one multivariant playlist. ServerURI is never persisted or returned to a
// player; callers resolve and fetch it only through the shared outbound
// security policy.
type ContentSteering struct {
	ServerURI string
	PathwayID string
}

// ValidPathwayID implements the Content Steering identifier grammar.
func ValidPathwayID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '.' && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// ParseContentSteering returns the single steering tag from a multivariant
// playlist. Unknown attributes pass through, while duplicate tags or
// required attributes fail closed.
func ParseContentSteering(body string) (ContentSteering, bool, error) {
	if err := checkSize(body); err != nil {
		return ContentSteering{}, false, err
	}
	var found ContentSteering
	seen := false
	for _, raw := range splitLines(body) {
		line := strings.TrimRight(raw, "\r")
		if !strings.HasPrefix(line, "#EXT-X-CONTENT-STEERING:") {
			continue
		}
		if seen {
			return ContentSteering{}, false, ErrSteering
		}
		seen = true
		server := serverURIAttrRe.FindAllStringSubmatch(line, -1)
		pathways := pathwayIDAttrRe.FindAllStringSubmatch(line, -1)
		if len(server) != 1 || server[0][1] == "" || len(pathways) > 1 {
			return ContentSteering{}, false, ErrSteering
		}
		found.ServerURI = server[0][1]
		if len(pathways) == 1 {
			if !ValidPathwayID(pathways[0][1]) {
				return ContentSteering{}, false, ErrSteering
			}
			found.PathwayID = pathways[0][1]
		}
	}
	return found, seen, nil
}

// RewriteContentSteering replaces only SERVER-URI on the one validated tag.
// Variant/rendition ordinals are untouched, preserving older session IDs.
func RewriteContentSteering(body, serverURI string) (string, error) {
	_, found, err := ParseContentSteering(body)
	if err != nil || !found {
		return body, err
	}
	lines := splitLines(body)
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if strings.HasPrefix(line, "#EXT-X-CONTENT-STEERING:") {
			loc := serverURIAttrRe.FindStringSubmatchIndex(line)
			lines[i] = line[:loc[2]] + serverURI + line[loc[3]:]
			break
		}
	}
	return strings.Join(lines, "\n"), nil
}

// MasterPathwayIDs returns explicit variant pathways in first-seen order.
func MasterPathwayIDs(body string) ([]string, error) {
	entries, err := ParseMaster(body)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var ids []string
	for _, entry := range entries {
		if entry.PathwayID == "" {
			continue
		}
		if _, ok := seen[entry.PathwayID]; ok {
			continue
		}
		seen[entry.PathwayID] = struct{}{}
		ids = append(ids, entry.PathwayID)
	}
	return ids, nil
}

// ── public entry points ─────────────────────────────────────────────────

// ParseMaster returns every variant/rendition entry in a master playlist, in
// the same document order and ordinal numbering RewriteMaster uses.
func ParseMaster(body string) ([]Entry, error) {
	var entries []Entry
	_, err := walkMaster(body, func(_ int, e Entry) (string, error) {
		entries = append(entries, e)
		return "", nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// ParseMedia returns every key/map/segment entry in a media playlist, in the
// same document order and ordinal numbering RewriteMedia uses.
func ParseMedia(body string) ([]Entry, error) {
	var entries []Entry
	_, err := walkMedia(body, func(_ int, e Entry) (string, error) {
		entries = append(entries, e)
		return "", nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// RewriteMaster rewrites a master playlist, calling mint once per
// variant/rendition entry (in document order) and substituting its
// returned text for the original URI. Every other line — attributes,
// unknown tags, blank lines — passes through byte-identical.
func RewriteMaster(body string, mint MintFunc) (string, error) {
	return walkMaster(body, entryFunc(mint))
}

// RewriteMedia rewrites a media (segment) playlist, calling mint once per
// key/map/segment entry (in document order) and substituting its returned
// text for the original URI (or URI attribute). Every other line passes
// through byte-identical.
func RewriteMedia(body string, mint MintFunc) (string, error) {
	return walkMedia(body, entryFunc(mint))
}

// ResolveURIRef resolves ref (absolute or relative) against base and — if
// the resolved reference carries no query of its own and shares base's
// host — inherits base's query string (HR-D6 "query-token inheritance":
// some HLS packagers omit repeating a required signed-URL token on every
// child/segment URI, relying on the manifest's own base carrying it; plain
// RFC 3986 relative resolution never copies a query string from the base,
// so this is applied explicitly and only same-host, never across an
// unrelated absolute origin).
func ResolveURIRef(base, ref string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("hls: parse base url: %w", err)
	}
	refURL, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", fmt.Errorf("hls: parse reference uri: %w", err)
	}
	resolved := baseURL.ResolveReference(refURL)
	if resolved.RawQuery == "" && baseURL.RawQuery != "" && resolved.Host == baseURL.Host {
		resolved.RawQuery = baseURL.RawQuery
	}
	return resolved.String(), nil
}
