package store

import (
	"context"
	"testing"
	"time"
)

func TestHTTPIDMappingRoundTrip(t *testing.T) {
	s := newTestStoreCOR(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 13, 23, 0, 0, 0, time.UTC)
	expires := now.Add(24 * time.Hour)
	s.SetClock(func() time.Time { return now })

	if err := s.PutHTTPIDMapping(ctx, HTTPIDMapping{
		SourceNamespace: "tvdb", SourceID: "777", TMDBID: "1234",
		IMDBID: "tt0099999", Provenance: "tmdb.find+external_ids",
	}); err != nil {
		t.Fatalf("put positive: %v", err)
	}
	got, err := s.GetHTTPIDMapping(ctx, "tvdb", "777")
	if err != nil || got == nil || got.TMDBID != "1234" || got.IMDBID != "tt0099999" || got.AuthoritativeEmpty {
		t.Fatalf("positive roundtrip: %+v %v", got, err)
	}

	if err := s.PutHTTPIDMapping(ctx, HTTPIDMapping{
		SourceNamespace: "tvdb", SourceID: "777", AuthoritativeEmpty: true,
		Provenance: "tmdb.find.tvdb_id", ExpiresAt: &expires,
	}); err != nil {
		t.Fatalf("put negative: %v", err)
	}
	got, err = s.GetHTTPIDMapping(ctx, "tvdb", "777")
	if err != nil || got == nil || !got.AuthoritativeEmpty || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("negative roundtrip: %+v %v", got, err)
	}
}
