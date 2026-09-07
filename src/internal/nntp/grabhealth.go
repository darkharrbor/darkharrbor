package nntp

import (
	"context"
	mrand "math/rand"
	"time"
)

// This file implements NS-6.3: a bounded, synchronous grab-time STAT check
// extending N13/P11-2's dead-post principle from probe-time to grab-time.
// It deliberately reuses NS-6.1's exact sampling/audit primitives
// (SelectSampleSegments, RunHealthAudit) rather than introducing a second
// STAT mechanism -- this file adds no new wire protocol, no new pool
// lifecycle, and no new persistence. The caller (internal/api) decides what
// "dead" means for acceptance purposes and owns rejection/blacklist
// behavior; this file only answers the bounded measurement question.

// GrabTimeHealthCheck implements provider.UsenetProvider.GrabTimeHealthCheck.
// It parses nzbData, draws up to sampleSize segments spread across every
// file in the NZB (mirroring NS-6.1's own "random sample ... per item"
// selection, not just the largest/video file), and runs the identical
// failover-aware STAT walk RunHealthAudit already uses for the periodic
// health audit. allDead is true only when at least one sample was
// conclusive AND every conclusive sample came back missing -- a partial
// miss (some present, some missing) is NOT treated as dead here; that softer
// completeness-threshold judgment remains NS-6.1/NS-4.2's own job. A parse
// failure, an empty/unsampleable NZB, or an entirely inconclusive pass
// (every candidate provider transiently unreachable, or ctx cancelled/timed
// out before any conclusive sample landed) always reports allDead=false --
// never a false-positive rejection. Callers are expected to wrap ctx with
// their own bounded timeout (the row's own "~1s" grab-time budget); this
// function itself imposes no additional timeout, relying entirely on ctx
// and RunHealthAudit's existing per-segment cancellation check.
func (p *NNTPProvider) GrabTimeHealthCheck(ctx context.Context, nzbData []byte, sampleSize int) (allDead bool, sampled int, err error) {
	nzb, perr := ParseNZB(nzbData)
	if perr != nil {
		// Malformed/unparseable NZB: not this row's job to diagnose -- the
		// existing accept path's own async parse-retry handles it. Abstain.
		return false, 0, perr
	}
	rng := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	segs := SelectSampleSegments(&nzb, sampleSize, rng)
	if len(segs) == 0 {
		return false, 0, nil
	}
	result := RunHealthAudit(ctx, p.pool, p.log, segs)
	return result.SegmentsSampled > 0 && result.SegmentsPresent == 0, result.SegmentsSampled, nil
}
