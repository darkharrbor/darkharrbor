package mediaidentity

import "testing"

func TestValidate(t *testing.T) {
	valid := &ProviderIdentity{
		Kind: "series", Title: "Collision-safe show",
		IDs:            ProviderIDs{TVDB: "123", TMDB: "456", IMDB: "tt0000789"},
		Episodes:       []EpisodeProviderIDs{{Season: 1, Episode: 2, TVDB: "987", RuntimeMinutes: 42}},
		Year:           2019,
		RuntimeMinutes: 45,
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("Validate(valid): %v", err)
	}
	if err := Validate(&ProviderIdentity{Kind: "movie", Title: "No optional fields set"}); err != nil {
		t.Fatalf("Validate(zero-value year/runtime): %v", err)
	}
	for _, invalid := range []*ProviderIdentity{
		{Kind: "other", IDs: ProviderIDs{TVDB: "1"}},
		{Kind: "series", IDs: ProviderIDs{TVDB: "https://example.invalid"}},
		{Kind: "series", IDs: ProviderIDs{IMDB: "tt-not-numeric"}},
		{Kind: "series", Episodes: []EpisodeProviderIDs{{Season: -1, Episode: 1, TVDB: "1"}}},
		{Kind: "movie", Year: -5},
		{Kind: "movie", Year: 1},
		{Kind: "movie", Year: 3000},
		{Kind: "movie", RuntimeMinutes: -1},
		{Kind: "movie", RuntimeMinutes: 601},
		{Kind: "series", Episodes: []EpisodeProviderIDs{{Season: 1, Episode: 1, RuntimeMinutes: -1}}},
		{Kind: "series", Episodes: []EpisodeProviderIDs{{Season: 1, Episode: 1, RuntimeMinutes: 601}}},
	} {
		if err := Validate(invalid); err == nil {
			t.Fatalf("Validate(%+v) succeeded", invalid)
		}
	}
}

func FuzzValidateProviderIdentity(f *testing.F) {
	f.Add("series", "Show", "123", "456", "tt0000789", 1, 2, "987", 2019, 45, 42)
	f.Add("movie", "Movie", "", "42", "tt0111161", 0, 0, "", 1994, 142, 0)
	f.Fuzz(func(t *testing.T, kind, title, tvdb, tmdb, imdb string, season, episode int, episodeTVDB string,
		year, runtimeMinutes, episodeRuntimeMinutes int,
	) {
		identity := &ProviderIdentity{
			Kind: kind, Title: title, IDs: ProviderIDs{TVDB: tvdb, TMDB: tmdb, IMDB: imdb},
			Episodes: []EpisodeProviderIDs{{
				Season: season, Episode: episode, TVDB: episodeTVDB,
				RuntimeMinutes: episodeRuntimeMinutes,
			}},
			Year:           year,
			RuntimeMinutes: runtimeMinutes,
		}
		_ = Validate(identity)
	})
}
