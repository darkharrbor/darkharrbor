package wizard

import (
	"bytes"
	"strings"
	"testing"
)

func TestEachNonTorBoxProviderUnlocksProviderNeutralTorrent(t *testing.T) {
	providers := map[string]debridAnswers{
		"realdebrid": {RealDebrid: "configured"},
		"alldebrid":  {AllDebrid: "configured"},
		"premiumize": {Premiumize: "configured"},
	}
	for name, answers := range providers {
		t.Run(name, func(t *testing.T) {
			if !answers.any() {
				t.Fatal("configured provider was not recognized")
			}
			restore := stubReadLine(t, "")
			defer restore()
			var out bytes.Buffer
			preference, err := promptPreference(&out, answers.any(), false, false, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(preference) != 1 || preference[0] != "torbox_torrent" {
				t.Fatalf("preference=%v", preference)
			}
		})
	}
}

func TestDeclinedTorrentLaneSkipsAllDebridPrompts(t *testing.T) {
	withResponses(t, []string{"must-not-be-consumed"})
	var out bytes.Buffer
	answers, err := promptOtherDebridProviders(t.Context(), &out, nil)
	if err != nil || answers.any() {
		t.Fatalf("answers=%+v err=%v", answers, err)
	}
	line, _ := stdinReader.ReadString('\n')
	if line != "must-not-be-consumed\n" {
		t.Fatalf("declined parent consumed child input: %q", line)
	}
	if !strings.Contains(out.String(), "no additional debrid provider was selected") {
		t.Fatalf("missing skip consequence: %s", out.String())
	}
}

func TestSelectedProviderRequiresOnlyItsCredential(t *testing.T) {
	withResponses(t, []string{""})
	var out bytes.Buffer
	_, err := promptOtherDebridProviders(t.Context(), &out, []string{"realdebrid"})
	if err == nil || !strings.Contains(err.Error(), "selected but no API token") {
		t.Fatalf("expected selected-provider credential error, got %v", err)
	}
	text := out.String()
	if strings.Contains(text, "AllDebrid API key") || strings.Contains(text, "Premiumize API key") {
		t.Fatalf("unselected provider credential was requested:\n%s", text)
	}
}
