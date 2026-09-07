package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// fakeGrabHealthProvider is a minimal provider.UsenetProvider stand-in that
// lets tests control GrabTimeHealthCheck's verdict directly, isolating
// rejectDeadGrabTimeNZB's own decision/blacklist/logging logic from the
// real STAT-sampling mechanics (covered separately in internal/nntp).
type fakeGrabHealthProvider struct {
	allDead bool
	sampled int
	err     error
	calls   int
}

func (f *fakeGrabHealthProvider) Name() string { return "fake" }
func (f *fakeGrabHealthProvider) Probe(context.Context, []byte, int64) (string, error) {
	return "", nil
}
func (f *fakeGrabHealthProvider) Stream(context.Context, string, []byte, int, http.ResponseWriter, string) error {
	return nil
}
func (f *fakeGrabHealthProvider) CorrectedTotal(context.Context, string, []byte, int) int64 {
	return 0
}
func (f *fakeGrabHealthProvider) StreamRARManifest(context.Context, string, int64, http.ResponseWriter, string, string, string) error {
	return nil
}
func (f *fakeGrabHealthProvider) StreamZIPEntry(context.Context, string, int, http.ResponseWriter, string, string, string) error {
	return nil
}
func (f *fakeGrabHealthProvider) GrabTimeHealthCheck(context.Context, []byte, int) (bool, int, error) {
	f.calls++
	return f.allDead, f.sampled, f.err
}

var _ provider.UsenetProvider = (*fakeGrabHealthProvider)(nil)

func newGrabHealthTestServer(t *testing.T, up provider.UsenetProvider) (*Server, *bytes.Buffer, *store.Store) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "grabhealth.db")
	db, err := store.Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	st := store.New(db)

	cfg := &config.Config{}
	cfg.Routing.Preference = []string{config.LaneNNTPNZB}
	cfg.GrabHealth.Enabled = true
	cfg.GrabHealth.SampleSize = 4
	cfg.GrabHealth.TimeoutMS = 1500

	var logs bytes.Buffer
	s := &Server{
		cfg:   cfg,
		log:   slog.New(slog.NewTextHandler(&logs, nil)),
		store: st,
	}
	if up != nil {
		s.usenetProviders = map[string]provider.UsenetProvider{"fake": up}
		s.usenetOrder = []string{"fake"}
	}
	return s, &logs, st
}

func TestRejectDeadGrabTimeNZB_AllDead_RejectsAndBlacklists(t *testing.T) {
	up := &fakeGrabHealthProvider{allDead: true, sampled: 4}
	s, logs, st := newGrabHealthTestServer(t, up)

	rawNZB := "raw-dead-nzb-content"
	rejected := s.rejectDeadGrabTimeNZB(context.Background(), []byte(rawNZB), "tv", "Dead.Release.S01E01")
	if !rejected {
		t.Fatal("expected rejection for allDead=true")
	}
	if up.calls != 1 {
		t.Fatalf("GrabTimeHealthCheck calls = %d, want 1", up.calls)
	}

	blacklisted, err := st.IsBlacklisted(context.Background(), NZBBlacklistKey(rawNZB))
	if err != nil {
		t.Fatalf("IsBlacklisted: %v", err)
	}
	if !blacklisted {
		t.Fatal("expected the dead NZB's content-key to be blacklisted")
	}

	gotLogs := logs.String()
	for _, want := range []string{
		"event=grabhealth_rejected",
		"display_name=Dead.Release.S01E01",
		"category=tv",
		"sampled=4",
	} {
		if !bytes.Contains([]byte(gotLogs), []byte(want)) {
			t.Fatalf("logs missing %q: %s", want, gotLogs)
		}
	}
	if bytes.Contains([]byte(gotLogs), []byte(rawNZB)) {
		t.Fatalf("logs leaked raw NZB content: %s", gotLogs)
	}
}

func TestRejectDeadGrabTimeNZB_MixedSample_DoesNotReject(t *testing.T) {
	up := &fakeGrabHealthProvider{allDead: false, sampled: 4}
	s, _, st := newGrabHealthTestServer(t, up)

	rawNZB := "raw-mixed-nzb-content"
	rejected := s.rejectDeadGrabTimeNZB(context.Background(), []byte(rawNZB), "tv", "Mixed.Release")
	if rejected {
		t.Fatal("expected no rejection for allDead=false")
	}
	blacklisted, err := st.IsBlacklisted(context.Background(), NZBBlacklistKey(rawNZB))
	if err != nil {
		t.Fatalf("IsBlacklisted: %v", err)
	}
	if blacklisted {
		t.Fatal("a non-dead verdict must never blacklist")
	}
}

