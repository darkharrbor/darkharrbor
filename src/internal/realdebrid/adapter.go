package realdebrid

import (
	"context"
	"fmt"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// Adapter implements provider.Provider and provider.SlotSource for Real-Debrid.
// It is the only RD-specific type that main.go constructs; everything else
// flows through the neutral provider.Provider interface.
type Adapter struct {
	name        string
	client      Client
	policy      provider.Policy
	caps        provider.Capabilities
	premium     bool // discovered at startup
	slotCeiling int  // /torrents/activeCount limit (PremiumActiveSlots=100)
}

// NewAdapter constructs an Adapter.
func NewAdapter(name string, client Client, policy provider.Policy) *Adapter {
	return &Adapter{name: name, client: client, policy: policy}
}

// SetCapabilities stores the startup-discovered capability set.
func (a *Adapter) SetCapabilities(caps provider.Capabilities) *Adapter {
	a.caps = caps
	return a
}

// SetPremium records the startup-discovered premium status. When false,
// Submit will reject all requests (non-premium RD accounts cannot use DH).
func (a *Adapter) SetPremium(premium bool) *Adapter {
	a.premium = premium
	return a
}

// SetSlotCaps records RD's active-torrent ceiling so the governor can gate
// on slot headroom. Call with PremiumActiveSlots (100) at startup.
func (a *Adapter) SetSlotCaps(ceiling int) *Adapter {
	a.slotCeiling = ceiling
	return a
}

var _ provider.SlotSource = (*Adapter)(nil)

// SlotStatus implements provider.SlotSource. Counts active (non-downloaded)
// torrents in the user's RD account against the Premium slot ceiling.
// Called by the governor on every uncached-submission check.
func (a *Adapter) SlotStatus(ctx context.Context) (*provider.SlotStatus, error) {
	ceiling := a.slotCeiling
	if ceiling < 1 {
		ceiling = PremiumActiveSlots
	}
	// /torrents/activeCount returns {"nb": N, "limit": 100, "list": [...]}
	active, err := a.client.ActiveCount(ctx)
	if err != nil {
		// Non-fatal: on error, report headroom=1 so the governor isn't
		// permanently blocked by a transient API failure.
		return &provider.SlotStatus{AllowedActiveSlots: ceiling, ActiveCount: ceiling - 1}, nil
	}
	return &provider.SlotStatus{AllowedActiveSlots: ceiling, ActiveCount: active}, nil
}

var _ provider.Provider = (*Adapter)(nil)

// Name returns the registry name.
func (a *Adapter) Name() string { return a.name }

// Capabilities returns the provider capability set.
func (a *Adapter) Capabilities() provider.Capabilities { return a.caps }

// CheckCached always returns Cached=false for Real-Debrid.
// RD's instantAvailability endpoint was disabled (error_code 37, verified
// 2026-07-11). Cache detection must use the add-then-poll model: Submit adds
// the magnet; if RD has it cached the status reaches "downloaded" within
// seconds of selectFiles. There is no pre-submit oracle.
// NZBs are not supported (UsenetIngest=false).
func (a *Adapter) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	return &provider.CheckCachedResult{Cached: false}, nil
}

// Submit adds a torrent to RD and starts file selection.
// For RD this is two calls: addMagnet + selectFiles.
// Only cached submissions are expected (CacheOracle=true, requireCached=true default).
// Uncached submissions are allowed when the governor permits.
func (a *Adapter) Submit(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	if !a.premium {
		return nil, fmt.Errorf("rd: non-premium account cannot submit (upgrade required)")
	}
	if item.SourceType == store.SourceTypeNZB {
		return nil, fmt.Errorf("rd: NZB submission not supported")
	}

	magnet := itemMagnet(item)
	if magnet == "" {
		return nil, fmt.Errorf("rd: cannot build magnet for item %s (no infohash or source URI)", item.ID)
	}

	if opts.AddOnlyIfCached {
		if rdID, found, err := a.client.FindByHash(ctx, itemInfoHash(item)); err != nil {
			return nil, fmt.Errorf("rd: find downloaded hash: %w", err)
		} else if found {
			return &provider.CreateTaskResponse{RemoteID: rdID}, nil
		}
	}

	rdID, err := a.client.AddMagnet(ctx, magnet)
	if err != nil {
		// error_code 35 (infringing_file): RD has DMCA-blocked this hash.
		// Permanent, non-retryable — surface as a plain (non-retryable) error
		// so the resolver fails the item immediately and the arr blocklists it.
		if IsInfringingFile(err) {
			return nil, fmt.Errorf("rd: DMCA-blocked hash (infringing_file): %w", err)
		}
		return nil, fmt.Errorf("rd: addMagnet: %w", err)
	}

	if err := a.client.SelectFiles(ctx, rdID, "all"); err != nil {
		// Best-effort cleanup — delete the half-submitted item.
		_ = a.client.DeleteTorrent(context.Background(), rdID)
		return nil, fmt.Errorf("rd: selectFiles: %w", err)
	}

	return &provider.CreateTaskResponse{RemoteID: rdID}, nil
}

