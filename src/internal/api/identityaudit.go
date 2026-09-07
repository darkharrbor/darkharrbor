// identityaudit.go implements ID-03: a retroactive re-run of ID-01's
// identity verification over already-Ready items (any lane), on both a
// slow background cadence (wired in cmd/darkharrbor/main.go, mirroring
// HealthAudit's own ticker exactly) and an on-demand HTTP trigger.
//
// This row deliberately never takes enforcement action: no suppression, no
// blacklist, no StateFailed. Retroactively auto-failing already-Ready
// library content people may be actively watching is a materially
// different -- and much riskier -- decision than ID-01's own live
// grab-time enforce path, which only ever acts on a release that has not
// yet reached a player. ID-03's entire job is measurement and persistence
// of a legible possible-wrong-content report; acting on that report, if
// ever wanted, is deliberately left to an operator or a future row (the
// same "measurement-only" boundary NS-6.1 already established for its own
// health-audit janitor).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/darkharrbor/darkharrbor/internal/identitycheck"
)

// defaultIdentityAuditFindingsLimit and maxIdentityAuditFindingsLimit bound
// the on-demand findings report (DG-07): an operator-supplied ?limit= is
// clamped into this range rather than trusted verbatim.
const (
	defaultIdentityAuditFindingsLimit = 100
	maxIdentityAuditFindingsLimit     = 500
)

// IdentityAuditCycleResult summarizes one RunIdentityAuditCycle pass for
// logging and the on-demand HTTP response. Audited is false when no
// eligible Ready item existed (an empty library, or every item terminal
// between selection attempts) -- a legitimate no-op, not an error.
type IdentityAuditCycleResult struct {
	Audited      bool     `json:"audited"`
	ItemID       string   `json:"item_id,omitempty"`
	VerdictCount int      `json:"verdict_count"`
	Reasons      []string `json:"reasons,omitempty"`
}

// RunIdentityAuditCycle runs one ID-03 pass: select the single
// least-recently-audited Ready item (any lane, store.NextIdentityAuditItem),
// re-run identitycheck.Verify against its already-persisted
// ProviderIdentity (ID-02) and MediaFacts (HR5.2/SF-04) -- no second probe,
// no second classifier -- and persist the verdicts. An item with no
// captured ProviderIdentity is not skipped from rotation (it is still
// marked audited with an empty verdict set, exactly mirroring ID-01's own
// abstain-not-guess convention and NS-6.1's precedent of cycling through
// every eligible item regardless of per-item data completeness); it simply
// produces no finding.
//
// Guarded by s.identityAuditMu so the periodic ticker (cmd/darkharrbor/
// main.go) and an on-demand HTTP trigger can never race the same
// select-then-persist operation and never double-process the same item in
// one instant (DG-07).
func (s *Server) RunIdentityAuditCycle(ctx context.Context) (*IdentityAuditCycleResult, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("identityaudit: server not initialized")
	}
	s.identityAuditMu.Lock()
	defer s.identityAuditMu.Unlock()

	item, err := s.store.NextIdentityAuditItem(ctx)
	if err != nil {
		return nil, fmt.Errorf("identityaudit: select next item: %w", err)
	}
	if item == nil {
		return &IdentityAuditCycleResult{Audited: false}, nil
	}

	var verdicts []identitycheck.Verdict
	identity := item.Metadata.ProviderIdentity
	if identity != nil {
		season, episode, _ := identitycheck.ParseSeasonEpisode(item.DisplayName)
		// expectedAudioLangCode: same disclosed limitation as ID-01's own
		// CheckIdentity -- not yet wired to a real Arr-side original-
		// language source, so this check always abstains today.
		const expectedAudioLangCode = ""
		verdicts = identitycheck.Verify(identity, item.DisplayName, season, episode, item.Metadata.MediaFacts, expectedAudioLangCode)
	}

	if verdicts == nil {
		// identitycheck.Verify returns a nil slice (not an empty one) when
		// no check fires; json.Marshal(nil) encodes as the JSON literal
		// "null", which ListIdentityAuditFindings' `!= '[]'` filter would
		// NOT recognize as clean -- normalize here rather than depend on
		// identitycheck's own return shape (a byte-identical, unmodified
		// dependency per this row's own reuse rule).
		verdicts = []identitycheck.Verdict{}
	}
	verdictsJSON, err := json.Marshal(verdicts)
	if err != nil {
		return nil, fmt.Errorf("identityaudit: marshal verdicts for item %s: %w", item.ID, err)
	}
	if err := s.store.UpsertIdentityAudit(ctx, item.ID, string(verdictsJSON)); err != nil {
		return nil, fmt.Errorf("identityaudit: persist item %s: %w", item.ID, err)
	}

	reasons := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		reasons = append(reasons, string(v.Reason))
	}
	if len(verdicts) > 0 {
		s.log.Warn("identityaudit: retroactive pass found possible mismatch",
			"event", "identity_audit_finding",
			"item_id", item.ID,
			"reasons", reasons,
		)
	} else {
		s.log.Info("identityaudit: retroactive pass clean",
			"item_id", item.ID,
			"had_identity", identity != nil,
		)
	}

	return &IdentityAuditCycleResult{
		Audited:      true,
		ItemID:       item.ID,
		VerdictCount: len(verdicts),
		Reasons:      reasons,
	}, nil
}

