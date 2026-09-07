package api

import (
	"testing"
	"time"
)

// TestRX95BatchBindingAbstainsOnAmbiguity is RX-9.5's regression. The binding
// must reproduce the CAPTURED 2026-08-18 shape: the addon is queried more than
// once for one resolution, then a single batch arrives seconds later carrying a
// metadata_id on only some of its URLs. It must bind the whole batch, stay
// idempotent across the repeat, and ABSTAIN whenever the identity is genuinely
// unknown rather than guess -- a wrong identity commits the wrong file.
func TestRX95BatchBindingAbstainsOnAmbiguity(t *testing.T) {
	base := time.Date(2026, 8, 18, 11, 30, 10, 0, time.UTC)
	movie := rx95Identity{Kind: "movie", IMDb: "tt0093629"}
	other := rx95Identity{Kind: "movie", IMDb: "tt0111161"}
	const metaID = "4358396fbaa06c7ecf3f488bf9ff5e18b1a73350d8cdc2b30ebb930a84990618"

	t.Run("repeat observation is idempotent and binds the batch", func(t *testing.T) {
		s := newRX95Store()
		s.Observe(movie, base)
		s.Observe(movie, base.Add(105*time.Millisecond)) // the captured duplicate
		got, ok := s.Resolve(metaID, base.Add(9*time.Second+700*time.Millisecond))
		if !ok || got != movie {
			t.Fatalf("duplicate query must not read as ambiguity: got=%+v ok=%v", got, ok)
		}
	})

	t.Run("batch with no metadata_id still binds", func(t *testing.T) {
		// 56 of 92 captured URLs were external addon links carrying none.
		s := newRX95Store()
		s.Observe(movie, base)
		if got, ok := s.Resolve("", base.Add(time.Second)); !ok || got != movie {
			t.Fatalf("external-link batch must bind per batch: got=%+v ok=%v", got, ok)
		}
	})

	t.Run("concurrent resolutions bind nothing", func(t *testing.T) {
		s := newRX95Store()
		s.Observe(movie, base)
		s.Observe(other, base.Add(time.Second))
		if _, ok := s.Resolve("", base.Add(2*time.Second)); ok {
			t.Fatal("two identities in flight must abstain, not guess")
		}
	})

	t.Run("learned binding survives later ambiguity", func(t *testing.T) {
		s := newRX95Store()
		s.Observe(movie, base)
		if _, ok := s.Resolve(metaID, base.Add(time.Second)); !ok {
			t.Fatal("first resolve should bind")
		}
		s.Observe(other, base.Add(2*time.Second))
		got, ok := s.Resolve(metaID, base.Add(3*time.Second))
		if !ok || got != movie {
			t.Fatalf("known metadata_id must win over an ambiguous window: got=%+v ok=%v", got, ok)
		}
	})

	t.Run("stale observation expires", func(t *testing.T) {
		s := newRX95Store()
		s.Observe(movie, base)
		if _, ok := s.Resolve("", base.Add(rx95ObservationWindow+time.Second)); ok {
			t.Fatal("an expired observation must not bind a later unrelated batch")
		}
	})

	t.Run("no observation binds nothing", func(t *testing.T) {
		s := newRX95Store()
		if _, ok := s.Resolve(metaID, base); ok {
			t.Fatal("unseen metadata_id with no observation must abstain")
		}
	})

	t.Run("invalid coordinates are never observed", func(t *testing.T) {
		s := newRX95Store()
		s.Observe(rx95Identity{Kind: "movie", IMDb: "notanid"}, base)
		s.Observe(rx95Identity{Kind: "series", IMDb: "tt0093629", Season: 1}, base) // episode 0
		s.Observe(rx95Identity{Kind: "collection", IMDb: "tt0093629"}, base)
		if _, ok := s.Resolve("", base); ok {
			t.Fatal("malformed identities must not bind")
		}
	})
}

// TestRX95FilenameOnlyContradicts enforces the standing rule that a release
// title may never ESTABLISH an identity and may only disagree with one.
func TestRX95FilenameOnlyContradicts(t *testing.T) {
	episode := rx95Identity{Kind: "series", IMDb: "tt0070991", Season: 3, Episode: 17}
	movie := rx95Identity{Kind: "movie", IMDb: "tt0093629"}

	if rx95Contradicted(episode, "Good.Times.S03E17.1080p.WEBDL.mkv") {
		t.Fatal("a matching coordinate must not contradict")
	}
	if !rx95Contradicted(episode, "Good.Times.S05E02.1080p.WEBDL.mkv") {
		t.Fatal("a disagreeing coordinate must contradict")
	}
	// Unparseable, and movies, must ABSTAIN rather than contradict: silence is
	// not disagreement.
	if rx95Contradicted(episode, "some.release.without.coordinates.mkv") {
		t.Fatal("an unparseable title must not contradict")
	}
	if rx95Contradicted(movie, "A.Nightmare.on.Elm.Street.3.S03E17.mkv") {
		t.Fatal("a movie identity must never be contradicted by a filename")
	}
}

// TestRX95BatchMetadataIDRequiresAgreement covers the corroboration input: the
// captured batch carried one identical value across all 36 owned URLs, so
// disagreement means the batch is not one coherent resolution.
func TestRX95BatchMetadataIDRequiresAgreement(t *testing.T) {
	owned := func(meta string) mediaFlowURLRequest {
		return mediaFlowURLRequest{
			DestinationURL: "http" + "://aio.example.test" + mediaFlowAIOPlaybackPrefix +
				"store-auth/-/descriptor/" + meta + "/file.mkv",
		}
	}
	external := mediaFlowURLRequest{DestinationURL: "http" + "://comet.example.test/playback/abc/file.mkv"}

	if got := mediaFlowBatchMetadataID([]mediaFlowURLRequest{owned("aaa"), owned("aaa"), external}); got != "aaa" {
		t.Fatalf("agreeing owned URLs must yield their shared value, got %q", got)
	}
	if got := mediaFlowBatchMetadataID([]mediaFlowURLRequest{owned("aaa"), owned("bbb")}); got != "" {
		t.Fatalf("disagreeing values must corroborate nothing, got %q", got)
	}
	if got := mediaFlowBatchMetadataID([]mediaFlowURLRequest{external}); got != "" {
		t.Fatalf("external-only batch must yield no value, got %q", got)
	}
}
