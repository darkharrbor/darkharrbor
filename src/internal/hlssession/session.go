// Package hlssession owns DarkHarrbor's shared playback-session graph
// (HR4.1, HR-D6). It is the single subsystem that assigns stable, opaque DH
// resource IDs to the pieces of a manifest-based playback attempt — master
// manifest, child/media playlists, segments, initialization maps, keys, and
// subtitles — so a player only ever sees DH-owned identities, never an
// upstream URL, query token, or header (LC-02, DG-04).
//
// A Session is one bounded, transient playback attempt against one resolved
// (item, file) representation. A Resource is one opaque ID registered under
// that session, carrying only a Reference: a secret-free logical descriptor
// (owning handler, backend, selector, ordinal, optional byte range) that lets
// the owning lane handler re-resolve the real upstream location on demand.
// Reference is never a URL and is never presented to a client or logged
// verbatim by any caller.
//
// Both Session and Resource rows are durably persisted (not merely held in
// memory), which is what makes a session "restart-resumable without full
// re-resolve" (master plan §4.4, HR4.1): after a DarkHarrbor restart, a
// resource ID a player already holds (e.g. embedded in a previously served
// child playlist) still resolves to its Reference directly from the
// database — DarkHarrbor does not need to re-fetch and re-parse the master
// manifest from scratch and mint an entirely new ID tree before it can
// answer that request.
//
// This package has no consumer yet: HR4.2 (playlist rewrite) is the first
// row that will actually populate Resource rows from a real HLS manifest.
// HR4.1's own scope is the graph, ID minting, validation, expiry, and bounded
// persistence — the same "foundation first, adapter later" shape HR1.2 and
// HR1.5 already established for the Content Proof Graph and Representation
// Continuity Ledger.
package hlssession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ResourceKind classifies what an opaque resource ID stands for within a
// session (HR-D6): the master manifest itself, a child/media playlist
// (rendition), a media segment, an EXT-X-MAP initialization segment, an AES
// key, or a subtitle resource. Shared vocabulary for HR4.2-HR4.8 — a second
// resource-kind enum must never be invented downstream (WORKFLOW rule 3).
type ResourceKind string

const (
	KindMaster   ResourceKind = "master"
	KindMedia    ResourceKind = "media"
	KindSegment  ResourceKind = "segment"
	KindMap      ResourceKind = "map"
	KindKey      ResourceKind = "key"
	KindSubtitle ResourceKind = "subtitle"
)

func validKind(k ResourceKind) bool {
	switch k {
	case KindMaster, KindMedia, KindSegment, KindMap, KindKey, KindSubtitle:
		return true
	}
	return false
}

// noByteRange is the sentinel pair meaning "this resource is not a
// byte-range slice of its parent" (e.g. a whole child playlist or an
// unsliced key). Held distinct from the valid 0,0 single-byte range.
const noByteRange = -1

// Reference is the secret-free logical descriptor of what a resource points
// to. It contains no URL, query string, header, cookie, or credential —
// only coordinates a lane handler (HR2.1's httpstream.Handler family, or a
// future HLS-specific handler method) can use to re-resolve the live
// upstream location at request time. DG-04 is enforced by construction:
// every string field is validated to reject "://" and is length-bounded.
type Reference struct {
	// Handler is the owning protocol handler name (ia | omss | stremio |
	// generic, or a future HLS-specific handler) — never a URL.
	Handler string `json:"handler"`
	// BackendID is the handler's configured backend instance identity
	// (D5), never a URL.
	BackendID string `json:"backend_id,omitempty"`
	// Selector is the handler-stable representation selector this
	// resource's parent manifest was resolved from (D10 precedent) —
	// opaque, never a URL.
	Selector string `json:"selector,omitempty"`
	// Ordinal is this resource's position within its parent (variant index
	// within a master manifest, or segment index within a media playlist).
	Ordinal int `json:"ordinal,omitempty"`
	// ByteStart/ByteEnd are an inclusive byte range into the parent
	// resource's own upstream representation (EXT-X-BYTERANGE segments,
	// keys, or maps). Both noByteRange means "whole resource, no slice."
	ByteStart int64 `json:"byte_start,omitempty"`
	ByteEnd   int64 `json:"byte_end,omitempty"`
	// MediaSequence is the absolute segment coordinate used by sliding live
	// HLS windows. The presence bit preserves the established ordinal
	// semantics for VOD and older persisted references.
	MediaSequence    int64 `json:"media_sequence,omitempty"`
	HasMediaSequence bool  `json:"has_media_sequence,omitempty"`
	// PartIndex disambiguates LL-HLS Partial Segments that share one Media
	// Sequence Number. The presence bit keeps older persisted references
	// and ordinary full segments backward-compatible.
	PartIndex    int  `json:"part_index,omitempty"`
	HasPartIndex bool `json:"has_part_index,omitempty"`
	// RenditionIndex is the stable position among rendition reports in one
	// media playlist, independent of moving segment/part entries.
	RenditionIndex    int  `json:"rendition_index,omitempty"`
	HasRenditionIndex bool `json:"has_rendition_index,omitempty"`
	// MediaStartNS/MediaEndNS are an exclusive native VOD presentation-time
	// span. Live resources and non-media leaves omit it.
	MediaStartNS     int64 `json:"media_start_ns,omitempty"`
	MediaEndNS       int64 `json:"media_end_ns,omitempty"`
	HasMediaTime     bool  `json:"has_media_time,omitempty"`
	MediaDurationNS  int64 `json:"media_duration_ns,omitempty"`
	HasMediaDuration bool  `json:"has_media_duration,omitempty"`
	// PathwayID is the origin-declared HLS Content Steering pathway inherited
	// by a variant's descendants. It is an opaque protocol ID, never a URL.
	PathwayID string `json:"pathway_id,omitempty"`
}

