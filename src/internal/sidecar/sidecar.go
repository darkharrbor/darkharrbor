// Package sidecar owns DarkHarrbor's stable, bounded non-media resource
// contract. Lane adapters register subtitles, chapters, attachments, and
// other sidecars here; players receive only the resulting opaque DH path.
package sidecar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Kind string

const (
	KindSubtitle   Kind = "subtitle"
	KindChapters   Kind = "chapters"
	KindAttachment Kind = "attachment"
	KindSidecar    Kind = "sidecar"
)

const (
	DefaultMaxResourcesPerItem = 128
	DefaultMaxResourceBytes    = 8 << 20
	DefaultMaxBytesPerItem     = 64 << 20
)

var (
	ErrNotFound = errors.New("sidecar: resource not found")
	ErrCapacity = errors.New("sidecar: item resource capacity exceeded")
	ErrRejected = errors.New("sidecar: resource rejected")

	resourceIDPattern = regexp.MustCompile(`^sc[0-9a-f]{24}$`)
)

type Resource struct {
	ID        string
	ItemID    string
	FileID    string
	Kind      Kind
	Filename  string
	MediaType string
	Language  string
	Bytes     []byte
	CreatedAt time.Time
}

func (r Resource) Path() string { return "/sidecar/" + r.ID }

type Input struct {
	ItemID    string
	FileID    string
	Kind      Kind
	Filename  string
	MediaType string
	Language  string
	Bytes     []byte
}

type Repository interface {
	InsertSidecarResource(context.Context, Resource, int, int64) error
	GetSidecarResource(context.Context, string) (*Resource, error)
	ListSidecarResources(context.Context, string, string) ([]Resource, error)
}

type Options struct {
	Now                 func() time.Time
	MaxResourcesPerItem int
	MaxResourceBytes    int
	MaxBytesPerItem     int64
}

type Registry struct {
	repo                Repository
	now                 func() time.Time
	maxResourcesPerItem int
	maxResourceBytes    int
	maxBytesPerItem     int64
}

func New(repo Repository, opts Options) (*Registry, error) {
	if repo == nil {
		return nil, errors.New("sidecar: nil repository")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxResourcesPerItem <= 0 {
		opts.MaxResourcesPerItem = DefaultMaxResourcesPerItem
	}
	if opts.MaxResourceBytes <= 0 {
		opts.MaxResourceBytes = DefaultMaxResourceBytes
	}
	if opts.MaxBytesPerItem <= 0 {
		opts.MaxBytesPerItem = DefaultMaxBytesPerItem
	}
	return &Registry{
		repo:                repo,
		now:                 opts.Now,
		maxResourcesPerItem: opts.MaxResourcesPerItem,
		maxResourceBytes:    opts.MaxResourceBytes,
		maxBytesPerItem:     opts.MaxBytesPerItem,
	}, nil
}

func (r *Registry) Register(ctx context.Context, in Input) (Resource, error) {
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}
	if err := validateInput(in, r.maxResourceBytes); err != nil {
		return Resource{}, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	id := resourceID(in)
	resource := Resource{
		ID:        id,
		ItemID:    in.ItemID,
		FileID:    in.FileID,
		Kind:      in.Kind,
		Filename:  in.Filename,
		MediaType: in.MediaType,
		Language:  in.Language,
		Bytes:     append([]byte(nil), in.Bytes...),
		CreatedAt: r.now().UTC(),
	}
	if err := r.repo.InsertSidecarResource(ctx, resource, r.maxResourcesPerItem, r.maxBytesPerItem); err != nil {
		return Resource{}, err
	}
	persisted, err := r.repo.GetSidecarResource(ctx, id)
	if err != nil {
		return Resource{}, err
	}
	if persisted == nil {
		return Resource{}, ErrNotFound
	}
	persisted.Bytes = append([]byte(nil), persisted.Bytes...)
	return *persisted, nil
}

func (r *Registry) Resolve(ctx context.Context, id string) (Resource, error) {
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}
	if !ValidResourceID(id) {
		return Resource{}, ErrNotFound
	}
	resource, err := r.repo.GetSidecarResource(ctx, id)
	if err != nil {
		return Resource{}, err
	}
	if resource == nil {
		return Resource{}, ErrNotFound
	}
	resource.Bytes = append([]byte(nil), resource.Bytes...)
	return *resource, nil
}

