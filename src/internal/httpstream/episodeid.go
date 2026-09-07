package httpstream

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const (
	MaxEpisodeIdentities  = 2048
	MaxEpisodeNameBytes   = 4096
	episodeMatchThreshold = 65
)

// EpisodeIdentity is one bounded authoritative episode fact supplied by the
// Arr that owns the requested series. It is transient search context only.
type EpisodeIdentity struct {
	Season   int
	Episode  int
	Absolute int
	AirDate  string
	Title    string
}

// EpisodeMatch reports whether candidate identifies target strongly enough.
// Ambiguous means another authoritative episode tied the target's best score;
// callers may surface that fact, but must not silently choose another episode.
type EpisodeMatch struct {
	Matched   bool
	Ambiguous bool
	Score     int
}

var (
	seasonEpisodePattern = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s0*([0-9]{1,3})e0*([0-9]{1,3})(?:[^0-9]|$)`)
	nByMPattern          = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])0*([0-9]{1,3})x0*([0-9]{1,3})(?:[^0-9]|$)`)
	numberPattern        = regexp.MustCompile(`(?:^|[^0-9])([0-9]{1,5})(?:[^0-9]|$)`)
)

// MatchEpisode scores the common episode-name forms named by HR2.8 against a
// bounded authoritative list. Exact numbering outranks air date, absolute
// number, and title evidence; below-threshold evidence is never accepted.
func MatchEpisode(candidate string, target EpisodeIdentity, episodes []EpisodeIdentity) EpisodeMatch {
	if len(candidate) == 0 || len(candidate) > MaxEpisodeNameBytes || len(episodes) == 0 || len(episodes) > MaxEpisodeIdentities {
		return EpisodeMatch{}
	}
	name := normalizeEpisodeName(candidate)
	if name == "" {
		return EpisodeMatch{}
	}

	best := 0
	targetBest := 0
	winners := map[string]struct{}{}
	for _, episode := range episodes {
		score := scoreEpisodeName(name, candidate, episode)
		if score < episodeMatchThreshold {
			continue
		}
		if score > best {
			best = score
			clear(winners)
		}
		if score == best {
			winners[episodeIdentityKey(episode)] = struct{}{}
		}
		if sameEpisode(episode, target) && score > targetBest {
			targetBest = score
		}
	}
	if targetBest == 0 || targetBest != best {
		return EpisodeMatch{}
	}
	return EpisodeMatch{Matched: true, Ambiguous: len(winners) > 1, Score: best}
}

func scoreEpisodeName(normalized, raw string, episode EpisodeIdentity) int {
	for _, match := range seasonEpisodePattern.FindAllStringSubmatch(raw, -1) {
		if parseEpisodeNumber(match[1]) == episode.Season && parseEpisodeNumber(match[2]) == episode.Episode {
			return 100
		}
	}
	for _, match := range nByMPattern.FindAllStringSubmatch(raw, -1) {
		if parseEpisodeNumber(match[1]) == episode.Season && parseEpisodeNumber(match[2]) == episode.Episode {
			return 95
		}
	}
	if date := normalizeEpisodeName(episode.AirDate); date != "" && strings.Contains(" "+normalized+" ", " "+date+" ") {
		return 90
	}
	if episode.Absolute > 0 {
		for _, match := range numberPattern.FindAllStringSubmatch(raw, -1) {
			if parseEpisodeNumber(match[1]) == episode.Absolute {
				return 80
			}
		}
	}
	title := normalizeEpisodeName(episode.Title)
	if title == "" {
		return 0
	}
	if strings.Contains(" "+normalized+" ", " "+title+" ") {
		return 75
	}
	words := meaningfulWords(title)
	if len(words) == 0 {
		return 0
	}
	candidateWords := map[string]bool{}
	for _, word := range strings.Fields(normalized) {
		candidateWords[word] = true
	}
	hits := 0
	for _, word := range words {
		if candidateWords[word] {
			hits++
		}
	}
	if hits*4 >= len(words)*3 {
		return 65
	}
	return 0
}

func normalizeEpisodeName(name string) string {
	name = strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	var out strings.Builder
	out.Grow(len(name))
	space := true
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			space = false
		} else if !space {
			out.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(out.String())
}

func meaningfulWords(title string) []string {
	var out []string
	for _, word := range strings.Fields(title) {
		switch word {
		case "the", "and", "for", "with":
			continue
		}
		if len(word) >= 3 {
			out = append(out, word)
		}
	}
	return out
}

func parseEpisodeNumber(raw string) int {
	n, _ := strconv.Atoi(raw)
	return n
}

func sameEpisode(a, b EpisodeIdentity) bool {
	return a.Season == b.Season && a.Episode == b.Episode
}

func episodeIdentityKey(e EpisodeIdentity) string {
	return strconv.Itoa(e.Season) + ":" + strconv.Itoa(e.Episode)
}
