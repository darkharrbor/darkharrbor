package premiumize

import (
	"context"
	"errors"
	"fmt"
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
	mu            sync.Mutex
	cached        bool
	create        Transfer
	transfer      Transfer
	files         []File
	blockCreate   bool
	createCalls   int
	deleted       []string
	transferError error
}

func (f *fakeClient) GetAccount(context.Context) (*AccountInfo, error) {
	return &AccountInfo{PremiumUntil: time.Now().Add(time.Hour).Unix()}, nil
}
func (f *fakeClient) CheckCache(context.Context, string) (bool, error) { return f.cached, nil }
func (f *fakeClient) CreateTransfer(ctx context.Context, _ string) (*Transfer, error) {
	if f.blockCreate {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	f.createCalls++
	f.mu.Unlock()
	tr := f.create
	return &tr, nil
}
func (f *fakeClient) GetTransfer(context.Context, string) (*Transfer, error) {
	if f.transferError != nil {
		return nil, f.transferError
	}
	tr := f.transfer
	return &tr, nil
}
func (f *fakeClient) FilesForTransfer(context.Context, *Transfer) ([]File, error) {
	return append([]File(nil), f.files...), nil
}
func (f *fakeClient) GetFile(context.Context, string) (*File, error) {
	if len(f.files) == 0 {
		return nil, errors.New("missing")
	}
	file := f.files[0]
	return &file, nil
}
func (f *fakeClient) DeleteTransfer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, "transfer:"+id)
	return nil
}
func (f *fakeClient) DeleteItem(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, "item:"+id)
	return nil
}
func (f *fakeClient) DeleteFolder(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, "folder:"+id)
	return nil
}

func torrentItem() *store.Item {
	hash := "0123456789abcdef0123456789abcdef01234567"
	return &store.Item{ID: "item", SourceType: store.SourceTypeTorrent, InfoHash: &hash}
}

func TestAdapterCacheSubmitPollPlayRemove(t *testing.T) {
	f := &fakeClient{
		cached:   true,
		create:   Transfer{ID: "transfer-1", Name: "release"},
		transfer: Transfer{ID: "transfer-1", Name: "release", Status: "finished", Progress: 1, FolderID: "folder-1", FileID: "file-1"},
		files:    []File{{ID: "file-1", Name: "video.mkv", RelativePath: "video.mkv", Size: 200, link: "https://cdn.invalid/file"}},
	}
	a := NewAdapter("premiumize", f).SetPremium(true).SetCapabilities(provider.Capabilities{CacheOracle: true})
	item := torrentItem()
	cache, err := a.CheckCached(context.Background(), item)
	if err != nil || !cache.Cached {
		t.Fatalf("cache=%#v err=%v", cache, err)
	}
	created, err := a.Submit(context.Background(), item, provider.SubmitOptions{AddOnlyIfCached: true})
	if err != nil || created.RemoteID != "transfer-1" {
		t.Fatalf("create=%#v err=%v", created, err)
	}
	item.RemoteID = &created.RemoteID
	status, err := a.Poll(context.Background(), item)
	if err != nil || !status.DownloadReady || status.Outcome != provider.TorrentOutcomeReady ||
		len(status.Files) != 1 || status.Files[0].FileID != "file-1" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	direct, err := a.RequestDownloadURL(context.Background(), item, "file-1")
	if err != nil || direct != f.files[0].link {
		t.Fatalf("direct=%q err=%v", direct, err)
	}
	if err := a.Remove(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.deleted, ","); got != "item:file-1,transfer:transfer-1" {
		t.Fatalf("deleted=%s", got)
	}
	if _, ok := any(a).(provider.SlotSource); ok {
		t.Fatal("Premiumize must not invent an unpublished slot model")
	}
}

func TestCachedOnlyMissDoesNotCreate(t *testing.T) {
	f := &fakeClient{cached: false, create: Transfer{ID: "should-not-run", Name: "x"}}
	a := NewAdapter("premiumize", f).SetPremium(true)
	if _, err := a.Submit(context.Background(), torrentItem(), provider.SubmitOptions{AddOnlyIfCached: true}); err == nil {
		t.Fatal("cached-only miss accepted")
	}
	if f.createCalls != 0 {
		t.Fatalf("create calls=%d", f.createCalls)
	}
}

