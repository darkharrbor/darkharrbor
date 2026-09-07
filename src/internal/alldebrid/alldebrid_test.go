package alldebrid

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type fakeClient struct {
	mu        sync.Mutex
	upload    Upload
	magnet    Magnet
	files     []File
	direct    string
	active    int
	deleted   []string
	uploadErr error
	block     bool
}

func (f *fakeClient) GetUser(context.Context) (*AccountInfo, error) {
	return &AccountInfo{Premium: true}, nil
}
func (f *fakeClient) UploadMagnet(ctx context.Context, _ string) (*Upload, error) {
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	v := f.upload
	return &v, nil
}
func (f *fakeClient) GetMagnet(context.Context, string) (*Magnet, error) {
	v := f.magnet
	return &v, nil
}
func (f *fakeClient) GetFiles(context.Context, string) ([]File, error) {
	return append([]File(nil), f.files...), nil
}
func (f *fakeClient) UnlockLink(context.Context, string) (string, error) { return f.direct, nil }
func (f *fakeClient) DeleteMagnet(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeClient) ActiveCount(context.Context) (int, error) { return f.active, nil }

func torrentItem() *store.Item {
	hash := "0123456789abcdef0123456789abcdef01234567"
	return &store.Item{ID: "item", SourceType: store.SourceTypeTorrent, InfoHash: &hash}
}

func TestAdapterCachedUncachedPollPlayRemoveAndSlots(t *testing.T) {
	f := &fakeClient{
		upload: Upload{ID: "41", Hash: "0123456789abcdef0123456789abcdef01234567", Name: "release", Ready: true},
		magnet: Magnet{ID: "41", Name: "release", Size: 200, Downloaded: 200, Status: "Ready", StatusCode: 4},
		files:  []File{{ID: "0", Name: "video.mkv", RelativePath: "show/video.mkv", Size: 200, link: "sealed-coordinate"}},
		direct: "https://cdn.invalid/content",
		active: 7,
	}
	a := NewAdapter("alldebrid", f).SetPremium(true).SetCapabilities(provider.Capabilities{SlotModel: true})
	item := torrentItem()

	created, err := a.Submit(context.Background(), item, provider.SubmitOptions{AddOnlyIfCached: true})
	if err != nil || created.RemoteID != "41" {
		t.Fatalf("cached submit = %#v, %v", created, err)
	}
	item.RemoteID = &created.RemoteID
	status, err := a.Poll(context.Background(), item)
	if err != nil || !status.DownloadReady || len(status.Files) != 1 || status.Files[0].RelativePath != "show/video.mkv" {
		t.Fatalf("poll = %#v, %v", status, err)
	}
	direct, err := a.RequestDownloadURL(context.Background(), item, "0")
	if err != nil || direct != f.direct {
		t.Fatalf("download URL = %q, %v", direct, err)
	}
	slots, err := a.SlotStatus(context.Background())
	if err != nil || slots.AllowedActiveSlots != 30 || slots.ActiveCount != 7 {
		t.Fatalf("slots = %#v, %v", slots, err)
	}
	if err := a.Remove(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "41" {
		t.Fatalf("deleted = %v", f.deleted)
	}
}

func TestCachedOnlyUncachedDeletesAndAbstains(t *testing.T) {
	f := &fakeClient{upload: Upload{ID: "42", Ready: false}}
	a := NewAdapter("alldebrid", f).SetPremium(true)
	_, err := a.Submit(context.Background(), torrentItem(), provider.SubmitOptions{AddOnlyIfCached: true})
	if err == nil || len(f.deleted) != 1 || f.deleted[0] != "42" {
		t.Fatalf("err=%v deleted=%v", err, f.deleted)
	}
}

func TestUncachedSubmitDoesNotUseTorBoxSlotContract(t *testing.T) {
	f := &fakeClient{upload: Upload{ID: "43", Ready: false}}
	a := NewAdapter("alldebrid", f).SetPremium(true)
	got, err := a.Submit(context.Background(), torrentItem(), provider.SubmitOptions{})
	if err != nil || got.RemoteID != "43" {
		t.Fatalf("uncached submit = %#v, %v", got, err)
	}
	// AllDebrid exposes its independent 30-active-magnet SlotSource. Nothing
	// here touches or imports TorBox's ten-slot owner.
	if ActiveMagnetLimit != 30 {
		t.Fatalf("AllDebrid limit = %d", ActiveMagnetLimit)
	}
}

func TestSubmitCancellation(t *testing.T) {
	f := &fakeClient{block: true}
	a := NewAdapter("alldebrid", f).SetPremium(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Submit(ctx, torrentItem(), provider.SubmitOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		code    int
		outcome provider.TorrentOutcome
		failed  bool
	}{
		{0, provider.TorrentOutcomeUnknown, false},
		{4, provider.TorrentOutcomeReady, false},
		{7, provider.TorrentOutcomeTerminalDeadSource, true},
		{9, provider.TorrentOutcomeTransientError, false},
	} {
		got := magnetToTaskStatus(&Magnet{ID: "1", StatusCode: tc.code})
		if got.Outcome != tc.outcome || got.Failed != tc.failed {
			t.Fatalf("code %d = %#v", tc.code, got)
		}
	}
}

func TestParseNestedFilesAndRejectContradictions(t *testing.T) {
	body := []byte(`{"status":"success","data":{"magnets":[{"id":41,"files":[{"n":"show","e":[{"n":"video.mkv","s":123,"l":"opaque"}]},{"n":"root.mkv","s":456,"l":"opaque2"}]}]}}`)
	files, err := parseFiles(body, "41")
	if err != nil || len(files) != 2 || files[0].ID == "" || files[0].RelativePath != "show/video.mkv" {
		t.Fatalf("files=%#v err=%v", files, err)
	}
	reordered := []byte(`{"status":"success","data":{"magnets":[{"id":41,"files":[{"n":"root.mkv","s":456,"l":"opaque2"},{"n":"show","e":[{"n":"video.mkv","s":123,"l":"opaque"}]}]}]}}`)
	reorderedFiles, err := parseFiles(reordered, "41")
	if err != nil || files[0].ID != reorderedFiles[1].ID || files[1].ID != reorderedFiles[0].ID {
		t.Fatalf("stable identities changed across reorder: before=%#v after=%#v err=%v", files, reorderedFiles, err)
	}
	bad := []byte(`{"status":"success","data":{"magnets":[{"id":41,"files":[{"n":"dir","s":1,"l":"x","e":[{"n":"v.mkv","s":1,"l":"y"}]}]}]}}`)
	if _, err := parseFiles(bad, "41"); err == nil {
		t.Fatal("contradictory node accepted")
	}
}

func TestHTTPClientBearerRateHoldAndRedaction(t *testing.T) {
	const key = "private-test-key"
	var sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") == "Bearer "+key
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	policy := NewPolicy()
	defer policy.Stop()
	client := NewHTTPClient(server.URL, key, "test", time.Second, policy)
	_, err := client.GetUser(context.Background())
	if err == nil || !provider.IsRetryable(err) || strings.Contains(err.Error(), key) {
		t.Fatalf("err=%v", err)
	}
	if !sawAuth {
		t.Fatal("bearer authorization absent")
	}
	if _, ok := policy.Limits().Holds[provider.OpQuery]; !ok {
		t.Fatal("429 hold not installed")
	}
}

func TestHTTPClientFailureClasses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		partial   bool
		retryable bool
		hold      bool
	}{
		{name: "not-found", status: http.StatusNotFound},
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "unavailable", status: http.StatusServiceUnavailable, retryable: true, hold: true},
		{name: "partial-disconnect", status: http.StatusOK, partial: true, retryable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.partial {
					w.Header().Set("Content-Length", "100")
					_, _ = w.Write([]byte("{"))
					return
				}
				if tc.hold {
					w.Header().Set("Retry-After", "1")
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			policy := NewPolicy()
			defer policy.Stop()
			client := NewHTTPClient(server.URL, "test-key", "test", time.Second, policy)
			_, err := client.GetUser(context.Background())
			if err == nil || provider.IsRetryable(err) != tc.retryable {
				t.Fatalf("retryable=%t err=%v", provider.IsRetryable(err), err)
			}
			_, held := policy.Limits().Holds[provider.OpQuery]
			if held != tc.hold {
				t.Fatalf("hold=%t, want %t", held, tc.hold)
			}
		})
	}
}

