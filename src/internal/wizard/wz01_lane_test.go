package wizard

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// stubReadLine replaces the package-level stdin reader for the duration of a
// test, so promptPreference can be driven without a terminal.
func stubReadLine(t *testing.T, input string) func() {
	t.Helper()
	orig := stdinReader
	// Every prompt in these tests consumes at most a few lines; repeat the
	// input so a trailing confirmation prompt cannot block on EOF.
	stdinReader = bufio.NewReader(strings.NewReader(strings.Repeat(input+"\n", 8)))
	return func() { stdinReader = orig }
}

// WZ-01. A usenet-only deployment is documented as supported in
// docs/FEATURE-MAP.md, and REL-04's clean-host run proved the wizard blocked
// it. These tests pin the corrected contract.

func TestPreferenceWithoutTorBoxDefaultsToNNTP(t *testing.T) {
	var out bytes.Buffer
	// Empty input => accept the offered default.
	restore := stubReadLine(t, "")
	defer restore()

	pref, err := promptPreference(&out, false /*torrent*/, false /*torbox*/, false, true /*nntp*/, false)
	if err != nil {
		t.Fatalf("usenet-only deployment must be able to choose a preference: %v", err)
	}
	if len(pref) != 1 || pref[0] != "nntp_nzb" {
		t.Fatalf("expected [nntp_nzb], got %v", pref)
	}
	if strings.Contains(out.String(), "torbox_torrent") {
		t.Fatalf("TorBox lanes must not be offered when TorBox is unconfigured:\n%s", out.String())
	}
}

func TestPreferenceWithoutTorBoxRejectsTorBoxLanes(t *testing.T) {
	var out bytes.Buffer
	restore := stubReadLine(t, "torbox_torrent,nntp_nzb")
	defer restore()

	pref, err := promptPreference(&out, false, false, false, true, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, l := range pref {
		if strings.HasPrefix(l, "torbox") || strings.HasPrefix(l, "uncached") {
			t.Fatalf("TorBox lane %q survived on a deployment with no TorBox: %v", l, pref)
		}
	}
	if len(pref) != 1 || pref[0] != "nntp_nzb" {
		t.Fatalf("expected the NNTP lane to survive alone, got %v", pref)
	}
	// The operator must be TOLD, not silently corrected.
	if !strings.Contains(out.String(), "Ignoring unavailable choices") {
		t.Fatalf("dropping lanes must be reported to the operator:\n%s", out.String())
	}
}

// With neither TorBox nor NNTP there is no lane at all, and that must fail
// during configuration rather than at acquisition time.
func TestPreferenceWithNoLaneAtAllFails(t *testing.T) {
	var out bytes.Buffer
	restore := stubReadLine(t, "")
	defer restore()

	_, err := promptPreference(&out, false, false, false, false, false)
	if err == nil {
		t.Fatalf("expected failure when no acquisition lane exists")
	}
	if !strings.Contains(err.Error(), "no acquisition lane") {
		t.Fatalf("error must name the cause, got: %v", err)
	}
}

// TorBox still behaves exactly as before when it IS configured -- the row
// must not regress the existing path.
func TestPreferenceWithTorBoxUnchanged(t *testing.T) {
	var out bytes.Buffer
	restore := stubReadLine(t, "")
	defer restore()

	pref, err := promptPreference(&out, true, true, true, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pref) != 2 || pref[0] != "torbox_torrent" || pref[1] != "torbox_nzb" {
		t.Fatalf("expected the documented paid-with-usenet default, got %v", pref)
	}
}

func TestAvailableLaneListNamesOnlyUsableLanes(t *testing.T) {
	got := availableLaneList(false, false, false, true)
	if got != "direct-usenet" {
		t.Fatalf("usenet-only deployment should advertise only direct-usenet, got %q", got)
	}
	if l := availableLaneList(false, false, false, false); !strings.Contains(l, "none") {
		t.Fatalf("with no provider the list must say so, got %q", l)
	}
	if l := availableLaneList(true, true, true, true); !strings.Contains(l, "cached-nzb") || !strings.Contains(l, "direct-usenet") {
		t.Fatalf("full deployment should advertise every lane, got %q", l)
	}
}

func TestPreferenceWithNonTorBoxDebridUsesGenericTorrentChoice(t *testing.T) {
	var out bytes.Buffer
	restore := stubReadLine(t, "")
	defer restore()

	pref, err := promptPreference(&out, true, false, false, false, false)
	if err != nil {
		t.Fatalf("non-TorBox debrid must provide a torrent preference: %v", err)
	}
	if len(pref) != 1 || pref[0] != "torbox_torrent" {
		t.Fatalf("expected legacy runtime token, got %v", pref)
	}
	if !strings.Contains(out.String(), "any configured debrid provider") || strings.Contains(out.String(), "torbox_torrent") {
		t.Fatalf("operator must see the provider-neutral name only:\n%s", out.String())
	}
}

func TestFriendlyPreferenceNamesMapToStableRuntimeValues(t *testing.T) {
	got := parsePreference("cached-torrent,direct-usenet,uncached-torrent-last", true, true)
	want := []string{"torbox_torrent", "nntp_nzb", "uncached_torrent_derank"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("preference=%v want=%v", got, want)
	}
}