func TestUncachedSubmitAndCancellation(t *testing.T) {
	t.Run("uncached", func(t *testing.T) {
		f := &fakeClient{create: Transfer{ID: "transfer-2", Name: "release"}}
		a := NewAdapter("premiumize", f).SetPremium(true)
		got, err := a.Submit(context.Background(), torrentItem(), provider.SubmitOptions{})
		if err != nil || got.RemoteID != "transfer-2" || f.createCalls != 1 {
			t.Fatalf("got=%#v calls=%d err=%v", got, f.createCalls, err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		f := &fakeClient{blockCreate: true}
		a := NewAdapter("premiumize", f).SetPremium(true)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := a.Submit(ctx, torrentItem(), provider.SubmitOptions{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestPollClassificationsAndRemovalShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tr      Transfer
		trErr   error
		outcome provider.TorrentOutcome
	}{
		{name: "queued", tr: Transfer{ID: "t", Name: "n", Status: "queued"}, outcome: provider.TorrentOutcomeUnknown},
		{name: "running", tr: Transfer{ID: "t", Name: "n", Status: "running", Progress: .5}, outcome: provider.TorrentOutcomeUnknown},
		{name: "error", tr: Transfer{ID: "t", Name: "n", Status: "error"}, outcome: provider.TorrentOutcomeTransientError},
		{name: "removed", trErr: ErrTransferNotFound, outcome: provider.TorrentOutcomeRemoteRemoved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeClient{transfer: tc.tr, transferError: tc.trErr}
			a := NewAdapter("premiumize", f).SetPremium(true)
			id := "t"
			item := torrentItem()
			item.RemoteID = &id
			got, err := a.Poll(context.Background(), item)
			if err != nil || got.Outcome != tc.outcome {
				t.Fatalf("got=%#v err=%v", got, err)
			}
		})
	}

	f := &fakeClient{transfer: Transfer{ID: "t", Name: "n", Status: "finished", Progress: 1, FolderID: "folder"}}
	a := NewAdapter("premiumize", f).SetPremium(true)
	id := "t"
	item := torrentItem()
	item.RemoteID = &id
	if err := a.Remove(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.deleted, ","); got != "folder:folder,transfer:t" {
		t.Fatalf("deleted=%s", got)
	}
}

func TestPremiumAtBoundary(t *testing.T) {
	now := time.Unix(1000, 0)
	if PremiumAt(nil, now) || PremiumAt(&AccountInfo{PremiumUntil: 1000}, now) ||
		!PremiumAt(&AccountInfo{PremiumUntil: 1001}, now) {
		t.Fatal("premium boundary mismatch")
	}
}

func TestParsersFailClosed(t *testing.T) {
	account, err := parseAccount([]byte(`{"status":"success","premium_until":2000,"limit_used":0.25,"booster_points":1}`))
	if err != nil || account.PremiumUntil != 2000 {
		t.Fatalf("account=%#v err=%v", account, err)
	}
	if _, err := parseCache([]byte(`{"status":"success","response":[true,false]}`)); err == nil {
		t.Fatal("parallel cache mismatch accepted")
	}
	if _, err := parseTransfers([]byte(`{"status":"success","transfers":[{"id":"t","name":"n","status":"finished","progress":1}]}`)); err == nil {
		t.Fatal("finished transfer without content accepted")
	}
	if _, err := parseFolder([]byte(`{"status":"success","folder_id":"root","content":[{"id":"f","name":"../bad","type":"file","size":1,"link":"https://cdn.invalid/f"}]}`), "root"); err == nil {
		t.Fatal("traversal accepted")
	}
	if _, err := parseItem([]byte(`{"status":"success","id":"f","name":"v.mkv","size":1,"link":"https://user@cdn.invalid/f"}`), "f"); err == nil {
		t.Fatal("userinfo link accepted")
	}
}

func TestHTTPClientNestedFilesBearerRateHoldAndDelete(t *testing.T) {
	const key = "private-test-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("bearer authorization absent")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","premium_until":4102444800,"limit_used":0.1,"booster_points":0}`)
		case "/api/cache/check":
			fmt.Fprint(w, `{"status":"success","response":[true]}`)
		case "/api/transfer/create":
			fmt.Fprint(w, `{"status":"success","id":"t1","name":"release"}`)
		case "/api/transfer/list":
			fmt.Fprint(w, `{"status":"success","transfers":[{"id":"t1","name":"release","status":"finished","progress":1,"folder_id":"root","file_id":null}]}`)
		case "/api/folder/list":
			switch r.URL.Query().Get("id") {
			case "root":
				fmt.Fprint(w, `{"status":"success","folder_id":"root","content":[{"id":"sub","name":"show","type":"folder"},{"id":"f2","name":"root.mkv","type":"file","size":2,"link":"https://cdn.invalid/root"}]}`)
			case "sub":
				fmt.Fprint(w, `{"status":"success","folder_id":"sub","content":[{"id":"f1","name":"video.mkv","type":"file","size":1,"link":"https://cdn.invalid/video"}]}`)
			default:
				http.Error(w, "unexpected", http.StatusBadRequest)
			}
		case "/api/item/delete", "/api/folder/delete", "/api/transfer/delete":
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	policy := NewPolicy()
	client := NewHTTPClient(server.URL+"/api", key, "test", time.Second, policy)
	if info, err := client.GetAccount(context.Background()); err != nil || info.PremiumUntil == 0 {
		t.Fatalf("account=%#v err=%v", info, err)
	}
	if cached, err := client.CheckCache(context.Background(), "magnet:test"); err != nil || !cached {
		t.Fatalf("cached=%t err=%v", cached, err)
	}
	tr, err := client.CreateTransfer(context.Background(), "magnet:test")
	if err != nil {
		t.Fatal(err)
	}
	tr, err = client.GetTransfer(context.Background(), tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	files, err := client.FilesForTransfer(context.Background(), tr)
	if err != nil || len(files) != 2 || files[1].RelativePath != "show/video.mkv" {
		t.Fatalf("files=%#v err=%v", files, err)
	}
	if err := client.DeleteFolder(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteTransfer(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPClientRejectsFolderCycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/folder/list" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"status":"success","folder_id":%q,"content":[{"id":"root","name":"again","type":"folder"}]}`, r.URL.Query().Get("id"))
	}))
	defer server.Close()
	client := NewHTTPClient(server.URL, "test-key", "test", time.Second, NewPolicy())
	_, err := client.FilesForTransfer(context.Background(), &Transfer{ID: "t", FolderID: "root"})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err=%v", err)
	}
}