func newWholeReference() (int64, int64) { return noByteRange, noByteRange }

// Session is one bounded, transient playback attempt against one resolved
// (item, file) representation.
type Session struct {
	SessionID string
	ItemID    string
	FileID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Resource is one opaque, stable DH resource ID registered under a session.
type Resource struct {
	ResourceID string
	SessionID  string
	Kind       ResourceKind
	Reference  Reference
	CreatedAt  time.Time
}

// Repository is the durable persistence contract. The concrete *store.Store
// satisfies it, exactly like contentproof.Repository/ContinuityRepository.
type Repository interface {
	// InsertSession persists a new session row.
	InsertSession(ctx context.Context, s Session) error
	// GetSession returns the session if present and not expired as of now,
	// nil otherwise.
	GetSession(ctx context.Context, sessionID string, now time.Time) (*Session, error)
	// TouchSession extends an existing session's expiry (sliding TTL on
	// active use). Returns false if the session does not exist.
	TouchSession(ctx context.Context, sessionID string, expiresAt time.Time) (bool, error)
	// InsertResource persists a new resource row bound to sessionID,
	// atomically enforcing maxPerSession: an attempt at or over the cap for
	// that exact session returns ErrSessionResourceCapacity. It also fails
	// (ErrSessionNotFound) when the session is absent or already expired as
	// of now.
	InsertResource(ctx context.Context, r Resource, maxPerSession int, now time.Time) error
	// GetResource returns the resource and its owning session if both exist
	// and the session is not expired as of now, nil/nil otherwise.
	GetResource(ctx context.Context, resourceID string, now time.Time) (*Resource, *Session, error)
	// ListResources returns every resource for sessionID, oldest first, when
	// the session is not expired as of now.
	ListResources(ctx context.Context, sessionID string, now time.Time) ([]Resource, error)
	// DeleteResources removes the named transient resources only when they
	// belong to sessionID.
	DeleteResources(ctx context.Context, sessionID string, resourceIDs []string) (int, error)
	// PruneExpired deletes every session (and, by cascade, its resources)
	// whose ExpiresAt is at or before now. Returns the number of sessions
	// removed.
	PruneExpired(ctx context.Context, now time.Time) (int, error)
}

// Sentinel errors. Kept distinct from opaque wrapped store errors so callers
// (and tests) can branch on them with errors.Is.
var (
	ErrSessionNotFound         = errors.New("hlssession: session not found or expired")
	ErrResourceNotFound        = errors.New("hlssession: resource not found or expired")
	ErrSessionResourceCapacity = errors.New("hlssession: session resource capacity exceeded")
)

const (
	// DefaultSessionTTL bounds how long a playback session graph is
	// resolvable without an active request touching it. Every successful
	// Resolve slides this forward (sliding-window keep-alive), so an
	// actively playing session does not expire mid-stream. No config knob
	// yet — mirrors HR1.2/HR1.5's own no-new-config precedent for a
	// foundation row with no live consumer; a future consuming row (HR4.2+)
	// can expose one once real playback duration data exists.
	DefaultSessionTTL = 6 * time.Hour
	// DefaultMaxResourcesPerSession bounds worst-case persisted state per
	// session (DG-07: bounded state). A feature-length movie at typical HLS
	// segment durations across a handful of renditions plus subtitles stays
	// far under this; it exists to cap a misbehaving or hostile manifest,
	// not to constrain ordinary playback.
	DefaultMaxResourcesPerSession = 8192
)

// Options configures a Graph. Zero values fall back to the package defaults.
type Options struct {
	Now                    func() time.Time
	SessionTTL             time.Duration
	MaxResourcesPerSession int
}

// Graph is the shared playback-session graph owner.
type Graph struct {
	repo Repository
	now  func() time.Time
	ttl  time.Duration
	max  int
}

// New constructs a Graph over repo.
func New(repo Repository, opts Options) (*Graph, error) {
	if repo == nil {
		return nil, errors.New("hlssession: nil repository")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = DefaultSessionTTL
	}
	if opts.MaxResourcesPerSession <= 0 {
		opts.MaxResourcesPerSession = DefaultMaxResourcesPerSession
	}
	return &Graph{repo: repo, now: opts.Now, ttl: opts.SessionTTL, max: opts.MaxResourcesPerSession}, nil
}

// CreateSession mints a new session for (itemID, fileID) — DH's own opaque
// item/file identities (httpstream.PersistedFile.FileID and store.Item.ID),
// never upstream-derived. Opportunistically prunes expired sessions first so
// routine use bounds total persisted state without a background sweep
// goroutine (DG-07): every create call amortizes cleanup instead of relying
// on an unscheduled consumer to ever call PruneExpired directly.
func (g *Graph) CreateSession(ctx context.Context, itemID, fileID string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if err := validateOpaque("item id", itemID); err != nil {
		return Session{}, err
	}
	if err := validateOpaque("file id", fileID); err != nil {
		return Session{}, err
	}
	now := g.now().UTC()
	if _, err := g.repo.PruneExpired(ctx, now); err != nil {
		return Session{}, fmt.Errorf("hlssession: prune before create: %w", err)
	}
	id, err := newSessionID()
	if err != nil {
		return Session{}, err
	}
	s := Session{
		SessionID: id,
		ItemID:    itemID,
		FileID:    fileID,
		CreatedAt: now,
		ExpiresAt: now.Add(g.ttl),
	}
	if err := g.repo.InsertSession(ctx, s); err != nil {
		return Session{}, fmt.Errorf("hlssession: insert session: %w", err)
	}
	return s, nil
}

// Session returns the live session, or ErrSessionNotFound if absent/expired.
func (g *Graph) Session(ctx context.Context, sessionID string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if err := validSessionID(sessionID); err != nil {
		return Session{}, err
	}
	s, err := g.repo.GetSession(ctx, sessionID, g.now().UTC())
	if err != nil {
		return Session{}, fmt.Errorf("hlssession: get session: %w", err)
	}
	if s == nil {
		return Session{}, ErrSessionNotFound
	}
	return *s, nil
}

// RegisterResource mints a fresh opaque resource ID under sessionID for kind,
// pointing at ref. Fails with ErrSessionNotFound if the session is
// absent/expired, and ErrSessionResourceCapacity if the session is already
// at its bound.
func (g *Graph) RegisterResource(ctx context.Context, sessionID string, kind ResourceKind, ref Reference) (Resource, error) {
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}
	if err := validSessionID(sessionID); err != nil {
		return Resource{}, err
	}
	if !validKind(kind) {
		return Resource{}, fmt.Errorf("hlssession: invalid resource kind %q", kind)
	}
	if err := validateReference(ref); err != nil {
		return Resource{}, err
	}
	id, err := newResourceID()
	if err != nil {
		return Resource{}, err
	}
	r := Resource{
		ResourceID: id,
		SessionID:  sessionID,
		Kind:       kind,
		Reference:  ref,
		CreatedAt:  g.now().UTC(),
	}
	if err := g.repo.InsertResource(ctx, r, g.max, g.now().UTC()); err != nil {
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrSessionResourceCapacity) {
			return Resource{}, err
		}
		return Resource{}, fmt.Errorf("hlssession: insert resource: %w", err)
	}
	return r, nil
}

