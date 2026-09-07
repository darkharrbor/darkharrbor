package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// newProofTestServer wires a *Server against a real, migrated, on-disk
// SQLite store and a real contentproof.Graph -- HR2.7's own record/reject
// logic is security-sensitive (it feeds the shared proof graph other lanes
// will later trust), so it is proven against the real repository, the same
// pattern newHLSTestServer already established for hlssession.Graph.
func newProofTestServer(t *testing.T) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "httpproof-api-test.db")
	db, err := store.Open(context.Background(), path, 5*time.Second)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	st := store.New(db)

	graph, err := contentproof.New(st, contentproof.Options{})
	if err != nil {
		t.Fatalf("contentproof.New: %v", err)
	}
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.store = st
	s.httpProofGraph = graph
	return s
}

func TestRecordAuthoritativeDigestAcceptsFirstThenIdempotent(t *testing.T) {
	s := newProofTestServer(t)
	ctx := context.Background()
	repID := "http:testrepr0000000000000000000000000000000000000000000000000000"
	digest := httpstream.SourceDigest{Algorithm: "sha256", Hex: hexOfString("hello")}

	s.recordAuthoritativeDigest(ctx, repID, 1234, contentproof.ProvenanceIADigest, digest, s.nowUTC())
	s.recordAuthoritativeDigest(ctx, repID, 1234, contentproof.ProvenanceIADigest, digest, s.nowUTC())

	proofs, err := s.store.ListContentProofs(ctx, []string{repID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListContentProofs: %v", err)
	}
	if len(proofs) != 1 {
		t.Fatalf("expected exactly one recorded proof after idempotent re-record, got %d", len(proofs))
	}
	if proofs[0].Provenance != contentproof.ProvenanceIADigest || proofs[0].Kind != contentproof.KindAuthoritative {
		t.Fatalf("unexpected proof shape: %+v", proofs[0])
	}
}

func TestRecordAuthoritativeDigestRejectsConflict(t *testing.T) {
	s := newProofTestServer(t)
	ctx := context.Background()
	repID := "http:testrepr1111111111111111111111111111111111111111111111111111"

	first := httpstream.SourceDigest{Algorithm: "sha256", Hex: hexOfString("hello")}
	second := httpstream.SourceDigest{Algorithm: "sha256", Hex: hexOfString("goodbye")}

	s.recordAuthoritativeDigest(ctx, repID, 1234, contentproof.ProvenanceIADigest, first, s.nowUTC())
	s.recordAuthoritativeDigest(ctx, repID, 1234, contentproof.ProvenanceMetalink, second, s.nowUTC())

	proofs, err := s.store.ListContentProofs(ctx, []string{repID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListContentProofs: %v", err)
	}
	if len(proofs) != 1 {
		t.Fatalf("conflicting digest must be rejected, not appended: got %d proofs", len(proofs))
	}
	if proofs[0].Provenance != contentproof.ProvenanceIADigest {
		t.Fatalf("first-recorded authoritative proof must survive a later conflict, got provenance %q", proofs[0].Provenance)
	}
}

func TestRecordAuthoritativeDigestDropsMalformed(t *testing.T) {
	s := newProofTestServer(t)
	ctx := context.Background()
	repID := "http:testrepr2222222222222222222222222222222222222222222222222222"

	s.recordAuthoritativeDigest(ctx, repID, 1234, contentproof.ProvenanceIADigest, httpstream.SourceDigest{Algorithm: "md5", Hex: "not-hex"}, s.nowUTC())
	s.recordAuthoritativeDigest(ctx, repID, 1234, contentproof.ProvenanceIADigest, httpstream.SourceDigest{Algorithm: "unknown", Hex: hexOfString("x")}, s.nowUTC())

	proofs, err := s.store.ListContentProofs(ctx, []string{repID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListContentProofs: %v", err)
	}
	if len(proofs) != 0 {
		t.Fatalf("malformed digests must never reach the graph, got %d proofs", len(proofs))
	}
}

func TestRecordSameOriginETagAcceptsThenRejectsConflict(t *testing.T) {
	s := newProofTestServer(t)
	ctx := context.Background()
	repID := "http:testrepr3333333333333333333333333333333333333333333333333333"

	s.recordSameOriginETag(ctx, repID, 999, `"etag-v1"`, "backend-a", "generic", s.nowUTC())
	proofs, err := s.store.ListContentProofs(ctx, []string{repID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListContentProofs: %v", err)
	}
	if len(proofs) != 1 || proofs[0].Kind != contentproof.KindSameOrigin {
		t.Fatalf("expected exactly one same-origin proof, got %+v", proofs)
	}
	firstDigest := append([]byte(nil), proofs[0].Digest...)

	// Same ETag observed again: no-op, not a duplicate/conflict.
	s.recordSameOriginETag(ctx, repID, 999, `"etag-v1"`, "backend-a", "generic", s.nowUTC())
	proofs, err = s.store.ListContentProofs(ctx, []string{repID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListContentProofs: %v", err)
	}
	if len(proofs) != 1 {
		t.Fatalf("re-observing the identical ETag must not duplicate evidence, got %d", len(proofs))
	}

	// A changed ETag for the exact same representation+origin+span is a
	// conflict: rejected, not overwritten.
	s.recordSameOriginETag(ctx, repID, 999, `"etag-v2-changed"`, "backend-a", "generic", s.nowUTC())
	proofs, err = s.store.ListContentProofs(ctx, []string{repID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListContentProofs: %v", err)
	}
	if len(proofs) != 1 {
		t.Fatalf("changed ETag must be rejected, not appended: got %d proofs", len(proofs))
	}
	if string(proofs[0].Digest) != string(firstDigest) {
		t.Fatalf("original ETag evidence must survive a later conflicting observation")
	}
}

func TestRecordSameOriginETagUnrelatedOriginNeverConflicts(t *testing.T) {
	s := newProofTestServer(t)
	ctx := context.Background()
	repID := "http:testrepr4444444444444444444444444444444444444444444444444444"

	s.recordSameOriginETag(ctx, repID, 500, `"etag-a"`, "backend-a", "generic", s.nowUTC())
	s.recordSameOriginETag(ctx, repID, 500, `"etag-b-different-origin"`, "backend-b", "ia", s.nowUTC())

	proofs, err := s.store.ListContentProofs(ctx, []string{repID}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListContentProofs: %v", err)
	}
	if len(proofs) != 2 {
		t.Fatalf("two genuinely unrelated origins must both be recorded, never treated as conflicting, got %d", len(proofs))
	}
	if proofs[0].OriginID == proofs[1].OriginID {
		t.Fatalf("distinct backend/handler pairs must not collapse into the same OriginID")
	}
}

func TestRecordHTTPSourceProofsNilGraphIsNoop(t *testing.T) {
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	// s.httpProofGraph and s.store are both nil here -- must never panic.
	s.recordHTTPSourceProofs(context.Background(), "http:whatever", 100,
		[]httpstream.SourceDigest{{Algorithm: "sha256", Hex: hexOfString("x")}},
		http.Header{"Etag": []string{`"e"`}}, "b", "ia")
}

func hexOfString(s string) string {
	sum := sha256.Sum256([]byte(s))
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, v := range sum {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0xf]
	}
	return string(out)
}
