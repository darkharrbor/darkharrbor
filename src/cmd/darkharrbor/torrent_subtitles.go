package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/api"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

type torrentSubtitleURLProvider interface {
	RequestDownloadURL(context.Context, *store.Item, string) (string, error)
}

func readTorrentFileSubtitles(
	ctx context.Context,
	item *store.Item,
	files []provider.CachedFile,
	meta *torrentmeta.TorrentMeta,
	targets []sidecar.SubtitleTarget,
	prov torrentSubtitleURLProvider,
	gov *accountgov.Governor,
	windowBytes int64,
) ([]sidecar.SubtitlePayload, error) {
	payloads, err := readTorrentDirectSubtitles(ctx, item, files, meta, targets, prov, gov, windowBytes,
		sidecar.DefaultMaxResourceBytes, sidecar.DefaultMaxResourcesPerItem, sidecar.DefaultMaxBytesPerItem)
	remainingCount := sidecar.DefaultMaxResourcesPerItem - len(payloads)
	remainingBytes := sidecar.DefaultMaxBytesPerItem - subtitlePayloadBytes(payloads)
	if remainingCount <= 0 || remainingBytes <= 0 {
		return payloads, err
	}
	archived, archiveErr := readTorrentZIPSubtitles(ctx, item, files, meta, targets, prov, gov, windowBytes,
		sidecar.DefaultMaxResourceBytes, remainingCount, remainingBytes)
	payloads = append(payloads, archived...)
	if err == nil {
		err = archiveErr
	}
	return payloads, err
}

func torrentSubtitleTargets(files []provider.CachedFile, meta *torrentmeta.TorrentMeta) []sidecar.SubtitleTarget {
	matches := output.MatchTorrentMetaBySize(files, meta)
	targets := make([]sidecar.SubtitleTarget, 0, len(files))
	for i, file := range files {
		name := torrentSidecarPath(file, matches[i])
		if file.FileID != "" && isVideoFilename(name) {
			targets = append(targets, sidecar.SubtitleTarget{Filename: name, FileID: file.FileID})
		}
	}
	return targets
}

func readTorrentDirectSubtitles(
	ctx context.Context,
	item *store.Item,
	files []provider.CachedFile,
	meta *torrentmeta.TorrentMeta,
	targets []sidecar.SubtitleTarget,
	prov torrentSubtitleURLProvider,
	gov *accountgov.Governor,
	windowBytes int64,
	maxBytes, maxCount int,
	maxTotalBytes int64,
) ([]sidecar.SubtitlePayload, error) {
	if item == nil || prov == nil || len(targets) == 0 || maxBytes <= 0 || maxCount <= 0 || maxTotalBytes <= 0 {
		return nil, nil
	}
	matches := output.MatchTorrentMetaBySize(files, meta)
	out := make([]sidecar.SubtitlePayload, 0, min(len(files), maxCount))
	var totalBytes int64
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		filename, ok := sidecar.SubtitleFilename(torrentSidecarPath(file, matches[i]))
		if !ok || file.FileID == "" || file.Size <= 0 || file.Size > int64(maxBytes) || file.Size > maxTotalBytes-totalBytes {
			continue
		}
		fileID, ok := sidecar.MatchSubtitleTarget(filename, targets)
		if !ok {
			continue
		}
		src, ok := torrentSubtitleSource(ctx, item, file, prov, gov, windowBytes)
		if !ok {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return out, ctxErr
			}
			continue
		}
		data := make([]byte, int(file.Size))
		n, readErr := src.ReadAt(ctx, data, 0)
		if n != len(data) || readErr != nil && !errors.Is(readErr, io.EOF) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return out, ctxErr
			}
			continue
		}
		out = append(out, sidecar.SubtitlePayload{Filename: filename, FileID: fileID, Bytes: data})
		totalBytes += int64(len(data))
		if len(out) == maxCount {
			break
		}
	}
	return out, nil
}

func readTorrentZIPSubtitles(
	ctx context.Context,
	item *store.Item,
	files []provider.CachedFile,
	meta *torrentmeta.TorrentMeta,
	targets []sidecar.SubtitleTarget,
	prov torrentSubtitleURLProvider,
	gov *accountgov.Governor,
	windowBytes int64,
	maxBytes, maxCount int,
	maxTotalBytes int64,
) ([]sidecar.SubtitlePayload, error) {
	if item == nil || prov == nil || len(targets) == 0 || maxBytes <= 0 || maxCount <= 0 || maxTotalBytes <= 0 {
		return nil, nil
	}
	matches := output.MatchTorrentMetaBySize(files, meta)
	var out []sidecar.SubtitlePayload
	var totalBytes int64
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !strings.EqualFold(path.Ext(torrentSidecarPath(file, matches[i])), ".zip") || file.FileID == "" || file.Size <= 0 {
			continue
		}
		src, ok := torrentSubtitleSource(ctx, item, file, prov, gov, windowBytes)
		if !ok {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			continue
		}
		payloads, err := sidecar.ReadZIPSubtitles(ctx, src, targets, maxBytes, maxCount-len(out), maxTotalBytes-totalBytes)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return out, ctxErr
			}
			continue
		}
		out = append(out, payloads...)
		totalBytes += subtitlePayloadBytes(payloads)
		if len(out) == maxCount {
			break
		}
	}
	return out, nil
}

func subtitlePayloadBytes(payloads []sidecar.SubtitlePayload) int64 {
	var total int64
	for _, payload := range payloads {
		total += int64(len(payload.Bytes))
	}
	return total
}

func torrentSubtitleSource(ctx context.Context, item *store.Item, file provider.CachedFile, prov torrentSubtitleURLProvider, gov *accountgov.Governor, windowBytes int64) (bytesource.ByteSource, bool) {
	cdnURL := file.RequestDLURL
	if cdnURL == "" {
		cdnURL, _ = prov.RequestDownloadURL(ctx, item, file.FileID)
	}
	if cdnURL == "" {
		return nil, false
	}
	providerFileID := file.FileID
	reresolve := func(rctx context.Context) (string, error) {
		return prov.RequestDownloadURL(rctx, item, providerFileID)
	}
	src, ranged, err := api.NewProbedCDNByteSource(ctx,
		"torrent-subtitle|"+api.ResolveKey(item.ID, providerFileID), cdnURL, windowBytes,
		reresolve, nil, gov, accountgov.PriorityGrab, item.ID)
	return src, err == nil && ranged && src != nil && src.Size() == file.Size
}

func registerTorrentSubtitles(ctx context.Context, log *slog.Logger, writer *output.StrmWriter, item *store.Item, payloads []sidecar.SubtitlePayload) error {
	registered := 0
	for _, payload := range payloads {
		_, err := writer.RegisterSubtitle(ctx, item, payload.FileID, payload.Filename, payload.Bytes)
		if errors.Is(err, sidecar.ErrRejected) {
			continue
		}
		if errors.Is(err, sidecar.ErrCapacity) {
			break
		}
		if err != nil {
			return err
		}
		registered++
	}
	if registered > 0 {
		log.Info("resolver: torrent subtitles registered", "count", registered)
	}
	return nil
}

func torrentSidecarPath(file provider.CachedFile, matched *torrentmeta.FileEntry) string {
	if matched != nil {
		return matched.Path
	}
	if file.RelativePath != "" {
		return file.RelativePath
	}
	return file.Name
}
