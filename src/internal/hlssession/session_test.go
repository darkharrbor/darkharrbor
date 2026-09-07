package hlssession

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeRepo is a deterministic in-memory Repository for unit tests. It
// mirrors the store package's SQL semantics closely enough to exercise the
// Graph's own logic (validation, ID minting, capacity, expiry-aware
// resolution) without a real database.
type fakeRepo struct {
	mu        sync.Mutex
	sessions  map[string]Session
	resources map[string]Resource // resourceID -> resource
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{sessions: map[string]Session{}, resources: map[string]Resource{}}
}

func (r *fakeRepo) InsertSession(_ context.Context, s Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[s.SessionID] = s
	return nil
}

func (r *fakeRepo) GetSession(_ context.Context, sessionID string, now time.Time) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[sessionID]
	if !ok || !s.ExpiresAt.After(now) {
		return nil, nil
	}
	cp := s
	return &cp, nil
}

func (r *fakeRepo) TouchSession(_ context.Context, sessionID string, expiresAt time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[sessionID]
	if !ok {
		return false, nil
	}
	s.ExpiresAt = expiresAt
	r.sessions[sessionID] = s
	return true, nil
}

func (r *fakeRepo) InsertResource(_ context.Context, res Resource, maxPerSession int, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.sessions[res.SessionID]
	if !ok || !sess.ExpiresAt.After(now) {
		return ErrSessionNotFound
	}
	count := 0
	for _, existing := range r.resources {
		if existing.SessionID == res.SessionID {
			count++
		}
	}
	if count >= maxPerSession {
		return ErrSessionResourceCapacity
	}
	r.resources[res.ResourceID] = res
	return nil
}

func (r *fakeRepo) GetResource(_ context.Context, resourceID string, now time.Time) (*Resource, *Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.resources[resourceID]
	if !ok {
		return nil, nil, nil
	}
	sess, ok := r.sessions[res.SessionID]
	if !ok || !sess.ExpiresAt.After(now) {
		return nil, nil, nil
	}
	rcp, scp := res, sess
	return &rcp, &scp, nil
}

func (r *fakeRepo) ListResources(_ context.Context, sessionID string, now time.Time) ([]Resource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.sessions[sessionID]
	if !ok || !sess.ExpiresAt.After(now) {
		return nil, nil
	}
	var out []Resource
	for _, res := range r.resources {
		if res.SessionID == sessionID {
			out = append(out, res)
		}
	}
	return out, nil
}

func (r *fakeRepo) DeleteResources(_ context.Context, sessionID string, resourceIDs []string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	deleted := 0
	for _, id := range resourceIDs {
		if res, ok := r.resources[id]; ok && res.SessionID == sessionID {
			delete(r.resources, id)
			deleted++
		}
	}
	return deleted, nil
}

func (r *fakeRepo) PruneExpired(_ context.Context, now time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, s := range r.sessions {
		if !s.ExpiresAt.After(now) {
			delete(r.sessions, id)
			for rid, res := range r.resources {
				if res.SessionID == id {
					delete(r.resources, rid)
				}
			}
			n++
		}
	}
	return n, nil
}

