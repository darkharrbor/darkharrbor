package api

// identitycheck.go wires ID-01 (grab-time identity verification against the
// grabbing Arr's authoritative metadata) into every lane that already
// captures internal/mediaidentity.ProviderIdentity at grab time (ID-02:
// torrent, NZB, and HTTP submission paths all persist it). The pure
// verification logic itself lives in internal/identitycheck and is never
// duplicated here — this file only decides what to DO with a verdict.
//
// Action taken on a mismatch mirrors two existing, already-reviewed
// precedents rather than inventing a third recovery path:
//   - suppression: internal/api/suppression.go's recordSuppression (SF-03),
//     reusing the already-reserved suppress.ReasonIdentityMismatch.
//   - blacklist + terminal StateFailed: cmd/darkharrbor's own
//     handleAllProvidersDead (NNTP) and TS-4.1's repair-exhaustion terminal
//     step (torrent) both blacklist the exact source identity and mark the
//     item StateFailed so the arr's own queue polling blocklists + re-
//     searches it — "existing plumbing", per the master-plan row's own
//     "repair/blacklist/re-search" phrasing.
//
// TriggerRepairAsync (TS-4.1) is deliberately NOT reused for an identity
// mismatch: repair re-adds the exact same source identity (same infohash)
// to a(nother) provider lane, which returns byte-identical content. That
// can recover a broken/interrupted transfer, but it cannot ever change
// what the release actually is — reusing it here would silently retry the
// same wrong content and never converge. Blacklist + suppression + re-
// search is the only action that can actually resolve a genuine mismatch.
import (
	"context"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/identitycheck"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/suppress"
)

// CheckIdentity runs ID-01 verification for item and, depending on the
// configured strictness, logs (warn) or additionally suppresses +
// blacklists + fails the item (enforce). A "off" strictness or a missing
// captured identity is a fast no-op. facts may be nil (the NNTP lane has no
// persisted media-truth facts today, a disclosed SF-04 limitation) — every
// individual check inside internal/identitycheck safely abstains on a nil/
// zero-value input rather than guessing, so passing nil facts simply
// narrows this call to the title-only checks (year, conflicting source
// tags). season/episode are 0 for a movie or an ambiguous/season-pack
// series release (see identitycheck.ParseSeasonEpisode).
//
// Best-effort and synchronous: called inline from an already-bounded
// resolve path (the same call sites SF-04/TS-3.3 use), never spawns a
// goroutine, and every failure along the action path is logged, not
// propagated — exactly like recordSuppression and TriggerRepairAsync's own
// documented non-fatal conventions.
func (s *Server) CheckIdentity(ctx context.Context, item *store.Item, season, episode int, facts *mediatruth.Facts) {
	if s == nil || s.cfg == nil || s.store == nil || item == nil {
		return
	}
	strictness := identitycheck.ParseStrictness(s.cfg.Identity.Strictness)
	if strictness == identitycheck.StrictnessOff {
		return
	}
	identity := item.Metadata.ProviderIdentity
	if identity == nil {
		return
	}
	// expectedAudioLangCode: not yet wired to a real Arr-side original-
	// language source in this session (disclosed limitation — see the
	// row's own WORKLOG entry). Always "" today, so checkAudioLanguage
	// always abstains; the check itself is implemented and tested so a
	// future row need only supply the code, not new verification logic.
	const expectedAudioLangCode = ""
	verdicts := identitycheck.Verify(identity, item.DisplayName, season, episode, facts, expectedAudioLangCode)
	if len(verdicts) == 0 {
		return
	}
	reasons := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		reasons = append(reasons, string(v.Reason))
	}
	s.log.Warn("identity: grab-time verification found mismatch",
		"event", "identity_mismatch",
		"item_id", item.ID,
		"reasons", reasons,
		"strictness", string(strictness),
	)
	if strictness != identitycheck.StrictnessEnforce {
		return
	}

	lane := suppress.LaneTorrent
	if item.SourceType == store.SourceTypeNZB {
		lane = suppress.LaneNZB
	}
	s.recordSuppression(ctx, item.DisplayName, item.TotalSize, lane, suppress.ReasonIdentityMismatch)

	if hash := torrentRepairIdentity(item); hash != "" {
		if _, err := s.store.BlacklistImmediately(ctx, hash); err != nil {
			s.log.Warn("identity: blacklist failed (non-fatal)", "item_id", item.ID, "error", err)
		}
	} else if item.SourceType == store.SourceTypeNZB && item.SourceURI != nil && *item.SourceURI != "" {
		key := NZBBlacklistKey(*item.SourceURI)
		if _, err := s.store.BlacklistImmediately(ctx, key); err != nil {
			s.log.Warn("identity: blacklist failed (non-fatal)", "item_id", item.ID, "error", err)
		}
	}

	msg := "identity_mismatch: grab-time verification against Arr metadata failed (" + strings.Join(reasons, ",") + ")"
	if err := s.store.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
		s.log.Error("identity: persist failed state failed", "item_id", item.ID, "error", err)
		return
	}
	s.log.Warn("identity: item failed on enforced mismatch",
		"event", "identity_mismatch_enforced",
		"item_id", item.ID,
		"reasons", reasons,
	)
	if s.arrRefresh != nil {
		s.arrRefresh.Notify()
	}
}
