package main

import (
	"os"
	"testing"
)

func TestParseMonitorFlipIDs(t *testing.T) {
	ids, err := parseMonitorFlipIDs("1,20,300")
	if err != nil || len(ids) != 3 || ids[0] != 1 || ids[1] != 20 || ids[2] != 300 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	for _, raw := range []string{"", "0", "-1", "1,", ",1", "1, 2", "1,1", "x"} {
		if _, err := parseMonitorFlipIDs(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestMonitorFlipLockExcludesConcurrentRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := acquireMonitorFlipLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMonitorFlipLock(first)
	if second, err := acquireMonitorFlipLock(dir); err == nil {
		releaseMonitorFlipLock(second)
		t.Fatal("second lock acquired")
	}
	releaseMonitorFlipLock(first)
	first = nil
	third, err := acquireMonitorFlipLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	releaseMonitorFlipLock(third)
}

func FuzzParseMonitorFlipIDs(f *testing.F) {
	for _, seed := range []string{"1", "1,2,3", "0", "1, 2", "1,1"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		ids, err := parseMonitorFlipIDs(raw)
		if err != nil {
			return
		}
		if len(ids) == 0 || len(ids) > 1000 {
			t.Fatalf("accepted length %d", len(ids))
		}
		seen := make(map[int]struct{}, len(ids))
		for _, id := range ids {
			if id < 1 {
				t.Fatalf("accepted id %d", id)
			}
			if _, exists := seen[id]; exists {
				t.Fatalf("accepted duplicate id %d", id)
			}
			seen[id] = struct{}{}
		}
	})
}
