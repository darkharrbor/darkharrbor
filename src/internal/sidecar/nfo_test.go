package sidecar

import (
	"bytes"
	"encoding/xml"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
)

func TestProviderNFOs(t *testing.T) {
	identity := &mediaidentity.ProviderIdentity{
		Kind: "series", Title: "Same Name",
		IDs:      mediaidentity.ProviderIDs{TVDB: "100", TMDB: "200", IMDB: "tt0000300"},
		Episodes: []mediaidentity.EpisodeProviderIDs{{Season: 1, Episode: 2, TVDB: "400"}},
	}
	series, ok := SeriesNFO(identity)
	if !ok || series.Filename != "tvshow.nfo" {
		t.Fatalf("SeriesNFO = (%+v, %v)", series, ok)
	}
	episode, ok := EpisodeNFO(identity, "Same.Name.S01E02.strm", 1, 2)
	if !ok || episode.Filename != "Same.Name.S01E02.nfo" {
		t.Fatalf("EpisodeNFO = (%+v, %v)", episode, ok)
	}
	for _, body := range [][]byte{series.Bytes, episode.Bytes} {
		if bytes.Contains(body, []byte("://")) || bytes.Contains(bytes.ToLower(body), []byte("tok=")) {
			t.Fatalf("NFO contains prohibited marker: %q", body)
		}
		var doc struct {
			XMLName xml.Name
		}
		if err := xml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("NFO XML invalid: %v", err)
		}
	}
	if !bytes.Contains(series.Bytes, []byte(`<uniqueid type="tvdb" default="true">100</uniqueid>`)) {
		t.Fatalf("series NFO missing provider identity: %s", series.Bytes)
	}
	if !bytes.Contains(episode.Bytes, []byte(`<uniqueid type="tvdb" default="true">400</uniqueid>`)) {
		t.Fatalf("episode NFO missing episode identity: %s", episode.Bytes)
	}
	if _, ok := EpisodeNFO(identity, "Same.Name.S01E03.strm", 1, 3); ok {
		t.Fatal("EpisodeNFO guessed an unknown episode")
	}
}
