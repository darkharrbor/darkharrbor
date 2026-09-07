package config

import "strings"

// ValidReactiveCommitMode recognizes RX-5.2's complete, default-off mode set.
func ValidReactiveCommitMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "supervised", "auto":
		return true
	}
	return false
}