// Poll returns the current download status for a submitted item.
func (a *Adapter) Poll(ctx context.Context, item *store.Item) (*provider.TaskStatus, error) {
	if item.RemoteID == nil || *item.RemoteID == "" {
		return nil, fmt.Errorf("rd: poll: item %s has no remote_id", item.ID)
	}
	info, err := a.client.GetTorrentInfo(ctx, *item.RemoteID)
	if err != nil {
		return nil, fmt.Errorf("rd: poll: %w", err)
	}
	return torrentInfoToTaskStatus(info), nil
}

// RequestDownloadURL returns a direct CDN URL for a specific file in a resolved item.
//
// Real-Debrid requires two live API calls per request:
//  1. GET /torrents/info/{id} — to get the current internal link for the file.
//  2. POST /unrestrict/link — to convert the internal link to a CDN URL.
//
// RD's unrestricted URLs expire (they are signed CDN links, not permanent).
// They MUST NOT be stored in the DB — call this method at stream time, not at
// resolve time. The resolver stores only the RD item ID (remote_id) and the
// file list; the CDN URL is always fetched fresh on play. This is why RD items
// use a different stream path than TorBox (where requestdl URLs are stable).
//
// fileID is the RD file ID integer as a string (stored in file_list).
func (a *Adapter) RequestDownloadURL(ctx context.Context, item *store.Item, fileID string) (string, error) {
	if item.RemoteID == nil || *item.RemoteID == "" {
		return "", fmt.Errorf("rd: requestdl: item %s has no remote_id", item.ID)
	}

	// Step 1: fetch current torrent info to get the internal link for this file.
	info, err := a.client.GetTorrentInfo(ctx, *item.RemoteID)
	if err != nil {
		return "", fmt.Errorf("rd: requestdl: get torrent info: %w", err)
	}

	link, err := findLinkForFile(info, fileID)
	if err != nil {
		return "", fmt.Errorf("rd: requestdl: %w", err)
	}

	// Step 2: unrestrict to get the CDN URL. This URL expires — the caller
	// must use it immediately and must not cache/persist it.
	dlURL, err := a.client.UnrestrictLink(ctx, link)
	if err != nil {
		return "", fmt.Errorf("rd: requestdl: unrestrict: %w", err)
	}
	return dlURL, nil
}

// Remove deletes a submitted item from the user's RD account.
func (a *Adapter) Remove(ctx context.Context, item *store.Item) error {
	if item.RemoteID == nil || *item.RemoteID == "" {
		return nil // nothing to remove
	}
	if err := a.client.DeleteTorrent(ctx, *item.RemoteID); err != nil {
		return fmt.Errorf("rd: remove: %w", err)
	}
	return nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// itemInfoHash extracts the hex infohash from an item, preferring RealInfoHash
// (the real hash when item was grabbed via a synthetic season release).
func itemInfoHash(item *store.Item) string {
	if item.Metadata.RealInfoHash != "" {
		return item.Metadata.RealInfoHash
	}
	if item.InfoHash != nil && *item.InfoHash != "" {
		return *item.InfoHash
	}
	return ""
}

// itemMagnet builds a magnet URI from the item. Prefers SourceURI when it
// already is a magnet; falls back to constructing one from the infohash.
func itemMagnet(item *store.Item) string {
	if item.SourceURI != nil {
		uri := strings.TrimSpace(*item.SourceURI)
		if strings.HasPrefix(strings.ToLower(uri), "magnet:") {
			return uri
		}
	}
	hash := itemInfoHash(item)
	if hash == "" {
		return ""
	}
	return "magnet:?xt=urn:btih:" + hash
}

// findLinkForFile maps a fileID string to the corresponding RD link.
// RD's links array is parallel to the selected-files subset of the files array.
func findLinkForFile(info *TorrentInfo, fileID string) (string, error) {
	if len(info.Links) == 0 {
		return "", fmt.Errorf("torrent %s has no download links (status=%s)", info.ID, info.Status)
	}
	// Find the position of fileID among selected files.
	selectedIdx := 0
	for _, f := range info.Files {
		if !f.Selected {
			continue
		}
		if fmt.Sprintf("%d", f.ID) == fileID {
			if selectedIdx >= len(info.Links) {
				return "", fmt.Errorf("file %s index %d exceeds links count %d", fileID, selectedIdx, len(info.Links))
			}
			return info.Links[selectedIdx], nil
		}
		selectedIdx++
	}
	// Fallback: if only one link, use it regardless of fileID.
	if len(info.Links) == 1 {
		return info.Links[0], nil
	}
	return "", fmt.Errorf("file %s not found in torrent %s selected files", fileID, info.ID)
}
