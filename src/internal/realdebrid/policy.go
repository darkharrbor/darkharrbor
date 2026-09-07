package realdebrid

import (
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

// NewRealDebridPolicy builds the RD rate Policy.
//
// Published limits (api.real-debrid.com, verified 2026-07-11):
//   - 250 requests/min global across ALL operations.
//   - No published per-endpoint create bucket (unlike TorBox's 60/hr).
//   - No slot ceiling on Premium — governor runs rolling-window-only.
//
// 429 Retry-After handling: HTTPClient.feedback installs a per-op-class
// hold-until deadline on every 429, identical to TorBox's mechanism.
func NewRealDebridPolicy() *provider.BasicPolicy {
	global := provider.NewTokenBucket(250, time.Minute)

	p := provider.NewBasicPolicy("realdebrid")
	p.Own(global)

	// All op classes share the single global budget.
	p.AddLimit(provider.OpCreate, global, provider.LimitInfo{Capacity: 250, Window: time.Minute, Kind: "global"})
	p.AddLimit(provider.OpCreateCached, global, provider.LimitInfo{Capacity: 250, Window: time.Minute, Kind: "global"})
	p.AddLimit(provider.OpCreateUncached, global, provider.LimitInfo{Capacity: 250, Window: time.Minute, Kind: "global"})
	p.AddLimit(provider.OpEdge, global, provider.LimitInfo{Capacity: 250, Window: time.Minute, Kind: "global"})
	p.AddLimit(provider.OpQuery, global, provider.LimitInfo{Capacity: 250, Window: time.Minute, Kind: "global"})
	p.AddLimit(provider.OpControl, global, provider.LimitInfo{Capacity: 250, Window: time.Minute, Kind: "global"})

	return p
}