func (r *Registry) List(ctx context.Context, itemID, fileID string) ([]Resource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateOpaque("item id", itemID); err != nil {
		return nil, err
	}
	if err := validateOpaque("file id", fileID); err != nil {
		return nil, err
	}
	return r.repo.ListSidecarResources(ctx, itemID, fileID)
}

func ValidResourceID(id string) bool { return resourceIDPattern.MatchString(id) }

// SubtitleMediaType classifies the four subtitle formats admitted by the
// consolidated sidecar contract. Names must already be safe basenames; the
// function deliberately does not infer language from release naming.
func SubtitleMediaType(filename string) (string, bool) {
	if filename == "" || len(filename) > 255 || !utf8.ValidString(filename) ||
		path.Base(filename) != filename || strings.ContainsAny(filename, `\/`) ||
		strings.Contains(filename, "..") || strings.ContainsAny(filename, "\r\n\x00") {
		return "", false
	}
	switch strings.ToLower(path.Ext(filename)) {
	case ".srt":
		return "application/x-subrip", true
	case ".ass":
		return "text/x-ssa; charset=utf-8", true
	case ".idx":
		return "text/plain; charset=utf-8", true
	case ".sub":
		return "application/octet-stream", true
	default:
		return "", false
	}
}

func validateInput(in Input, maxBytes int) error {
	if err := validateOpaque("item id", in.ItemID); err != nil {
		return err
	}
	if err := validateOpaque("file id", in.FileID); err != nil {
		return err
	}
	switch in.Kind {
	case KindSubtitle, KindChapters, KindAttachment, KindSidecar:
	default:
		return fmt.Errorf("sidecar: invalid kind %q", in.Kind)
	}
	if in.Filename == "" || len(in.Filename) > 255 || !utf8.ValidString(in.Filename) ||
		path.Base(in.Filename) != in.Filename || strings.ContainsAny(in.Filename, `\/`) ||
		strings.Contains(in.Filename, "..") || strings.ContainsAny(in.Filename, "\r\n\x00") {
		return errors.New("sidecar: unsafe filename")
	}
	if len(in.MediaType) > 255 || strings.ContainsAny(in.MediaType, "\r\n\x00") {
		return errors.New("sidecar: invalid media type")
	}
	if _, _, err := mime.ParseMediaType(in.MediaType); err != nil {
		return errors.New("sidecar: invalid media type")
	}
	if len(in.Language) > 35 || strings.ContainsAny(in.Language, ":/\\\r\n\x00") {
		return errors.New("sidecar: invalid language")
	}
	if len(in.Bytes) == 0 {
		return errors.New("sidecar: empty resource")
	}
	if len(in.Bytes) > maxBytes {
		return fmt.Errorf("sidecar: resource exceeds %d-byte limit", maxBytes)
	}
	if bytes.Contains(in.Bytes, []byte("://")) || bytes.Contains(bytes.ToLower(in.Bytes), []byte("tok=")) {
		return errors.New("sidecar: URL-shaped or token-shaped payload rejected")
	}
	return nil
}

func validateOpaque(name, value string) error {
	if value == "" || len(value) > 255 || !utf8.ValidString(value) ||
		strings.Contains(value, "://") || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("sidecar: invalid %s", name)
	}
	return nil
}

func resourceID(in Input) string {
	hash := sha256.New()
	for _, value := range []string{in.ItemID, in.FileID, string(in.Kind), in.Filename, in.MediaType, in.Language} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write(in.Bytes)
	return "sc" + hex.EncodeToString(hash.Sum(nil)[:12])
}
