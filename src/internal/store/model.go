package store

import (
	"time"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
)

// SourceType identifies whether a submission came in as a torrent or NZB.
type SourceType string

const (
	SourceTypeTorrent SourceType = "torrent"
	SourceTypeNZB     SourceType = "nzb"
	// SourceTypeHTTP marks items grabbed through the HTTP stream provider
	// (/torznab/http-stream → /grab/http → qBit add). HTTP items never touch
	// cachegate, debrid submit, provider polling, remote IDs, or the
	// governor; they resolve in one pass and re-resolve at play time from
	// the persisted resolve_key (HTTP-STREAM-MASTER-PLAN.md D1/D10/D13).
	SourceTypeHTTP SourceType = "http"
)

// ClientKind identifies which ingest adapter accepted the submission.
type ClientKind string

const (
	ClientKindQBit ClientKind = "qbit"
	ClientKindSAB  ClientKind = "sab"
)

// Dark Harrbor does not download files; it resolves them to .strm + sidecar.
type ItemState string

const (
	// StateAccepted: ingest adapter accepted the submission, resolution not yet started.
	StateAccepted ItemState = "accepted"
	// StateResolving: checkcached / createtorrent|createusenetdownload in flight,
	// or item is on TorBox's as_queued list awaiting cache population.
	StateResolving ItemState = "resolving"
	// StateReady: release is cached on TorBox, .strm + sidecar written, importable by arr.
	StateReady ItemState = "ready"
	// StateFailed: resolution failed — uncached and rejected by cachegate, TorBox
	// API error, or probe failure. Terminal.
	StateFailed ItemState = "failed"
	// StateRemoved: item removed by the post-play janitor or manual request. Terminal.
	StateRemoved ItemState = "removed"
)

// Closed reports whether this state is terminal (no further transitions expected).
func (s ItemState) Closed() bool {
	return s == StateFailed || s == StateRemoved
}

