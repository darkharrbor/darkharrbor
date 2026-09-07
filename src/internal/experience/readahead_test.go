package experience

import "testing"

func TestAdaptiveReadaheadRecommend(t *testing.T) {
	cases := []struct {
		name         string
		ar           AdaptiveReadahead
		bitrateBps   int64
		perWorkerBps int64
		headroom     int
		want         int
	}{
		{
			name:     "unknown bitrate falls back to max",
			ar:       AdaptiveReadahead{MinWorkers: 1, MaxWorkers: 4},
			headroom: -1,
			want:     4,
		},
		{
			name:         "typical case computes ceil with safety multiple",
			ar:           AdaptiveReadahead{MinWorkers: 1, MaxWorkers: 8, SafetyMultiple: 2.0},
			bitrateBps:   5_000_000,
			perWorkerBps: 2_000_000,
			headroom:     -1,
			want:         5, // ceil(5e6*2/2e6) = ceil(5.0) = 5
		},
		{
			name:         "clamped to max",
			ar:           AdaptiveReadahead{MinWorkers: 1, MaxWorkers: 3, SafetyMultiple: 2.0},
			bitrateBps:   50_000_000,
			perWorkerBps: 1_000_000,
			headroom:     -1,
			want:         3,
		},
		{
			name:         "clamped to min",
			ar:           AdaptiveReadahead{MinWorkers: 2, MaxWorkers: 8, SafetyMultiple: 2.0},
			bitrateBps:   1_000,
			perWorkerBps: 10_000_000,
			headroom:     -1,
			want:         2,
		},
		{
			name:         "headroom clamps below computed want",
			ar:           AdaptiveReadahead{MinWorkers: 1, MaxWorkers: 8, SafetyMultiple: 2.0},
			bitrateBps:   16_000_000,
			perWorkerBps: 2_000_000,
			headroom:     3,
			want:         3, // computed want=16 (clamped to max 8), then clamped to headroom 3
		},
		{
			name:         "thin headroom below floor still yields floor",
			ar:           AdaptiveReadahead{MinWorkers: 2, MaxWorkers: 8, SafetyMultiple: 2.0},
			bitrateBps:   16_000_000,
			perWorkerBps: 2_000_000,
			headroom:     0,
			want:         2,
		},
		{
			name:         "zero/negative safety multiple defaults to 2.0",
			ar:           AdaptiveReadahead{MinWorkers: 1, MaxWorkers: 8, SafetyMultiple: 0},
			bitrateBps:   5_000_000,
			perWorkerBps: 2_000_000,
			headroom:     -1,
			want:         5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ar.Recommend(tc.bitrateBps, tc.perWorkerBps, tc.headroom)
			if got != tc.want {
				t.Fatalf("Recommend(%d,%d,headroom=%d) = %d, want %d",
					tc.bitrateBps, tc.perWorkerBps, tc.headroom, got, tc.want)
			}
		})
	}
}
