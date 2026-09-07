package httpstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

type memoryMappingStore struct {
	row  *store.HTTPIDMapping
	puts int
}

func (s *memoryMappingStore) GetHTTPIDMapping(context.Context, string, string) (*store.HTTPIDMapping, error) {
	return s.row, nil
}
func (s *memoryMappingStore) PutHTTPIDMapping(_ context.Context, m store.HTTPIDMapping) error {
	s.puts++
	copy := m
	s.row = &copy
	return nil
}

func TestIDMapperPositiveMappingAndCache(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.Contains(r.URL.Path, "/find/") {
			if r.URL.Query().Get("external_source") != "tvdb_id" {
				t.Fatal("missing external_source")
			}
			_, _ = w.Write([]byte(`{"tv_results":[{"id":1234}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"imdb_id":"tt0099999"}`))
	}))
	defer srv.Close()

	st := &memoryMappingStore{}
	m := NewIDMapper(srv.Client(), st, "secret")
	m.baseURL = srv.URL
	got, err := m.MapTVDB(context.Background(), "777")
	if err != nil || got.TMDBID != "1234" || got.IMDBID != "tt0099999" {
		t.Fatalf("mapping = %+v, %v", got, err)
	}
	if st.row == nil || st.row.Provenance == "" || st.row.AuthoritativeEmpty {
		t.Fatalf("bad persisted mapping: %+v", st.row)
	}
	_, err = m.MapTVDB(context.Background(), "777")
	if err != nil || calls != 2 {
		t.Fatalf("cache miss: calls=%d err=%v", calls, err)
	}
}

func TestIDMapperAuthoritativeEmptyFiniteTTL(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"tv_results":[]}`))
	}))
	defer srv.Close()

	now := time.Date(2026, 7, 13, 23, 0, 0, 0, time.UTC)
	st := &memoryMappingStore{}
	m := NewIDMapper(srv.Client(), st, "secret")
	m.baseURL = srv.URL
	m.now = func() time.Time { return now }
	_, err := m.MapTVDB(context.Background(), "888")
	if !errors.Is(err, ErrMappingUnavailable) || st.row == nil || !st.row.AuthoritativeEmpty || st.row.ExpiresAt == nil {
		t.Fatalf("negative result not cached correctly: err=%v row=%+v", err, st.row)
	}
	_, _ = m.MapTVDB(context.Background(), "888")
	if calls != 1 {
		t.Fatalf("negative cache not used: calls=%d", calls)
	}
	m.now = func() time.Time { return now.Add(25 * time.Hour) }
	_, _ = m.MapTVDB(context.Background(), "888")
	if calls != 2 {
		t.Fatalf("expired negative cache not refreshed: calls=%d", calls)
	}
}

func TestIDMapperTransientFailureNotCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	st := &memoryMappingStore{}
	m := NewIDMapper(srv.Client(), st, "secret")
	m.baseURL = srv.URL
	_, err := m.MapTVDB(context.Background(), "999")
	if err == nil || st.puts != 0 || !strings.Contains(err.Error(), "retry_after=30") {
		t.Fatalf("transient response mishandled: err=%v puts=%d", err, st.puts)
	}
}

func TestIDMapperRejectsAmbiguousFind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tv_results":[{"id":1},{"id":2}]}`))
	}))
	defer srv.Close()
	m := NewIDMapper(srv.Client(), &memoryMappingStore{}, "secret")
	m.baseURL = srv.URL
	_, err := m.MapTVDB(context.Background(), "1000")
	if !errors.Is(err, ErrMappingAmbiguous) {
		t.Fatalf("expected ambiguous mapping, got %v", err)
	}
}
