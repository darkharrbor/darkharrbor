package sidecar

import (
	"encoding/xml"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
)

// NFO describes one Jellyfin/Kodi-compatible NFO sibling. Registration and
// persistence still belong to Registry; this type only owns safe XML bytes.
type NFO struct {
	Filename string
	Bytes    []byte
}

type nfoUniqueID struct {
	Type    string `xml:"type,attr"`
	Default string `xml:"default,attr,omitempty"`
	Value   string `xml:",chardata"`
}

type tvShowNFO struct {
	XMLName   xml.Name      `xml:"tvshow"`
	Title     string        `xml:"title,omitempty"`
	TVDBID    string        `xml:"tvdbid,omitempty"`
	TMDBID    string        `xml:"tmdbid,omitempty"`
	IMDBID    string        `xml:"imdb_id,omitempty"`
	UniqueIDs []nfoUniqueID `xml:"uniqueid"`
}

type episodeNFO struct {
	XMLName   xml.Name      `xml:"episodedetails"`
	ShowTitle string        `xml:"showtitle,omitempty"`
	Season    int           `xml:"season"`
	Episode   int           `xml:"episode"`
	UniqueIDs []nfoUniqueID `xml:"uniqueid"`
}

// SeriesNFO returns tvshow.nfo for a provider-identified series.
func SeriesNFO(identity *mediaidentity.ProviderIdentity) (NFO, bool) {
	if identity == nil || identity.Kind != "series" {
		return NFO{}, false
	}
	ids := mediaidentity.NormalizedIDs(identity.IDs)
	if ids == (mediaidentity.ProviderIDs{}) {
		return NFO{}, false
	}
	doc := tvShowNFO{
		Title: strings.TrimSpace(identity.Title), TVDBID: ids.TVDB,
		TMDBID: ids.TMDB, IMDBID: ids.IMDB, UniqueIDs: uniqueIDs(ids),
	}
	body, err := xml.Marshal(doc)
	if err != nil {
		return NFO{}, false
	}
	return NFO{Filename: "tvshow.nfo", Bytes: append([]byte(xml.Header), body...)}, true
}

// EpisodeNFO returns a same-basename .nfo for one exact season/episode.
func EpisodeNFO(identity *mediaidentity.ProviderIdentity, basename string, season, episode int) (NFO, bool) {
	if identity == nil || identity.Kind != "series" || season < 0 || episode < 1 {
		return NFO{}, false
	}
	var ids mediaidentity.ProviderIDs
	for _, candidate := range identity.Episodes {
		if candidate.Season == season && candidate.Episode == episode {
			ids = mediaidentity.NormalizedIDs(mediaidentity.ProviderIDs{
				TVDB: candidate.TVDB, TMDB: candidate.TMDB, IMDB: candidate.IMDB,
			})
			break
		}
	}
	if ids == (mediaidentity.ProviderIDs{}) {
		return NFO{}, false
	}
	doc := episodeNFO{
		ShowTitle: strings.TrimSpace(identity.Title), Season: season,
		Episode: episode, UniqueIDs: uniqueIDs(ids),
	}
	body, err := xml.Marshal(doc)
	if err != nil {
		return NFO{}, false
	}
	return NFO{
		Filename: strings.TrimSuffix(basename, ".strm") + ".nfo",
		Bytes:    append([]byte(xml.Header), body...),
	}, true
}

func uniqueIDs(ids mediaidentity.ProviderIDs) []nfoUniqueID {
	values := []struct {
		name, value string
	}{
		{"tvdb", ids.TVDB},
		{"tmdb", ids.TMDB},
		{"imdb", ids.IMDB},
	}
	out := make([]nfoUniqueID, 0, len(values))
	for _, value := range values {
		if value.value == "" {
			continue
		}
		out = append(out, nfoUniqueID{
			Type: value.name, Default: strconv.FormatBool(len(out) == 0), Value: value.value,
		})
	}
	return out
}

// ValidateProviderIdentity rejects malformed or oversized identity context at
// the transport/store boundary. Missing IDs are allowed and become a no-op.
func ValidateProviderIdentity(identity *mediaidentity.ProviderIdentity) error {
	return mediaidentity.Validate(identity)
}