func TestRejectDeadGrabTimeNZB_ProviderError_AbstainsNoReject(t *testing.T) {
	up := &fakeGrabHealthProvider{err: context.DeadlineExceeded}
	s, _, st := newGrabHealthTestServer(t, up)

	rawNZB := "raw-error-nzb-content"
	rejected := s.rejectDeadGrabTimeNZB(context.Background(), []byte(rawNZB), "tv", "Error.Release")
	if rejected {
		t.Fatal("a GrabTimeHealthCheck error must abstain, never reject")
	}
	blacklisted, err := st.IsBlacklisted(context.Background(), NZBBlacklistKey(rawNZB))
	if err != nil {
		t.Fatalf("IsBlacklisted: %v", err)
	}
	if blacklisted {
		t.Fatal("an abstained check must never blacklist")
	}
}

func TestRejectDeadGrabTimeNZB_ConfigDisabled_SkipsCheckEntirely(t *testing.T) {
	up := &fakeGrabHealthProvider{allDead: true, sampled: 4}
	s, _, _ := newGrabHealthTestServer(t, up)
	s.cfg.GrabHealth.Enabled = false

	rejected := s.rejectDeadGrabTimeNZB(context.Background(), []byte("whatever"), "tv", "X")
	if rejected {
		t.Fatal("a disabled GrabHealth config must never reject")
	}
	if up.calls != 0 {
		t.Fatalf("GrabTimeHealthCheck calls = %d, want 0 when disabled (must not even call the provider)", up.calls)
	}
}

func TestRejectDeadGrabTimeNZB_NNTPDisabled_Abstains(t *testing.T) {
	up := &fakeGrabHealthProvider{allDead: true, sampled: 4}
	s, _, _ := newGrabHealthTestServer(t, up)
	s.cfg.Routing.Preference = []string{config.LaneTorBoxNZB} // NNTP not in preference

	rejected := s.rejectDeadGrabTimeNZB(context.Background(), []byte("whatever"), "tv", "X")
	if rejected {
		t.Fatal("with NNTP disabled there is nothing to STAT-check; must abstain")
	}
	if up.calls != 0 {
		t.Fatalf("GrabTimeHealthCheck calls = %d, want 0 when NNTP lane is disabled", up.calls)
	}
}

func TestRejectDeadGrabTimeNZB_NoUsenetProviderConfigured_Abstains(t *testing.T) {
	s, _, _ := newGrabHealthTestServer(t, nil) // no usenetProviders/usenetOrder set

	rejected := s.rejectDeadGrabTimeNZB(context.Background(), []byte("whatever"), "tv", "X")
	if rejected {
		t.Fatal("with no usenet provider configured, must abstain rather than reject")
	}
}

// --- NS-6.3 lane-scope correction --------------------------------------
//
// These cover torboxNZBCacheMayServe and rejectDeadGrabTimeNZB's preferred-
// lane abstain arm: an NNTP-dead NZB must NOT be rejected or blacklisted
// when torbox_nzb outranks nntp_nzb and the TorBox usenet cache can still
// serve it, nor when the cache cannot be consulted conclusively.

type fakeCacheLaneProvider struct {
	name      string
	result    *provider.CheckCachedResult
	err       error
	calls     int
	sawSource store.SourceType
	sawURI    string
}

func (f *fakeCacheLaneProvider) Name() string                        { return f.name }
func (f *fakeCacheLaneProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (f *fakeCacheLaneProvider) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	f.calls++
	if item != nil {
		f.sawSource = item.SourceType
		if item.SourceURI != nil {
			f.sawURI = *item.SourceURI
		}
	}
	return f.result, f.err
}
func (f *fakeCacheLaneProvider) Submit(context.Context, *store.Item, provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	return nil, nil
}
func (f *fakeCacheLaneProvider) Poll(context.Context, *store.Item) (*provider.TaskStatus, error) {
	return nil, nil
}
func (f *fakeCacheLaneProvider) RequestDownloadURL(context.Context, *store.Item, string) (string, error) {
	return "", nil
}
func (f *fakeCacheLaneProvider) Remove(context.Context, *store.Item) error { return nil }

