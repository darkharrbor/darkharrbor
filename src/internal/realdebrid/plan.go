package realdebrid

// RD plan model. Real-Debrid has two effective tiers from DH's perspective:
// Premium (paid) and Free. Premium allows up to 100 concurrent active torrents
// (verified live via /torrents/activeCount, 2026-07-11). No published
// per-create rate budget beyond the global 250 req/min API limit.
// Free accounts are non-functional for DH.
//
// Key live-verified facts (2026-07-11, Premium account):
//   - instantAvailability: disabled (error_code 37) — CacheOracle=false
//   - infringing_file (error_code 35): RD DMCA-blocks hashes at API level
//   - activeCount limit: 100 concurrent torrents — SlotModel=true, ceiling=100
//   - unrestrict/link URLs: expire — must call at stream time, never store
//   - add-then-poll: only fulfillment model; cached items → "downloaded" fast

const (
	// PremiumDaysDefault is the minimum recommended purchase window.
	PremiumDaysDefault = 180

	// maxBytesUnlimited signals no per-item size enforcement on Premium.
	maxBytesUnlimited = int64(0)
)

// AccountInfo holds the subset of /user data DH uses for capability detection.
type AccountInfo struct {
	// Premium is true when the account has an active premium subscription.
	Premium bool
	// ExpirationDays is the number of days remaining on the premium subscription
	// (0 when not premium).
	ExpirationDays int
	// Points is the account's fidelity point balance (informational only).
	Points int
}

// PremiumActiveSlots is RD's verified concurrent active-torrent ceiling.
// Source: /torrents/activeCount response, verified live 2026-07-11.
const PremiumActiveSlots = 100

// ResolveCaps computes RD capabilities from discovered account data.
// Returns (premium bool, slots int, maxBytes int64).
// Conservative: nil info → non-premium (DH will not submit).
func ResolveCaps(info *AccountInfo) (premium bool, slots int, maxBytes int64) {
	if info == nil || !info.Premium {
		return false, 0, 0
	}
	return true, PremiumActiveSlots, maxBytesUnlimited
}
