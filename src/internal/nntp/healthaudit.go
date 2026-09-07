package nntp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand"

	"github.com/darkharrbor/darkharrbor/internal/ladder"
	"github.com/darkharrbor/darkharrbor/internal/outcome"
)

// This file implements NS-6.1: a rate-capped STAT health audit over an
// already-Ready NZB item's segments, producing a persisted completeness
// estimate and a bounded dead-region map. It deliberately does NOT trigger
// any Arr re-search (NS-6.2's job) and does NOT attempt repair (NS-4.2's
// job, not yet DONE) -- this row only measures and persists.
//
// The audit is intentionally sequential and single-attempt per candidate:
// N13's own rationale is "STAT is header-only and nearly free; cadence
// caps make provider load negligible", so there is no bounded-retry loop
// here the way fetchArticleFromPoolTotal has for BODY -- a single transient
// failure across every candidate provider is treated as INCONCLUSIVE for
// that sample (excluded from the completeness denominator) rather than
// evidence of decay, so a momentary network hiccup can never cause a false
// decayed marking.

// DeadRegion is one persisted, URL-free, message-ID-scoped record of a
// segment this audit has confirmed the item's provider(s) no longer carry.
// It is deliberately small and stable so a future NS-4.2 repair consumer
// can target exactly these segments without re-deriving them from the raw
// NZB. Message-IDs are already persisted verbatim in the item's own stored
// NZB blob (locked constraint: manifests persist on the item like
// RarManifest/ZIPManifest today) -- this is the same class of data, not a
// new secret-safety exposure. Per WORKFLOW.md, message-IDs must never be
// written to logs or WORKLOG evidence; only counts are logged (see
// runHealthAudit's caller in cmd/darkharrbor).
type DeadRegion struct {
	FileIndex     int    `json:"file_index"`
	SegmentNumber int    `json:"segment_number"`
	MessageID     string `json:"message_id"`
}

// SampledSegment identifies one segment chosen for a health-audit STAT
// sample.
type SampledSegment struct {
	FileIndex     int
	SegmentNumber int
	MessageID     string
}

// SegmentResult is one sample's outcome.
type SegmentResult struct {
	SampledSegment
	// Present is meaningful only when Conclusive is true.
	Present bool
	// Conclusive is false when no candidate provider could give a
	// definitive present/missing answer (every candidate failed
	// transiently, e.g. a dial/auth hiccup). Inconclusive samples are
	// excluded from the completeness computation entirely -- neither a
	// hit nor a miss -- rather than guessed either way.
	Conclusive bool
}

// AuditResult is the accounting produced by one health-audit pass over one
// item's sampled segments.
type AuditResult struct {
	// SegmentsSampled counts only CONCLUSIVE samples (DG-05: an audit pass
	// that could reach no provider decisively is not evidence of decay).
	SegmentsSampled int
	SegmentsPresent int
	// Completeness is SegmentsPresent/SegmentsSampled, or 1.0 (healthy) if
	// SegmentsSampled is 0 -- an entirely inconclusive pass must never be
	// interpreted as 0% complete.
	Completeness float64
	// Dead lists every segment this pass conclusively confirmed missing,
	// for the caller to merge into the persisted dead-region map.
	Dead []SampledSegment
	// Recovered lists every segment this pass conclusively confirmed
	// present, for the caller to clear from any prior dead-region entry.
	Recovered []SampledSegment
}

