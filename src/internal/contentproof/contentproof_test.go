package contentproof_test

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestGraphProofUpgradeConflictAndDowngrade(t *testing.T) {
	graph, st, now := newGraph(t, 8)
	ctx := context.Background()
	leftTOFU := proof("left", contentproof.KindTOFU, contentproof.ProvenancePlaybackObserved, []byte("same bytes"), *now)
	rightTOFU := proof("right", contentproof.KindTOFU, contentproof.ProvenancePlaybackObserved, []byte("same bytes"), *now)
	record(t, graph, leftTOFU, rightTOFU)
	assertRelation(t, graph, contentproof.RelationNoProof)

	leftAuth := proof("left", contentproof.KindAuthoritative, contentproof.ProvenanceIADigest, []byte("same bytes"), *now)
	rightAuth := proof("right", contentproof.KindAuthoritative, contentproof.ProvenanceHTTPDigest, []byte("same bytes"), *now)
	record(t, graph, leftAuth, rightAuth)
	assertRelation(t, graph, contentproof.RelationProven)

	conflict := proof("right", contentproof.KindAuthoritative, contentproof.ProvenanceMetalink, []byte("different bytes"), *now)
	record(t, graph, conflict)
	assertRelation(t, graph, contentproof.RelationConflict)

	if err := graph.Revoke(ctx, keyOf(conflict)); err != nil {
		t.Fatalf("Revoke conflict: %v", err)
	}
	assertRelation(t, graph, contentproof.RelationProven)
	if err := graph.Revoke(ctx, keyOf(rightAuth)); err != nil {
		t.Fatalf("Revoke authoritative proof: %v", err)
	}
	assertRelation(t, graph, contentproof.RelationNoProof)

	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM content_proofs`).Scan(&rows); err != nil {
		t.Fatalf("count persisted proofs: %v", err)
	}
	if rows != 3 {
		t.Fatalf("persisted proofs = %d, want 3", rows)
	}
}

func TestGraphBlockBoundsExpiryAndRestartPersistence(t *testing.T) {
	graph, st, now := newGraph(t, 2)
	ctx := context.Background()
	expires := now.Add(time.Minute)
	left := proof("left", contentproof.KindAuthoritative, contentproof.ProvenanceHTTPDigest, []byte("block"), *now)
	left.Scope, left.Offset, left.Length, left.ExpiresAt = contentproof.ScopeBlock, 1024, 4096, &expires
	right := proof("right", contentproof.KindAuthoritative, contentproof.ProvenanceHTTPDigest, []byte("block"), *now)
	right.Scope, right.Offset, right.Length, right.ExpiresAt = contentproof.ScopeBlock, 1024, 4096, &expires
	record(t, graph, left, right)

	decision, err := graph.Compare(ctx, "left", "right", 2048, 1024)
	if err != nil || decision.Relation != contentproof.RelationProven {
		t.Fatalf("contained block Compare = %+v, %v", decision, err)
	}
	decision, err = graph.Compare(ctx, "left", "right", 0, 8192)
	if err != nil || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("cross-boundary Compare = %+v, %v", decision, err)
	}

	// A new graph over the same store models a daemon restart.
	restarted, err := contentproof.New(st, contentproof.Options{MaxProofsPerRepresentation: 2, Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatalf("New restarted graph: %v", err)
	}
	decision, err = restarted.Compare(ctx, "left", "right", 2048, 1024)
	if err != nil || decision.Relation != contentproof.RelationProven {
		t.Fatalf("restart Compare = %+v, %v", decision, err)
	}

	*now = now.Add(2 * time.Minute)
	decision, err = restarted.Compare(ctx, "left", "right", 2048, 1024)
	if err != nil || decision.Relation != contentproof.RelationNoProof {
		t.Fatalf("expired Compare = %+v, %v", decision, err)
	}

	// The per-representation bound retains only the newest two assertions.
	for i, provenance := range []contentproof.Provenance{
		contentproof.ProvenanceIADigest, contentproof.ProvenanceHTTPDigest, contentproof.ProvenanceMetalink,
	} {
		e := proof("bounded", contentproof.KindAuthoritative, provenance, []byte{byte(i)}, now.Add(time.Duration(i)*time.Second))
		if err := graph.Record(ctx, e); err != nil {
			t.Fatalf("Record bounded proof %d: %v", i, err)
		}
	}
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM content_proofs WHERE representation_id='bounded'`).Scan(&rows); err != nil {
		t.Fatalf("count bounded proofs: %v", err)
	}
	if rows != 2 {
		t.Fatalf("bounded proof rows = %d, want 2", rows)
	}
}

