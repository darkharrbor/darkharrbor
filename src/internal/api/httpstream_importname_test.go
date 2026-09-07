package api

import (
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func stremioEpisodeItem(displayName string, episodes ...mediaidentity.EpisodeProviderIDs) *store.Item {
	return &store.Item{
		DisplayName: displayName,
		Metadata: store.SubmissionMetadata{
			ProviderIdentity: &mediaidentity.ProviderIdentity{
				Kind:     "series",
				Episodes: episodes,
			},
		},
	}
}

func TestStremioEpisodeImportNameUsesExactArrIdentityForPackLabel(t *testing.T) {
	item := stremioEpisodeItem(
		"Example.Show.S08E05.720p.WEBDL.DH-HTTP",
		mediaidentity.EpisodeProviderIDs{Season: 8, Episode: 5},
	)

	got := stremioEpisodeImportName(item, "Example Show Season 8 Complete 720p HDTV")
	if got != item.DisplayName {
		t.Fatalf("stremioEpisodeImportName() = %q, want exact Arr display name %q", got, item.DisplayName)
	}
}

func TestStremioEpisodeImportNamePreservesBackendEpisodeIdentity(t *testing.T) {
	item := stremioEpisodeItem(
		"Example.Show.S08E05.720p.WEBDL.DH-HTTP",
		mediaidentity.EpisodeProviderIDs{Season: 8, Episode: 5},
	)

	for _, resolvedName := range []string{
		"Example.Show.S08E05.Source.Name",
		"Example.Show.S08E06.Source.Name",
	} {
		if got := stremioEpisodeImportName(item, resolvedName); got != resolvedName {
			t.Fatalf("stremioEpisodeImportName(%q) = %q, want backend identity preserved", resolvedName, got)
		}
	}
}

func TestStremioEpisodeImportNameAbstainsWithoutExactArrToken(t *testing.T) {
	item := stremioEpisodeItem(
		"Example Show Season 8",
		mediaidentity.EpisodeProviderIDs{Season: 8, Episode: 5},
	)
	const resolvedName = "Example Show Season 8 Complete 720p HDTV"

	if got := stremioEpisodeImportName(item, resolvedName); got != resolvedName {
		t.Fatalf("stremioEpisodeImportName() = %q, want safe abstain %q", got, resolvedName)
	}
}

func TestStremioEpisodeImportNameAbstainsForMultiEpisodeIdentity(t *testing.T) {
	item := stremioEpisodeItem(
		"Example.Show.S08E05.720p.WEBDL.DH-HTTP",
		mediaidentity.EpisodeProviderIDs{Season: 8, Episode: 5},
		mediaidentity.EpisodeProviderIDs{Season: 8, Episode: 6},
	)
	const resolvedName = "Example Show Season 8 Complete 720p HDTV"

	if got := stremioEpisodeImportName(item, resolvedName); got != resolvedName {
		t.Fatalf("stremioEpisodeImportName() = %q, want safe abstain %q", got, resolvedName)
	}
}
