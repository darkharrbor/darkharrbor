package nntp

import (
	"context"
	"net/http"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

// provider.UsenetProvider. Newshosting/NNTP is usenet implementation #1 with
// ZERO streaming-logic change — Probe/Stream/StreamRAR bodies are untouched
// the connection-count Policy, and the manifest-form RAR entry point.

// providerName is the registry name this NNTPProvider instance answers to.
// Defaulted in New (newshosting); overridable for additional named NNTP
// providers (scenario U-1) via SetProviderName.
const defaultProviderName = "newshosting"

// SetProviderName sets the registry name (call during wiring, before serving;
// not goroutine-safe after startup, same contract as SetCache).
func (p *NNTPProvider) SetProviderName(name string) {
	if name != "" {
		p.name = name
		p.pool.setFinalRungAccounting(p.Name(), p.finalRungReporter)
		if p.cache != nil {
			p.cache.useFinalRungAccounting(p.Name(), p.finalRungReporter)
		}
	}
}

// Name returns the registry name of this provider instance.
func (p *NNTPProvider) Name() string {
	if p.name == "" {
		return defaultProviderName
	}
	return p.name
}

// StreamRARManifest streams RAR-packed video bytes described by the
// JSON-encoded manifest — the representation already stored on
// store.Item.RarManifest. Thin unmarshal-and-delegate wrapper over the
// unchanged concrete StreamRAR (documented interface-form deviation,
// gate artifact §F-A: lifting []RARPart verbatim would force a
// provider->nntp import, inverting the dependency direction).
func (p *NNTPProvider) StreamRARManifest(ctx context.Context, manifestJSON string, totalVideoBytes int64, w http.ResponseWriter, rangeHeader, itemID, contentKey string) error {
	return p.StreamEncryptedRARManifest(ctx, manifestJSON, totalVideoBytes, w, rangeHeader, itemID, contentKey, "")
}

// StreamEncryptedRARManifest supplies a request-local archive password without
// widening provider.UsenetProvider or persisting password/key material.
func (p *NNTPProvider) StreamEncryptedRARManifest(ctx context.Context, manifestJSON string, totalVideoBytes int64, w http.ResponseWriter, rangeHeader, itemID, contentKey, password string) error {
	manifest, err := UnmarshalRARManifest(manifestJSON)
	if err != nil {
		return err
	}
	return p.streamRAR(ctx, manifest, totalVideoBytes, w, rangeHeader, itemID, contentKey, password)
}

// AuditHealth runs the NS-6.1 STAT health-audit sample against this
// provider's pool (including any wired NS-1.2 failover) for segs, and
// returns the aggregate completeness/dead-region result. It performs no
// persistence and triggers no repair or re-search -- pure measurement,
// consistent with this row's scope; the caller (cmd/darkharrbor) owns
// persisting the result and deciding whether the item is now decayed.
func (p *NNTPProvider) AuditHealth(ctx context.Context, segs []SampledSegment) AuditResult {
	return RunHealthAudit(ctx, p.pool, p.log, segs)
}

// Interface conformance.
var _ provider.UsenetProvider = (*NNTPProvider)(nil)

// CorrectedTotal implements provider.UsenetProvider. Parses the NZB to find
// the target file's segments, then delegates to SegmentCache.CorrectedTotal.
func (p *NNTPProvider) CorrectedTotal(ctx context.Context, itemID string, nzbData []byte, fileIndex int) int64 {
	if p.cache == nil {
		return 0
	}
	nzb, err := ParseNZB(nzbData)
	if err != nil || len(nzb.Files) == 0 {
		return 0
	}
	files := nzb.Files
	videoFiles := filterNZBVideoFiles(files)
	if len(videoFiles) > 0 {
		files = videoFiles
	}
	if fileIndex < 0 || fileIndex >= len(files) {
		fileIndex = 0
	}
	main := files[fileIndex]
	declSizes := make([]int64, len(main.Segments))
	for i, seg := range main.Segments {
		declSizes[i] = seg.Bytes
	}
	offsetKey := SegmentOffsetKey(ContentKey(nzbData), main.Segments)
	return p.cache.CorrectedTotal(ctx, itemID, offsetKey, fileIndex, len(main.Segments), declSizes)
}
