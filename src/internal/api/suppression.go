package api

import (
	"context"

	"github.com/darkharrbor/darkharrbor/internal/suppress"
)

// isReleaseSuppressed reports whether a release currently has an active
// SF-03 own-measurement suppression (see internal/suppress), fingerprinted
// on its normalized name+size+lane. Best-effort like isTorrentBlacklisted:
// a lookup failure is logged and treated as "not suppressed" rather than
// failing the whole search, since a suppression is a hygiene filter, not a
// correctness requirement — the arr seeing one extra ineligible release it
// has already failed on before is far less costly than a feed outage.
func (s *Server) isReleaseSuppressed(ctx context.Context, name string, size int64, lane suppress.Lane) bool {
	if s.store == nil {
		return false
	}
	fp := suppress.Fingerprint(name, size, lane)
	suppressed, err := s.store.IsSuppressed(ctx, fp)
	if err != nil {
		s.log.Warn("selection: suppression check failed (non-fatal, proceeding)",
			"event", "suppression_check_failed",
			"lane", string(lane),
			"error", err,
		)
		return false
	}
	if suppressed {
		s.log.Info("selection: excluded suppressed release",
			"event", "release_selection_excluded_suppressed",
			"lane", string(lane),
		)
	}
	return suppressed
}

// recordSuppression is the write side: called from a real deterministic
// failure site (currently only NNTP all-providers-dead-post, see main.go's
// handleAllProvidersDead) to fingerprint and suppress a release from
// DarkHarrbor's own feeds for the configured TTL. Best-effort: a persist
// failure is logged, not propagated, since it must never block or fail the
// resolve path it is called from.
func (s *Server) recordSuppression(ctx context.Context, name string, size int64, lane suppress.Lane, reason suppress.Reason) {
	if s.store == nil {
		return
	}
	fp := suppress.Fingerprint(name, size, lane)
	if err := s.store.RecordSuppression(ctx, fp, string(lane), string(reason), s.cfg.SuppressionTTL()); err != nil {
		s.log.Warn("suppression: record failed (non-fatal)",
			"event", "suppression_record_failed",
			"lane", string(lane),
			"reason", string(reason),
			"error", err,
		)
		return
	}
	s.log.Info("suppression: recorded",
		"event", "suppression_recorded",
		"lane", string(lane),
		"reason", string(reason),
		"ttl_hours", s.cfg.Suppression.TTLHours,
	)
}
