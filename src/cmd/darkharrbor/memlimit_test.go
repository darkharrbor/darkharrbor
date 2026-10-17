package main

import "testing"

func TestParseCgroupMemoryMax(t *testing.T) {
	for in, want := range map[string]int64{"max\n": 0, "": 0, "8589934592\n": 8589934592, "junk": 0, "-1": 0} {
		got, ok := parseCgroupMemoryMax(in)
		if (want == 0 && ok) || got != want {
			t.Errorf("%q: got %d ok=%v want %d", in, got, ok, want)
		}
	}
}
