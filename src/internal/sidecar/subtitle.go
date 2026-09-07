package sidecar

import (
	"context"
	"errors"
	"path"
	"strings"
	"unicode/utf8"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// SubtitleTarget binds one discovered subtitle to an already selected video.
type SubtitleTarget struct {
	Filename string
	FileID   string
}

// SubtitlePayload is a bounded, URL-free lane result ready for Register.
type SubtitlePayload struct {
	Filename string
	FileID   string
	Bytes    []byte
}

// SubtitleFilename returns a safe basename for one supported subtitle path.
func SubtitleFilename(name string) (string, bool) {
	base, ok := safePathBase(name)
	if !ok {
		return "", false
	}
	_, ok = SubtitleMediaType(base)
	return base, ok
}

// MatchSubtitleTarget associates by exact stem, allowing one trailing
// language tag. A single selected video is unambiguous by construction.
func MatchSubtitleTarget(name string, targets []SubtitleTarget) (string, bool) {
	name, ok := SubtitleFilename(name)
	if !ok {
		return "", false
	}
	if len(targets) == 1 {
		_, targetOK := safePathBase(targets[0].Filename)
		return targets[0].FileID, targetOK && targets[0].FileID != ""
	}
	stem := strings.TrimSuffix(name, path.Ext(name))
	candidateStems := []string{stem}
	if dot := strings.LastIndexByte(stem, '.'); dot >= 0 {
		candidateStems = append(candidateStems, stem[:dot])
	}
	matched := ""
	for _, target := range targets {
		targetName, targetOK := safePathBase(target.Filename)
		if !targetOK || target.FileID == "" {
			continue
		}
		targetStem := strings.TrimSuffix(targetName, path.Ext(targetName))
		for _, candidate := range candidateStems {
			if strings.EqualFold(candidate, targetStem) {
				if matched != "" && matched != target.FileID {
					return "", false
				}
				matched = target.FileID
			}
		}
	}
	return matched, matched != ""
}

// ReadZIPSubtitles discovers and extracts stored/deflated members through the
// shared archive reader. Unsupported or contradictory entries abstain.
func ReadZIPSubtitles(ctx context.Context, src bytesource.ByteSource, targets []SubtitleTarget, maxBytes, maxCount int, maxTotalBytes int64) ([]SubtitlePayload, error) {
	if maxBytes <= 0 || maxCount <= 0 || maxTotalBytes <= 0 {
		return nil, nil
	}
	entries, _, err := archiveparser.ParseZIPSubtitleEntries(ctx, src)
	if errors.Is(err, archiveparser.ErrNoSubtitleEntries) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]SubtitlePayload, 0, min(len(entries), maxCount))
	var totalBytes int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		filename, ok := SubtitleFilename(entry.Filename)
		if !ok {
			continue
		}
		fileID, ok := MatchSubtitleTarget(filename, targets)
		if !ok {
			continue
		}
		data, readErr := archiveparser.ReadZIPSubtitle(ctx, src, entry, maxBytes)
		if readErr != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			continue
		}
		if int64(len(data)) > maxTotalBytes-totalBytes {
			break
		}
		out = append(out, SubtitlePayload{Filename: filename, FileID: fileID, Bytes: data})
		totalBytes += int64(len(data))
		if len(out) == maxCount {
			break
		}
	}
	return out, nil
}

func safePathBase(name string) (string, bool) {
	if name == "" || len(name) > 4096 || !utf8.ValidString(name) || path.IsAbs(name) ||
		strings.ContainsAny(name, "\\\r\n\x00") {
		return "", false
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".." {
			return "", false
		}
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	base := path.Base(clean)
	return base, base != "" && base != "." && base != ".."
}
