package api

import (
	"context"
	"errors"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/availability"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// altFakeProvider is a minimal provider.Provider fake for TS-2.2 rung 4
// confirm/resolve unit tests. It records call counts so tests can assert
// "bounded" behavior (e.g. Submit called at most once).
type altFakeProvider struct {
	name string
	caps provider.Capabilities

	checkCachedResult *provider.CheckCachedResult
	checkCachedErr    error
	checkCachedCalls  int

	submitResp  *provider.CreateTaskResponse
	submitErr   error
	submitCalls int

	pollStatus *provider.TaskStatus
	pollErr    error
	pollCalls  int

	removeCalls int

	dlURL        string
	dlErr        error
	dlFileIDSeen string
	dlCalls      int
}

func (f *altFakeProvider) Name() string                        { return f.name }
func (f *altFakeProvider) Capabilities() provider.Capabilities { return f.caps }
func (f *altFakeProvider) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	f.checkCachedCalls++
	return f.checkCachedResult, f.checkCachedErr
}
func (f *altFakeProvider) Submit(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	f.submitCalls++
	return f.submitResp, f.submitErr
}
func (f *altFakeProvider) Poll(ctx context.Context, item *store.Item) (*provider.TaskStatus, error) {
	f.pollCalls++
	return f.pollStatus, f.pollErr
}
func (f *altFakeProvider) RequestDownloadURL(ctx context.Context, item *store.Item, fileID string) (string, error) {
	f.dlCalls++
	f.dlFileIDSeen = fileID
	return f.dlURL, f.dlErr
}
func (f *altFakeProvider) Remove(ctx context.Context, item *store.Item) error {
	f.removeCalls++
	return nil
}

var _ provider.Provider = (*altFakeProvider)(nil)

func torrentItemWithHash(hash string) *store.Item {
	h := hash
	return &store.Item{
		ID:          "item-1",
		SourceType:  store.SourceTypeTorrent,
		InfoHash:    &h,
		DisplayName: "Some.Release.2026",
	}
}

// --- matchAltFile ---

func TestMatchAltFileExactNameAndSize(t *testing.T) {
	files := []provider.RemoteFile{{FileID: "1", Name: "movie.mkv", Size: 100}}
	id, ok := matchAltFile(files, nil, "movie.mkv", 100)
	if !ok || id != "1" {
		t.Fatalf("got id=%q ok=%v, want 1/true", id, ok)
	}
}

func TestMatchAltFileBasenameFallback(t *testing.T) {
	files := []provider.RemoteFile{{FileID: "2", Name: "Release/movie.mkv", Size: 100}}
	id, ok := matchAltFile(files, nil, "Other.Path/movie.mkv", 100)
	if !ok || id != "2" {
		t.Fatalf("got id=%q ok=%v, want 2/true", id, ok)
	}
}

func TestMatchAltFileWrongSizeRejected(t *testing.T) {
	files := []provider.RemoteFile{{FileID: "3", Name: "movie.mkv", Size: 999}}
	if _, ok := matchAltFile(files, nil, "movie.mkv", 100); ok {
		t.Fatal("wrong-size file must never match")
	}
}

func TestMatchAltFileUniqueSizeFallback(t *testing.T) {
	files := []provider.RemoteFile{{FileID: "4", Name: "totally-different-name.mkv", Size: 12345}}
	id, ok := matchAltFile(files, nil, "movie.mkv", 12345)
	if !ok || id != "4" {
		t.Fatalf("unique same-size file should match as a fallback, got id=%q ok=%v", id, ok)
	}
}

func TestMatchAltFileAmbiguousSizeRejected(t *testing.T) {
	files := []provider.RemoteFile{
		{FileID: "5", Name: "a.mkv", Size: 555},
		{FileID: "6", Name: "b.mkv", Size: 555},
	}
	if _, ok := matchAltFile(files, nil, "movie.mkv", 555); ok {
		t.Fatal("an ambiguous same-size match must never be guessed")
	}
}

func TestMatchAltFileChecksCachedFilesToo(t *testing.T) {
	cached := []provider.CachedFile{{FileID: "7", Name: "movie.mkv", Size: 100}}
	id, ok := matchAltFile(nil, cached, "movie.mkv", 100)
	if !ok || id != "7" {
		t.Fatalf("got id=%q ok=%v, want 7/true", id, ok)
	}
}