// handleIdentityAuditRun is the on-demand trigger (POST
// /api/v1/identity-audit/run): runs exactly one bounded RunIdentityAuditCycle
// pass, synchronously, and returns its result. Always honored regardless of
// cfg.IdentityAudit.Enabled -- that flag only gates the background ticker,
// never an operator's explicit request for one pass right now.
func (s *Server) handleIdentityAuditRun(w http.ResponseWriter, r *http.Request) {
	result, err := s.RunIdentityAuditCycle(r.Context())
	if err != nil {
		s.log.Error("identityaudit: on-demand run failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "identity audit run failed"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleIdentityAuditFindings is the legible possible-wrong-content report
// (GET /api/v1/identity-audit/findings): bounded, most-recently-audited
// first. ?limit= is clamped to [1, maxIdentityAuditFindingsLimit]; an
// invalid/absent value falls back to defaultIdentityAuditFindingsLimit
// (never fails the request, matching every other config/param abstain
// convention in this program).
func (s *Server) handleIdentityAuditFindings(w http.ResponseWriter, r *http.Request) {
	limit := defaultIdentityAuditFindingsLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxIdentityAuditFindingsLimit {
		limit = maxIdentityAuditFindingsLimit
	}

	findings, err := s.store.ListIdentityAuditFindings(r.Context(), limit)
	if err != nil {
		s.log.Error("identityaudit: list findings failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "identity audit findings unavailable"})
		return
	}

	type findingView struct {
		ItemID        string                  `json:"item_id"`
		DisplayName   string                  `json:"display_name"`
		LastAuditedAt string                  `json:"last_audited_at"`
		Verdicts      []identitycheck.Verdict `json:"verdicts"`
	}
	out := make([]findingView, 0, len(findings))
	for _, f := range findings {
		var verdicts []identitycheck.Verdict
		if err := json.Unmarshal([]byte(f.VerdictsJSON), &verdicts); err != nil {
			// Malformed persisted JSON fails closed for this one row only
			// (skip, log) rather than failing the whole report -- DG-05's
			// mutation-injection posture applied to a read path.
			s.log.Warn("identityaudit: skipping finding with malformed verdicts JSON", "item_id", f.ItemID, "error", err)
			continue
		}
		out = append(out, findingView{
			ItemID:        f.ItemID,
			DisplayName:   f.DisplayName,
			LastAuditedAt: f.LastAuditedAt.Format("2006-01-02T15:04:05Z07:00"),
			Verdicts:      verdicts,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": out})
}
