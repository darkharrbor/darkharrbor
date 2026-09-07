package experience

import "math"

// AdaptiveReadahead recommends a bounded CDN readahead worker count from a
// media's own bitrate, a source's measured single-connection throughput,
// and the shared governor's spare capacity (TS-1.4; torrent-plan T14) —
// "readahead caps near realtime under thin governor headroom" from the
// master plan's own scope text for this row. It is a pure function so its
// bounds are deterministically testable independent of any live network
// path (DG-01/DG-07).
type AdaptiveReadahead struct {
	MinWorkers int
	MaxWorkers int
	// SafetyMultiple bounds how far above the media's own bitrate the
	// recommendation targets before it stops asking for more workers.
	// <=0 uses 2.0 (stay comfortably ahead of realtime, not merely at it).
	SafetyMultiple float64
}

// Recommend returns a worker count in [MinWorkers, MaxWorkers].
//
// bitrateBps is the media's own bits-per-second (0 = unknown: stays at
// MaxWorkers, conservative-safe for a source with no measurement yet).
// perWorkerBps is one connection's measured achievable throughput (<=0:
// same conservative fallback). headroom is the governor's spare capacity
// for this operation class (<0 means unknown/unbounded — no additional
// clamp is applied; a live playback lease acquisition, not this
// recommendation, is what actually arbitrates real contention).
func (a AdaptiveReadahead) Recommend(bitrateBps int64, perWorkerBps int64, headroom int) int {
	minW, maxW := a.MinWorkers, a.MaxWorkers
	if minW < 1 {
		minW = 1
	}
	if maxW < minW {
		maxW = minW
	}

	want := maxW
	if bitrateBps > 0 && perWorkerBps > 0 {
		mult := a.SafetyMultiple
		if mult <= 0 {
			mult = 2.0
		}
		needed := math.Ceil(float64(bitrateBps) * mult / float64(perWorkerBps))
		want = int(needed)
		if want < minW {
			want = minW
		}
		if want > maxW {
			want = maxW
		}
	}

	if headroom >= 0 {
		if headroom < minW {
			// A thin headroom below the floor still yields the floor: a
			// caller always needs at least one connection to play at all.
			// The governor's own admission queue — not this
			// recommendation — is what actually arbitrates real
			// contention between prewarm and playback.
			return minW
		}
		if want > headroom {
			want = headroom
		}
	}
	return want
}
