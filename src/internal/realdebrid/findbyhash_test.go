package realdebrid

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type findByHashClient struct {
	foundID     string
	found       bool
	findErr     error
	findCalls   int
	addCalls    int
	selectCalls int
}

func (f *findByHashClient) GetUser(context.Context) (*AccountInfo, error) {
	return nil, nil
}

func (f *findByHashClient) InstantAvailability(context.Context, []string) (map[string][]CachedGroup, error) {
	return nil, nil
}

func (f *findByHashClient) AddMagnet(context.Context, string) (string, error) {
	f.addCalls++
	return "new-id", nil
}

func (f *findByHashClient) SelectFiles(context.Context, string, string) error {
	f.selectCalls++
	return nil
}

func (f *findByHashClient) GetTorrentInfo(context.Context, string) (*TorrentInfo, error) {
	return nil, nil
}

func (f *findByHashClient) UnrestrictLink(context.Context, string) (string, error) {
	return "", nil
}

func (f *findByHashClient) DeleteTorrent(context.Context, string) error {
	return nil
}

func (f *findByHashClient) ActiveCount(context.Context) (int, error) {
	return 0, nil
}

func (f *findByHashClient) FindByHash(context.Context, string) (string, bool, error) {
	f.findCalls++
	return f.foundID, f.found, f.findErr
}

func (f *findByHashClient) ListTorrents(context.Context, int) ([]TorrentInfo, error) {
	return nil, nil
}

func TestSubmitAddOnlyIfCachedReusesDownloadedHash(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef01234567"
	client := &findByHashClient{foundID: "existing-id", found: true}
	adapter := NewAdapter("realdebrid", client, nil).SetPremium(true)
	response, err := adapter.Submit(t.Context(), &store.Item{
		ID:         "item",
		SourceType: store.SourceTypeTorrent,
		InfoHash:   &hash,
	}, provider.SubmitOptions{AddOnlyIfCached: true})
	if err != nil {
		t.Fatal(err)
	}
	if response.RemoteID != "existing-id" {
		t.Fatalf("RemoteID = %q, want existing-id", response.RemoteID)
	}
	if client.findCalls != 1 || client.addCalls != 0 || client.selectCalls != 0 {
		t.Fatalf("calls find/add/select = %d/%d/%d, want 1/0/0", client.findCalls, client.addCalls, client.selectCalls)
	}
}

func TestSubmitNormalPathDoesNotListExistingHashes(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef01234567"
	client := &findByHashClient{foundID: "existing-id", found: true}
	adapter := NewAdapter("realdebrid", client, nil).SetPremium(true)
	response, err := adapter.Submit(t.Context(), &store.Item{
		ID:         "item",
		SourceType: store.SourceTypeTorrent,
		InfoHash:   &hash,
	}, provider.SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if response.RemoteID != "new-id" {
		t.Fatalf("RemoteID = %q, want new-id", response.RemoteID)
	}
	if client.findCalls != 0 || client.addCalls != 1 || client.selectCalls != 1 {
		t.Fatalf("calls find/add/select = %d/%d/%d, want 0/1/1", client.findCalls, client.addCalls, client.selectCalls)
	}
}

func TestFindByHashUsesDownloadedListHash(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/torrents" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `[
			{"id":"pending","filename":"pending","hash":"%s","status":"downloading","progress":50},
			{"id":"downloaded","filename":"ready","hash":"%s","status":"downloaded","progress":100}
		]`, hash, hash)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "test-token", "", 0, NewRealDebridPolicy())
	id, found, err := client.FindByHash(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if !found || id != "downloaded" {
		t.Fatalf("FindByHash = %q, %t; want downloaded, true", id, found)
	}
}
