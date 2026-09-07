// Package reactivepromotion owns RX-5.1's deliberate proxy-to-file cutover.
package reactivepromotion

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const MaxProofs = 4096

type Repository interface {
	GetReactiveCommit(context.Context, string) (store.ReactiveCommit, bool, error)
	GetItemByID(context.Context, string) (*store.Item, error)
	ListContentProofs(context.Context, []string, time.Time) ([]contentproof.Evidence, error)
	BeginReactivePromotion(context.Context, string, string, int64, time.Time) error
	GetReactivePromotion(context.Context, string) (store.ReactivePromotion, bool, error)
	CompleteReactivePromotion(context.Context, string, int64, time.Time) error
	FailReactivePromotion(context.Context, string, time.Time) error
	RemoveReactivePromotion(context.Context, string, time.Time) error
	SetReactivePromotionHeadroom(context.Context, int64, time.Time) error
}

// Remove records the operator's deliberate deletion before removing bytes,
// so a later absence is never interpreted as representation loss.
func (s *Service) Remove(ctx context.Context, representationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	promotion, found, err := s.repo.GetReactivePromotion(ctx, representationID)
	if err != nil || !found || promotion.State != "promoted" {
		return errors.New("reactivepromotion: promoted representation unavailable")
	}
	commit, found, err := s.repo.GetReactiveCommit(ctx, representationID)
	if err != nil || !found {
		return errors.New("reactivepromotion: committed representation unavailable")
	}
	if err := s.repo.RemoveReactivePromotion(ctx, representationID, s.now().UTC()); err != nil {
		return err
	}
	if err := os.Remove(promotion.TargetPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("reactivepromotion: deliberate removal failed")
	}
	return s.rescanner.Rescan(ctx, commit)
}

type Cache interface {
	YieldForPromotion(context.Context, string, int64, int) (int64, error)
	GetChunk(context.Context, rangecache.ChunkSource, rangecache.Mode, rangecache.ChunkRef) ([]byte, error)
}

type Opener interface {
	OpenPromotionSource(context.Context, *store.Item, string) (bytesource.ByteSource, error)
}
type Rescanner interface {
	Rescan(context.Context, store.ReactiveCommit) error
	Materialize(context.Context, store.ReactiveCommit, string, int64) error
}

type Service struct {
	repo         Repository
	cache        Cache
	opener       Opener
	rescanner    Rescanner
	proofs       *contentproof.Graph
	root         string
	floorPercent int
	now          func() time.Time
}

type Options struct {
	Root         string
	FloorPercent int
	Now          func() time.Time
}

