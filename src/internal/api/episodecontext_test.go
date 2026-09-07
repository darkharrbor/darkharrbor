package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

type episodeContextCapture struct {
	query httpstream.StreamQuery
}

func (*episodeContextCapture) Name() string { return "capture" }
func (h *episodeContextCapture) Search(_ context.Context, query httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	h.query = query
	return nil, nil
}
func (*episodeContextCapture) Resolve(context.Context, httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	return nil, nil
}

func TestEpisodeContextFromOwningSonarr(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			t.Fatal("missing transient Arr authentication")
		}
		_, _ = w.Write([]byte(`[{"id":33,"tvdbId":341164,"title":"Yellowstone (2018)"}]`))
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("seriesId") != "33" {
			t.Fatalf("seriesId = %q", r.URL.Query().Get("seriesId"))
		}
		_, _ = w.Write([]byte(`[
			{"seasonNumber":5,"episodeNumber":7,"absoluteEpisodeNumber":46,"airDate":"2022-11-20","title":"The Dream Is Not Me"},
			{"seasonNumber":5,"episodeNumber":8,"absoluteEpisodeNumber":47,"airDate":"2022-11-27","title":"A Knife and No Coin"}
		]`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	cfg := &config.Config{Arrs: []config.ArrTarget{{Name: "sonarr", BaseURL: ts.URL, APIKey: "secret"}}}
	s := &Server{cfg: cfg, arrMetadataClient: ts.Client()}
	title, episodes := s.episodeContext(context.Background(), "341164")
	if title != "Yellowstone (2018)" || len(episodes) != 2 {
		t.Fatalf("context = %q %+v", title, episodes)
	}
	if episodes[0].Absolute != 46 || episodes[0].AirDate != "2022-11-20" || episodes[0].Title != "The Dream Is Not Me" {
		t.Fatalf("episode facts = %+v", episodes[0])
	}
}

func TestHTTPStreamSearchSuppliesAuthoritativeEpisodeContext(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":33,"tvdbId":341164,"title":"Yellowstone (2018)"}]`))
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"seasonNumber":5,"episodeNumber":7,"absoluteEpisodeNumber":46,"airDate":"2022-11-20","title":"The Dream Is Not Me"}]`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.cfg.Arrs = []config.ArrTarget{{BaseURL: ts.URL, APIKey: "secret"}}
	s.arrMetadataClient = ts.Client()
	s.httpHandlers = httpstream.NewRegistry()
	capture := &episodeContextCapture{}
	s.httpHandlers.Register("capture", 0, capture)
	s.httpHandlers.Start(context.Background(), nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/torznab/http-stream?t=tvsearch&tvdbid=341164&season=5&ep=7", nil)
	s.handleTorznabHTTPStream(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if capture.query.Title != "Yellowstone (2018)" || len(capture.query.Episodes) != 1 {
		t.Fatalf("captured query = %+v", capture.query)
	}
	if got := capture.query.Episodes[0]; got.Absolute != 46 || got.Title != "The Dream Is Not Me" {
		t.Fatalf("captured episode = %+v", got)
	}
}

func TestEpisodeContextBoundsAndCancellation(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[{"id":1,"tvdbId":2,"title":"Show"}]`))
		})
		mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("[" + strings.Repeat(`{"seasonNumber":1,"episodeNumber":1},`, 2048) + `{"seasonNumber":1,"episodeNumber":1}]`))
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		s := &Server{cfg: &config.Config{Arrs: []config.ArrTarget{{BaseURL: ts.URL, APIKey: "x"}}}, arrMetadataClient: ts.Client()}
		title, episodes := s.episodeContext(context.Background(), "2")
		if title != "Show" || episodes != nil {
			t.Fatalf("oversized context = %q len=%d", title, len(episodes))
		}
	})

	t.Run("cancel", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		t.Cleanup(ts.Close)
		s := &Server{cfg: &config.Config{Arrs: []config.ArrTarget{{BaseURL: ts.URL, APIKey: "x"}}}, arrMetadataClient: ts.Client()}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if title, episodes := s.episodeContext(ctx, "2"); title != "" || episodes != nil {
			t.Fatalf("cancelled context = %q %+v", title, episodes)
		}
	})
}

func TestGetArrJSONRejectsRedirectWithoutForwardingKey(t *testing.T) {
	forwarded := make(chan string, 1)
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded <- r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(dst.Close)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL, http.StatusFound)
	}))
	t.Cleanup(src.Close)

	client := src.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	s := &Server{arrMetadataClient: client}
	var out []sonarrSeries
	if err := s.getArrJSON(context.Background(), src.URL, "secret", &out); err == nil {
		t.Fatal("redirect unexpectedly accepted")
	}
	select {
	case key := <-forwarded:
		t.Fatalf("redirect followed; key=%q", key)
	default:
	}
}

func TestGetArrJSONRejectsOversizedBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `["`+strings.Repeat("x", maxArrMetadataBytes)+`"]`)
	}))
	t.Cleanup(ts.Close)
	s := &Server{arrMetadataClient: ts.Client()}
	var out []string
	if err := s.getArrJSON(context.Background(), ts.URL, "secret", &out); err == nil {
		t.Fatal("oversized body unexpectedly accepted")
	}
}