// Resolve looks up resourceID and, on success, slides the owning session's
// expiry forward by the configured TTL (an active playback attempt is never
// allowed to expire mid-stream purely from wall-clock age). Returns
// ErrResourceNotFound if the resource or its owning session is absent or
// expired.
func (g *Graph) Resolve(ctx context.Context, resourceID string) (Resource, error) {
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}
	if err := validResourceID(resourceID); err != nil {
		return Resource{}, err
	}
	now := g.now().UTC()
	r, s, err := g.repo.GetResource(ctx, resourceID, now)
	if err != nil {
		return Resource{}, fmt.Errorf("hlssession: get resource: %w", err)
	}
	if r == nil || s == nil {
		return Resource{}, ErrResourceNotFound
	}
	if _, terr := g.repo.TouchSession(ctx, s.SessionID, now.Add(g.ttl)); terr != nil {
		// Non-fatal: the resource was resolved correctly; a keep-alive
		// extension failing only shortens the session's remaining life, it
		// never invalidates the answer just returned.
		return *r, nil
	}
	return *r, nil
}

// ListResources returns every resource registered under sessionID, oldest
// first, or ErrSessionNotFound if the session is absent/expired.
func (g *Graph) ListResources(ctx context.Context, sessionID string) ([]Resource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validSessionID(sessionID); err != nil {
		return nil, err
	}
	now := g.now().UTC()
	if _, err := g.repo.GetSession(ctx, sessionID, now); err != nil {
		return nil, fmt.Errorf("hlssession: get session: %w", err)
	}
	resources, err := g.repo.ListResources(ctx, sessionID, now)
	if err != nil {
		return nil, fmt.Errorf("hlssession: list resources: %w", err)
	}
	return resources, nil
}

