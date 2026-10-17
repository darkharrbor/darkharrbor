package nntp

import (
	"reflect"
	"testing"
)

func TestVolumeGeometryStarts(t *testing.T) {
	g := volumeGeometry{part: 716800, total: 716800*3 + 1000, segments: 4}
	if !g.ok() {
		t.Fatal("valid geometry rejected")
	}
	want := []int64{0, 716800, 1433600, 2150400, 2151400}
	if got := g.starts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("starts=%v want %v", got, want)
	}
	for _, bad := range []volumeGeometry{
		{part: 716800, total: 716800 * 5, segments: 4}, // last segment larger than a part
		{part: 716800, total: 716800 * 3, segments: 4}, // last segment empty
		{part: 0, total: 10, segments: 1},
	} {
		if bad.ok() {
			t.Errorf("inconsistent geometry accepted: %+v", bad)
		}
	}
}
