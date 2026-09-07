package torbox

import (
	"context"
	"crypto/md5" // #nosec G501 -- TorBox usenet checkcached keys on MD5(NZB); not a security primitive
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// ErrNotSubmitted is returned by Poll when the item carries neither a remote
// nor a queued id (the submit loop has not run yet).
var ErrNotSubmitted = errors.New("torbox provider: item has no remote or queued id")

// Adapter implements provider.Provider over the TorBox Client
// cache-check switch, and poll ladder are lifted verbatim from
// cmd/darkharrbor/main.go submitItem/resolveItem and internal/cachegate; all
// retry/state persistence) stays in core and is provider-neutral.
type Adapter struct {
	name   string
	client Client
	policy provider.Policy
	caps   provider.Capabilities

	// slots/freeTier feed SlotStatus (R2 governor redesign). Set once at
	// startup via SetSlotCaps from bootstrap plan detection (ResolveCaps).
	// Zero-value slots defaults to 1 (Free-plan-safe) in SlotStatus.
	slots    int
	freeTier bool

	// B-N4: SlotStatus cache — share one mylist+userinfo probe across a burst
	// of uncached submits. Cache TTL is 30 s; stale on miss, replaced atomically.
	slotMu       sync.Mutex
	slotCached   *provider.SlotStatus
	slotCachedAt time.Time
	slotCacheTTL time.Duration
}

func NewAdapter(name string, client Client, policy provider.Policy) *Adapter {
	return &Adapter{name: name, client: client, policy: policy}
}

var _ provider.Provider = (*Adapter)(nil)
var _ provider.SlotSource = (*Adapter)(nil)
var _ provider.MetainfoSource = (*Adapter)(nil)

// SetSlotCaps wires the bootstrap-discovered Allowed Active Slots ceiling and
// free-tier flag (from torbox.ResolveCaps) into the adapter so SlotStatus can
// report real headroom without re-deriving plan capability logic.
func (a *Adapter) SetSlotCaps(slots int, freeTier bool) *Adapter {
	a.slots = slots
	a.freeTier = freeTier
	return a
}

// SlotStatus implements provider.SlotSource. It combines the bootstrap-known
// Allowed Active Slots ceiling with a live TorBox mylist count of transfers
// currently occupying a slot, plus the account's current abuse-cooldown
// deadline. Called by the governor on every uncached-submission check.
//
// B-N4: Results are cached for slotCacheTTL (default 30 s) so a burst of
// uncached submits shares one mylist+userinfo probe pair instead of N pairs.
func (a *Adapter) SlotStatus(ctx context.Context) (*provider.SlotStatus, error) {
	ttl := a.slotCacheTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}

	a.slotMu.Lock()
	if a.slotCached != nil && time.Since(a.slotCachedAt) < ttl {
		cached := *a.slotCached
		a.slotMu.Unlock()
		return &cached, nil
	}
	a.slotMu.Unlock()

	active, err := a.client.CountActiveSlots(ctx)
	if err != nil {
		return nil, fmt.Errorf("torbox provider: count active slots: %w", err)
	}

	var cooldown time.Time
	if info, infoErr := a.client.GetUserInfo(ctx); infoErr == nil && info != nil {
		cooldown = info.CooldownUntil
	}
	// A failed GetUserInfo does not block slot gating — cooldown reads as
	// "not in cooldown" for this check; the next call will pick it up.

	slots := a.slots
	if slots < 1 {
		slots = 1
	}
	status := &provider.SlotStatus{
		AllowedActiveSlots: slots,
		ActiveCount:        active,
		CooldownUntil:      cooldown,
		FreeTier:           a.freeTier,
	}

	a.slotMu.Lock()
	a.slotCached = status
	a.slotCachedAt = time.Now()
	a.slotMu.Unlock()

	return status, nil
}

func (a *Adapter) Name() string { return a.name }

// Metrics returns the live API call + egress byte counters (B-O4).
// Delegates to the underlying Client; returns nil if the client does not
// implement the metrics accessor (e.g. in tests).
func (a *Adapter) Metrics() *Metrics {
	if m, ok := a.client.(interface{ Metrics() *Metrics }); ok {
		return m.Metrics()
	}
	return nil
}

func (a *Adapter) Capabilities() provider.Capabilities { return a.caps }

func (a *Adapter) SetCapabilities(caps provider.Capabilities) *Adapter {
	a.caps = caps
	return a
}

