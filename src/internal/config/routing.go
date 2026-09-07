package config

import "strings"

// Acquisition lane identifiers. The user-set HARRBOR_PREFERENCE is an ordered,
// comma-separated list of these: position = priority, presence = enabled. The
// same list drives BOTH cache-aware selection (which lanes are eligible / the
// cached-vs-uncached cut at search time) and per-grab fulfillment (the ordered
// lane walk). Unknown tokens are ignored.
const (
	PreferenceNone            = "none"                    // explicit provider-lane opt-out
	LaneTorBoxTorrent         = "torbox_torrent"          // TorBox cached torrent (CDN)
	LaneTorBoxNZB             = "torbox_nzb"              // TorBox cached NZB/usenet (CDN)
	LaneNNTPNZB               = "nntp_nzb"                // NNTP streaming (usenet floor)
	LaneUncachedTorrent       = "uncached_torrent"        // TorBox uncached torrent (governor-gated)
	LaneUncachedTorrentDerank = "uncached_torrent_derank" // Uncached torrent, truthfully tagged for Arr deranking
)

func knownLane(s string) bool {
	switch s {
	case LaneTorBoxTorrent, LaneTorBoxNZB, LaneNNTPNZB, LaneUncachedTorrent, LaneUncachedTorrentDerank:
		return true
	}
	return false
}

// parsePreference parses a comma-separated lane list, lower-cased and trimmed,
// dropping duplicates and unknown tokens while preserving order.
func parsePreference(raw string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		lane := strings.ToLower(strings.TrimSpace(part))
		if lane == "" || seen[lane] || !knownLane(lane) {
			continue
		}
		if (lane == LaneUncachedTorrent && seen[LaneUncachedTorrentDerank]) ||
			(lane == LaneUncachedTorrentDerank && seen[LaneUncachedTorrent]) {
			continue
		}
		seen[lane] = true
		out = append(out, lane)
	}
	return out
}

func applyPreference(raw string) (preference []string, explicit bool) {
	if strings.EqualFold(strings.TrimSpace(raw), PreferenceNone) {
		return nil, true
	}
	preference = parsePreference(raw)
	return preference, len(preference) > 0
}

// LaneEnabled reports whether a lane appears in the preference.
func (c *Config) LaneEnabled(lane string) bool {
	for _, l := range c.Routing.Preference {
		if l == lane {
			return true
		}
	}
	return false
}

// LaneRank returns the priority index of a lane (lower = higher priority), or a
// large sentinel when the lane is not enabled.
func (c *Config) LaneRank(lane string) int {
	for i, l := range c.Routing.Preference {
		if l == lane {
			return i
		}
	}
	return 1 << 30
}

// TorrentsEnabled reports whether any torrent lane is enabled.
func (c *Config) TorrentsEnabled() bool {
	return c.LaneEnabled(LaneTorBoxTorrent) ||
		c.LaneEnabled(LaneUncachedTorrent) ||
		c.LaneEnabled(LaneUncachedTorrentDerank)
}

// UncachedTorrentAllowed reports whether uncached torrent adds are permitted.
func (c *Config) UncachedTorrentAllowed() bool {
	return c.LaneEnabled(LaneUncachedTorrent) || c.UncachedTorrentDeranked()
}

// UncachedTorrentDeranked reports whether authoritative cache misses should be
// kept and tagged for the Arr-native derank policy.
func (c *Config) UncachedTorrentDeranked() bool {
	return c.LaneEnabled(LaneUncachedTorrentDerank)
}

// NZBViaTorBox reports whether NZBs may be fulfilled through the TorBox usenet cache.
func (c *Config) NZBViaTorBox() bool { return c.LaneEnabled(LaneTorBoxNZB) }

// NZBViaNNTP reports whether NZBs may be fulfilled through NNTP streaming.
func (c *Config) NZBViaNNTP() bool { return c.LaneEnabled(LaneNNTPNZB) }

// NZBEnabled reports whether any NZB lane is enabled.
func (c *Config) NZBEnabled() bool { return c.NZBViaTorBox() || c.NZBViaNNTP() }

// HTTPStreamEnabled reports whether the HTTP stream provider is enabled
// (HARRBOR_HTTP_STREAM_ENABLED). Independent of the lane preference order —
// HTTP is a fallback lane by contract (D2), not a ranked debrid lane.
func (c *Config) HTTPStreamEnabled() bool { return c.HTTPStream.Enabled }

// TorBoxNZBPreferredOverNNTP reports whether a grabbed NZB should consult the
// TorBox cache before falling to NNTP (torbox_nzb enabled and ranked ahead of
// nntp_nzb, or nntp disabled). When NNTP outranks TorBox, NNTP is the floor and
// TorBox would never be reached, so the TorBox pass is skipped.
func (c *Config) TorBoxNZBPreferredOverNNTP() bool {
	if !c.NZBViaTorBox() {
		return false
	}
	if !c.NZBViaNNTP() {
		return true
	}
	return c.LaneRank(LaneTorBoxNZB) < c.LaneRank(LaneNNTPNZB)
}

// SelectionMode reports whether DH should act as a cache-aware meta-indexer
// (prefilter Prowlarr results before the arr sees them). Enabled only when a
// Prowlarr base URL and API key are configured.
func (c *Config) SelectionMode() bool {
	return strings.TrimSpace(c.Prowlarr.BaseURL) != "" && strings.TrimSpace(c.Prowlarr.APIKey) != ""
}