// DeleteResources removes obsolete transient resources from a live window.
func (g *Graph) DeleteResources(ctx context.Context, sessionID string, resourceIDs []string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := validSessionID(sessionID); err != nil {
		return 0, err
	}
	for _, id := range resourceIDs {
		if err := validResourceID(id); err != nil {
			return 0, err
		}
	}
	n, err := g.repo.DeleteResources(ctx, sessionID, resourceIDs)
	if err != nil {
		return 0, fmt.Errorf("hlssession: delete resources: %w", err)
	}
	return n, nil
}

// PruneExpired explicitly sweeps every expired session (and its resources).
// CreateSession already calls this opportunistically; a future scheduling
// row (mirroring the existing Lifecycle prune-loop pattern in
// cmd/darkharrbor/main.go) may also call this directly once real HLS
// sessions exist. Exposed so this bounded-state contract is independently
// testable and independently schedulable, per this row's own honest
// no-consumer-yet limitation.
func (g *Graph) PruneExpired(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n, err := g.repo.PruneExpired(ctx, g.now().UTC())
	if err != nil {
		return 0, fmt.Errorf("hlssession: prune expired: %w", err)
	}
	return n, nil
}

// ── ID minting and validation ───────────────────────────────────────────

var (
	sessionIDRe  = regexp.MustCompile(`^hs[0-9a-f]{12}$`)
	resourceIDRe = regexp.MustCompile(`^hz[0-9a-f]{12}$`)
)

// newSessionID returns a fresh path-safe opaque session ID ("hs" + 12 hex),
// matching the shape of httpstream.NewFileID's "hf" IDs.
func newSessionID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("hlssession: generate session id: %w", err)
	}
	return "hs" + hex.EncodeToString(b[:]), nil
}

// newResourceID returns a fresh path-safe opaque resource ID ("hz" + 12 hex).
func newResourceID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("hlssession: generate resource id: %w", err)
	}
	return "hz" + hex.EncodeToString(b[:]), nil
}

// ValidSessionID reports whether s is a DH-generated session ID.
func ValidSessionID(s string) bool { return sessionIDRe.MatchString(s) }

// ValidResourceID reports whether s is a DH-generated resource ID.
func ValidResourceID(s string) bool { return resourceIDRe.MatchString(s) }

func validSessionID(s string) error {
	if !ValidSessionID(s) {
		return errors.New("hlssession: malformed session id")
	}
	return nil
}

func validResourceID(s string) error {
	if !ValidResourceID(s) {
		return errors.New("hlssession: malformed resource id")
	}
	return nil
}

