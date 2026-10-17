package nntp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
)

const maxSevenZipVolumes = 1000

// SevenZipVolume describes one physical file in an ordered 7z volume set.
type SevenZipVolume struct {
	Number     int          `json:"number"`
	NZBFileIdx int          `json:"nzb_file_idx"`
	Size       int64        `json:"size"`
	CumOffset  int64        `json:"cum_offset"`
	Segments   []NZBSegment `json:"segments"`
}

// SevenZipManifest is the durable, URL-free map from Copy-coded members to
// their byte spans across an ordered set of NNTP-backed 7z volumes.
type SevenZipManifest struct {
	Volumes  []SevenZipVolume `json:"volumes"`
	Entries  []SevenZipEntry  `json:"entries"`
	Sidecars []SevenZipEntry  `json:"sidecars,omitempty"`
}

// SevenZipEntry identifies one directly streamable member.
type SevenZipEntry struct {
	EntryIdx int    `json:"entry_idx"`
	Filename string `json:"filename"`
	DataOff  int64  `json:"data_off"`
	Size     int64  `json:"size"`
}

func MarshalSevenZipManifest(manifest SevenZipManifest) (string, error) {
	if err := validateSevenZipManifest(manifest); err != nil {
		return "", err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func UnmarshalSevenZipManifest(raw string) (SevenZipManifest, error) {
	var manifest SevenZipManifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return manifest, fmt.Errorf("7z: unmarshal manifest: %w", err)
	}
	if err := validateSevenZipManifest(manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func validateSevenZipManifest(manifest SevenZipManifest) error {
	if len(manifest.Volumes) == 0 || len(manifest.Volumes) > maxSevenZipVolumes {
		return fmt.Errorf("7z: invalid volume count %d", len(manifest.Volumes))
	}
	if len(manifest.Entries) == 0 {
		return errors.New("7z: empty entry manifest")
	}
	var total int64
	seenFileIndexes := make(map[int]struct{}, len(manifest.Volumes))
	for i, volume := range manifest.Volumes {
		if volume.Number != i+1 || volume.NZBFileIdx < 0 || volume.Size <= 0 ||
			volume.CumOffset != total || len(volume.Segments) == 0 {
			return fmt.Errorf("7z: invalid volume %d", i)
		}
		if _, exists := seenFileIndexes[volume.NZBFileIdx]; exists {
			return fmt.Errorf("7z: duplicate volume file index %d", volume.NZBFileIdx)
		}
		seenFileIndexes[volume.NZBFileIdx] = struct{}{}
		if volume.Size > int64(^uint64(0)>>1)-total {
			return errors.New("7z: volume size overflow")
		}
		total += volume.Size
	}
	for _, group := range []struct {
		kind    string
		entries []SevenZipEntry
	}{{"entry", manifest.Entries}, {"sidecar", manifest.Sidecars}} {
		for i, entry := range group.entries {
			if entry.EntryIdx != i || entry.Filename == "" || entry.DataOff < 0 || entry.Size <= 0 ||
				entry.DataOff > total || entry.Size > total-entry.DataOff {
				return fmt.Errorf("7z: invalid %s %d", group.kind, i)
			}
		}
	}
	return nil
}

// ErrNoSevenZipVolumes and ErrAmbiguousSevenZipVolumes are layout failures
// determined by the NZB itself: retrying cannot change them.
var (
	ErrNoSevenZipVolumes        = errors.New("7z: no volume files found")
	ErrAmbiguousSevenZipVolumes = errors.New("7z: no unique contiguous volume set")
)

// BuildSevenZipManifest parses a Copy-coded 7z archive through the existing
// cancellable NNTP ByteSource ladder. Split .7z.001 volumes are composed by raw
// concatenation, which is the 7z volume format.
func (p *NNTPProvider) BuildSevenZipManifest(ctx context.Context, nzb NZB) (SevenZipManifest, int64, error) {
	type candidate struct {
		file  NZBFile
		index int
		stem  string
		part  int
	}
	candidates := make([]candidate, 0, len(nzb.Files))
	for i, file := range nzb.Files {
		stem, part, ok := parseSevenZipVolumeSubject(file.Subject)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{file: file, index: i, stem: stem, part: part})
	}
	if len(candidates) == 0 {
		return SevenZipManifest{}, 0, ErrNoSevenZipVolumes
	}
	groups := make(map[string][]candidate)
	for _, candidate := range candidates {
		groups[candidate.stem] = append(groups[candidate.stem], candidate)
	}
	var selected []candidate
	var selectedBytes int64
	ambiguous := false
	for _, group := range groups {
		sort.SliceStable(group, func(i, j int) bool { return group[i].part < group[j].part })
		if len(group) > maxSevenZipVolumes {
			continue
		}
		split := group[0].part > 0
		var groupBytes int64
		valid := true
		for i, candidate := range group {
			wantPart := 0
			if split {
				wantPart = i + 1
			}
			if candidate.part != wantPart || len(candidate.file.Segments) == 0 || candidate.file.TotalBytes <= 0 ||
				candidate.file.TotalBytes > int64(^uint64(0)>>1)-groupBytes {
				valid = false
				break
			}
			groupBytes += candidate.file.TotalBytes
		}
		if !valid {
			continue
		}
		if groupBytes == selectedBytes {
			ambiguous = true
			continue
		}
		if groupBytes > selectedBytes {
			selected, selectedBytes = group, groupBytes
			ambiguous = false
		}
	}
	if len(selected) == 0 || ambiguous {
		return SevenZipManifest{}, 0, ErrAmbiguousSevenZipVolumes
	}
	candidates = selected

	manifest := SevenZipManifest{Volumes: make([]SevenZipVolume, len(candidates))}
	sources := make([]bytesource.ByteSource, len(candidates))
	starts := make([]int64, len(candidates)+1)
	// NZB-declared segment bytes are encoded article sizes (~3% above the
	// decoded payload), so addressing the archive in declared coordinates
	// put the 7z next header (at the archive's end) at the wrong offset and
	// every split release failed with "next header CRC mismatch". Derive
	// exact decoded geometry from the yEnc headers of the first segment of
	// the first and last volume (see decodedVolumeGeometry).
	if p.log != nil {
		layout := make([]string, 0, len(candidates))
		for _, c := range candidates {
			layout = append(layout, fmt.Sprintf("%d:%d", c.part, len(c.file.Segments)))
		}
		p.log.Debug("7z: selected volumes", "count", len(candidates), "part_segments", strings.Join(layout, ","))
	}
	files := make([]NZBFile, len(candidates))
	for i, candidate := range candidates {
		files[i] = candidate.file
	}
	geoms := p.sevenZipVolumeGeometry(ctx, files)
	for i, candidate := range candidates {
		geom := geoms[i]
		source, err := p.archiveByteSourceGeometry(ctx, candidate.index, candidate.file.Segments, geom)
		if err != nil {
			return SevenZipManifest{}, 0, fmt.Errorf("7z: volume %d byte source: %w", i+1, err)
		}
		sources[i] = source
		volSize := candidate.file.TotalBytes
		if geom.ok() {
			volSize = geom.total
		}
		starts[i+1] = starts[i] + volSize
		manifest.Volumes[i] = SevenZipVolume{
			Number: i + 1, NZBFileIdx: candidate.index, Size: candidate.file.TotalBytes,
			CumOffset: starts[i], Segments: candidate.file.Segments,
		}
	}
	source := &sevenZipConcatSource{sources: sources, starts: starts, key: "nntp-7z"}
	parsed, err := archiveparser.ParseSevenZipCopyEntries(ctx, source)
	if err != nil {
		return SevenZipManifest{}, 0, err
	}
	var total int64
	for _, entry := range parsed {
		if entry.Size <= 0 {
			continue
		}
		if subtitleExtension(entry.Name) {
			if len(manifest.Sidecars) < sidecar.DefaultMaxResourcesPerItem && entry.Size <= sidecar.DefaultMaxResourceBytes {
				manifest.Sidecars = append(manifest.Sidecars, SevenZipEntry{
					EntryIdx: len(manifest.Sidecars), Filename: entry.Name,
					DataOff: entry.DataOffset, Size: entry.Size,
				})
			}
			continue
		}
		if !isVideoFilename(entry.Name) {
			continue
		}
		manifest.Entries = append(manifest.Entries, SevenZipEntry{
			EntryIdx: len(manifest.Entries), Filename: entry.Name,
			DataOff: entry.DataOffset, Size: entry.Size,
		})
		if entry.Size > int64(^uint64(0)>>1)-total {
			return SevenZipManifest{}, 0, errors.New("7z: entry size overflow")
		}
		total += entry.Size
	}
	if err := validateSevenZipManifest(manifest); err != nil {
		return SevenZipManifest{}, 0, err
	}
	return manifest, total, nil
}

func (p *NNTPProvider) BuildSevenZipManifestForItem(ctx context.Context, itemID string, nzb NZB) (SevenZipManifest, int64, error) {
	return p.BuildSevenZipManifest(withAccountingItemID(ctx, itemID), nzb)
}

func (p *NNTPProvider) StreamSevenZipEntry(ctx context.Context, manifestJSON string, entryIdx int, w http.ResponseWriter, rangeHeader, itemID, contentKey string) error {
	ctx = withAccountingItemID(ctx, itemID)
	manifest, err := UnmarshalSevenZipManifest(manifestJSON)
	if err != nil {
		return err
	}
	return streamSevenZipEntry(ctx, manifest, entryIdx, w, rangeHeader, func(ctx context.Context, volume SevenZipVolume, start, end int64, dst io.Writer, flush func()) error {
		file := NZBFile{Segments: volume.Segments}
		return p.streamNZBByteRange(ctx, file, volume.NZBFileIdx, start, end, dst, flush, itemID, contentKey)
	})
}

type sevenZipVolumeStreamer func(context.Context, SevenZipVolume, int64, int64, io.Writer, func()) error

func streamSevenZipEntry(ctx context.Context, manifest SevenZipManifest, entryIdx int, w http.ResponseWriter, rangeHeader string, stream sevenZipVolumeStreamer) error {
	if err := validateSevenZipManifest(manifest); err != nil {
		return err
	}
	if stream == nil {
		return errors.New("7z: nil volume streamer")
	}
	if entryIdx < 0 || entryIdx >= len(manifest.Entries) {
		return fmt.Errorf("7z: entry %d out of range", entryIdx)
	}
	entry := manifest.Entries[entryIdx]
	start, end, partial, ok := parseSevenZipRange(rangeHeader, entry.Size)
	if !ok {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return nil
	}
	w.Header().Set("Content-Type", zipMIME(entry.Filename))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, entry.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	absoluteStart, absoluteEnd := entry.DataOff+start, entry.DataOff+end
	counter := &countingWriter{w: w}
	flush := func() {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	for _, volume := range manifest.Volumes {
		volumeEnd := volume.CumOffset + volume.Size - 1
		if volumeEnd < absoluteStart {
			continue
		}
		if volume.CumOffset > absoluteEnd {
			break
		}
		localStart := max(int64(0), absoluteStart-volume.CumOffset)
		localEnd := min(volume.Size-1, absoluteEnd-volume.CumOffset)
		if err := stream(ctx, volume, localStart, localEnd, counter, flush); err != nil {
			return fmt.Errorf("7z: stream volume %d: %w", volume.Number, err)
		}
	}
	if counter.n != end-start+1 {
		return io.ErrUnexpectedEOF
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func parseSevenZipRange(header string, size int64) (start, end int64, partial, ok bool) {
	if size <= 0 {
		return 0, 0, false, false
	}
	if header == "" {
		return 0, size - 1, false, true
	}
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, 0, false, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false, false
	}
	if dash == 0 {
		suffix, err := strconv.ParseInt(spec[1:], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, false
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true, true
	}
	start, err := strconv.ParseInt(spec[:dash], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, false
	}
	end = size - 1
	if dash+1 < len(spec) {
		end, err = strconv.ParseInt(spec[dash+1:], 10, 64)
		if err != nil || end < start {
			return 0, 0, false, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, true
}

func parseSevenZipVolumeSubject(subject string) (stem string, part int, ok bool) {
	lower := strings.ToLower(subject)
	pos := strings.Index(lower, ".7z")
	if pos < 0 {
		return "", 0, false
	}
	start := 0
	if quote := strings.LastIndexAny(lower[:pos], "\"'"); quote >= 0 {
		start = quote + 1
	} else {
		start = pos
		for start > 0 && lower[start-1] != '/' && lower[start-1] != '\\' && lower[start-1] != ' ' {
			start--
		}
	}
	stem = lower[start:pos]
	if stem == "" {
		return "", 0, false
	}
	rest := lower[pos+3:]
	if strings.HasPrefix(rest, ".") {
		if len(rest) < 4 {
			return "", 0, false
		}
		digits := rest[1:4]
		for i := range digits {
			if digits[i] < '0' || digits[i] > '9' {
				return "", 0, false
			}
		}
		if len(rest) > 4 && rest[4] != '"' && rest[4] != '\'' && rest[4] != ' ' {
			return "", 0, false
		}
		number, err := strconv.Atoi(digits)
		if err != nil || number == 0 {
			return "", 0, false
		}
		return stem, number, true
	}
	if len(rest) > 0 && rest[0] != '"' && rest[0] != '\'' && rest[0] != ' ' {
		return "", 0, false
	}
	return stem, 0, true
}

func isVideoFilename(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv", ".mp4", ".m4v", ".avi", ".ts", ".m2ts", ".webm":
		return true
	default:
		return false
	}
}

type sevenZipConcatSource struct {
	sources []bytesource.ByteSource
	starts  []int64
	key     string
}

func (s *sevenZipConcatSource) Size() int64 { return s.starts[len(s.starts)-1] }
func (s *sevenZipConcatSource) Key() string { return s.key }
func (s *sevenZipConcatSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: false, TailCost: bytesource.TailCostSegmented}
}

func (s *sevenZipConcatSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
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
	for i, source := range s.sources {
		if n == len(p) {
			break
		}
		end := s.starts[i+1]
		if off >= end {
			continue
		}
		local := max(int64(0), off-s.starts[i])
		want := min(int64(len(p)-n), end-(s.starts[i]+local))
		got, err := source.ReadAt(ctx, p[n:n+int(want)], local)
		n += got
		off += int64(got)
		if err != nil && !errors.Is(err, io.EOF) {
			return n, err
		}
		if int64(got) != want {
			return n, io.ErrUnexpectedEOF
		}
	}
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}

// volumeGeometry is the exact decoded layout of one NZB file posted with a
// fixed yEnc part size: every segment decodes to part bytes except the last.
type volumeGeometry struct {
	part, total int64
	segments    int
}

func (g volumeGeometry) ok() bool {
	if g.part <= 0 || g.total <= 0 || g.segments <= 0 {
		return false
	}
	last := g.total - g.part*int64(g.segments-1)
	return last > 0 && last <= g.part
}

// starts returns the cumulative decoded offset table (len segments+1).
func (g volumeGeometry) starts() []int64 {
	out := make([]int64, g.segments+1)
	for i := 1; i < g.segments; i++ {
		out[i] = int64(i) * g.part
	}
	out[g.segments] = g.total
	return out
}

// sevenZipVolumeGeometry returns an exact decoded geometry per volume, or a
// zero geometry (declared-size addressing) where it could not be derived.
// Split 7z volumes share a size except the last, but posters vary the first
// (seen live: 150 MiB first, 200 MiB middle), so one volume per distinct
// segment count is probed and reused for the others; the last is probed on
// its own. Each probe is one article fetch.
func (p *NNTPProvider) sevenZipVolumeGeometry(ctx context.Context, files []NZBFile) []volumeGeometry {
	out := make([]volumeGeometry, len(files))
	bySegments := map[int]volumeGeometry{}
	for i, file := range files {
		n := len(file.Segments)
		if i < len(files)-1 {
			if g, seen := bySegments[n]; seen {
				out[i] = g
				continue
			}
		}
		g := p.probeVolumeGeometry(ctx, file)
		out[i] = g
		if i < len(files)-1 {
			bySegments[n] = g
		}
	}
	return out
}

// probeVolumeGeometry fetches a file's first segment and reads its decoded
// length (the part size) and the =ybegin size= file total. Any failure or
// inconsistency yields a zero geometry, i.e. declared-size addressing.
func (p *NNTPProvider) probeVolumeGeometry(ctx context.Context, file NZBFile) volumeGeometry {
	if p.pool == nil || len(file.Segments) == 0 {
		return volumeGeometry{}
	}
	seg := file.Segments[0]
	var g volumeGeometry
	err := fetchArticleFromPoolTotal(ctx, p.pool, p.log, "7z-geometry", seg.Number, seg.MessageID, !p.skipCRC,
		func(data []byte, total int64) error {
			g = volumeGeometry{part: int64(len(data)), total: total, segments: len(file.Segments)}
			return nil
		})
	if p.log != nil {
		p.log.Debug("7z: volume geometry probe", "segments", len(file.Segments), "part", g.part, "total", g.total, "declared_total", file.TotalBytes, "ok", err == nil && g.ok(), "error", err)
	}
	if err != nil || !g.ok() {
		return volumeGeometry{}
	}
	return g
}

// archiveByteSourceGeometry is archiveByteSource with an exact decoded offset
// table when geom is valid; recorded offsets still take precedence in the
// cache path, and an invalid geom keeps the declared-size fallback.
func (p *NNTPProvider) archiveByteSourceGeometry(ctx context.Context, fileIndex int, segments []NZBSegment, geom volumeGeometry) (bytesource.ByteSource, error) {
	if !geom.ok() || geom.segments != len(segments) {
		return p.archiveByteSource(ctx, fileIndex, segments)
	}
	itemID := accountingItemID(ctx)
	fileKey := "nntp-archive"
	if itemID != "" {
		fileKey += "|" + itemID
	}
	if p.cache != nil {
		return p.cache.segmentByteSourceStarts(ctx, fileKey, itemID, "", fileIndex, segments, "build", geom.starts())
	}
	fetch := func(fetchCtx context.Context, chunk bytesource.Chunk) ([]byte, error) {
		segment := segments[chunk.Index]
		return fetchArticleBytesWithRetryAt(fetchCtx, p.pool, p.log, "build", segment.Number, segment.MessageID, segment.PostedAt, !p.skipCRC)
	}
	return newSegmentByteSource(fileKey, segments, geom.starts(), fetch)
}
