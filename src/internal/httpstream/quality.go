package httpstream

import "strings"

// Torznab presentation helpers (HS-1.10, D2, D17).

// HTTPTitleToken is the stable release-title token that marks HTTP-lane
// results; the wizard installs a negative-score custom format matching it
// across all arr profiles (D2, HS-5.3).
const HTTPTitleToken = "DH-HTTP"

// TagTitle appends the DH-HTTP token to a release title exactly once.
func TagTitle(title string) string {
	if strings.Contains(title, HTTPTitleToken) {
		return title
	}
	return strings.TrimRight(title, ". -_") + "." + HTTPTitleToken
}

// QualityToRes maps a backend quality label to a presentation resolution for
// WEBDL-{res} rendering (D17). ok=false means skip/unranked — never guess.
func QualityToRes(q string) (res string, ok bool) {
	switch strings.ToUpper(strings.TrimSpace(q)) {
	case "8K", "4320P":
		return "2160p", true // presented capped at 2160p per D17
	case "4K", "2160P", "UHD":
		return "2160p", true
	case "QHD", "1440P":
		return "1440p", true
	case "FHD", "1080P":
		return "1080p", true
	case "HD", "720P":
		return "720p", true
	case "SD", "480P":
		return "480p", true
	default:
		// "Auto" and unknown labels are never guessed (D17).
		return "", false
	}
}

// SizeEstimate returns the deterministic search-time size (bytes) used when
// a backend provides no size (OMSS never does — GPT §2.4). The grab-time
// bytes=0-0 preflight supplies the truth before HEAD/import. Documented
// table; values chosen inside typical arr per-quality size windows.
func SizeEstimate(kind, res string) int64 {
	const gib = int64(1) << 30
	episode := strings.EqualFold(kind, "episode") || strings.EqualFold(kind, "tv")
	switch res {
	case "2160p":
		if episode {
			return 6 * gib
		}
		return 20 * gib
	case "1440p":
		if episode {
			return 3 * gib
		}
		return 10 * gib
	case "1080p":
		if episode {
			return 2 * gib
		}
		return 8 * gib
	case "720p":
		if episode {
			return 1 * gib
		}
		return 4 * gib
	case "480p":
		if episode {
			return 500 << 20
		}
		return 2 * gib
	default:
		if episode {
			return 1 * gib
		}
		return 4 * gib
	}
}
