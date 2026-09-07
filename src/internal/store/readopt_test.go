package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseStableStrmURL(t *testing.T) {
	t.Parallel()
	valid := []string{
		"http://old-host:8381/stream/item-1/0?tok=0123456789abcdef0123456789abcdef",
		"https://new-host/dav/tv/Release%20Name/Episode%201.mkv",
	}
	for _, raw := range valid {
		if _, err := ParseStableStrmURL(raw); err != nil {
			t.Errorf("ParseStableStrmURL(%q): %v", raw, err)
		}
	}
	invalid := []string{
		"", " /stream/a/b", "http://user:pass@host/stream/a/b",
		"http://host/stream/a/b#fragment", "http://host/stream/a/b?unknown=x",
		"http://host/stream/a%2Fb/c", "http://host/dav/a/../c.mkv",
		"http://host/dav/a/b/c.mkv?tok=x", "http://host/other/a/b",
	}
	for _, raw := range invalid {
		if _, err := ParseStableStrmURL(raw); err == nil {
			t.Errorf("ParseStableStrmURL(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestReAdoptionScanReportsVerifiedOrphanGhostAndMalformed(t *testing.T) {
	ctx := context.Background()
	s := newSweepTestStore(t)
	root := t.TempDir()
	for _, id := range []string{"verified", "ghost", "unconsumed", "removed"} {
		mustCreateSweepItem(t, s, id)
		if id != "unconsumed" {
			if _, err := s.execWrite(ctx, "UPDATE items SET download_consumed=1 WHERE id=?", id); err != nil {
				t.Fatal(err)
			}
		}
		if id == "removed" {
			if _, err := s.execWrite(ctx, "UPDATE items SET state='removed' WHERE id=?", id); err != nil {
				t.Fatal(err)
			}
		}
		stableURL := "http://old-host:8381/stream/" + id + "/0?tok=0123456789abcdef0123456789abcdef"
		if err := s.UpsertStrmBlob(ctx, StrmBlob{ItemID: id, FileIndex: 0, RelPath: id + ".strm", URL: stableURL}); err != nil {
			t.Fatal(err)
		}
		if id == "verified" {
			if err := os.WriteFile(filepath.Join(root, id+".strm"), []byte(stableURL), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(root, "orphan.strm"), []byte("http://old-host:8381/stream/missing/0?tok=0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.strm"), []byte("first\nsecond"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReAdoptionScan(ctx, root, 100)
	if err != nil {
		t.Fatalf("ReAdoptionScan: %v", err)
	}
	if res.Verified != 1 || res.Orphans != 1 || res.Ghosts != 1 || res.Malformed != 1 {
		t.Fatalf("result = %+v", res)
	}
	for _, finding := range res.Findings {
		if strings.Contains(finding.Path, "tok=") || strings.Contains(finding.Path, "://") {
			t.Fatalf("finding leaked URL-shaped data: %+v", finding)
		}
	}
}

func TestReAdoptionScanPartialDuplicateSymlinkAndLimit(t *testing.T) {
	ctx := context.Background()
	s := newSweepTestStore(t)
	mustCreateSweepItem(t, s, "partial")
	mustCreateSweepItem(t, s, "partial-only")
	root := t.TempDir()
	stableURL := "http://host/stream/partial/0"
	for _, name := range []string{"a.strm", "b.strm"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(stableURL), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "partial.strm"), []byte("http://host/stream/partial-only/0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a.strm"), filepath.Join(root, "linked.strm")); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReAdoptionScan(ctx, root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ambiguous != 2 || res.Partial != 1 || res.StrmFiles != 3 {
		t.Fatalf("partial/duplicate/symlink result = %+v", res)
	}
	if _, err := s.ReAdoptionScan(ctx, root, 1); !errors.Is(err, ErrReAdoptionLimit) {
		t.Fatalf("limit error = %v", err)
	}
}

func TestReAdoptionScanCancellationAndOversize(t *testing.T) {
	s := newSweepTestStore(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.strm"), []byte(strings.Repeat("x", MaxStrmBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReAdoptionScan(context.Background(), root, 10)
	if err != nil || res.Malformed != 1 {
		t.Fatalf("oversize result=%+v err=%v", res, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ReAdoptionScan(ctx, root, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func FuzzParseStableStrmURL(f *testing.F) {
	f.Add("http://host/stream/item/0?tok=0123456789abcdef0123456789abcdef")
	f.Add("https://host/dav/tv/Release/Episode.mkv")
	f.Add("http://host/stream/a%2Fb/0")
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > MaxStrmBytes+1 {
			return
		}
		ref, err := ParseStableStrmURL(raw)
		if err == nil && ref.key() == "" {
			t.Fatal("successful parse returned empty key")
		}
	})
}