func testGraph(t *testing.T, clock func() time.Time) (*Graph, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	g, err := New(repo, Options{Now: clock, MaxResourcesPerSession: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g, repo
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestCreateSessionAndRegisterResourceHappyPath(t *testing.T) {
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	g, _ := testGraph(t, fixedClock(now))
	ctx := context.Background()

	sess, err := g.CreateSession(ctx, "item-1", "hffileidfileid")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !ValidSessionID(sess.SessionID) {
		t.Fatalf("session id %q does not match expected shape", sess.SessionID)
	}
	if !sess.ExpiresAt.After(now) {
		t.Fatalf("expected expiry after now")
	}

	bs, be := NewWholeReference()
	ref := Reference{Handler: "ia", Selector: "sel-1", Ordinal: 0, ByteStart: bs, ByteEnd: be}
	res, err := g.RegisterResource(ctx, sess.SessionID, KindMaster, ref)
	if err != nil {
		t.Fatalf("RegisterResource: %v", err)
	}
	if !ValidResourceID(res.ResourceID) {
		t.Fatalf("resource id %q does not match expected shape", res.ResourceID)
	}

	got, err := g.Resolve(ctx, res.ResourceID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reference.Handler != "ia" || got.Reference.Selector != "sel-1" {
		t.Fatalf("unexpected resolved reference: %+v", got.Reference)
	}

	list, err := g.ListResources(ctx, sess.SessionID)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(list) != 1 || list[0].ResourceID != res.ResourceID {
		t.Fatalf("unexpected resource list: %+v", list)
	}
}

func TestResolveUnknownResourceIsNotFound(t *testing.T) {
	g, _ := testGraph(t, fixedClock(time.Now()))
	_, err := g.Resolve(context.Background(), "hz000000000000")
	if !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("expected ErrResourceNotFound, got %v", err)
	}
}

func TestRegisterResourceOnExpiredSessionFails(t *testing.T) {
	start := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	var now time.Time = start
	g, _ := testGraph(t, func() time.Time { return now })
	ctx := context.Background()

	sess, err := g.CreateSession(ctx, "item-1", "file-1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	now = sess.ExpiresAt.Add(time.Second) // advance past expiry

	_, err = g.RegisterResource(ctx, sess.SessionID, KindSegment, Reference{Handler: "ia"})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound on expired session, got %v", err)
	}
}

func TestRegisterResourceCapacityBound(t *testing.T) {
	g, _ := testGraph(t, fixedClock(time.Now())) // MaxResourcesPerSession: 4 from testGraph
	ctx := context.Background()
	sess, err := g.CreateSession(ctx, "item-1", "file-1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := g.RegisterResource(ctx, sess.SessionID, KindSegment, Reference{Handler: "ia", Ordinal: i}); err != nil {
			t.Fatalf("RegisterResource %d: %v", i, err)
		}
	}
	_, err = g.RegisterResource(ctx, sess.SessionID, KindSegment, Reference{Handler: "ia", Ordinal: 4})
	if !errors.Is(err, ErrSessionResourceCapacity) {
		t.Fatalf("expected ErrSessionResourceCapacity, got %v", err)
	}
}

func TestResolveSlidesSessionExpiryForward(t *testing.T) {
	start := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	var now time.Time = start
	g, repo := testGraph(t, func() time.Time { return now })
	ctx := context.Background()

	sess, err := g.CreateSession(ctx, "item-1", "file-1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	res, err := g.RegisterResource(ctx, sess.SessionID, KindSegment, Reference{Handler: "ia"})
	if err != nil {
		t.Fatalf("RegisterResource: %v", err)
	}

	// Advance close to (but before) expiry, then resolve — expiry should
	// slide forward so the session remains alive well past the original
	// expiry, proving an active playback attempt is not starved by
	// wall-clock age alone.
	now = sess.ExpiresAt.Add(-time.Minute)
	if _, err := g.Resolve(ctx, res.ResourceID); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	now = sess.ExpiresAt.Add(time.Minute) // would have expired without the slide
	if _, err := g.Resolve(ctx, res.ResourceID); err != nil {
		t.Fatalf("expected Resolve to still succeed after slide, got %v", err)
	}

	repo.mu.Lock()
	stored := repo.sessions[sess.SessionID]
	repo.mu.Unlock()
	if !stored.ExpiresAt.After(sess.ExpiresAt) {
		t.Fatalf("expected session expiry to have slid forward, original=%v stored=%v", sess.ExpiresAt, stored.ExpiresAt)
	}
}

func TestCreateSessionRejectsMalformedIDs(t *testing.T) {
	g, _ := testGraph(t, fixedClock(time.Now()))
	ctx := context.Background()
	cases := []struct{ itemID, fileID string }{
		{"", "file-1"},
		{"item-1", ""},
		{"http://evil.example/x", "file-1"},
		{"item-1", "has spaces"},
	}
	for _, c := range cases {
		if _, err := g.CreateSession(ctx, c.itemID, c.fileID); err == nil {
			t.Fatalf("expected rejection for itemID=%q fileID=%q", c.itemID, c.fileID)
		}
	}
}

func TestRegisterResourceRejectsURLShapedFields(t *testing.T) {
	g, _ := testGraph(t, fixedClock(time.Now()))
	ctx := context.Background()
	sess, err := g.CreateSession(ctx, "item-1", "file-1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cases := []Reference{
		{Handler: ""},
		{Handler: "http://evil.example"},
		{Handler: "ia", Selector: "https://evil.example/segment.ts"},
		{Handler: "ia", Ordinal: -1},
		{Handler: "ia", BackendID: "not a token!!"},
	}
	for _, ref := range cases {
		if _, err := g.RegisterResource(ctx, sess.SessionID, KindSegment, ref); err == nil {
			t.Fatalf("expected rejection for reference %+v", ref)
		}
	}
}

func TestRegisterResourceRejectsInvalidByteRange(t *testing.T) {
	g, _ := testGraph(t, fixedClock(time.Now()))
	ctx := context.Background()
	sess, err := g.CreateSession(ctx, "item-1", "file-1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ref := Reference{Handler: "ia", ByteStart: 10, ByteEnd: 5}
	if _, err := g.RegisterResource(ctx, sess.SessionID, KindSegment, ref); err == nil {
		t.Fatal("expected rejection for ByteEnd < ByteStart")
	}
}

func TestRegisterResourceRejectsInvalidKind(t *testing.T) {
	g, _ := testGraph(t, fixedClock(time.Now()))
	ctx := context.Background()
	sess, err := g.CreateSession(ctx, "item-1", "file-1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := g.RegisterResource(ctx, sess.SessionID, ResourceKind("bogus"), Reference{Handler: "ia"}); err == nil {
		t.Fatal("expected rejection for unknown resource kind")
	}
}

func TestReferenceMarshalRoundTripAndSizeBound(t *testing.T) {
	ref := Reference{
		Handler: "ia", BackendID: "backend-1", Selector: "sel", Ordinal: 3, ByteStart: 0, ByteEnd: 1023,
		MediaSequence: 12, HasMediaSequence: true, PartIndex: 2, HasPartIndex: true,
		PathwayID: "CDN-A_1",
	}
	encoded, err := MarshalReference(ref)
	if err != nil {
		t.Fatalf("MarshalReference: %v", err)
	}
	decoded, err := UnmarshalReference(encoded)
	if err != nil {
		t.Fatalf("UnmarshalReference: %v", err)
	}
	if decoded != ref {
		t.Fatalf("round-trip mismatch: got %+v want %+v", decoded, ref)
	}
	report := Reference{Handler: "ia", RenditionIndex: 1, HasRenditionIndex: true}
	encoded, err = MarshalReference(report)
	if err != nil {
		t.Fatalf("MarshalReference rendition: %v", err)
	}
	decoded, err = UnmarshalReference(encoded)
	if err != nil || decoded != report {
		t.Fatalf("rendition round trip = %+v, %v; want %+v", decoded, err, report)
	}

	huge := Reference{Handler: "ia", Selector: string(make([]byte, maxReferenceJSONBytes))}
	if _, err := MarshalReference(huge); err == nil {
		t.Fatal("expected oversized reference to be rejected")
	}
}

func TestReferenceRejectsInvalidLowLatencyCoordinates(t *testing.T) {
	cases := []Reference{
		{Handler: "ia", PartIndex: -1, HasPartIndex: true, HasMediaSequence: true},
		{Handler: "ia", PartIndex: 1, HasPartIndex: true},
		{Handler: "ia", RenditionIndex: -1, HasRenditionIndex: true},
		{Handler: "ia", HasMediaSequence: true, HasRenditionIndex: true},
	}
	for _, ref := range cases {
		if err := validateReference(ref); err == nil {
			t.Fatalf("validateReference accepted invalid LL-HLS coordinate %+v", ref)
		}
	}
}

func TestReferenceMediaTimeCoordinates(t *testing.T) {
	valid := Reference{Handler: "ia", ByteStart: -1, ByteEnd: -1, MediaStartNS: 1, MediaEndNS: 2, HasMediaTime: true, MediaDurationNS: 3, HasMediaDuration: true}
	encoded, err := MarshalReference(valid)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalReference(encoded)
	if err != nil || decoded != valid {
		t.Fatalf("media-time round trip=%+v err=%v", decoded, err)
	}
	for _, ref := range []Reference{
		{Handler: "ia", ByteStart: -1, ByteEnd: -1, MediaStartNS: -1, MediaEndNS: 2, HasMediaTime: true},
		{Handler: "ia", ByteStart: -1, ByteEnd: -1, MediaStartNS: 2, MediaEndNS: 2, HasMediaTime: true},
		{Handler: "ia", ByteStart: -1, ByteEnd: -1, MediaEndNS: 2},
		{Handler: "ia", ByteStart: -1, ByteEnd: -1, MediaEndNS: 2, HasMediaTime: true, HasMediaSequence: true},
		{Handler: "ia", ByteStart: -1, ByteEnd: -1, MediaStartNS: 1, MediaEndNS: 2, HasMediaTime: true, MediaDurationNS: 1, HasMediaDuration: true},
		{Handler: "ia", ByteStart: -1, ByteEnd: -1, MediaStartNS: 1, MediaEndNS: 2, HasMediaTime: true, MediaDurationNS: 3},
		{Handler: "ia", ByteStart: -1, ByteEnd: -1, MediaDurationNS: 3, HasMediaDuration: true},
	} {
		if err := validateReference(ref); err == nil {
			t.Fatalf("accepted invalid media-time coordinate %+v", ref)
		}
	}
}

func TestReferenceRejectsURLShapedPathwayID(t *testing.T) {
	for _, pathwayID := range []string{"https://invalid.example/path", "bad:path", "bad/path"} {
		ref := Reference{
			Handler: "ia", ByteStart: -1, ByteEnd: -1,
			PathwayID: pathwayID,
		}
		if err := validateReference(ref); err == nil {
			t.Fatal("validateReference accepted invalid pathway ID")
		}
	}
}

func TestCreateSessionOpportunisticallyPrunesExpired(t *testing.T) {
	start := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	var now time.Time = start
	g, repo := testGraph(t, func() time.Time { return now })
	ctx := context.Background()

	old, err := g.CreateSession(ctx, "item-old", "file-old")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	now = old.ExpiresAt.Add(time.Minute)

	if _, err := g.CreateSession(ctx, "item-new", "file-new"); err != nil {
		t.Fatalf("CreateSession (new): %v", err)
	}

	repo.mu.Lock()
	_, stillThere := repo.sessions[old.SessionID]
	repo.mu.Unlock()
	if stillThere {
		t.Fatal("expected expired session to be pruned by the next CreateSession call")
	}
}

func TestPruneExpiredExplicit(t *testing.T) {
	start := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	var now time.Time = start
	g, _ := testGraph(t, func() time.Time { return now })
	ctx := context.Background()

	sess, err := g.CreateSession(ctx, "item-1", "file-1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	now = sess.ExpiresAt.Add(time.Minute)
	n, err := g.PruneExpired(ctx)
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 pruned session, got %d", n)
	}
}

func TestSessionAndResolveRejectCancelledContext(t *testing.T) {
	g, _ := testGraph(t, fixedClock(time.Now()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.CreateSession(ctx, "item-1", "file-1"); err == nil {
		t.Fatal("expected cancelled context to be rejected")
	}
	if _, err := g.Resolve(ctx, "hz000000000000"); err == nil {
		t.Fatal("expected cancelled context to be rejected")
	}
	if _, err := g.Session(ctx, "hs000000000000"); err == nil {
		t.Fatal("expected cancelled context to be rejected")
	}
}

func TestNewRejectsNilRepository(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("expected nil repository to be rejected")
	}
}