// SelectSampleSegments deterministically-from-rng picks up to n distinct
// segments spread across every file in nzb (not just the largest/video
// file), matching N13's literal "random sample of segments per ... item"
// wording. rng must be non-nil; callers needing determinism (tests) supply
// a seeded *mrand.Rand, live callers supply one seeded from the real clock.
// Returns fewer than n only if the NZB has fewer than n segments total.
func SelectSampleSegments(nzb *NZB, n int, rng *mrand.Rand) []SampledSegment {
	if nzb == nil || n <= 0 || rng == nil {
		return nil
	}
	all := make([]SampledSegment, 0)
	for fi, f := range nzb.Files {
		for _, seg := range f.Segments {
			if seg.MessageID == "" {
				continue
			}
			all = append(all, SampledSegment{
				FileIndex:     fi,
				SegmentNumber: seg.Number,
				MessageID:     seg.MessageID,
			})
		}
	}
	if len(all) == 0 {
		return nil
	}
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// statSegmentPresence walks pool's primary-then-failover candidates (the
// same NS-1.2 preference order fetchArticleWithRetry uses) issuing one STAT
// per reachable candidate, and reports a conclusive present/missing verdict
// as soon as any candidate answers definitively. A known-missing candidate
// (NS-1.1 negative cache) short-circuits with zero wire round trips, same
// as the BODY path. Every candidate failing transiently (none reachable, or
// none answering definitively) reports conclusive=false.
//
// This intentionally does NOT use fetchArticleWithRetry's bounded 3-attempt
// retry loop per candidate: a health-audit STAT is meant to be cheap and
// single-shot (N13: "bounded, ~1s" for the grab-time variant; the periodic
// variant is even less time-sensitive), and a single transient failure
// should fall through to the next candidate rather than spend the retry
// budget BODY reserves for actual playback delivery.
func statSegmentPresence(ctx context.Context, pool *Pool, log *slog.Logger, messageID string) (present, conclusive bool) {
	if pool == nil {
		return false, false
	}
	candidates := append([]*Pool{pool}, pool.failover...)

	rungs := make([]ladder.Rung, 0, len(candidates))
	allKnownMissing := true
	for _, c := range candidates {
		if c == nil {
			continue
		}
		if c.knownMissing(messageID) {
			continue
		}
		allKnownMissing = false
		if c.health != nil && c != pool && c.health.Backoff(c.providerName) > 0 {
			continue
		}
		cc := c
		name := rungNameNNTPPrimary
		if cc != pool {
			name = "nntp_failover_" + cc.providerName
		}
		rungs = append(rungs, ladder.Rung{
			Name:        name,
			Op:          "nntp_audit_stat",
			Priority:    rungPriorityForOp("audit"),
			MaxAttempts: 1,
			Attempt: func(ctx context.Context) error {
				return statFromPool(ctx, cc, log, messageID)
			},
		})
	}
	if len(rungs) == 0 {
		if allKnownMissing {
			// Every candidate has already definitively said it does not
			// carry this article: conclusive miss, zero wire cost.
			return false, true
		}
		// Every remaining candidate is in backoff -- inconclusive, not a
		// miss (mirrors fetchArticleWithRetry's own fallback: try the
		// primary alone rather than report zero rungs attempted).
		rungs = append(rungs, ladder.Rung{
			Name:        rungNameNNTPPrimary,
			Op:          "nntp_audit_stat",
			Priority:    rungPriorityForOp("audit"),
			MaxAttempts: 1,
			Attempt: func(ctx context.Context) error {
				return statFromPool(ctx, pool, log, messageID)
			},
		})
	}

	l := ladder.New(nil, rungs...)
	res, err := l.Run(ctx, "")
	// NS-9.2: STAT presence samples have no associated posting timestamp
	// (SampledSegment carries no PostedAt), so 0 is passed -- RecordSuccessAt
	// safely no-ops the lag-model observation for a non-positive postedAt
	// while its RecordSuccess backoff-clearing behavior is unchanged. Real
	// grab-time BODY fetches (fetchArticleWithRetryAtCRC) are the row's
	// actual data source for the lag model.
	recordFailoverHealth(pool, candidates, res, 0)
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, ladder.ErrExhausted):
		if last := lastRungErr(res); last != nil && errors.Is(last, ErrArticleMissing) {
			return false, true
		}
		// Exhausted on a non-missing classification (every candidate
		// failed transiently): inconclusive.
		return false, false
	default:
		// Context cancellation or account-level: inconclusive, never a
		// miss verdict.
		return false, false
	}
}