// opaqueFieldPattern bounds every free-text Reference field: printable ASCII
// (letters, digits, and a small punctuation set), 1-256 characters. It is
// deliberately conservative rather than trying to enumerate every handler
// name a future lane might add.
var (
	opaqueFieldPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
	pathwayIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func validateOpaque(field, value string) error {
	if !opaqueFieldPattern.MatchString(value) {
		return fmt.Errorf("hlssession: %s must be an opaque secret-free token", field)
	}
	if strings.Contains(value, "://") {
		return fmt.Errorf("hlssession: %s must not look like a URL", field)
	}
	return nil
}

// validateReference enforces DG-04 by construction: Handler is required and
// opaque; BackendID/Selector are optional but, if present, must also be
// opaque and URL-shape-free; the byte range is either the whole-resource
// sentinel or a valid non-negative inclusive span.
func validateReference(ref Reference) error {
	if err := validateOpaque("handler", ref.Handler); err != nil {
		return err
	}
	if ref.BackendID != "" {
		if err := validateOpaque("backend id", ref.BackendID); err != nil {
			return err
		}
	}
	if ref.Selector != "" {
		if strings.Contains(ref.Selector, "://") {
			return errors.New("hlssession: selector must not look like a URL")
		}
		if len(ref.Selector) > 512 {
			return errors.New("hlssession: selector too long")
		}
	}
	if ref.PathwayID != "" {
		if !pathwayIDPattern.MatchString(ref.PathwayID) {
			return errors.New("hlssession: invalid pathway id")
		}
	}
	if ref.Ordinal < 0 {
		return errors.New("hlssession: negative ordinal")
	}
	if ref.HasMediaSequence && ref.MediaSequence < 0 {
		return errors.New("hlssession: negative media sequence")
	}
	if ref.HasPartIndex && (!ref.HasMediaSequence || ref.PartIndex < 0) {
		return errors.New("hlssession: invalid part coordinate")
	}
	if ref.HasRenditionIndex && ref.RenditionIndex < 0 {
		return errors.New("hlssession: negative rendition index")
	}
	if ref.HasRenditionIndex && (ref.HasMediaSequence || ref.HasPartIndex) {
		return errors.New("hlssession: conflicting live coordinates")
	}
	if ref.HasMediaTime {
		if ref.MediaStartNS < 0 || ref.MediaEndNS <= ref.MediaStartNS || ref.HasMediaSequence {
			return errors.New("hlssession: invalid media-time span")
		}
	} else if ref.MediaStartNS != 0 || ref.MediaEndNS != 0 {
		return errors.New("hlssession: media-time span missing presence bit")
	}
	if ref.HasMediaDuration {
		if !ref.HasMediaTime || ref.MediaDurationNS < ref.MediaEndNS {
			return errors.New("hlssession: invalid media duration")
		}
	} else if ref.MediaDurationNS != 0 {
		return errors.New("hlssession: media duration missing presence bit")
	}
	whole := ref.ByteStart == noByteRange && ref.ByteEnd == noByteRange
	if !whole {
		if ref.ByteStart < 0 || ref.ByteEnd < ref.ByteStart {
			return errors.New("hlssession: invalid byte range")
		}
	}
	return nil
}

// marshalReference/unmarshalReference are the store package's persistence
// helpers (kept here so the JSON shape and its size bound live next to the
// type they serialize). maxReferenceJSONBytes rejects a pathological
// manifest-driven Reference before it ever reaches the database.
const maxReferenceJSONBytes = 4096

func marshalReference(ref Reference) (string, error) {
	b, err := json.Marshal(ref)
	if err != nil {
		return "", fmt.Errorf("hlssession: marshal reference: %w", err)
	}
	if len(b) > maxReferenceJSONBytes {
		return "", errors.New("hlssession: reference too large")
	}
	return string(b), nil
}

func unmarshalReference(s string) (Reference, error) {
	var ref Reference
	if len(s) > maxReferenceJSONBytes {
		return ref, errors.New("hlssession: persisted reference too large")
	}
	if err := json.Unmarshal([]byte(s), &ref); err != nil {
		return ref, fmt.Errorf("hlssession: unmarshal reference: %w", err)
	}
	return ref, nil
}

// MarshalReference and UnmarshalReference are the exported forms used by the
// store package's Repository implementation.
func MarshalReference(ref Reference) (string, error) { return marshalReference(ref) }
func UnmarshalReference(s string) (Reference, error) { return unmarshalReference(s) }

// NewWholeReference returns the ByteStart/ByteEnd sentinel pair meaning "this
// resource is not a byte-range slice of its parent" — exported so lane
// owners building a Reference for a whole child playlist, key, or unsliced
// segment do not need to know the sentinel's internal value.
func NewWholeReference() (int64, int64) { return newWholeReference() }