func TestGraphVerifiesTorrentAndPAR2KnownAnswers(t *testing.T) {
	graph, _, now := newGraph(t, 8)
	ctx := context.Background()
	data := []byte("real candidate block bytes")

	torrentDigest := sha1.Sum(data)
	torrent := contentproof.Evidence{
		RepresentationID: "torrent-representation", Scope: contentproof.ScopeBlock, Offset: 0, Length: int64(len(data)),
		Kind: contentproof.KindAuthoritative, Algorithm: contentproof.AlgorithmSHA1, Digest: torrentDigest[:],
		Provenance: contentproof.ProvenanceTorrentPiece, ObservedAt: *now,
	}
	par2Digest := md5.Sum(data)
	par2 := contentproof.Evidence{
		RepresentationID: "nntp-representation", Scope: contentproof.ScopeBlock, Offset: 0, Length: int64(len(data)),
		Kind: contentproof.KindAuthoritative, Algorithm: contentproof.AlgorithmMD5, Digest: par2Digest[:],
		Provenance: contentproof.ProvenancePAR2IFSC, ObservedAt: *now,
	}
	record(t, graph, torrent, par2)

	for _, tc := range []struct {
		id        string
		algorithm contentproof.Algorithm
		digest    []byte
	}{
		{"torrent-representation", contentproof.AlgorithmSHA1, torrentDigest[:]},
		{"nntp-representation", contentproof.AlgorithmMD5, par2Digest[:]},
	} {
		decision, err := graph.VerifyDigest(ctx, tc.id, contentproof.ScopeBlock, 0, int64(len(data)), tc.algorithm, tc.digest)
		if err != nil || decision.Relation != contentproof.RelationProven {
			t.Fatalf("VerifyDigest(%s) = %+v, %v", tc.id, decision, err)
		}
		bad := append([]byte(nil), tc.digest...)
		bad[0] ^= 0xff
		decision, err = graph.VerifyDigest(ctx, tc.id, contentproof.ScopeBlock, 0, int64(len(data)), tc.algorithm, bad)
		if err != nil || decision.Relation != contentproof.RelationConflict {
			t.Fatalf("VerifyDigest corrupt(%s) = %+v, %v", tc.id, decision, err)
		}
	}
}

func TestVerifyDigestFailsClosedOnContradictoryAuthoritativeProofs(t *testing.T) {
	graph, _, now := newGraph(t, 8)
	data := []byte("candidate bytes")
	match := sha256.Sum256(data)
	conflict := sha256.Sum256([]byte("different bytes"))
	record(t, graph,
		contentproof.Evidence{RepresentationID: "contradictory", Scope: contentproof.ScopeWhole, Length: int64(len(data)), Kind: contentproof.KindAuthoritative, Algorithm: contentproof.AlgorithmSHA256, Digest: match[:], Provenance: contentproof.ProvenanceHTTPDigest, ObservedAt: *now},
		contentproof.Evidence{RepresentationID: "contradictory", Scope: contentproof.ScopeWhole, Length: int64(len(data)), Kind: contentproof.KindAuthoritative, Algorithm: contentproof.AlgorithmSHA256, Digest: conflict[:], Provenance: contentproof.ProvenanceMetalink, ObservedAt: *now},
	)
	decision, err := graph.VerifyDigest(context.Background(), "contradictory", contentproof.ScopeWhole, 0, int64(len(data)), contentproof.AlgorithmSHA256, match[:])
	if err != nil || decision.Relation != contentproof.RelationConflict {
		t.Fatalf("VerifyDigest contradictory = %+v, %v", decision, err)
	}
}

