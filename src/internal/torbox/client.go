package torbox

import (
	"context"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

// Client is the interface the TorBox provider plugin implements.
// Core depends only on this interface and never imports http_client directly.
type Client interface {
	CreateTorrentTask(ctx context.Context, req CreateTorrentTaskRequest) (*CreateTaskResponse, error)
	CreateUsenetTask(ctx context.Context, req CreateUsenetTaskRequest) (*CreateTaskResponse, error)
	CheckCachedTorrent(ctx context.Context, hash string) (*CheckCachedResult, error)
	CheckCachedUsenet(ctx context.Context, nzbHash string) (*CheckCachedResult, error)
	GetQueuedStatus(ctx context.Context, sourceType string, queuedID string) (*TaskStatus, error)
	GetTaskStatus(ctx context.Context, sourceType string, remoteID string) (*TaskStatus, error)
	// RequestDownloadURL returns the requestdl permalink for a specific file
	// in a TorBox torrent or usenet item. sourceType is "torrents" or "usenet".
	// remoteID is the TorBox torrent_id / usenetdownload_id. fileID is the
	// file-level id from the item's file list.
	// The URL has a 3-hour window to START streaming; it is metered for
	RequestDownloadURL(ctx context.Context, sourceType string, remoteID string, fileID string) (string, error)

	// ControlTorrent sends a control action to a TorBox torrent item.
	// action must be "delete" for janitor cleanup.
	ControlTorrent(ctx context.Context, remoteID, action string) error

	// ControlUsenet sends a control action to a TorBox usenet item.
	// action must be "delete" for janitor cleanup.
	ControlUsenet(ctx context.Context, remoteID, action string) error

	// GetUserInfo fetches plan/subscription/cooldown state from TorBox
	// (R2 governor redesign: SlotStatus reads CooldownUntil from this).
	GetUserInfo(ctx context.Context) (*UserInfo, error)

	// CountActiveSlots returns the live count of uncached TorBox torrents
	// occupying an Allowed Active Slot. Cached torrents and Usenet work do
	// not consume this ceiling.
	CountActiveSlots(ctx context.Context) (int, error)
	// Metrics returns the live API call + egress byte counters (B-O4).
	Metrics() *Metrics

	// ExportTorrentFile returns the raw .torrent bytes TorBox holds for an
	// already-submitted torrent (TS-0.2/T1). Calls GET
	// /api/torrents/exportdata?type=file, which TorBox documents as
	// incompatible with already-cached downloads -- callers must treat any
	// error as best-effort absence, never fatal.
	ExportTorrentFile(ctx context.Context, remoteID string) ([]byte, error)
}

// CreateTorrentTaskRequest mirrors TorBoxarr verbatim — fields kept for
// full qBit emulator compatibility. TorBox-specific request shapes stay in
// this package (plan §3.1.1).
type CreateTorrentTaskRequest struct {
	// TorrentFile, when non-empty, is the raw .torrent file content; it is
	// uploaded to createtorrent as a multipart "file" field instead of a
	// magnet. Set by the adapter when the grab's source URI is an HTTP
	// .torrent link (URL-only indexers like RuTracker) — TorBox rejects raw
	// HTTP URLs passed as magnet with BOZO_TORRENT.
	TorrentFile     []byte
	TorrentFileName string
	Magnet          string
	PayloadPath     string
	Name            string
	Seed            int
	AllowZip        bool
	AsQueued        bool
	AddOnlyIfCached bool
	PolicyOp        provider.Op
}

// CreateUsenetTaskRequest mirrors TorBoxarr verbatim.
type CreateUsenetTaskRequest struct {
	Link            string
	PayloadPath     string
	Name            string
	Password        string
	PostProcessing  *int
	AsQueued        bool
	AddOnlyIfCached bool
	PolicyOp        provider.Op
}

// ---------------------------------------------------------------------------
//
// The shared result types relocated to internal/provider (dependency
// inversion: torbox imports provider, never the reverse). These aliases keep
// the diff scoped: packages outside this gate's authorization
// (internal/cachegate, internal/output, internal/api) continue to compile
// unchanged, and Go type identity makes torbox.X == provider.X, so the types
// flowing through core ARE the neutral provider types. The aliases are
// removed at 11.feature_implementation_audit once the remaining importers'
// gates authorize their spelling cleanup.
// ---------------------------------------------------------------------------

type CreateTaskResponse = provider.CreateTaskResponse

type RemoteFile = provider.RemoteFile

type CachedFile = provider.CachedFile

type CheckCachedResult = provider.CheckCachedResult

type TaskStatus = provider.TaskStatus

type RetryableError = provider.RetryableError

// MarkRetryable forwards to provider.MarkRetryable (relocated).
func MarkRetryable(err error) error { return provider.MarkRetryable(err) }

// IsRetryable forwards to provider.IsRetryable (relocated).
func IsRetryable(err error) bool { return provider.IsRetryable(err) }
