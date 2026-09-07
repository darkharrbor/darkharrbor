package reactivecommit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

type monitorFlipArr struct {
	mu        sync.Mutex
	monitored map[int]bool
	searches  []int
}

func (a *monitorFlipArr) handler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.URL.Path == "/api/v3/command" && r.Method == http.MethodPost {
		var body struct {
			Name       string `json:"name"`
			EpisodeIDs []int  `json:"episodeIds"`
			MovieIDs   []int  `json:"movieIds"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ids := body.EpisodeIDs
		if body.Name == "MoviesSearch" {
			ids = body.MovieIDs
		}
		if len(ids) != 1 || (body.Name != "EpisodeSearch" && body.Name != "MoviesSearch") {
			http.Error(w, "bad command", http.StatusBadRequest)
			return
		}
		a.searches = append(a.searches, ids[0])
		w.WriteHeader(http.StatusCreated)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "v3" || (parts[2] != "episode" && parts[2] != "movie") {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.Atoi(parts[3])
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "monitored": a.monitored[id]})
	case http.MethodPut:
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		value, ok := body["monitored"].(bool)
		if !ok {
			http.Error(w, "missing monitored", http.StatusBadRequest)
			return
		}
		a.monitored[id] = value
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestPacedMonitorFlipBatchesAndSearches(t *testing.T) {
	arr := &monitorFlipArr{monitored: map[int]bool{}}
	server := httptest.NewServer(http.HandlerFunc(arr.handler))
	t.Cleanup(server.Close)
	client := NewArrClient([]config.ArrTarget{{Name: "classic", BaseURL: server.URL, APIKey: "secret"}})
	var sleeps []time.Duration
	client.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	result, err := client.PacedMonitorFlip(context.Background(), MonitorFlipRequest{
		TargetName: "classic", Kind: "episode", IDs: []int{1, 2, 3, 4, 5}, Rate: 2, Interval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Flipped != 5 || result.Searched != 5 || len(sleeps) != 2 || sleeps[0] != time.Minute || sleeps[1] != time.Minute {
		t.Fatalf("result=%+v sleeps=%v", result, sleeps)
	}
	arr.mu.Lock()
	defer arr.mu.Unlock()
	if len(arr.searches) != 5 {
		t.Fatalf("searches=%v", arr.searches)
	}
	for _, id := range []int{1, 2, 3, 4, 5} {
		if !arr.monitored[id] {
			t.Fatalf("id %d not monitored", id)
		}
	}
}

func TestPacedMonitorFlipCancellationLeavesRemainderUntouched(t *testing.T) {
	arr := &monitorFlipArr{monitored: map[int]bool{}}
	server := httptest.NewServer(http.HandlerFunc(arr.handler))
	t.Cleanup(server.Close)
	client := NewArrClient([]config.ArrTarget{{Name: "classic", BaseURL: server.URL}})
	ctx, cancel := context.WithCancel(context.Background())
	client.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	result, err := client.PacedMonitorFlip(ctx, MonitorFlipRequest{
		TargetName: "classic", Kind: "episode", IDs: []int{1, 2, 3, 4}, Rate: 2, Interval: time.Second,
	})
	if err == nil || result.Flipped != 2 || result.Searched != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	arr.mu.Lock()
	defer arr.mu.Unlock()
	if !arr.monitored[1] || !arr.monitored[2] || arr.monitored[3] || arr.monitored[4] || len(arr.searches) != 2 {
		t.Fatalf("monitored=%v searches=%v", arr.monitored, arr.searches)
	}
}

func TestPacedMonitorFlipPreflightFailsClosed(t *testing.T) {
	arr := &monitorFlipArr{monitored: map[int]bool{2: true}}
	server := httptest.NewServer(http.HandlerFunc(arr.handler))
	t.Cleanup(server.Close)
	client := NewArrClient([]config.ArrTarget{{Name: "classic", BaseURL: server.URL}})
	result, err := client.PacedMonitorFlip(context.Background(), MonitorFlipRequest{
		TargetName: "classic", Kind: "episode", IDs: []int{1, 2, 3}, Rate: 1, Interval: time.Second,
	})
	if err == nil || result != (MonitorFlipResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	arr.mu.Lock()
	defer arr.mu.Unlock()
	if arr.monitored[1] || arr.monitored[3] || len(arr.searches) != 0 {
		t.Fatalf("preflight mutated state: monitored=%v searches=%v", arr.monitored, arr.searches)
	}
}

func TestPacedMonitorFlipMovieUsesNativeSearchCommand(t *testing.T) {
	arr := &monitorFlipArr{monitored: map[int]bool{}}
	server := httptest.NewServer(http.HandlerFunc(arr.handler))
	t.Cleanup(server.Close)
	client := NewArrClient([]config.ArrTarget{{Name: "radarr", BaseURL: server.URL}})
	result, err := client.PacedMonitorFlip(context.Background(), MonitorFlipRequest{
		TargetName: "radarr", Kind: "movie", IDs: []int{7}, Rate: 1, Interval: time.Second,
	})
	if err != nil || result.Flipped != 1 || result.Searched != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	arr.mu.Lock()
	defer arr.mu.Unlock()
	if !arr.monitored[7] || len(arr.searches) != 1 || arr.searches[0] != 7 {
		t.Fatalf("monitored=%v searches=%v", arr.monitored, arr.searches)
	}
}

func TestPacedMonitorFlipRejectsMalformedRequest(t *testing.T) {
	client := NewArrClient(nil)
	for _, req := range []MonitorFlipRequest{
		{},
		{TargetName: "x", Kind: "series", IDs: []int{1}, Rate: 1, Interval: time.Second},
		{TargetName: "x", Kind: "episode", IDs: []int{1, 1}, Rate: 1, Interval: time.Second},
		{TargetName: "x", Kind: "movie", IDs: []int{0}, Rate: 1, Interval: time.Second},
	} {
		if _, err := client.PacedMonitorFlip(context.Background(), req); err == nil {
			t.Fatalf("request accepted: %+v", req)
		}
	}
}