func TestGraphRejectsURLShapedStateAndHonorsCancellation(t *testing.T) {
	graph, st, now := newGraph(t, 8)
	e := proof("https://origin.invalid/file?tok=secret", contentproof.KindAuthoritative, contentproof.ProvenanceHTTPDigest, []byte("bytes"), *now)
	err := graph.Record(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "opaque secret-free ID") {
		t.Fatalf("Record URL-shaped ID error = %v", err)
	}
	e = proof(strings.Repeat("a", 129), contentproof.KindAuthoritative, contentproof.ProvenanceHTTPDigest, []byte("bytes"), *now)
	if err := graph.Record(context.Background(), e); err == nil || !strings.Contains(err.Error(), "opaque secret-free ID") {
		t.Fatalf("Record oversized ID error = %v", err)
	}
	e = proof("safe-id", contentproof.KindAuthoritative, contentproof.ProvenanceHTTPDigest, []byte("bytes"), *now)
	e.Digest = make([]byte, 65)
	if err := graph.Record(context.Background(), e); err == nil || !strings.Contains(err.Error(), "digest length") {
		t.Fatalf("Record oversized digest error = %v", err)
	}

	e = proof("safe-id", contentproof.KindSameOrigin, contentproof.ProvenanceSameOriginETag, []byte("etag"), *now)
	e.Algorithm = contentproof.AlgorithmETAGSHA256
	e.OriginID = "https://origin.invalid"
	err = graph.Record(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "opaque secret-free ID") {
		t.Fatalf("Record URL-shaped origin error = %v", err)
	}

	e = proof("safe-id", contentproof.KindAuthoritative, contentproof.ProvenancePAR2IFSC, []byte("bytes"), *now)
	if err := graph.Record(context.Background(), e); err == nil || !strings.Contains(err.Error(), "kind/provenance") {
		t.Fatalf("Record invalid PAR2 proof error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := graph.Compare(ctx, "left", "right", 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Compare canceled error = %v", err)
	}
	if err := graph.Record(ctx, proof("safe-id", contentproof.KindAuthoritative, contentproof.ProvenanceIADigest, []byte("bytes"), *now)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Record canceled error = %v", err)
	}

	var leaked int
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM content_proofs
		WHERE representation_id LIKE '%http%' OR representation_id LIKE '%tok=%'
		   OR origin_id LIKE '%http%' OR origin_id LIKE '%tok=%'`).Scan(&leaked); err != nil {
		t.Fatalf("redaction scan: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("URL/token-shaped durable rows = %d, want 0", leaked)
	}
}

func newGraph(t *testing.T, max int) (*contentproof.Graph, *store.Store, *time.Time) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "proof.db"), time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	st := store.New(db)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	graph, err := contentproof.New(st, contentproof.Options{MaxProofsPerRepresentation: max, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return graph, st, &now
}

func proof(id string, kind contentproof.Kind, provenance contentproof.Provenance, data []byte, observed time.Time) contentproof.Evidence {
	digest := sha256.Sum256(data)
	return contentproof.Evidence{
		RepresentationID: id,
		Scope:            contentproof.ScopeWhole, Length: 8192,
		Kind: kind, Algorithm: contentproof.AlgorithmSHA256, Digest: digest[:],
		Provenance: provenance, ObservedAt: observed,
	}
}

func keyOf(e contentproof.Evidence) contentproof.Key {
	return contentproof.Key{
		RepresentationID: e.RepresentationID, Scope: e.Scope, Offset: e.Offset, Length: e.Length,
		Kind: e.Kind, Algorithm: e.Algorithm, Provenance: e.Provenance, OriginID: e.OriginID,
	}
}

func record(t *testing.T, graph *contentproof.Graph, evidence ...contentproof.Evidence) {
	t.Helper()
	for _, e := range evidence {
		if err := graph.Record(context.Background(), e); err != nil {
			t.Fatalf("Record(%s/%s): %v", e.RepresentationID, e.Provenance, err)
		}
	}
}

func assertRelation(t *testing.T, graph *contentproof.Graph, want contentproof.Relation) {
	t.Helper()
	decision, err := graph.Compare(context.Background(), "left", "right", 0, 8192)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if decision.Relation != want {
		t.Fatalf("Compare relation = %s, want %s", decision.Relation, want)
	}
}
