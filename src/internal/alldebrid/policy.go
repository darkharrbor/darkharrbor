package alldebrid

import (
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

func NewPolicy() *provider.BasicPolicy {
	perSecond := provider.NewTokenBucket(12, time.Second)
	perMinute := provider.NewTokenBucket(600, time.Minute)
	p := provider.NewBasicPolicy("alldebrid")
	p.Own(perSecond).Own(perMinute)
	for _, op := range []provider.Op{
		provider.OpCreate, provider.OpCreateCached, provider.OpCreateUncached,
		provider.OpEdge, provider.OpQuery, provider.OpControl,
	} {
		p.AddLimit(op, perSecond, provider.LimitInfo{Capacity: 12, Window: time.Second, Kind: "global"})
		p.AddLimit(op, perMinute, provider.LimitInfo{Capacity: 600, Window: time.Minute, Kind: "global"})
	}
	return p
}
