package httpstream

import "testing"

func TestMatchEpisodeNotationCorpus(t *testing.T) {
	episodes := []EpisodeIdentity{
		{Season: 1, Episode: 2, Absolute: 2, AirDate: "1983-09-13", Title: "The Beastly Shadow"},
		{Season: 1, Episode: 3, Absolute: 3, AirDate: "1983-09-14", Title: "Disappearing Act"},
	}
	target := episodes[0]
	for _, name := range []string{
		"Show.S01E02.720p.mp4",
		"Show 1x02 The Beastly Shadow.mkv",
		"He-Man 02 - The Beastly Shadow.mp4",
		"Show.1983.09.13.The.Beastly.Shadow.mp4",
		"Show - Beastly Shadow.mp4",
	} {
		if got := MatchEpisode(name, target, episodes); !got.Matched {
			t.Errorf("%q did not match: %+v", name, got)
		}
	}
}

// TestMatchEpisodeRealIACorpus pins public filenames observed in the live IA
// metadata gate. They cover IA originals, generated derivatives, punctuation,
// an exact season/episode form, and an absolute-ordinal-only form.
func TestMatchEpisodeRealIACorpus(t *testing.T) {
	episodes := []EpisodeIdentity{
		{Season: 1, Episode: 1, Absolute: 1, Title: "The Cobra Strikes"},
		{Season: 1, Episode: 2, Absolute: 2, Title: "Slave of the Cobra Master"},
		{Season: 1, Episode: 3, Absolute: 3, Title: "The Worms of Death"},
	}
	target := episodes[1]
	for _, name := range []string{
		`02 'The Cobra Strikes' The M.A.S.S. Device Pt.  2.mkv`,
		`02 'The Cobra Strikes' The M.A.S.S. Device Pt.  2.mp4`,
		`s01e02 The M.A.S.S. Device (2) Slave of the Cobra Master-{edition-HD60}.mkv`,
		`s01e02 The M.A.S.S. Device (2) Slave of the Cobra Master-{edition-HD60}.mp4`,
	} {
		if got := MatchEpisode(name, target, episodes); !got.Matched {
			t.Errorf("%q did not match: %+v", name, got)
		}
	}
}

func TestMatchEpisodeRejectsWrongAndWeakEvidence(t *testing.T) {
	episodes := []EpisodeIdentity{
		{Season: 2, Episode: 7, Absolute: 33, Title: "A Fine Day"},
		{Season: 2, Episode: 8, Absolute: 34, Title: "Another Day"},
	}
	target := episodes[0]
	for _, name := range []string{"Show.S02E08.mp4", "Show.S02E70.mp4", "Show.Day.mp4", "Show.2024.mp4"} {
		if got := MatchEpisode(name, target, episodes); got.Matched {
			t.Errorf("%q unexpectedly matched: %+v", name, got)
		}
	}
}

func TestMatchEpisodeSurfacesAmbiguity(t *testing.T) {
	episodes := []EpisodeIdentity{
		{Season: 1, Episode: 1, Title: "Pilot"},
		{Season: 2, Episode: 1, Title: "Pilot"},
	}
	got := MatchEpisode("Show Pilot.mp4", episodes[0], episodes)
	if !got.Matched || !got.Ambiguous || got.Score != 75 {
		t.Fatalf("ambiguous match = %+v", got)
	}
}

func TestMatchEpisodeBounds(t *testing.T) {
	episodes := make([]EpisodeIdentity, MaxEpisodeIdentities+1)
	if got := MatchEpisode("Show.S01E01.mp4", EpisodeIdentity{Season: 1, Episode: 1}, episodes); got.Matched {
		t.Fatalf("oversized episode list matched: %+v", got)
	}
	if got := MatchEpisode(string(make([]byte, MaxEpisodeNameBytes+1)), EpisodeIdentity{Season: 1, Episode: 1}, []EpisodeIdentity{{Season: 1, Episode: 1}}); got.Matched {
		t.Fatalf("oversized name matched: %+v", got)
	}
}

func FuzzMatchEpisode(f *testing.F) {
	f.Add("Show.S01E02.mp4", 1, 2, 2, "1983-09-13", "The Beastly Shadow")
	f.Add("Show 1x02.mkv", 1, 2, 2, "", "Pilot")
	f.Add("He-Man 02 - The Beastly Shadow.mp4", 1, 2, 2, "", "The Beastly Shadow")
	f.Fuzz(func(t *testing.T, name string, season, episode, absolute int, airDate, title string) {
		if len(name) > MaxEpisodeNameBytes*2 || len(airDate) > 128 || len(title) > 1024 {
			return
		}
		identity := EpisodeIdentity{Season: season, Episode: episode, Absolute: absolute, AirDate: airDate, Title: title}
		got := MatchEpisode(name, identity, []EpisodeIdentity{identity})
		if got.Score < 0 || got.Score > 100 {
			t.Fatalf("score out of bounds: %+v", got)
		}
	})
}
