package torbox

import (
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

// NewTorBoxPolicy builds the TorBox rate/quota Policy (plan §3.2, scenarios
// G-1/G-2/G-3). Documented client-side limits, mirroring TorBox's published
// constraints (upstream reference: TorBox API docs in reference_sources/;
// the fake's server-side enforcement remains the authority in harness runs):
//
//   - 300 requests/min global across ALL API operations,
//   - 60 uncached task creations/hour,
//   - 10 creations/min create-edge burst class,
//   - plan pre-checks (active slots / max item size) from configuration
//     only; zero values leave the check unenforced client-side.
//
// Delta vs the pre-gate wiring (recorded in the gate artifact): the old
// client held three independent buckets — create 60/h, poll 300/min, dl
// 300/min — allowing up to 600 combined query-class requests/min. The plan's
// limit table specifies one 300/min global budget shared by every op class;
// the plan numbers are binding, so the combined ceiling tightens to 300/min.
//
// Retry-After holds: BasicPolicy installs a per-op-class hold-until deadline
// whenever the client reports a 429 with Retry-After (fed back by
// HTTPClient.feedback), satisfying the G-1 backoff contract.
func NewTorBoxPolicy(planSlots int, planMaxBytes int64) *provider.BasicPolicy {
	global := provider.NewTokenBucket(300, time.Minute)
	createHour := provider.NewTokenBucket(60, time.Hour)
	createEdge := provider.NewTokenBucket(10, time.Minute)

	p := provider.NewBasicPolicy("torbox").SetPlan(planSlots, planMaxBytes)
	p.Own(global).Own(createHour).Own(createEdge)

	// OpCreateUncached: edge burst, then uncached hourly budget, then global.
	p.AddLimit(provider.OpCreateUncached, createEdge, provider.LimitInfo{Capacity: 10, Window: time.Minute, Kind: "create-edge"})
	p.AddLimit(provider.OpCreateUncached, createHour, provider.LimitInfo{Capacity: 60, Window: time.Hour, Kind: "create-hourly"})
	p.AddLimit(provider.OpCreateUncached, global, provider.LimitInfo{Capacity: 300, Window: time.Minute, Kind: "global"})

	// OpCreateCached: cached creates are slot-free and do not spend the
	// uncached 60/hour bucket; they still share create-edge + global budgets.
	p.AddLimit(provider.OpCreateCached, createEdge, provider.LimitInfo{Capacity: 10, Window: time.Minute, Kind: "create-edge"})
	p.AddLimit(provider.OpCreateCached, global, provider.LimitInfo{Capacity: 300, Window: time.Minute, Kind: "global"})

	// OpCreate preserves the legacy/generic class as uncached-safe for any
	// caller that has not yet selected cached vs uncached explicitly.
	p.AddLimit(provider.OpCreate, createEdge, provider.LimitInfo{Capacity: 10, Window: time.Minute, Kind: "create-edge"})
	p.AddLimit(provider.OpCreate, createHour, provider.LimitInfo{Capacity: 60, Window: time.Hour, Kind: "create-hourly"})
	p.AddLimit(provider.OpCreate, global, provider.LimitInfo{Capacity: 300, Window: time.Minute, Kind: "global"})

	// OpEdge: the create-edge class without the hourly budget (reserved for
	// future edge-classified endpoints; createtorrent/createusenetdownload route
	// through OpCreateCached/OpCreateUncached above).
	p.AddLimit(provider.OpEdge, createEdge, provider.LimitInfo{Capacity: 10, Window: time.Minute, Kind: "create-edge"})
	p.AddLimit(provider.OpEdge, global, provider.LimitInfo{Capacity: 300, Window: time.Minute, Kind: "global"})

	// OpQuery / OpControl: global budget only.
	p.AddLimit(provider.OpQuery, global, provider.LimitInfo{Capacity: 300, Window: time.Minute, Kind: "global"})
	p.AddLimit(provider.OpControl, global, provider.LimitInfo{Capacity: 300, Window: time.Minute, Kind: "global"})

	return p
}
