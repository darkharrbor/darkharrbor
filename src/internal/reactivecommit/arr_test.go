package reactivecommit

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestArrClientCommitsExactMultiEpisodeCandidate(t *testing.T) {
	var mu sync.Mutex
	imported := false
	monitored := map[int]bool{101: false, 102: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]any{map[string]any{"id": 10, "monitored": false}})
	})
	mux.HandleFunc("/api/v3/manualimport", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]any{map[string]any{
			"path": "/mnt/darkharrbor/tv/show.strm", "series": map[string]any{"id": 10},
			"episodes": []any{map[string]any{"id": 101, "seasonNumber": 1, "episodeNumber": 1}, map[string]any{"id": 102, "seasonNumber": 1, "episodeNumber": 2}},
			"quality":  map[string]any{"quality": map[string]any{"id": 1}}, "languages": []any{},
		}})
	})
	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["name"] != "ManualImport" || body["importMode"] != "copy" {
			t.Errorf("unexpected command shape")
		}
		mu.Lock()
		imported = true
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 7})
	})
	mux.HandleFunc("/api/v3/command/7", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "result": "successful"})
	})
	for _, id := range []string{"101", "102"} {
		id := id
		mux.HandleFunc("/api/v3/episode/"+id, func(w http.ResponseWriter, r *http.Request) {
			epID := 101
			if id == "102" {
				epID = 102
			}
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodPut {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				monitored[epID], _ = body["monitored"].(bool)
				w.WriteHeader(http.StatusAccepted)
				json.NewEncoder(w).Encode(body)
				return
			}
			fileID := 0
			if imported {
				fileID = 55
			}
			json.NewEncoder(w).Encode(map[string]any{"id": epID, "monitored": monitored[epID], "hasFile": imported, "episodeFileId": fileID})
		})
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewArrClient([]config.ArrTarget{{Name: "classic", BaseURL: server.URL, APIKey: "secret"}})
	identity := &store.ProviderIdentity{Kind: "series", IDs: store.ProviderIDs{TVDB: "100"}, Episodes: []store.EpisodeProviderIDs{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}}
	got, err := client.Commit(context.Background(), CommitRequest{Identity: identity, SourcePath: "/data/tv/show.strm", MonitorEpisodes: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.ArrName != "classic" || got.ItemID != 10 || len(got.FileIDs) != 1 || got.FileIDs[0] != 55 || len(got.Episodes) != 2 {
		t.Fatalf("registration=%+v", got)
	}
	if !monitored[101] || !monitored[102] {
		t.Fatalf("monitoring=%v", monitored)
	}
}

func TestArrClientRescansOnlyRecordedOwner(t *testing.T) {
	var commandName string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		commandName, _ = body["name"].(string)
		if body["movieId"] != float64(41) {
			t.Errorf("owner id=%v", body["movieId"])
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9})
	})
	mux.HandleFunc("/api/v3/command/9", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "result": "successful"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewArrClient([]config.ArrTarget{{Name: "classic", BaseURL: server.URL, APIKey: "secret"}})
	if err := client.Rescan(t.Context(), store.ReactiveCommit{ArrName: "classic", ArrKind: "movie", ArrItemID: 41}); err != nil {
		t.Fatal(err)
	}
	if commandName != "RescanMovie" {
		t.Fatalf("command=%q", commandName)
	}
	if err := client.Rescan(t.Context(), store.ReactiveCommit{ArrName: "other", ArrKind: "movie", ArrItemID: 41}); err == nil {
		t.Fatal("unknown owner accepted")
	}
}

func TestArrClientMaterializesThroughRecordedMovieOwner(t *testing.T) {
	var imported, rescanned bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/manualimport", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"path": "/mnt/darkharrbor/movie.mkv", "quality": map[string]any{}, "languages": []any{},
		}})
	})
	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] == "ManualImport" {
			files, _ := body["files"].([]any)
			file, _ := files[0].(map[string]any)
			if file["movieId"] != float64(41) {
				t.Errorf("movie owner=%v", file["movieId"])
			}
			imported = true
		} else if body["name"] == "RescanMovie" {
			rescanned = true
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9})
	})
	mux.HandleFunc("/api/v3/command/9", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "result": "successful"})
	})
	mux.HandleFunc("/api/v3/movie/41", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"hasFile": true, "movieFileId": 55})
	})
	mux.HandleFunc("/api/v3/moviefile/55", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"size": 1024, "relativePath": "movie.mkv"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewArrClient([]config.ArrTarget{{Name: "radarr", BaseURL: server.URL, APIKey: "secret"}})
	if err := client.Materialize(t.Context(), store.ReactiveCommit{ArrName: "radarr", ArrKind: "movie", ArrItemID: 41}, "/data/movie.mkv", 1024); err != nil {
		t.Fatal(err)
	}
	if !imported || !rescanned {
		t.Fatalf("imported=%t rescanned=%t", imported, rescanned)
	}
}

func TestArrClientMaterializeFailsClosedOnProxyRegistration(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/manualimport", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"path": "/mnt/darkharrbor/movie.mkv", "quality": map[string]any{}, "languages": []any{}}})
	})
	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9})
	})
	mux.HandleFunc("/api/v3/command/9", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "result": "successful"})
	})
	mux.HandleFunc("/api/v3/movie/41", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"hasFile": true, "movieFileId": 55})
	})
	mux.HandleFunc("/api/v3/moviefile/55", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"size": 115, "relativePath": "movie.strm"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewArrClient([]config.ArrTarget{{Name: "radarr", BaseURL: server.URL, APIKey: "secret"}})
	if err := client.Materialize(t.Context(), store.ReactiveCommit{ArrName: "radarr", ArrKind: "movie", ArrItemID: 41}, "/data/movie.mkv", 1024); err == nil {
		t.Fatal("proxy registration accepted as materialized")
	}
}

