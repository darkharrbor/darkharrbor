package reactivedispatch

import (
	"testing"
	"time"
)

func TestEntryBackoffDelayDoublesToCap(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, w := range want {
		if got := entryBackoffDelay(i + 1); got != w {
			t.Errorf("fails=%d got %v want %v", i+1, got, w)
		}
	}
	if got := entryBackoffDelay(40); got != maxEntryBackoff {
		t.Errorf("cap: got %v", got)
	}
}

func TestDeferredUntilBackoffElapses(t *testing.T) {
	now := time.Date(2026, 10, 17, 19, 0, 0, 0, time.UTC)
	d := &Dispatcher{now: func() time.Time { return now }}
	if d.deferred("rep", now) {
		t.Fatal("deferred before any failure")
	}
	d.noteFailure("rep", now)
	if !d.deferred("rep", now.Add(10*time.Second)) {
		t.Fatal("not deferred inside first backoff")
	}
	if d.deferred("rep", now.Add(31*time.Second)) {
		t.Fatal("still deferred after first backoff")
	}
	d.noteFailure("rep", now)
	if !d.deferred("rep", now.Add(45*time.Second)) {
		t.Fatal("second failure did not double the backoff")
	}
	d.clearBackoff("rep")
	if d.deferred("rep", now) {
		t.Fatal("deferred after clear")
	}
}