func TestHTTPClientFailureClassesBoundsRedirectAndCancellation(t *testing.T) {
	t.Run("business-rate-limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"status":"error","code":"rate_limit_reached","message":"private free form"}`)
		}))
		defer server.Close()
		policy := NewPolicy()
		client := NewHTTPClient(server.URL, "test-key", "test", time.Second, policy)
		_, err := client.GetAccount(context.Background())
		if err == nil || !provider.IsRetryable(err) || strings.Contains(err.Error(), "private free form") {
			t.Fatalf("err=%v", err)
		}
		if _, held := policy.Limits().Holds[provider.OpQuery]; !held {
			t.Fatal("rate-limit hold absent")
		}
	})

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
		{name: "server-error", status: http.StatusInternalServerError, retryable: true},
		{name: "partial", status: http.StatusOK, partial: true, retryable: true},
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
			client := NewHTTPClient(server.URL, "test-key", "test", time.Second, policy)
			_, err := client.GetAccount(context.Background())
			if err == nil || provider.IsRetryable(err) != tc.retryable {
				t.Fatalf("retryable=%t err=%v", provider.IsRetryable(err), err)
			}
			_, held := policy.Limits().Holds[provider.OpQuery]
			if held != tc.hold {
				t.Fatalf("hold=%t want=%t", held, tc.hold)
			}
		})
	}

	t.Run("oversized", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
		}))
		defer server.Close()
		client := NewHTTPClient(server.URL, "test-key", "test", time.Second, NewPolicy())
		if _, err := client.GetAccount(context.Background()); err == nil || provider.IsRetryable(err) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("redirect-does-not-forward-bearer", func(t *testing.T) {
		var forwarded bool
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			forwarded = true
		}))
		defer target.Close()
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}))
		defer source.Close()
		client := NewHTTPClient(source.URL, "test-key", "test", time.Second, NewPolicy())
		if _, err := client.GetAccount(context.Background()); err == nil || forwarded {
			t.Fatalf("err=%v forwarded=%t", err, forwarded)
		}
	})

	t.Run("transport-error-redacts-url", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		baseURL := server.URL + "/private-path"
		server.Close()
		client := NewHTTPClient(baseURL, "test-key", "test", time.Second, NewPolicy())
		_, err := client.GetAccount(context.Background())
		if err == nil || !provider.IsRetryable(err) || strings.Contains(err.Error(), baseURL) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		client := NewHTTPClient("https://127.0.0.1.invalid", "test-key", "test", time.Second, NewPolicy())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.GetAccount(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
}

func FuzzParseResponses(f *testing.F) {
	f.Add([]byte(`{"status":"success","premium_until":2000,"limit_used":0.1,"booster_points":0}`))
	f.Add([]byte(`{"status":"success","response":[true]}`))
	f.Add([]byte(`{"status":"success","id":"t","name":"release"}`))
	f.Add([]byte(`{"status":"success","transfers":[{"id":"t","name":"n","status":"running","progress":0.5,"folder_id":null,"file_id":null}]}`))
	f.Add([]byte(`{"status":"success","folder_id":"root","content":[{"id":"f","name":"v.mkv","type":"file","size":1,"link":"https://cdn.invalid/f"}]}`))
	f.Add([]byte(`{"status":"success","id":"f","name":"v.mkv","size":1,"link":"https://cdn.invalid/f"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = parseAccount(body)
		_, _ = parseCache(body)
		_, _ = parseCreate(body)
		_, _ = parseTransfers(body)
		_, _ = parseFolder(body, "root")
		_, _ = parseItem(body, "f")
		_ = parseDelete(body)
	})
}
