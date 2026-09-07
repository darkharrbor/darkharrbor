package nntp

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"path"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/sidecar"
)

// SubtitleTarget binds a discovered subtitle to an already selected video.
// FileID is the stable sidecar association used by the shared registry.
type SubtitleTarget = sidecar.SubtitleTarget

// SubtitlePayload is a bounded, URL-free lane result ready for HR5.5.
type SubtitlePayload = sidecar.SubtitlePayload

// ReadDirectSubtitles acquires exact direct NZB subtitle files through the
// provider's existing segment ladder. Unmatched or unsafe candidates abstain.
func (p *NNTPProvider) ReadDirectSubtitles(ctx context.Context, itemID string, nzb NZB, targets []SubtitleTarget, maxBytes, maxCount int) ([]SubtitlePayload, error) {
	if maxBytes <= 0 || maxCount <= 0 {
		return nil, nil
	}
	var out []SubtitlePayload
	var totalBytes int64
	for fileIndex, file := range nzb.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, ok := subtitleSubjectFilename(file.Subject)
		if !ok || len(file.Segments) == 0 || file.TotalBytes <= 0 || file.TotalBytes > int64(maxBytes) ||
			file.TotalBytes > sidecar.DefaultMaxBytesPerItem-totalBytes {
			continue
		}
		fileID, ok := matchSubtitleTarget(name, targets)
		if !ok {
			continue
		}
		data, err := p.readNZBSubtitle(ctx, itemID, fileIndex, file, maxBytes)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			continue
		}
		if int64(len(data)) > sidecar.DefaultMaxBytesPerItem-totalBytes {
			continue
		}
		out = append(out, SubtitlePayload{Filename: name, FileID: fileID, Bytes: data})
		totalBytes += int64(len(data))
		if len(out) == maxCount {
			break
		}
	}
	return out, nil
}

// ReadZIPSubtitles discovers and extracts stored/deflated subtitle members
// through the same archive ByteSource used to build the video manifest.
func (p *NNTPProvider) ReadZIPSubtitles(ctx context.Context, itemID string, nzbFiles []NZBFile, targets []SubtitleTarget, maxBytes, maxCount int) ([]SubtitlePayload, error) {
	if maxBytes <= 0 || maxCount <= 0 {
		return nil, nil
	}
	for fileIndex, file := range nzbFiles {
		if !strings.Contains(strings.ToLower(file.Subject), ".zip") || strings.Contains(strings.ToLower(file.Subject), ".par2") {
			continue
		}
		src, err := p.archiveByteSource(withAccountingItemID(ctx, itemID), fileIndex, file.Segments)
		if err != nil {
			return nil, err
		}
		return sidecar.ReadZIPSubtitles(ctx, src, targets, maxBytes, maxCount, sidecar.DefaultMaxBytesPerItem)
	}
	return nil, nil
}

// ReadSevenZipSubtitles extracts Copy-coded members already discovered while
// building the 7z manifest; it does not reparse or create another archive path.
func (p *NNTPProvider) ReadSevenZipSubtitles(ctx context.Context, itemID string, manifest SevenZipManifest, targets []SubtitleTarget, maxBytes, maxCount int) ([]SubtitlePayload, error) {
	if maxBytes <= 0 || maxCount <= 0 {
		return nil, nil
	}
	out := make([]SubtitlePayload, 0, min(len(manifest.Sidecars), maxCount))
	var totalBytes int64
	for _, entry := range manifest.Sidecars {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fileID, ok := matchSubtitleTarget(entry.Filename, targets)
		if !ok || entry.Size <= 0 || entry.Size > int64(maxBytes) || entry.Size > sidecar.DefaultMaxBytesPerItem-totalBytes {
			continue
		}
		var dst bytes.Buffer
		limited := &subtitleLimitWriter{w: &dst, remaining: int64(maxBytes)}
		absoluteEnd := entry.DataOff + entry.Size - 1
		for _, volume := range manifest.Volumes {
			volumeEnd := volume.CumOffset + volume.Size - 1
			if volumeEnd < entry.DataOff {
				continue
			}
			if volume.CumOffset > absoluteEnd {
				break
			}
			start := max(int64(0), entry.DataOff-volume.CumOffset)
			end := min(volume.Size-1, absoluteEnd-volume.CumOffset)
			file := NZBFile{Segments: volume.Segments}
			if err := p.streamNZBByteRange(withAccountingItemID(ctx, itemID), file, volume.NZBFileIdx, start, end, limited, nil, itemID, ""); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				dst.Reset()
				break
			}
		}
		if limited.exceeded || int64(dst.Len()) != entry.Size {
			continue
		}
		out = append(out, SubtitlePayload{Filename: path.Base(entry.Filename), FileID: fileID, Bytes: dst.Bytes()})
		totalBytes += int64(dst.Len())
		if len(out) == maxCount {
			break
		}
	}
	return out, nil
}

func (p *NNTPProvider) readNZBSubtitle(ctx context.Context, itemID string, fileIndex int, file NZBFile, maxBytes int) ([]byte, error) {
	var dst bytes.Buffer
	limited := &subtitleLimitWriter{w: &dst, remaining: int64(maxBytes)}
	err := p.streamNZBByteRange(withAccountingItemID(ctx, itemID), file, fileIndex, 0, int64(maxBytes), limited, nil, itemID, "")
	if err != nil {
		return nil, err
	}
	if limited.exceeded || dst.Len() == 0 {
		return nil, fmt.Errorf("subtitle exceeds limit or is empty")
	}
	return dst.Bytes(), nil
}

type subtitleLimitWriter struct {
	w         io.Writer
	remaining int64
	exceeded  bool
}

func (w *subtitleLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.exceeded = true
		return 0, fmt.Errorf("subtitle exceeds byte limit")
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func subtitleSubjectFilename(subject string) (string, bool) {
	candidates := subjectFilenames(subject)
	for _, candidate := range candidates {
		if !subtitleExtension(candidate) {
			continue
		}
		return candidate, true
	}
	return "", false
}

func subjectFilenames(subject string) []string {
	subject = html.UnescapeString(subject)
	raw := []string{subject}
	for rest := subject; ; {
		first := strings.IndexByte(rest, '"')
		if first < 0 {
			break
		}
		rest = rest[first+1:]
		second := strings.IndexByte(rest, '"')
		if second < 0 {
			break
		}
		raw = append([]string{rest[:second]}, raw...)
		rest = rest[second+1:]
	}
	out := make([]string, 0, len(raw))
	for _, candidate := range raw {
		candidate = strings.TrimSpace(candidate)
		if cut := strings.Index(candidate, " yEnc"); cut >= 0 {
			candidate = strings.TrimSpace(candidate[:cut])
		}
		name := path.Base(candidate)
		if name != candidate || name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, "\\/\r\n\x00") {
			continue
		}
		out = append(out, name)
	}
	return out
}

func subtitleExtension(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".srt", ".ass", ".idx", ".sub":
		return true
	default:
		return false
	}
}

func matchSubtitleTarget(name string, targets []SubtitleTarget) (string, bool) {
	normalized := append([]SubtitleTarget(nil), targets...)
	for i, target := range normalized {
		targetName := target.Filename
		if parsed := subjectFilenames(target.Filename); len(parsed) > 0 {
			targetName = parsed[0]
		}
		normalized[i].Filename = targetName
	}
	return sidecar.MatchSubtitleTarget(name, normalized)
}
