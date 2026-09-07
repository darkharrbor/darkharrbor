package httpstream

import (
	"context"
	"time"
)

// StreamQuery is the arr-originated search input (H2+ handlers implement it;
// the /torznab/http-stream route constructs it).
type StreamQuery struct {
	Kind    string // movie | episode | tv (season/ep may be absent for tv)
	Title   string
	Year    int
	Season  int
	Episode int
	IMDBID  string
	TVDBID  string
	TMDBID  string
	// Episodes is bounded authoritative Sonarr context for tolerant filename
	// matching. It is transient and never enters a resolve key or durable row.
	Episodes []EpisodeIdentity
}

// SearchResult is one candidate representation surfaced to the arr. Size 0
// means unknown (the feed layer substitutes the D17 deterministic estimate).
type SearchResult struct {
	Title    string
	Filename string // transient protocol filename; never persisted or used as a selector
	Key      ResolveKey
	Quality  string // handler quality label (mapped via QualityToRes for presentation)
	Size     int64
	Season   int
	Episode  int
	// Protocol is empty for the HTTP resolve-key lane and "torrent" for a
	// lane-owned info-hash result.
	Protocol string
	InfoHash string
}

// StreamKind classifies a discovered stream candidate into the lane that
// must acquire it (HR2.1, HR-D2). It is shared vocabulary, not private to
// any one handler: OMSS/generic sources may reuse it as they grow richer
// source metadata. Progressive HTTP, info-hash, and remote-archive kinds have
// wired consumers; the others remain distinctly recognized so dependent rows
// can dispatch to existing owners without a second classifier (WORKFLOW rule 3).
type StreamKind string

const (
	KindProgressiveHTTP StreamKind = "progressive-http"
	KindHLS             StreamKind = "hls"
	KindInfoHash        StreamKind = "info-hash"
	KindNZB             StreamKind = "nzb"
	KindArchive         StreamKind = "archive"
	KindUnsupported     StreamKind = "unsupported"
)

// ResolvedFile is the TRANSIENT play/grab-time representation of a source
// file. URL and RequestHeaders are in-memory only: json tags drop them so an
// accidental marshal can never persist them (HS-1.9), and no code path may
// log them.
type ResolvedFile struct {
	Selector        string            `json:"selector"`
	Name            string            `json:"name"`
	Size            int64             `json:"size"`
	ContentType     string            `json:"content_type,omitempty"`
	URL             string            `json:"-"`
	RequestHeaders  map[string]string `json:"-"`
	ResponseHeaders map[string]string `json:"-"`
	SupportsRange   bool              `json:"supports_range,omitempty"`
	ExpiresAt       time.Time         `json:"-"`
	RefreshContext  string            `json:"-"`
	// Digests carries whole-object source digests the handler already knew
	// (HR2.7: IA md5/sha1, Metalink <hash>). Transient only -- never part of
	// the persisted FileList -- consumed once at preflight time to feed the
	// shared content proof graph.
	Digests []SourceDigest `json:"-"`
}

// ResolveOperation tells a handler whether normal resolution is acceptable or
// whether it must invalidate and replace the supplied handler-owned refresh
// context. The context is transient and opaque to the API layer.
type ResolveOperation string

const (
	ResolveNormal       ResolveOperation = "normal"
	ResolveForceRefresh ResolveOperation = "force_refresh"
)

// ResolveRequest is the transient protocol-neutral resolve contract (HS-3.1).
// Only Key is persisted. Operation and RefreshContext exist solely for the
// current resolve/play attempt and must never be logged or stored.
type ResolveRequest struct {
	Key            ResolveKey
	Operation      ResolveOperation
	RefreshContext string
}

// RemoteArchiveFormat is an operator-declared archive format. Callers must
// never infer it from a URL suffix.
type RemoteArchiveFormat string

const (
	RemoteArchiveZIP      RemoteArchiveFormat = "zip"
	RemoteArchiveRAR      RemoteArchiveFormat = "rar"
	RemoteArchiveSevenZip RemoteArchiveFormat = "7z"
	RemoteArchiveTGZ      RemoteArchiveFormat = "tgz"
	RemoteArchiveTAR      RemoteArchiveFormat = "tar"
)

// RemoteArchiveDescriptor is a transient, URL-free identity plus resolved
// source parts. Parts retain ResolvedFile's transient URL/header contract.
type RemoteArchiveDescriptor struct {
	Selector   string
	Format     RemoteArchiveFormat
	MemberHint string
	Parts      []ResolvedFile `json:"-"`
}

// RemoteArchiveMember is the URL-free result of safe archive inspection.
// HR5.1 owns consuming these spans for playback.
type RemoteArchiveMember struct {
	Selector          string
	Name              string
	Size              int64
	Format            RemoteArchiveFormat
	Part              int
	DataOffset        int64
	CompressedSize    int64
	CompressionMethod uint16
}

// RemoteArchiveHandler is the optional descriptor arm implemented by
// handlers that expose remote archives. HR5.1 consumes it explicitly.
type RemoteArchiveHandler interface {
	ResolveRemoteArchives(context.Context, ResolveRequest) ([]RemoteArchiveDescriptor, error)
}

// Handler is a protocol handler (ia | omss | stremio | generic). Handlers
// are registered per configured backend instance in H2/H3; H1 ships the
// registry empty.
type Handler interface {
	// Name returns the handler protocol name (matches ResolveKey.Handler).
	Name() string
	// Search returns candidate representations for an arr query.
	Search(ctx context.Context, q StreamQuery) ([]SearchResult, error)
	// Resolve re-resolves the key to live transient files. It must return
	// every currently-available representation file so the caller can match
	// the persisted Selector exactly (D10).
	Resolve(ctx context.Context, req ResolveRequest) ([]ResolvedFile, error)
}

// MatchSelector finds the resolved file matching the persisted selector.
// Exactly-one semantics: zero matches is a lost representation, and more
// than one is ambiguous — both are rejections, never a silent pick (D10).
func MatchSelector(files []ResolvedFile, selector string) (ResolvedFile, error) {
	var found []ResolvedFile
	for _, f := range files {
		if f.Selector == selector {
			found = append(found, f)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return ResolvedFile{}, NewError(ClassRepresentationLost, "grabbed representation no longer present")
	default:
		return ResolvedFile{}, NewError(ClassRepresentationLost, "grabbed representation is ambiguous")
	}
}
