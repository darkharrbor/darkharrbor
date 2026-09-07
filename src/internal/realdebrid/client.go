package realdebrid

import "context"

// Client is the interface the adapter uses to talk to Real-Debrid.
// The concrete HTTPClient implements it; tests inject a fake.
type Client interface {
	// GetUser fetches account info for plan/capability discovery.
	GetUser(ctx context.Context) (*AccountInfo, error)

	// InstantAvailability was RD's pre-submit cache oracle.
	// DISABLED by RD (error_code 37, "disabled_endpoint") as of late 2023.
	// Kept in the interface for potential re-enablement; always returns empty.
	InstantAvailability(ctx context.Context, hashes []string) (map[string][]CachedGroup, error)

	// AddMagnet submits a magnet URI to RD. Returns the RD item ID.
	AddMagnet(ctx context.Context, magnet string) (string, error)

	// SelectFiles tells RD which files to download for a submitted item.
	// Pass "all" to select everything.
	SelectFiles(ctx context.Context, rdID string, files string) error

	// GetTorrentInfo returns the current status and file list for a submitted item.
	GetTorrentInfo(ctx context.Context, rdID string) (*TorrentInfo, error)

	// UnrestrictLink converts an RD internal link to a direct CDN URL.
	UnrestrictLink(ctx context.Context, link string) (string, error)

	// DeleteTorrent removes a submitted item from the user's RD account.
	DeleteTorrent(ctx context.Context, rdID string) error

	// ActiveCount returns the number of currently active (non-completed) torrents.
	ActiveCount(ctx context.Context) (int, error)

	// FindByHash looks up a user's existing RD torrent by infohash.
	// Returns (id, found, err). Used to skip addMagnet when content is
	// already in the user's account as "downloaded" (pre-filter cached content).
	FindByHash(ctx context.Context, hash string) (string, bool, error)

	// ListTorrents returns the user's full torrent list (up to limit).
	ListTorrents(ctx context.Context, limit int) ([]TorrentInfo, error)
}

// CachedGroup represents one set of files for a cached hash on RD.
// RD returns multiple groups when the same hash has been cached in
// different file-selection states; DH picks the first non-empty group.
type CachedGroup struct {
	Files map[string]CachedFile // key = RD file ID string
}

// CachedFile is one file entry from an instantAvailability response.
type CachedFile struct {
	Filename string
	Filesize int64
}

// TorrentInfo is the parsed result of GET /torrents/info/{id}.
type TorrentInfo struct {
	ID       string
	Filename string
	Hash     string
	Status   string // "downloading", "downloaded", "error", "dead", etc.
	Progress float64
	Files    []TorrentFile
	Links    []string // RD internal links; must be unrestricted before use
}

// TorrentFile is one file entry from a torrent's file list.
type TorrentFile struct {
	ID       int
	Path     string
	Bytes    int64
	Selected bool
}
