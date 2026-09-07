package nntp

import (
	"context"
	"crypto/md5"
	"path/filepath"
	"sync"
	"testing"
	"time"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestPAR2ProofProducerAndSharedGraph(t *testing.T) {
	firstBytes := []byte("abcd")
	lastBytes := []byte("ef")
	first := md5.Sum(firstBytes)
	last := md5.Sum(lastBytes)
	info := archiveparser.PAR2Info{
		SliceSize: 4,
		Files: []archiveparser.PAR2FileDesc{{
			FileID: "00112233445566778899aabbccddeeff",
			Length: 6,
		}},
		IFSC: []archiveparser.PAR2IFSC{{
			FileID:   "00112233445566778899aabbccddeeff",
			BlockMD5: append(append([]byte(nil), first[:]...), last[:]...),
		}},
	}
	matches := []PAR2FileMatch{{NZBFileIdx: 3, Desc: info.Files[0]}}
	index, err := BuildPAR2ProofIndex(info, matches)
	if err != nil || index == nil {
		t.Fatalf("BuildPAR2ProofIndex = %#v, %v", index, err)
	}
	domain, ok := NewPAR2ProofDomain(index, 3)
	if !ok {
		t.Fatal("persisted proof domain rejected")
	}

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "proof.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	graph, err := contentproof.New(store.New(db), contentproof.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		offset int64
		data   []byte
	}{
		{0, firstBytes},
		{5, lastBytes},
	} {
		proof, ok := domain.ProofAt(test.offset)
		if !ok {
			t.Fatalf("ProofAt(%d) abstained", test.offset)
		}
		digest, err := proof.CandidateDigest(context.Background(), test.data)
		if err != nil {
			t.Fatal(err)
		}
		if err := graph.Record(context.Background(), proof.Evidence("nntp-item-file-3", now)); err != nil {
			t.Fatal(err)
		}
		decision, err := graph.VerifyDigest(context.Background(), "nntp-item-file-3",
			contentproof.ScopeBlock, proof.Offset, proof.Length, contentproof.AlgorithmMD5, digest)
		if err != nil || decision.Relation != contentproof.RelationProven {
			t.Fatalf("VerifyDigest = %+v, %v", decision, err)
		}
		bad := append([]byte(nil), digest...)
		bad[0] ^= 0xff
		decision, err = graph.VerifyDigest(context.Background(), "nntp-item-file-3",
			contentproof.ScopeBlock, proof.Offset, proof.Length, contentproof.AlgorithmMD5, bad)
		if err != nil || decision.Relation != contentproof.RelationConflict {
			t.Fatalf("VerifyDigest corrupt = %+v, %v", decision, err)
		}
	}

	noProof, err := graph.VerifyDigest(context.Background(), "nntp-item-file-3",
		contentproof.ScopeBlock, 99, 1, contentproof.AlgorithmMD5, make([]byte, md5.Size))
	if err != nil || noProof.Relation != contentproof.RelationNoProof {
		t.Fatalf("no-proof decision = %+v, %v", noProof, err)
	}
}

func TestBuildPAR2ProofIndexFailsClosed(t *testing.T) {
	desc := archiveparser.PAR2FileDesc{FileID: "00112233445566778899aabbccddeeff", Length: 5}
	match := PAR2FileMatch{NZBFileIdx: 1, Desc: desc}
	for _, test := range []struct {
		name string
		info archiveparser.PAR2Info
	}{
		{"missing main", archiveparser.PAR2Info{Files: []archiveparser.PAR2FileDesc{desc}}},
		{"oversized slice", archiveparser.PAR2Info{SliceSize: MaxPAR2ProofBlockBytes + 1, Files: []archiveparser.PAR2FileDesc{desc}}},
		{"missing IFSC", archiveparser.PAR2Info{SliceSize: 4, Files: []archiveparser.PAR2FileDesc{desc}}},
		{"short IFSC", archiveparser.PAR2Info{
			SliceSize: 4,
			Files:     []archiveparser.PAR2FileDesc{desc},
			IFSC:      []archiveparser.PAR2IFSC{{FileID: desc.FileID, BlockMD5: make([]byte, md5.Size)}},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			index, err := BuildPAR2ProofIndex(test.info, []PAR2FileMatch{match})
			if err != nil || index != nil {
				t.Fatalf("BuildPAR2ProofIndex = %#v, %v; want nil abstention", index, err)
			}
		})
	}
}

func TestPAR2ProofDomainFailsClosedAndCancels(t *testing.T) {
	index := &archiveparser.PAR2ProofIndex{
		SliceSize: 4,
		Files: []archiveparser.PAR2ProofFile{{
			NZBFileIndex: 1,
			Length:       5,
			BlockMD5:     make([]byte, md5.Size),
		}},
	}
	if _, ok := NewPAR2ProofDomain(index, 1); ok {
		t.Fatal("short hash index accepted")
	}
	index.Files[0].BlockMD5 = make([]byte, 2*md5.Size)
	domain, ok := NewPAR2ProofDomain(index, 1)
	if !ok {
		t.Fatal("valid proof domain rejected")
	}
	proof, ok := domain.ProofAt(0)
	if !ok {
		t.Fatal("valid proof missing")
	}
	if _, err := proof.CandidateDigest(context.Background(), []byte("abc")); err == nil {
		t.Fatal("short candidate accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := proof.CandidateDigest(ctx, []byte("abcd")); err == nil {
		t.Fatal("cancelled digest accepted")
	}
	if _, ok := domain.ProofAt(-1); ok {
		t.Fatal("negative offset accepted")
	}
	if _, ok := domain.ProofAt(5); ok {
		t.Fatal("past-EOF offset accepted")
	}
}

func TestPAR2ProofDomainConcurrentReads(t *testing.T) {
	block := []byte("abcd")
	sum := md5.Sum(block)
	index := &archiveparser.PAR2ProofIndex{
		SliceSize: 4,
		Files: []archiveparser.PAR2ProofFile{{
			NZBFileIndex: 1,
			Length:       4,
			BlockMD5:     append([]byte(nil), sum[:]...),
		}},
	}
	domain, ok := NewPAR2ProofDomain(index, 1)
	if !ok {
		t.Fatal("valid proof domain rejected")
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			proof, found := domain.ProofAt(0)
			if !found {
				t.Error("proof missing")
				return
			}
			if _, err := proof.CandidateDigest(context.Background(), block); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
