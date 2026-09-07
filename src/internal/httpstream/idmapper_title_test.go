package httpstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTitleMapper(t *testing.T, h http.HandlerFunc) (*IDMapper, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	m := NewIDMapper(srv.Client(), &memoryMappingStore{}, "secret")
	m.baseURL = srv.URL
	return m, srv
}

func TestTitleForMovieAndTV(t *testing.T) {
	m, srv := newTitleMapper(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/3/movie/") {
			_, _ = w.Write([]byte(`{"title":"National Lampoon's Christmas Vacation","release_date":"1989-11-30"}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"Breaking Bad","first_air_date":"2008-01-20"}`))
	})
	defer srv.Close()

	title, year := m.TitleFor(context.Background(), "5825", false)
	if title != "National Lampoon's Christmas Vacation" || year != 1989 {
		t.Fatalf("movie title = %q %d", title, year)
	}
	title, year = m.TitleFor(context.Background(), "1396", true)
	if title != "Breaking Bad" || year != 2008 {
		t.Fatalf("tv title = %q %d", title, year)
	}
}

func TestTitleForCachesIncludingNegative(t *testing.T) {
	calls := 0
	m, srv := newTitleMapper(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	})
	defer srv.Close()

	for i := 0; i < 3; i++ {
		if title, _ := m.TitleFor(context.Background(), "999", false); title != "" {
			t.Fatalf("expected empty title, got %q", title)
		}
	}
	if calls != 1 {
		t.Fatalf("negative result must be cached, upstream calls = %d", calls)
	}
}

func TestTitleForCacheExpiresOnInjectedClock(t *testing.T) {
	calls := 0
	m, srv := newTitleMapper(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"title":"Alien","release_date":"1979-05-25"}`))
	})
	defer srv.Close()

	base := time.Now().UTC()
	m.now = func() time.Time { return base }
	if title, _ := m.TitleFor(context.Background(), "348", false); title != "Alien" {
		t.Fatalf("unexpected title")
	}
	m.TitleFor(context.Background(), "348", false)
	if calls != 1 {
		t.Fatalf("expected cache hit, calls = %d", calls)
	}
	m.now = func() time.Time { return base.Add(titleCacheTTL + time.Minute) }
	m.TitleFor(context.Background(), "348", false)
	if calls != 2 {
		t.Fatalf("expected refetch after TTL, calls = %d", calls)
	}
}

// TestTitleForNeverFailsSearch pins the best-effort contract: no API key,
// no client, and an empty id all degrade silently to the caller's stub.
func TestTitleForNeverFailsSearch(t *testing.T) {
	m, srv := newTitleMapper(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not call upstream without a usable key/id")
	})
	defer srv.Close()

	noKey := NewIDMapper(srv.Client(), &memoryMappingStore{}, "")
	noKey.baseURL = srv.URL
	if title, year := noKey.TitleFor(context.Background(), "550", false); title != "" || year != 0 {
		t.Fatalf("expected empty on missing key, got %q %d", title, year)
	}
	if title, _ := m.TitleFor(context.Background(), "   ", false); title != "" {
		t.Fatalf("expected empty on blank id, got %q", title)
	}
	var nilMapper *IDMapper
	if title, _ := nilMapper.TitleFor(context.Background(), "550", false); title != "" {
		t.Fatalf("nil mapper must be safe, got %q", title)
	}
}

func TestTitleForMalformedBodyDegradesQuietly(t *testing.T) {
	m, srv := newTitleMapper(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"title":`))
	})
	defer srv.Close()
	if title, year := m.TitleFor(context.Background(), "550", false); title != "" || year != 0 {
		t.Fatalf("expected quiet degrade, got %q %d", title, year)
	}
}