var _ provider.Provider = (*fakeCacheLaneProvider)(nil)

// torboxPreferredServer builds a server whose preference ranks torbox_nzb
// ahead of nntp_nzb (the live production ordering this correction targets).
func torboxPreferredServer(t *testing.T, up provider.UsenetProvider, lanes ...*fakeCacheLaneProvider) (*Server, *bytes.Buffer, *store.Store) {
	t.Helper()
	s, logs, st := newGrabHealthTestServer(t, up)
	s.cfg.Routing.Preference = []string{config.LaneTorBoxNZB, config.LaneNNTPNZB}
	if !s.cfg.TorBoxNZBPreferredOverNNTP() {
		t.Fatal("test setup: expected torbox_nzb to outrank nntp_nzb")
	}
	for _, l := range lanes {
		s.lanes = append(s.lanes, ProviderLane{Prov: l})
	}
	return s, logs, st
}

func TestRejectDeadGrabTimeNZB_PreferredTorBoxCached_AbstainsAndDoesNotBlacklist(t *testing.T) {
	up := &fakeGrabHealthProvider{allDead: true, sampled: 4}
	lane := &fakeCacheLaneProvider{name: "torbox", result: &provider.CheckCachedResult{Cached: true}}
	s, logs, st := torboxPreferredServer(t, up, lane)

	raw := "nntp-dead-but-torbox-cached"
	if s.rejectDeadGrabTimeNZB(context.Background(), []byte(raw), "tv", "Cached.Release.S01E01") {
		t.Fatal("expected abstain: preferred TorBox lane has the content cached")
	}
	if lane.calls != 1 {
		t.Fatalf("CheckCached calls = %d, want 1", lane.calls)
	}
	if lane.sawSource != store.SourceTypeNZB || lane.sawURI != raw {
		t.Fatalf("probe item wrong: source=%v uri=%q", lane.sawSource, lane.sawURI)
	}
	blacklisted, err := st.IsBlacklisted(context.Background(), NZBBlacklistKey(raw))
	if err != nil {
		t.Fatalf("IsBlacklisted: %v", err)
	}
	if blacklisted {
		t.Fatal("must NOT blacklist a key the preferred lane can still serve")
	}
	if !bytes.Contains(logs.Bytes(), []byte("event=grabhealth_abstain_preferred_lane")) {
		t.Fatalf("missing abstain event, logs=%s", logs.String())
	}
}

func TestRejectDeadGrabTimeNZB_PreferredTorBoxNotCached_StillRejects(t *testing.T) {
	up := &fakeGrabHealthProvider{allDead: true, sampled: 4}
	lane := &fakeCacheLaneProvider{name: "torbox", result: &provider.CheckCachedResult{Cached: false}}
	s, _, st := torboxPreferredServer(t, up, lane)

	raw := "dead-on-every-lane"
	if !s.rejectDeadGrabTimeNZB(context.Background(), []byte(raw), "tv", "Dead.Everywhere.S01E01") {
		t.Fatal("expected rejection: dead on NNTP and conclusively not cached")
	}
	blacklisted, err := st.IsBlacklisted(context.Background(), NZBBlacklistKey(raw))
	if err != nil {
		t.Fatalf("IsBlacklisted: %v", err)
	}
	if !blacklisted {
		t.Fatal("expected blacklist when no lane can serve the content")
	}
}

func TestRejectDeadGrabTimeNZB_PreferredLaneInconclusive_Abstains(t *testing.T) {
	up := &fakeGrabHealthProvider{allDead: true, sampled: 4}
	lane := &fakeCacheLaneProvider{name: "torbox", err: context.DeadlineExceeded}
	s, _, st := torboxPreferredServer(t, up, lane)

	raw := "cache-oracle-unreachable"
	if s.rejectDeadGrabTimeNZB(context.Background(), []byte(raw), "tv", "Unknown.S01E01") {
		t.Fatal("expected abstain: cache oracle gave no conclusive answer")
	}
	blacklisted, err := st.IsBlacklisted(context.Background(), NZBBlacklistKey(raw))
	if err != nil {
		t.Fatalf("IsBlacklisted: %v", err)
	}
	if blacklisted {
		t.Fatal("must never blacklist on an inconclusive cache answer")
	}
}