// statFromPool performs exactly one STAT attempt against pool, mirroring
// fetchArticleFromPoolTotal's connection lifecycle handling but without its
// 3-attempt retry loop (see statSegmentPresence's doc comment).
func statFromPool(ctx context.Context, pool *Pool, log *slog.Logger, messageID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	statErr := conn.Stat(messageID)
	if statErr != nil {
		if errors.Is(statErr, ErrArticleMissing) {
			pool.recordMissing(messageID)
			// Connection stays healthy (Conn.Stat left it unpoisoned for
			// a definitive answer); release as a clean success.
			pool.Release(conn, nil)
			return outcome.Permanent(fmt.Errorf("nntp: audit stat: %w", statErr))
		}
		pool.Release(conn, statErr)
		return outcome.Transient(statErr)
	}
	pool.Release(conn, nil)
	return nil
}

// RunHealthAudit runs the NS-6.1 STAT sample over segs sequentially (no new
// goroutines: DG-07's bounded-goroutine requirement is trivially satisfied
// by construction) and returns the aggregate result. ctx cancellation stops
// the walk early; already-collected samples are still returned so a
// shutdown mid-audit degrades to a smaller-but-valid sample rather than
// losing the whole pass.
func RunHealthAudit(ctx context.Context, pool *Pool, log *slog.Logger, segs []SampledSegment) AuditResult {
	var res AuditResult
	for _, seg := range segs {
		if ctx.Err() != nil {
			break
		}
		present, conclusive := statSegmentPresence(ctx, pool, log, seg.MessageID)
		if !conclusive {
			continue
		}
		res.SegmentsSampled++
		if present {
			res.SegmentsPresent++
			res.Recovered = append(res.Recovered, seg)
		} else {
			res.Dead = append(res.Dead, seg)
		}
	}
	if res.SegmentsSampled == 0 {
		res.Completeness = 1.0
	} else {
		res.Completeness = float64(res.SegmentsPresent) / float64(res.SegmentsSampled)
	}
	return res
}

// MergeDeadRegions folds one audit pass's conclusive dead/recovered
// segments into an existing persisted dead-region map: newly-dead segments
// are added (deduped by file/segment identity), newly-recovered segments
// are removed, and the result is capped at maxRegions by dropping the
// oldest entries (front of the slice) first -- existing is assumed to
// already be in oldest-first order, matching how this function always
// appends new entries at the end. This bounds the map's growth (DG-09)
// against a pathological item whose provider coverage degrades segment by
// segment over many audit passes.
func MergeDeadRegions(existing []DeadRegion, dead, recovered []SampledSegment, maxRegions int) []DeadRegion {
	key := func(fi, sn int) [2]int { return [2]int{fi, sn} }

	recoveredSet := make(map[[2]int]bool, len(recovered))
	for _, r := range recovered {
		recoveredSet[key(r.FileIndex, r.SegmentNumber)] = true
	}

	merged := make([]DeadRegion, 0, len(existing)+len(dead))
	seen := make(map[[2]int]bool, len(existing)+len(dead))
	for _, d := range existing {
		k := key(d.FileIndex, d.SegmentNumber)
		if recoveredSet[k] {
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		merged = append(merged, d)
	}
	for _, d := range dead {
		k := key(d.FileIndex, d.SegmentNumber)
		if seen[k] {
			continue
		}
		seen[k] = true
		merged = append(merged, DeadRegion(d))
	}
	if maxRegions > 0 && len(merged) > maxRegions {
		merged = merged[len(merged)-maxRegions:]
	}
	return merged
}

// CompletenessThresholdMet reports whether completeness meets threshold,
// i.e. the item should NOT be marked decayed. Kept as a named function
// (rather than an inline `>=`) so the "after par2 headroom" rounding
// convention the frozen plan names has exactly one place to live if a
// future row (NS-2.2/NS-4.1 par2 density signal) adjusts it.
func CompletenessThresholdMet(completeness, threshold float64) bool {
	return completeness >= threshold
}
