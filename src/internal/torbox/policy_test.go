package torbox

import (
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

func TestCreatePolicyOpSplitsCachedAndUncached(t *testing.T) {
	if got := createPolicyOp("", true); got != provider.OpCreateCached {
		t.Fatalf("cached create op = %s, want %s", got, provider.OpCreateCached)
	}
	if got := createPolicyOp("", false); got != provider.OpCreateUncached {
		t.Fatalf("uncached create op = %s, want %s", got, provider.OpCreateUncached)
	}
	if got := createPolicyOp(provider.OpCreate, true); got != provider.OpCreate {
		t.Fatalf("explicit create op = %s, want %s", got, provider.OpCreate)
	}
}

func TestNewTorBoxPolicyCachedCreateSkipsHourlyBucket(t *testing.T) {
	limits := NewTorBoxPolicy(10, 0).Limits().Limits
	byOp := map[provider.Op]map[string]bool{}
	for _, limit := range limits {
		if byOp[limit.Op] == nil {
			byOp[limit.Op] = map[string]bool{}
		}
		byOp[limit.Op][limit.Kind] = true
	}

	cached := byOp[provider.OpCreateCached]
	if !cached["create-edge"] || !cached["global"] {
		t.Fatalf("cached create limits = %#v, want create-edge + global", cached)
	}
	if cached["create-hourly"] {
		t.Fatalf("cached create limits include create-hourly: %#v", cached)
	}

	uncached := byOp[provider.OpCreateUncached]
	if !uncached["create-edge"] || !uncached["create-hourly"] || !uncached["global"] {
		t.Fatalf("uncached create limits = %#v, want create-edge + create-hourly + global", uncached)
	}
}
