package torbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

// UserInfo holds the subset of /v1/api/user/me data Dark Harrbor uses for
// plan-capability discovery. PII fields (email, etc.) are not stored.
type UserInfo struct {
	Plan             int
	AdditionalSlots  int
	IsSubscribed     bool
	PremiumExpiresAt time.Time
	CooldownUntil    time.Time
}

// GetUserInfo fetches plan and subscription state from TorBox. It is called
// once at startup. Callers must treat any error as "use fallback/defaults" so
// startup never depends on plan discovery succeeding.
func (c *HTTPClient) GetUserInfo(ctx context.Context) (*UserInfo, error) {
	env, err := c.do(ctx, provider.OpQuery, http.MethodGet, "/api/user/me", nil, "")
	if err != nil {
		return nil, fmt.Errorf("user/me: %w", err)
	}
	if env == nil {
		return nil, fmt.Errorf("user/me: empty response envelope")
	}
	if !env.Success {
		return nil, fmt.Errorf("user/me: %v", env.Error)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, fmt.Errorf("user/me: empty data")
	}

	var data struct {
		Plan                      int    `json:"plan"`
		AdditionalConcurrentSlots int    `json:"additional_concurrent_slots"`
		IsSubscribed              bool   `json:"is_subscribed"`
		PremiumExpiresAt          string `json:"premium_expires_at"`
		CooldownUntil             string `json:"cooldown_until"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("user/me decode: %w", err)
	}

	info := &UserInfo{
		Plan:            data.Plan,
		AdditionalSlots: data.AdditionalConcurrentSlots,
		IsSubscribed:    data.IsSubscribed,
	}
	if t, err := time.Parse(time.RFC3339, data.PremiumExpiresAt); err == nil {
		info.PremiumExpiresAt = t
	}
	if t, err := time.Parse(time.RFC3339, data.CooldownUntil); err == nil {
		info.CooldownUntil = t
	}
	return info, nil
}