func TestRejectDeadGrabTimeNZB_PreferredLaneNoLanesConfigured_Abstains(t *testing.T) {
	up := &fakeGrabHealthProvider{allDead: true, sampled: 4}
	s, _, _ := torboxPreferredServer(t, up)

	if s.rejectDeadGrabTimeNZB(context.Background(), []byte("no-lanes"), "tv", "NoLanes.S01E01") {
		t.Fatal("expected abstain when torbox is preferred but no lane can answer")
	}
}

func TestRejectDeadGrabTimeNZB_NNTPPreferred_SkipsCacheProbeEntirely(t *testing.T) {
	up := &fakeGrabHealthProvider{allDead: true, sampled: 4}
	lane := &fakeCacheLaneProvider{name: "torbox", result: &provider.CheckCachedResult{Cached: true}}
	s, _, st := newGrabHealthTestServer(t, up)
	s.cfg.Routing.Preference = []string{config.LaneNNTPNZB, config.LaneTorBoxNZB}
	s.lanes = append(s.lanes, ProviderLane{Prov: lane})

	raw := "nntp-is-the-floor"
	if !s.rejectDeadGrabTimeNZB(context.Background(), []byte(raw), "tv", "NNTPFloor.S01E01") {
		t.Fatal("expected rejection unchanged when NNTP outranks TorBox")
	}
	if lane.calls != 0 {
		t.Fatalf("cache probe must not run when NNTP outranks TorBox; calls = %d", lane.calls)
	}
	blacklisted, err := st.IsBlacklisted(context.Background(), NZBBlacklistKey(raw))
	if err != nil {
		t.Fatalf("IsBlacklisted: %v", err)
	}
	if !blacklisted {
		t.Fatal("expected pre-existing blacklist behaviour to be preserved")
	}
}

func TestTorboxNZBCacheMayServe_FirstCachedLaneWinsAndStops(t *testing.T) {
	miss := &fakeCacheLaneProvider{name: "a", result: &provider.CheckCachedResult{Cached: false}}
	hit := &fakeCacheLaneProvider{name: "b", result: &provider.CheckCachedResult{Cached: true}}
	after := &fakeCacheLaneProvider{name: "c", result: &provider.CheckCachedResult{Cached: true}}
	s, _, _ := torboxPreferredServer(t, nil, miss, hit, after)

	mayServe, conclusive := s.torboxNZBCacheMayServe(context.Background(), []byte("x"))
	if !mayServe || !conclusive {
		t.Fatalf("mayServe=%v conclusive=%v, want true/true", mayServe, conclusive)
	}
	if after.calls != 0 {
		t.Fatalf("must stop at the first cached lane; trailing lane calls = %d", after.calls)
	}
}

func TestTorboxNZBCacheMayServe_CancelledContextIsInconclusive(t *testing.T) {
	lane := &fakeCacheLaneProvider{name: "torbox", result: &provider.CheckCachedResult{Cached: true}}
	s, _, _ := torboxPreferredServer(t, nil, lane)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mayServe, conclusive := s.torboxNZBCacheMayServe(ctx, []byte("x"))
	if mayServe || conclusive {
		t.Fatalf("cancelled ctx must be inconclusive; got mayServe=%v conclusive=%v", mayServe, conclusive)
	}
	if lane.calls != 0 {
		t.Fatalf("must not probe after cancellation; calls = %d", lane.calls)
	}
}

func TestTorboxNZBCacheMayServe_EmptyDataIsInconclusive(t *testing.T) {
	lane := &fakeCacheLaneProvider{name: "torbox", result: &provider.CheckCachedResult{Cached: true}}
	s, _, _ := torboxPreferredServer(t, nil, lane)
	if mayServe, conclusive := s.torboxNZBCacheMayServe(context.Background(), nil); mayServe || conclusive {
		t.Fatalf("empty data must be inconclusive; got %v/%v", mayServe, conclusive)
	}
	if lane.calls != 0 {
		t.Fatalf("must not probe on empty data; calls = %d", lane.calls)
	}
}
