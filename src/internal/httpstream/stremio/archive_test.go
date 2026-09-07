package stremio

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

func TestRemoteArchiveDescriptor(t *testing.T) {
	s := streamObject{
		ZipURLs: []string{"https://cdn.example/movie.zip"},
		BehaviorHints: behaviorHints{
			Filename:     "movie.mkv",
			ProxyHeaders: proxyHeaders{Request: map[string]string{"X-Archive-Test": "present"}},
		},
	}
	d, ok := remoteArchiveDescriptor(s)
	if !ok || d.Format != httpstream.RemoteArchiveZIP || d.MemberHint != "movie.mkv" || len(d.Parts) != 1 {
		t.Fatal("valid ZIP descriptor was rejected")
	}
	if d.Parts[0].URL == "" || d.Parts[0].RequestHeaders["X-Archive-Test"] == "" {
		t.Fatal("transient source coordinates were not preserved")
	}
	if _, ok := remoteArchiveDescriptor(streamObject{ZipURLs: s.ZipURLs, RarURLs: []string{"https://cdn.example/movie.rar"}, BehaviorHints: s.BehaviorHints}); ok {
		t.Fatal("contradictory archive formats must abstain")
	}
	credentialURL := (&url.URL{Scheme: "http", Host: "cdn.example", Path: "/movie.zip", User: url.User("x")}).String()
	if _, ok := remoteArchiveDescriptor(streamObject{ZipURLs: []string{credentialURL}, BehaviorHints: s.BehaviorHints}); ok {
		t.Fatal("credential-bearing URL must abstain")
	}
	tooMany := make([]string, maxRemoteArchiveParts+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("https://cdn.example/%d.zip", i)
	}
	if _, ok := remoteArchiveDescriptor(streamObject{ZipURLs: tooMany, BehaviorHints: s.BehaviorHints}); ok {
		t.Fatal("oversized part set must abstain")
	}
}

func TestResolveRemoteArchives(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0000001.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"streams":[{"zipUrls":["https://cdn.example/movie.zip"],"behaviorHints":{"filename":"movie.mkv"}}]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()
	descriptors, err := h.ResolveRemoteArchives(context.Background(), httpstream.ResolveRequest{Key: httpstream.ResolveKey{
		Kind: "movie", IDs: map[string]string{"imdb": "tt0000001"},
	}})
	if err != nil || len(descriptors) != 1 || descriptors[0].Format != httpstream.RemoteArchiveZIP {
		t.Fatalf("resolve archive count=%d err=%v", len(descriptors), err)
	}
}

func TestSearchDispatchesRemoteArchive(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0000002.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"streams":[
			{"zipUrls":["https://cdn.example/movie.zip"],"name":"1080p archive","behaviorHints":{"filename":"movie.mkv","videoSize":1234}},
			{"rarUrls":["https://cdn.example/movie.rar"],"zipUrls":["https://cdn.example/movie.zip"],"behaviorHints":{"filename":"ambiguous.mkv"}}
		]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()
	results, err := h.Search(context.Background(), movieQuery("tt0000002"))
	if err != nil || len(results) != 1 {
		t.Fatalf("archive search count=%d err=%v", len(results), err)
	}
	result := results[0]
	if !httpstream.IsRemoteArchiveSelector(result.Key.Selector) || result.Key.Handler != "stremio" || result.Size != 1234 {
		t.Fatalf("archive result=%+v", result)
	}
}