// --- buildAltProviderCandidates ---

func TestBuildAltProviderCandidatesNilForNZB(t *testing.T) {
	item := &store.Item{SourceType: store.SourceTypeNZB}
	primary := &altFakeProvider{name: "torbox"}
	all := map[string]provider.Provider{"torbox": primary, "rd": &altFakeProvider{name: "rd"}}
	if got := buildAltProviderCandidates(item, "f.mkv", 100, primary, all, []string{"torbox", "rd"}, nil); got != nil {
		t.Fatalf("NZB item must never get rung 4 candidates, got %v", got)
	}
}

func TestBuildAltProviderCandidatesNilForNoInfoHash(t *testing.T) {
	item := &store.Item{SourceType: store.SourceTypeTorrent}
	primary := &altFakeProvider{name: "torbox"}
	all := map[string]provider.Provider{"torbox": primary, "rd": &altFakeProvider{name: "rd"}}
	if got := buildAltProviderCandidates(item, "f.mkv", 100, primary, all, []string{"torbox", "rd"}, nil); got != nil {
		t.Fatalf("item with no infohash must never get rung 4 candidates, got %v", got)
	}
}

func TestBuildAltProviderCandidatesNilForUnknownSize(t *testing.T) {
	item := torrentItemWithHash("abc123")
	primary := &altFakeProvider{name: "torbox"}
	all := map[string]provider.Provider{"torbox": primary, "rd": &altFakeProvider{name: "rd"}}
	if got := buildAltProviderCandidates(item, "f.mkv", 0, primary, all, []string{"torbox", "rd"}, nil); got != nil {
		t.Fatalf("zero/unknown target size must never get rung 4 candidates, got %v", got)
	}
}

func TestBuildAltProviderCandidatesExcludesPrimaryPreservesOrder(t *testing.T) {
	item := torrentItemWithHash("abc123")
	primary := &altFakeProvider{name: "torbox"}
	rd := &altFakeProvider{name: "rd"}
	third := &altFakeProvider{name: "other"}
	all := map[string]provider.Provider{"torbox": primary, "rd": rd, "other": third}
	got := buildAltProviderCandidates(item, "f.mkv", 100, primary, all, []string{"torbox", "rd", "other"}, nil)
	if len(got) != 2 || got[0].name != "rd" || got[1].name != "other" {
		var names []string
		for _, c := range got {
			names = append(names, c.name)
		}
		t.Fatalf("candidates = %v, want [rd other] (primary excluded, plan order preserved)", names)
	}
}

func TestBuildAltProviderCandidatesRecordsSuccessfulAlternateConfirmation(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	s := newAvailabilityTestServer(t, st)
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	item := torrentItemWithHash(hash)
	primary := &altFakeProvider{name: "torbox"}
	rd := &altFakeProvider{
		name:       "rd",
		submitResp: &provider.CreateTaskResponse{RemoteID: "rid-observed"},
		pollStatus: &provider.TaskStatus{
			DownloadPresent: true,
			CachedFiles:     []provider.CachedFile{{FileID: "cf1", Name: "f.mkv", Size: 100}},
		},
	}

	candidates := buildAltProviderCandidates(
		item,
		"f.mkv",
		100,
		primary,
		map[string]provider.Provider{"torbox": primary, "rd": rd},
		[]string{"torbox", "rd"},
		func(ctx context.Context, providerName, infoHash string) {
			s.recordHashAvailability(ctx, providerName, infoHash, true, availability.SourceStream)
		},
	)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	if _, err := candidates[0].confirm(ctx); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	got, found, err := st.GetHashAvailability(ctx, "rd", hash)
	if err != nil || !found {
		t.Fatalf("expected alternate-provider observation: found=%v err=%v", found, err)
	}
	if !got.Cached || got.Source != string(availability.SourceStream) {
		t.Fatalf("unexpected observation: %+v", got)
	}
}