// SubmissionMetadata carries arr-side context supplied at submission time.
// Kept structurally compatible with TorBoxarr so the qBit/SAB emulators
// can populate it without changes.
type SubmissionMetadata struct {
	SavePath         string   `json:"save_path,omitempty"`
	Rename           string   `json:"rename,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	SkipChecking     bool     `json:"skip_checking,omitempty"`
	Paused           bool     `json:"paused,omitempty"`
	RootFolder       bool     `json:"root_folder,omitempty"`
	Password         string   `json:"password,omitempty"`
	PostProcessing   int      `json:"post_processing,omitempty"`
	AsQueued         bool     `json:"as_queued,omitempty"`
	AddOnlyIfCached  bool     `json:"add_only_if_cached,omitempty"`
	UploadedFilename string   `json:"uploaded_filename,omitempty"`
	OriginalFilename string   `json:"original_filename,omitempty"`

	// ProviderIdentity is the authoritative media identity captured from the
	// grabbing Arr. It is intentionally compact and URL-free so it can travel
	// through the existing metadata_json persistence boundary (ID-02).
	ProviderIdentity *ProviderIdentity `json:"provider_identity,omitempty"`

	// NNTPZeroFillCount is the cumulative number of item-associated NNTP
	// acquisition walks that reached the final rung with no usable bytes.
	// The historical plan calls these "zero-fill events", but DarkHarrbor
	// never writes literal zero bytes: the stream returns a classified
	// ladder-exhausted error instead. Persisting the count in metadata keeps
	// it restart-safe without adding a dedicated schema column (NS-1.3).
	NNTPZeroFillCount int64 `json:"nntp_zero_fill_count,omitempty"`

	// NNTPSeekIndex is the bounded container index produced by the shared
	// mediatruth owner after NNTP resolve. SourceKey binds it to the exact
	// raw/archive representation analyzed, so another file on the same item
	// cannot consume stale offsets.
	NNTPSeekIndex     *mediatruth.Index `json:"nntp_seek_index,omitempty"`
	NNTPSeekSourceKey string            `json:"nntp_seek_source_key,omitempty"`

	// NNTPPAR2ProofIndex is NS-4.1's compact, URL-free native IFSC material.
	// Exact ParseNZB file indexes bind the hashes to the same stable ordering
	// used by playback; candidate bytes remain outside persistence.
	NNTPPAR2ProofIndex *archiveparser.PAR2ProofIndex `json:"nntp_par2_proof_index,omitempty"`

	// RealInfoHash is the true infohash of the underlying torrent when this
	// item was grabbed via a synthetic per-season release (item.InfoHash then
	// holds the synthetic, client-facing hash Sonarr tracks). The provider
	// submit path MUST use this for any hash-derived fallback (magnet
	// construction, createtorrent dedup key) — submitting the synthetic hash
	// creates a fantasy transfer that sits at 0% forever (live 2026-07-08:
	// four AITF season grabs landed on TorBox as synth-hash transfers stuck
	// in "checking").
	RealInfoHash string `json:"real_infohash,omitempty"`

	// SABHistoryHidden is set when the arr deletes this item from the SAB
	// emulator's history. The item's lifecycle is untouched (StateReady keeps
	// the stream proxy alive), but it must never reappear in SAB history
	// output — Sonarr's DownloadEventHub re-issues the delete on every poll
	// (~90s) for as long as the entry stays visible (live 2026-07-08: a
	// StateReady S01 season NZB re-deleted every poll all day; the earlier
	// 15-min removed-tail fix could not converge this because the item never
	// leaves StateReady).
	SABHistoryHidden bool `json:"sab_history_hidden,omitempty"`

	// DownloadProgress is the last known TorBox download progress (0–1) for
	// an uncached torrent still being seeded. Updated on every resolver poll
	// so the qBit emulator can report real progress to Sonarr instead of 0%.
	// (F-7)
	DownloadProgress float64 `json:"download_progress,omitempty"`

	// DownloadETASeconds is the last known provider ETA (seconds) for an
	// uncached grab still materializing. 0 = unknown/none; the TorBox
	// never-sentinel (8640000) is filtered at the poll site. Feeds the qBit
	// `eta` field and the SAB `timeleft` column so the arrs render a live
	// countdown instead of a frozen row. (F-9)
	DownloadETASeconds int64 `json:"download_eta_seconds,omitempty"`

	// TorrentStalledAt is when the provider first continuously reported this
	// uncached torrent as stalled. A later non-stalled observation clears it.
	// Persisting it in the existing metadata JSON makes the configured stall
	// duration survive daemon restarts without a schema migration.
	TorrentStalledAt *time.Time `json:"torrent_stalled_at,omitempty"`

	// MediaFacts is bounded, secret-free media truth (HR5.2) for this item's
	// primary streamed file, persisted so an Arr ffprobe import (SF-04) can
	// be answered instantly from already-computed facts instead of a live
	// network probe. Nil until a grab-time prewarm successfully computes
	// non-empty facts; never fabricated, never a substitute for a real
	// probe when absent.
	MediaFacts *mediatruth.Facts `json:"media_facts,omitempty"`
	// MediaFactsFileID binds MediaFacts to the exact streamed file they
	// describe. A multi-file release must never serve one file's facts
	// under another file's probe request -- the same mistake class TS-1.4's
	// cachedFiles[0] bug made once (see its own WORKLOG entry).
	MediaFactsFileID string `json:"media_facts_file_id,omitempty"`
	// MediaFactsSourceKey binds MediaFacts to the exact ByteSource
	// representation analyzed, mirroring NNTPSeekSourceKey above.
	MediaFactsSourceKey string `json:"media_facts_source_key,omitempty"`

	// TorrentRARManifest maps a store-mode RAR member onto provider file IDs
	// and bounded payload spans. It is URL-free and survives daemon restarts;
	// CDN URLs are always resolved transiently at play time (TS-1.3).
	TorrentRARManifest *archiveparser.StoredRARManifest `json:"torrent_rar_manifest,omitempty"`

	// SevenZipManifest is the URL-free JSON manifest for a Copy-coded NNTP 7z
	// archive. Keeping it in the established metadata owner makes it durable
	// without coupling store to nntp's manifest types.
	SevenZipManifest *string `json:"seven_zip_manifest,omitempty"`

	// ErrorJournal is OBS-01's bounded, sanitized, item-associated ring of
	// recent SF-01 outcome events (class/rung/provider/time), last-N only
	// (config-bounded, oldest dropped first). It never carries an error
	// message, URL, or any field outside the outcome vocabulary/rung label/
	// provider name/timestamp -- those are already secret-safe by SF-01's
	// own contract. Written via store.AppendErrorJournal's targeted
	// metadata_json.$.error_journal json_set, mirroring NNTPZeroFillCount's
	// existing single-field-update convention above.
	ErrorJournal []ErrorJournalEntry `json:"error_journal,omitempty"`
}

// ErrorJournalEntry is one bounded, sanitized SF-01 outcome event (OBS-01).
// Class is the outcome.Class string value; Rung is the lane-owned rung/
// operation label (e.g. an ladder.Rung.Name or NNTP FinalRungEvent.Operation)
// -- never a raw error message, URL, or message ID. Provider is the
// lane-owned provider/backend name (may be empty when a lane has no
// single-provider concept for the event, e.g. a lane-agnostic shared cache).
type ErrorJournalEntry struct {
	Class    string    `json:"class"`
	Rung     string    `json:"rung"`
	Provider string    `json:"provider,omitempty"`
	At       time.Time `json:"at"`
}

// ProviderIDs are external media-catalog identifiers. Values are stored as
// strings because IMDb IDs are alphanumeric and the other providers' numeric
// ranges are owned by those providers, not DarkHarrbor.
type ProviderIDs = mediaidentity.ProviderIDs
type EpisodeProviderIDs = mediaidentity.EpisodeProviderIDs
type ProviderIdentity = mediaidentity.ProviderIdentity

// Item is the core record for a media submission in Dark Harrbor.
// It replaces TorBoxarr's Job struct, dropping the download-lifecycle fields
type Item struct {
	ID            string
	PublicID      string
	SourceType    SourceType
	ClientKind    ClientKind
	Category      string
	State         ItemState
	SubmissionKey string

	// RemoteID is the TorBox torrent or usenet item ID, set after createtorrent/
	// createusenetdownload succeeds.
	RemoteID *string
	// QueuedID is the as_queued ID returned when TorBox queues an uncached item.
	QueuedID *string
	// InfoHash is the torrent infohash (nil for NZB).
	InfoHash *string

	DisplayName string
	SourceURI   *string // magnet URI or NZB URL

	// Cached is true when TorBox confirms the release is in its global cache.
	Cached bool
	// StrmPath is the absolute path to the written .strm file on disk.
	StrmPath *string
	// SidecarPath is the absolute path to the written stub .mkv on disk (v1).
	SidecarPath *string
	ProbeJSON   *string
	// Used to restore per-item cleanup timers after a container restart.
	// NULL means the item has never been played.
	LastPlayedAt *time.Time
	// FileList is the resolved list of streamable file entries from TorBox,
	// stored as JSON (array of {name, size, requestdl_url}).
	FileList *string
	// TotalSize is the sum of all file sizes in bytes, populated at resolve time
	// from TorBox file metadata. Used for accurate qBit API size reporting.
	TotalSize int64

	ErrorMessage *string
	RetryCount   int
	NextRunAt    *time.Time

	Metadata SubmissionMetadata

	ClaimedBy *string
	ClaimedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time

	// Provider is the registry name of the provider that resolved this item
	// NULL means the default provider; the single-provider path is unaffected.
	Provider *string

	// RarManifest is a JSON-encoded []nntp.RARPart slice built at resolve time
	// all torrent items. When non-nil, handleStreamProxy routes to
	// NNTPProvider.StreamRAR() instead of the normal Stream() path.
	RarManifest *string

	// ResolveKey is the canonical, URL-free resolve key JSON for
	// source_type='http' items (httpstream.ResolveKey). It is the permanent
	// re-resolution identity behind the item's library .strm files; real
	// source URLs are fetched live at play time and never persisted (D10,
	// HS-1.9). Nil for torrent/NZB items.
	ResolveKey *string

	// DownloadConsumed is true when the Arr has imported (moved) the
	// download-side .strm from DH's download directory. Once consumed,
	// the blob reconciler skips re-materialization of the download path,
	// preventing the COR-3 infinite-recreate loop. The permanent library
	// .strm (placed by the Arr) is unaffected — playback resolves by item
	// ID, not filesystem presence. Set by positive signals: SAB history
	// delete or qBit delete.
	DownloadConsumed bool

	// ZIPManifest is a JSON-encoded []nntp.ZIPEntry slice built at resolve time
	// by BuildZIPManifest. Stores the ZIP central directory so StreamZIPEntry
	// can seek directly to any file's data offset at play time without re-parsing.
	ZIPManifest *string
}

// IsRARSplit returns true when this item has a pre-built RAR manifest stored,
// indicating it is a RAR-split NZB that should be served via StreamRAR.
func (item *Item) IsRARSplit() bool {
	return item.RarManifest != nil && *item.RarManifest != ""
}

// IsZIPArchive returns true when this item has a pre-built ZIP manifest stored,
// indicating it should be served via StreamZIPEntry at play time.
func (item *Item) IsZIPArchive() bool {
	return item.ZIPManifest != nil && *item.ZIPManifest != ""
}

// IsSevenZipArchive reports whether resolver-time 7z preparation completed.
func (item *Item) IsSevenZipArchive() bool {
	return item.Metadata.SevenZipManifest != nil && *item.Metadata.SevenZipManifest != ""
}

// ItemEvent records a state transition or notable event for an item.
type ItemEvent struct {
	ID        int64
	ItemID    string
	FromState *ItemState
	ToState   *ItemState
	Message   string
	CreatedAt time.Time
}

// When a hash evicts from TorBox's global cache repeatedly, Dark Harrbor stops
// notifying Sonarr to re-search until the blacklist window expires.
//
// Threshold: 3 failures → blacklisted for 7 days.
// After expiry the hash is retried; a fresh failure resets the 7-day window.
type FailedHash struct {
	Hash             string     // torrent infohash (lowercase hex)
	FailCount        int        // total eviction events recorded
	LastFailedAt     time.Time  // most recent failure timestamp
	BlacklistedUntil *time.Time // nil = not currently blacklisted
}