func New(repo Repository, cache Cache, opener Opener, rescanner Rescanner, proofs *contentproof.Graph, opts Options) (*Service, error) {
	root := filepath.Clean(opts.Root)
	if repo == nil || cache == nil || opener == nil || rescanner == nil || proofs == nil || !filepath.IsAbs(root) || opts.FloorPercent < 1 || opts.FloorPercent > 99 {
		return nil, errors.New("reactivepromotion: invalid dependencies or options")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{repo: repo, cache: cache, opener: opener, rescanner: rescanner, proofs: proofs, root: root, floorPercent: opts.FloorPercent, now: opts.Now}, nil
}

func (s *Service) Promote(ctx context.Context, representationID, target string) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	target = filepath.Clean(target)
	if representationID == "" || !filepath.IsAbs(target) || target == s.root || !strings.HasPrefix(target, s.root+string(filepath.Separator)) || strings.EqualFold(filepath.Ext(target), ".strm") {
		return errors.New("reactivepromotion: unsafe target")
	}
	realRoot, rootErr := filepath.EvalSymlinks(s.root)
	realDir, dirErr := filepath.EvalSymlinks(filepath.Dir(target))
	if rootErr != nil || dirErr != nil || (realDir != realRoot && !strings.HasPrefix(realDir, realRoot+string(filepath.Separator))) {
		return errors.New("reactivepromotion: target escapes data root")
	}
	commit, found, err := s.repo.GetReactiveCommit(ctx, representationID)
	if err != nil || !found || commit.State != "committed" {
		return errors.New("reactivepromotion: committed representation unavailable")
	}
	item, err := s.repo.GetItemByID(ctx, commit.ItemID)
	if err != nil || item == nil || item.StrmPath == nil || filepath.Dir(*item.StrmPath) != filepath.Dir(target) {
		return errors.New("reactivepromotion: target must share the committed output directory")
	}
	if info, err := os.Lstat(target); err == nil {
		promotion, found, getErr := s.repo.GetReactivePromotion(ctx, representationID)
		if getErr == nil && found && promotion.State == "promoted" && promotion.TargetPath == target && promotion.SizeBytes == info.Size() && info.Mode().IsRegular() {
			return s.rescanner.Materialize(ctx, commit, target, info.Size())
		}
		return errors.New("reactivepromotion: target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("reactivepromotion: target unavailable")
	}
	source, err := s.opener.OpenPromotionSource(ctx, item, commit.FileID)
	if err != nil || source == nil || source.Size() <= 0 || !source.Caps().ExactSize || !source.Caps().RangeSupport {
		return errors.New("reactivepromotion: exact byte source unavailable")
	}
	size := source.Size()
	now := s.now().UTC()
	if err := s.repo.BeginReactivePromotion(ctx, representationID, target, size, now); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = s.repo.FailReactivePromotion(cleanupCtx, representationID, s.now().UTC())
		}
	}()
	headroom, err := s.cache.YieldForPromotion(ctx, target, size, s.floorPercent)
	if err != nil {
		return err
	}
	if err := s.repo.SetReactivePromotionHeadroom(ctx, headroom, now); err != nil {
		return err
	}
	blocks, err := rangecache.NewByteSourceBlocks(source, 0)
	if err != nil {
		return err
	}
	stage := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".darkharrbor-part")
	if err := os.Remove(stage); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("reactivepromotion: stale stage removal failed")
	}
	f, err := os.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return errors.New("reactivepromotion: stage create failed")
	}
	defer func() {
		_ = f.Close()
		if retErr != nil {
			_ = os.Remove(stage)
		}
	}()
	var written int64
	for _, ref := range blocks.Chunks() {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := s.cache.GetChunk(ctx, blocks, rangecache.ModeFull, ref)
		if err != nil || int64(len(data)) != ref.Size {
			return errors.New("reactivepromotion: bounded source transfer failed")
		}
		n, err := f.Write(data)
		written += int64(n)
		if err != nil || n != len(data) {
			return errors.New("reactivepromotion: short stage write")
		}
	}
	if written != size {
		return errors.New("reactivepromotion: staged size mismatch")
	}
	if err := f.Sync(); err != nil {
		return errors.New("reactivepromotion: stage sync failed")
	}
	if err := f.Close(); err != nil {
		return errors.New("reactivepromotion: stage close failed")
	}
	if err := s.verify(ctx, representationID, stage, size); err != nil {
		return err
	}

	backup := *item.StrmPath + ".promotion-backup"
	if err := os.Rename(*item.StrmPath, backup); err != nil {
		return errors.New("reactivepromotion: proxy cutover failed")
	}
	rollback := true
	defer func() {
		if rollback {
			_ = os.Remove(target)
			_ = os.Rename(backup, *item.StrmPath)
		}
	}()
	if err := os.Rename(stage, target); err != nil {
		return errors.New("reactivepromotion: atomic materialization failed")
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return errors.New("reactivepromotion: materialization sync failed")
	}
	if err := s.rescanner.Materialize(ctx, commit, target, size); err != nil {
		return err
	}
	if err := s.repo.CompleteReactivePromotion(ctx, representationID, size, s.now().UTC()); err != nil {
		return err
	}
	rollback = false
	_ = os.Remove(backup)
	return nil
}

func (s *Service) verify(ctx context.Context, representationID, path string, size int64) error {
	proofs, err := s.repo.ListContentProofs(ctx, []string{representationID}, s.now().UTC())
	if err != nil || len(proofs) > MaxProofs {
		return errors.New("reactivepromotion: proof set unavailable or unbounded")
	}
	var selected *contentproof.Evidence
	for i := range proofs {
		p := &proofs[i]
		if p.Kind != contentproof.KindAuthoritative || !supportedProofAlgorithm(p.Algorithm) || p.Offset < 0 || p.Length <= 0 || p.Offset > size || p.Length > size-p.Offset {
			continue
		}
		if selected == nil || (p.Scope == contentproof.ScopeWhole && selected.Scope != contentproof.ScopeWhole) {
			selected = p
		}
	}
	if selected == nil {
		return errors.New("reactivepromotion: authoritative proof absent")
	}
	h, err := proofHash(selected.Algorithm)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("reactivepromotion: staged proof read failed")
	}
	defer f.Close()
	if _, err := f.Seek(selected.Offset, io.SeekStart); err != nil {
		return errors.New("reactivepromotion: staged proof seek failed")
	}
	buf := make([]byte, 1<<20)
	remaining := selected.Length
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := int64(len(buf))
		if want > remaining {
			want = remaining
		}
		n, err := io.ReadFull(f, buf[:want])
		if err != nil || int64(n) != want {
			return errors.New("reactivepromotion: staged proof read failed")
		}
		_, _ = h.Write(buf[:n])
		remaining -= int64(n)
	}
	decision, err := s.proofs.VerifyDigest(ctx, representationID, selected.Scope, selected.Offset, selected.Length, selected.Algorithm, h.Sum(nil))
	if err != nil || decision.Relation != contentproof.RelationProven {
		return errors.New("reactivepromotion: content proof did not authorize bytes")
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func supportedProofAlgorithm(algorithm contentproof.Algorithm) bool {
	return algorithm == contentproof.AlgorithmMD5 || algorithm == contentproof.AlgorithmSHA1 || algorithm == contentproof.AlgorithmSHA256
}

func proofHash(algorithm contentproof.Algorithm) (hash.Hash, error) {
	switch algorithm {
	case contentproof.AlgorithmMD5:
		return md5.New(), nil
	case contentproof.AlgorithmSHA1:
		return sha1.New(), nil
	case contentproof.AlgorithmSHA256:
		return sha256.New(), nil
	default:
		return nil, errors.New("reactivepromotion: unsupported proof algorithm")
	}
}