func TestBuildAltProviderCandidatesAbstainsOnFailureAndCancellation(t *testing.T) {
	tests := []struct {
		name      string
		ctx       func() context.Context
		poll      *provider.TaskStatus
		wantError bool
	}{
		{
			name:      "failed confirmation",
			ctx:       context.Background,
			poll:      &provider.TaskStatus{},
			wantError: true,
		},
		{
			name: "cancelled confirmation",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			poll: &provider.TaskStatus{
				DownloadPresent: true,
				CachedFiles:     []provider.CachedFile{{FileID: "cf1", Name: "f.mkv", Size: 100}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := torrentItemWithHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			primary := &altFakeProvider{name: "torbox"}
			rd := &altFakeProvider{
				name:       "rd",
				submitResp: &provider.CreateTaskResponse{RemoteID: "rid-abstain"},
				pollStatus: tt.poll,
			}
			observations := 0
			candidates := buildAltProviderCandidates(
				item,
				"f.mkv",
				100,
				primary,
				map[string]provider.Provider{"torbox": primary, "rd": rd},
				[]string{"torbox", "rd"},
				func(context.Context, string, string) { observations++ },
			)
			_, err := candidates[0].confirm(tt.ctx())
			if (err != nil) != tt.wantError {
				t.Fatalf("confirm error = %v, wantError=%v", err, tt.wantError)
			}
			if observations != 0 {
				t.Fatalf("observations = %d, want 0", observations)
			}
		})
	}
}

// --- confirmAltProvider ---

func TestConfirmAltProviderCacheOracleNotCached(t *testing.T) {
	item := torrentItemWithHash("abc123")
	p := &altFakeProvider{name: "torbox", caps: provider.Capabilities{CacheOracle: true}, checkCachedResult: &provider.CheckCachedResult{Cached: false}}
	if _, err := confirmAltProvider(context.Background(), p, item, "f.mkv", 100); err == nil {
		t.Fatal("not-cached must fail confirm")
	}
	if p.submitCalls != 0 {
		t.Fatalf("a cache-oracle provider reporting not-cached must never Submit, got %d calls", p.submitCalls)
	}
}

func TestConfirmAltProviderCacheOracleCachedResolves(t *testing.T) {
	item := torrentItemWithHash("abc123")
	p := &altFakeProvider{
		name:              "torbox",
		caps:              provider.Capabilities{CacheOracle: true},
		checkCachedResult: &provider.CheckCachedResult{Cached: true},
		submitResp:        &provider.CreateTaskResponse{RemoteID: "rid-1"},
		pollStatus: &provider.TaskStatus{
			DownloadReady: true,
			Files:         []provider.RemoteFile{{FileID: "f1", Name: "f.mkv", Size: 100}},
		},
		dlURL: "https://alt.example/cdn/f1?tok=x",
	}
	resolve, err := confirmAltProvider(context.Background(), p, item, "f.mkv", 100)
	if err != nil {
		t.Fatalf("confirm should succeed, got %v", err)
	}
	url, err := resolve(context.Background())
	if err != nil || url != "https://alt.example/cdn/f1?tok=x" {
		t.Fatalf("resolve() = %q, %v", url, err)
	}
	if p.dlFileIDSeen != "f1" {
		t.Fatalf("RequestDownloadURL called with fileID %q, want f1", p.dlFileIDSeen)
	}
	if p.checkCachedCalls != 1 || p.submitCalls != 1 || p.pollCalls != 1 {
		t.Fatalf("bounded confirm must be exactly one checkcached+submit+poll, got %d/%d/%d",
			p.checkCachedCalls, p.submitCalls, p.pollCalls)
	}
}

func TestConfirmAltProviderAddThenVerifyNoOracle(t *testing.T) {
	item := torrentItemWithHash("abc123")
	p := &altFakeProvider{
		name:       "rd",
		caps:       provider.Capabilities{CacheOracle: false}, // e.g. Real-Debrid
		submitResp: &provider.CreateTaskResponse{RemoteID: "rid-2"},
		pollStatus: &provider.TaskStatus{
			DownloadPresent: true,
			CachedFiles:     []provider.CachedFile{{FileID: "cf1", Name: "f.mkv", Size: 100}},
		},
		dlURL: "https://rd.example/cdn/cf1",
	}
	resolve, err := confirmAltProvider(context.Background(), p, item, "f.mkv", 100)
	if err != nil {
		t.Fatalf("add-then-verify confirm should succeed, got %v", err)
	}
	if p.checkCachedCalls != 0 {
		t.Fatalf("a no-oracle provider must never be CheckCached'd, got %d calls", p.checkCachedCalls)
	}
	url, err := resolve(context.Background())
	if err != nil || url != "https://rd.example/cdn/cf1" {
		t.Fatalf("resolve() = %q, %v", url, err)
	}
}

func TestProviderCacheEvictedAbstainsWithoutOracle(t *testing.T) {
	p := &altFakeProvider{
		name:              "rd",
		checkCachedResult: &provider.CheckCachedResult{Cached: false},
	}
	item := torrentItemWithHash("abc123")

	evicted, err := providerCacheEvicted(context.Background(), p, item)
	if err != nil || evicted {
		t.Fatalf("providerCacheEvicted() = %v, %v; want false, nil", evicted, err)
	}
	if p.checkCachedCalls != 0 {
		t.Fatalf("CheckCached calls = %d, want 0", p.checkCachedCalls)
	}
}

func TestProviderCacheEvictedUsesOracle(t *testing.T) {
	p := &altFakeProvider{
		name:              "torbox",
		caps:              provider.Capabilities{CacheOracle: true},
		checkCachedResult: &provider.CheckCachedResult{Cached: false},
	}
	item := torrentItemWithHash("abc123")

	evicted, err := providerCacheEvicted(context.Background(), p, item)
	if err != nil || !evicted {
		t.Fatalf("providerCacheEvicted() = %v, %v; want true, nil", evicted, err)
	}
	if p.checkCachedCalls != 1 {
		t.Fatalf("CheckCached calls = %d, want 1", p.checkCachedCalls)
	}
}

func TestConfirmAltProviderAddThenVerifyNotReadyCleansUp(t *testing.T) {
	item := torrentItemWithHash("abc123")
	p := &altFakeProvider{
		name:       "rd",
		submitResp: &provider.CreateTaskResponse{RemoteID: "rid-3"},
		pollStatus: &provider.TaskStatus{DownloadReady: false, DownloadPresent: false},
	}
	if _, err := confirmAltProvider(context.Background(), p, item, "f.mkv", 100); err == nil {
		t.Fatal("not-ready add-then-verify must fail confirm")
	}
	if p.removeCalls != 1 {
		t.Fatalf("a failed add-then-verify must clean itself up exactly once, got %d Remove calls", p.removeCalls)
	}
}

func TestConfirmAltProviderNoMatchingFile(t *testing.T) {
	item := torrentItemWithHash("abc123")
	p := &altFakeProvider{
		name:       "rd",
		submitResp: &provider.CreateTaskResponse{RemoteID: "rid-4"},
		pollStatus: &provider.TaskStatus{
			DownloadReady: true,
			Files:         []provider.RemoteFile{{FileID: "wrong", Name: "different.mkv", Size: 999}},
		},
	}
	if _, err := confirmAltProvider(context.Background(), p, item, "f.mkv", 100); err == nil {
		t.Fatal("no file-list match must fail confirm, never guess a wrong file")
	}
}

func TestConfirmAltProviderNeverMutatesRealItem(t *testing.T) {
	item := torrentItemWithHash("abc123")
	p := &altFakeProvider{
		name:       "rd",
		submitResp: &provider.CreateTaskResponse{RemoteID: "should-not-leak"},
		pollStatus: &provider.TaskStatus{
			DownloadReady: true,
			Files:         []provider.RemoteFile{{FileID: "f1", Name: "f.mkv", Size: 100}},
		},
	}
	if _, err := confirmAltProvider(context.Background(), p, item, "f.mkv", 100); err != nil {
		t.Fatal(err)
	}
	if item.RemoteID != nil {
		t.Fatalf("confirmAltProvider must never write back to the real item; RemoteID = %v", item.RemoteID)
	}
}

func TestConfirmAltProviderSubmitErrorPropagates(t *testing.T) {
	item := torrentItemWithHash("abc123")
	p := &altFakeProvider{name: "rd", submitErr: errors.New("boom")}
	if _, err := confirmAltProvider(context.Background(), p, item, "f.mkv", 100); err == nil {
		t.Fatal("a Submit failure must fail confirm")
	}
}
