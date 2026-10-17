package rangecache

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestReadaheadWindowBytesCap(t *testing.T) {
	mk := func(n int, size int64) []ChunkRef {
		refs := make([]ChunkRef, n)
		for i := range refs {
			refs[i] = ChunkRef{Index: i, Size: size}
		}
		return refs
	}
	const mb = int64(1 << 20)
	cases := []struct {
		name     string
		segs     int
		maxBytes int64
		refs     []ChunkRef
		want     int
	}{
		{"cdn 16MB chunks capped by bytes", 128, 256 * mb, mk(500, 16*mb), 16},
		{"nntp 700KB segments keep segment cap", 128, 256 * mb, mk(5000, 700*1024), 128},
		{"byte cap disabled", 128, 0, mk(500, 16*mb), 128},
		{"cap below one chunk still admits one", 128, 1 * mb, mk(10, 16*mb), 1},
		{"unknown sizes fall back to segments", 32, 256 * mb, mk(10, 0), 32},
	}
	for _, tc := range cases {
		c := &Cache{cfg: Config{ReadaheadMaxSegments: tc.segs, ReadaheadMaxBytes: tc.maxBytes}}
		if got := c.readaheadWindow(tc.refs); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func TestProbeSessionUsesSmallWindowUntilPlayback(t *testing.T) {
	c := New(Config{
		DiskCachePath:        t.TempDir(),
		ReadaheadEnabled:     true,
		ReadaheadWorkers:     1,
		ReadaheadMaxSegments: 10,
		MinBufferSegments:    1,
	}, testLogger())
	defer c.Close()
	src := newSteadyFakeSource("probe-src", 40, 10)

	rec := httptest.NewRecorder()
	if err := c.Stream(WithProbe(context.Background()), src, ModeReadahead, 400, rec, rangeHeaderFor(0, 10), 1); err != nil {
		t.Fatalf("probe Stream: %v", err)
	}
	sess, ok := sessionFor(c, src, "c0")
	if !ok {
		t.Fatal("no session after probe")
	}
	if got := sess.ahead.Load(); got != ProbeReadaheadSegments {
		t.Fatalf("probe window = %d, want %d", got, ProbeReadaheadSegments)
	}

	rec = httptest.NewRecorder()
	if err := c.Stream(context.Background(), src, ModeReadahead, 400, rec, rangeHeaderFor(1, 10), 1); err != nil {
		t.Fatalf("playback Stream: %v", err)
	}
	if got := sess.ahead.Load(); got != 10 {
		t.Fatalf("playback window = %d, want 10", got)
	}
}