// CheckCached mirrors internal/cachegate's source-type switch exactly:
// torrents check by infohash, NZBs by submission key.
func (a *Adapter) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	switch item.SourceType {
	case store.SourceTypeTorrent:
		// Prefer RealInfoHash (the actual multi-season pack hash) over InfoHash
		// (which may be a synthetic per-season fabrication). Synthetic items
		// that share a remote_id with other seasons would otherwise fail the
		// eviction check with their per-season hash, not the real pack hash.
		hash := item.Metadata.RealInfoHash
		if hash == "" {
			if item.InfoHash == nil {
				return nil, fmt.Errorf("torbox provider: item %s has no infohash", item.ID)
			}
			hash = *item.InfoHash
		}
		return a.client.CheckCachedTorrent(ctx, hash)
	case store.SourceTypeNZB:
		h, err := nzbContentHash(item)
		if err != nil {
			return nil, err
		}
		return a.client.CheckCachedUsenet(ctx, h)
	default:
		return nil, fmt.Errorf("torbox provider: unknown source type %s", item.SourceType)
	}
}

// Submit performs the create switch lifted from submitItem. The plan
// pre-check is applied first: values come from configuration only (zero
// values skip; item size is unknown pre-resolve, so the size check engages
// only when TotalSize is already populated). The upstream service — and in
// harness runs the faithful fake — remains the enforcement authority.
func (a *Adapter) Submit(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	if bp, ok := a.policy.(*provider.BasicPolicy); ok && bp != nil {
		if err := bp.PlanPreCheck(-1, item.TotalSize); err != nil {
			return nil, err
		}
	}
	createOp := provider.OpCreateUncached
	if opts.AddOnlyIfCached {
		createOp = provider.OpCreateCached
	}
	sourceURI := ""
	if item.SourceURI != nil {
		sourceURI = *item.SourceURI
	}
	switch item.SourceType {
	case store.SourceTypeTorrent:
		req := CreateTorrentTaskRequest{
			Magnet:          sourceURI,
			Name:            item.DisplayName,
			AddOnlyIfCached: opts.AddOnlyIfCached, // redundant safety net
			PolicyOp:        createOp,
		}
		// URL-only releases (RuTracker et al.): the grab's source URI is a
		// Prowlarr-proxied HTTP .torrent link, which TorBox rejects when
		// passed as magnet (BOZO_TORRENT). Download the .torrent and upload
		// the file instead. Magnet-only indexers (EZTV et al.) answer the
		// download URL with a 30x Location: magnet:?xt=… — Go's http client
		// errors on non-HTTP redirect schemes, so fetchTorrentFile captures
		// the magnet and we submit it directly (live 2026-07-08: this exact
		// redirect made all four AITF season submits fall through to the
		// hash fallback). If everything fails and a hash is known, fall back
		// to a bare magnet — the REAL infohash when the grab is synthetic
		// (item.InfoHash is then a fabricated per-season hash that exists in
		// no swarm), logged so the degradation is visible.
		if strings.HasPrefix(sourceURI, "http://") || strings.HasPrefix(sourceURI, "https://") {
			data, magnetLoc, ferr := fetchTorrentFile(ctx, sourceURI)
			fallbackHash := item.Metadata.RealInfoHash
			if fallbackHash == "" && item.InfoHash != nil {
				fallbackHash = *item.InfoHash
			}
			switch {
			case ferr == nil && magnetLoc != "":
				req.Magnet = magnetLoc
			case ferr == nil:
				req.Magnet = ""
				req.TorrentFile = data
				req.TorrentFileName = item.DisplayName + ".torrent"
			case fallbackHash != "":
				slog.Default().Warn("torbox provider: torrent link fetch failed; falling back to bare magnet",
					"item_id", item.ID, "display_name", item.DisplayName,
					"fallback_hash", fallbackHash, "error", ferr)
				req.Magnet = "magnet:?xt=urn:btih:" + fallbackHash +
					"&dn=" + url.QueryEscape(item.DisplayName)
			default:
				return nil, fmt.Errorf("torbox provider: torrent source is an http link and fetch failed (no infohash fallback): %w", ferr)
			}
		}
		return a.client.CreateTorrentTask(ctx, req)
	case store.SourceTypeNZB:
		return a.client.CreateUsenetTask(ctx, CreateUsenetTaskRequest{
			Link:            sourceURI,
			Name:            item.DisplayName,
			AddOnlyIfCached: opts.AddOnlyIfCached,
			PolicyOp:        createOp,
		})
	default:
		return nil, fmt.Errorf("torbox provider: unknown source type %s", item.SourceType)
	}
}

// Poll is the queued/remote id ladder lifted from resolveItem.
func (a *Adapter) Poll(ctx context.Context, item *store.Item) (*provider.TaskStatus, error) {
	switch {
	case item.QueuedID != nil:
		return a.client.GetQueuedStatus(ctx, adapterSourceType(item), *item.QueuedID)
	case item.RemoteID != nil:
		return a.client.GetTaskStatus(ctx, adapterSourceType(item), *item.RemoteID)
	}
	return nil, ErrNotSubmitted
}

