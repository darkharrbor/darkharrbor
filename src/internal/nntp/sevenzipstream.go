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
		return SevenZipManifest{}, 0, errors.New("7z: no volume files found")
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
		return SevenZipManifest{}, 0, errors.New("7z: no unique contiguous volume set")
	}
	candidates = selected

	manifest := SevenZipManifest{Volumes: make([]SevenZipVolume, len(candidates))}
	sources := make([]bytesource.ByteSource, len(candidates))
	starts := make([]int64, len(candidates)+1)
	for i, candidate := range candidates {
		source, err := p.archiveByteSource(ctx, candidate.index, candidate.file.Segments)
		if err != nil {
			return SevenZipManifest{}, 0, fmt.Errorf("7z: volume %d byte source: %w", i+1, err)
		}
		sources[i] = source
		starts[i+1] = starts[i] + candidate.file.TotalBytes
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