func TestHTTPClientResponseBoundAndCancellation(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
		}))
		defer server.Close()
		policy := NewPolicy()
		defer policy.Stop()
		client := NewHTTPClient(server.URL, "test-key", "test", time.Second, policy)
		if _, err := client.GetUser(context.Background()); err == nil || provider.IsRetryable(err) {
			t.Fatalf("oversized response err=%v", err)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			time.Sleep(50 * time.Millisecond)
		}))
		defer server.Close()
		policy := NewPolicy()
		defer policy.Stop()
		client := NewHTTPClient(server.URL, "test-key", "test", time.Second, policy)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.GetUser(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request err=%v", err)
		}
	})
}

func FuzzParseResponses(f *testing.F) {
	f.Add([]byte(`{"status":"success","data":{"user":{"isPremium":true,"premiumUntil":1}}}`))
	f.Add([]byte(`{"status":"success","data":{"magnets":[{"id":1,"hash":"a","name":"n","size":1,"ready":true}]}}`))
	f.Add([]byte(`{"status":"success","data":{"magnets":[{"id":1,"filename":"n","size":1,"status":"Ready","statusCode":4}]}}`))
	f.Add([]byte(`{"status":"success","data":{"magnets":[{"id":1,"files":[{"n":"v.mkv","s":1,"l":"opaque"}]}]}}`))
	f.Add([]byte(`{"status":"success","data":{"link":"https://cdn.invalid/f"}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = parseUser(body)
		_, _ = parseUpload(body)
		_, _ = parseMagnets(body)
		_, _ = parseFiles(body, "1")
		_, _ = parseUnlock(body)
	})
}