// RequestDownloadURL maps the item to the TorBox requestdl call.
func (a *Adapter) RequestDownloadURL(ctx context.Context, item *store.Item, fileID string) (string, error) {
	if item.RemoteID == nil {
		return "", fmt.Errorf("torbox provider: item %s has no remote id for requestdl", item.ID)
	}
	return a.client.RequestDownloadURL(ctx, adapterSourceType(item), *item.RemoteID, fileID)
}

// FetchTorrentFile implements provider.MetainfoSource (TS-0.2/T1): delegates
// to the TorBox exportdata endpoint for an already-submitted torrent. Any
// failure (endpoint rejection, cached-download incompatibility, transport
// error) is returned verbatim for the caller to treat as best-effort
// absence -- this method never itself decides that absence is acceptable.
func (a *Adapter) FetchTorrentFile(ctx context.Context, remoteID string) ([]byte, error) {
	return a.client.ExportTorrentFile(ctx, remoteID)
}

// Remove issues the delete control action for the item's endpoint family.
func (a *Adapter) Remove(ctx context.Context, item *store.Item) error {
	if item.RemoteID == nil {
		return fmt.Errorf("torbox provider: item %s has no remote id for remove", item.ID)
	}
	if item.SourceType == store.SourceTypeNZB {
		return a.client.ControlUsenet(ctx, *item.RemoteID, "delete")
	}
	return a.client.ControlTorrent(ctx, *item.RemoteID, "delete")
}

// adapterSourceType mirrors main.go's sourceTypeStr mapping.
func adapterSourceType(item *store.Item) string {
	if item.SourceType == store.SourceTypeNZB {
		return "usenet"
	}
	return "torrents"
}

// nzbContentHash computes the key TorBox's usenet checkcached oracle expects:
// the lowercase-hex MD5 of the raw NZB bytes. The NZB body is stored inline on
// item.SourceURI by the SAB shim. Verified live against the TorBox API
// (createusenetdownload echoes this exact hash for a given NZB) and matches the
// reference implementation (DebriDav DigestUtils.md5Hex(nzbBytes)). Using the
// item submission key here (the prior behavior) never matched TorBox's index, so
// the usenet cache check always missed.
func nzbContentHash(item *store.Item) (string, error) {
	if item.SourceURI == nil || *item.SourceURI == "" {
		return "", fmt.Errorf("torbox provider: item %s has no nzb content for usenet checkcached", item.ID)
	}
	sum := md5.Sum([]byte(*item.SourceURI)) // #nosec G401 -- MD5 mandated by the TorBox usenet checkcached API
	return hex.EncodeToString(sum[:]), nil
}

// torrentFetchClient fetches .torrent files from indexer/Prowlarr URLs at
// submit time. Package-level for test injection.
var torrentFetchClient = &http.Client{
	Timeout: 30 * time.Second,
	// Redirects are handled manually in fetchTorrentFile so Location:
	// magnet:?… (magnet-only indexers) can be captured instead of erroring.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// maxTorrentFileBytes caps a fetched .torrent file (metadata only; even huge
// packs stay well under this).
const maxTorrentFileBytes = 16 << 20

// fetchTorrentFile downloads the raw .torrent bytes from an HTTP source URI.
// fetchTorrentFile downloads a .torrent from an indexer/Prowlarr URL.
// Redirects are followed manually (max 5 hops) because magnet-only indexers
// answer with Location: magnet:?xt=… — returned as magnetRedirect instead of
// an error so the caller can submit the magnet directly.
func fetchTorrentFile(ctx context.Context, uri string) (data []byte, magnetRedirect string, err error) {
	current := uri
	for hop := 0; hop < 5; hop++ {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if rerr != nil {
			return nil, "", rerr
		}
		resp, derr := torrentFetchClient.Do(req)
		if derr != nil {
			return nil, "", derr
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if loc == "" {
				return nil, "", fmt.Errorf("fetch torrent: redirect (status %d) without Location", resp.StatusCode)
			}
			if strings.HasPrefix(strings.ToLower(loc), "magnet:") {
				return nil, loc, nil
			}
			resolved, perr := req.URL.Parse(loc)
			if perr != nil {
				return nil, "", fmt.Errorf("fetch torrent: bad redirect location %q: %w", loc, perr)
			}
			current = resolved.String()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, "", fmt.Errorf("fetch torrent: status %d", resp.StatusCode)
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxTorrentFileBytes+1))
		_ = resp.Body.Close()
		if rerr != nil {
			return nil, "", rerr
		}
		if len(body) == 0 {
			return nil, "", fmt.Errorf("fetch torrent: empty body")
		}
		if len(body) > maxTorrentFileBytes {
			return nil, "", fmt.Errorf("fetch torrent: exceeds %d bytes", maxTorrentFileBytes)
		}
		return body, "", nil
	}
	return nil, "", fmt.Errorf("fetch torrent: too many redirects")
}
