package output

import (
	"reflect"
	"testing"
)

func TestReleaseScopeKeep(t *testing.T) {
	pack := []string{"Show.S02E01.mkv", "Show.S02E02.mkv", "Show.S03E01.mkv", "Show.S03E02.mkv", "Extras.mkv"}
	for _, tt := range []struct {
		title string
		names []string
		want  []bool
	}{
		{"Show.S03.1080p.WEB", pack, []bool{false, false, true, true, true}},
		{"Show.S02E02.1080p", pack, []bool{false, true, false, false, true}},
		{"Show.S01-S05.1080p", pack, []bool{true, true, true, true, true}}, // multi-season: unscoped
		{"Show.S09.1080p", pack, []bool{false, false, false, false, true}}, // same as the torrent path: unparsed files survive
		{"Show.S09.1080p", pack[:4], []bool{true, true, true, true}},       // nothing would remain: unchanged
		{"Show.S03.1080p", []string{"Show.S02E01.mkv"}, []bool{true}},      // single file
	} {
		if got := ReleaseScopeKeep(tt.title, tt.names); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v want %v", tt.title, got, tt.want)
		}
	}
}
