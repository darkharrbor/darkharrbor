package premiumize

import "github.com/darkharrbor/darkharrbor/internal/provider"

// Premiumize publishes authoritative error codes but no numeric request rate.
// BasicPolicy therefore owns only provider-directed Retry-After/cooldown holds.
func NewPolicy() *provider.BasicPolicy {
	return provider.NewBasicPolicy("premiumize")
}