func TestArrClientCreatesAndRemovesMovieOwner(t *testing.T) {
	var ownerExists, imported bool
	var fileDeleted, ownerDeleted, monitoringUpdated bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if ownerExists {
				json.NewEncoder(w).Encode([]any{map[string]any{"id": 41, "monitored": true}})
			} else {
				json.NewEncoder(w).Encode([]any{})
			}
			return
		}
		ownerExists = true
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 41, "monitored": true})
	})
	mux.HandleFunc("/api/v3/movie/lookup/tmdb", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"title": "candidate", "tmdbId": 99})
	})
	mux.HandleFunc("/api/v3/manualimport", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]any{map[string]any{
			"path": "/mnt/darkharrbor/movies/candidate.strm", "movie": map[string]any{"id": 41},
			"quality": map[string]any{"quality": map[string]any{"id": 1}}, "languages": []any{},
		}})
	})
	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		imported = true
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 7})
	})
	mux.HandleFunc("/api/v3/command/7", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "result": "successful"})
	})
	mux.HandleFunc("/api/v3/movie/41", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			if r.URL.Query().Get("deleteFiles") != "false" || r.URL.Query().Get("addImportExclusion") != "false" {
				t.Errorf("unsafe owner-delete query: %s", r.URL.RawQuery)
			}
			ownerDeleted, ownerExists = true, false
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPut:
			monitoringUpdated = true
			w.WriteHeader(http.StatusAccepted)
		default:
			json.NewEncoder(w).Encode(map[string]any{"id": 41, "monitored": true, "hasFile": imported, "movieFileId": 73})
		}
	})
	mux.HandleFunc("/api/v3/moviefile/73", func(w http.ResponseWriter, r *http.Request) {
		fileDeleted = true
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewArrClient([]config.ArrTarget{{Name: "classic", BaseURL: server.URL, APIKey: "secret"}})
	identity := &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "99"}}
	registration, err := client.Commit(context.Background(), CommitRequest{
		Identity: identity, SourcePath: "/data/movies/candidate.strm", TargetName: "classic",
		RootFolder: "/movies", QualityProfile: 11, MonitorMovie: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !registration.OwnerCreated || registration.ItemID != 41 || len(registration.FileIDs) != 1 || registration.FileIDs[0] != 73 {
		t.Fatalf("registration=%+v", registration)
	}
	if err := client.Undo(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	if !fileDeleted || !ownerDeleted || ownerExists || monitoringUpdated {
		t.Fatalf("fileDeleted=%v ownerDeleted=%v ownerExists=%v monitoringUpdated=%v", fileDeleted, ownerDeleted, ownerExists, monitoringUpdated)
	}
}

func TestArrClientRemovesCreatedOwnerAfterImportFailure(t *testing.T) {
	var ownerDeleted bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode([]any{})
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 42})
	})
	mux.HandleFunc("/api/v3/movie/lookup/tmdb", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"title": "candidate", "tmdbId": 100})
	})
	mux.HandleFunc("/api/v3/manualimport", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/api/v3/movie/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method=%s", r.Method)
		}
		ownerDeleted = true
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewArrClient([]config.ArrTarget{{Name: "classic", BaseURL: server.URL, APIKey: "secret"}})
	_, err := client.Commit(context.Background(), CommitRequest{
		Identity:   &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "100"}},
		SourcePath: "/data/movies/candidate.strm", TargetName: "classic", RootFolder: "/movies", QualityProfile: 11,
	})
	if err == nil || !ownerDeleted {
		t.Fatalf("err=%v ownerDeleted=%v", err, ownerDeleted)
	}
}

func TestArrVisiblePathFailsClosed(t *testing.T) {
	for _, path := range []string{"relative.strm", "/tmp/out.strm", "/data/../private.strm"} {
		if _, err := arrVisiblePath(path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	got, err := arrVisiblePath("/data/tv/show.strm")
	if err != nil || got != "/mnt/darkharrbor/tv/show.strm" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func FuzzParseManualCandidates(f *testing.F) {
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"path":"/mnt/darkharrbor/a.strm","series":{"id":1},"episodes":[{"id":2,"seasonNumber":1,"episodeNumber":1}]}]`))
	f.Add([]byte(`{"not":"an array"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		candidates, err := parseManualCandidates(strings.NewReader(string(data)))
		if err == nil && len(candidates) > maxArrCandidates {
			t.Fatalf("unbounded candidates")
		}
	})
}

func TestManualCandidateParserRejectsOversizeAndTrailingInput(t *testing.T) {
	if _, err := parseManualCandidates(bytes.NewReader(bytes.Repeat([]byte("x"), maxArrBody+1))); err == nil {
		t.Fatal("oversized response accepted")
	}
	if _, err := parseManualCandidates(strings.NewReader("[] []")); err == nil {
		t.Fatal("trailing response accepted")
	}
}

func TestSleepContextCancelsWithoutGoroutine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepContext(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation was not prompt")
	}
}
