package wizard

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

func planResponses(connection, apply string) []string {
	discover := "y"
	if connection == "manual" {
		discover = "n"
	}
	return []string{
		discover, "torrent,http", "torbox", "recommended", "n", "n", "n", "n",
		"y", "y", "n", "n", apply,
	}
}

func withResponses(t *testing.T, responses []string) {
	t.Helper()
	old := stdinReader
	stdinReader = newAnswersReader(responses)
	t.Cleanup(func() { stdinReader = old })
}

func testInventory() setupInventory {
	return setupInventory{ProxyOK: true, Arrs: []string{"radarr"}, Prowlarrs: []string{"prowlarr"}, Jellyfins: []string{"jellyfin"}}
}

func collectOnboardingPlan(out io.Writer, inventory setupInventory) (onboardingPlan, bool, error) {
	plan, err := collectOnboardingChoices(out, inventory)
	if err != nil {
		return onboardingPlan{}, false, err
	}
	printOnboardingPlan(out, plan)
	return plan, promptYesNo(out, "  Apply this complete plan? [y/N]: ", false), nil
}

func TestInspectSetupInventoryIsReadOnlyAndSecretFree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ApiVersion":"1.51"}`))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/v1.51/containers/json" {
			t.Fatalf("unexpected discovery request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
  {"Id":"1","Names":["/radarr"],"Image":"lscr.io/linuxserver/radarr:latest","State":"running","Labels":{"secret":"must-not-print"}},
  {"Id":"2","Names":["/prowlarr"],"Image":"lscr.io/linuxserver/prowlarr:latest","State":"running"},
  {"Id":"3","Names":["/jellyfin"],"Image":"jellyfin/jellyfin@sha256:abcdef","State":"running"}
]`))
	}))
	defer server.Close()
	var out bytes.Buffer
	got := inspectSetupInventory(context.Background(), &out, server.URL)
	if !got.ProxyOK || !reflect.DeepEqual(got.Arrs, []string{"radarr"}) || !reflect.DeepEqual(got.Prowlarrs, []string{"prowlarr"}) || !reflect.DeepEqual(got.Jellyfins, []string{"jellyfin"}) {
		t.Fatalf("inventory=%+v", got)
	}
	text := out.String()
	if strings.Contains(text, "must-not-print") || strings.Contains(text, "latest") {
		t.Fatalf("discovery preview exposed hidden metadata:\n%s", text)
	}
}

func TestCollectOnboardingPlanSelectsOneJellyfinBeforeMutation(t *testing.T) {
	inventory := testInventory()
	inventory.Jellyfins = []string{"jellyfin-main", "jellyfin-kids"}
	responses := []string{"y", "torrent", "torbox", "recommended", "y", "n", "y", "2", "y", "n", "n", "n", "n"}
	withResponses(t, responses)
	var out bytes.Buffer
	plan, _, err := collectOnboardingPlan(&out, inventory)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Jellyfin || plan.JellyfinName != "jellyfin-kids" {
		t.Fatalf("plan=%+v", plan)
	}
	if !strings.Contains(out.String(), "Jellyfin instance: jellyfin-kids") {
		t.Fatalf("review omitted exact Jellyfin selection:\n%s", out.String())
	}
}

func TestCollectOnboardingPlanSelectsOneProwlarrBeforeExtraction(t *testing.T) {
	inventory := testInventory()
	inventory.Prowlarrs = []string{"prowlarr-main", "prowlarr-alt"}
	responses := []string{"y", "torrent", "torbox", "recommended", "y", "y", "2", "n", "y", "n", "n", "n", "n"}
	withResponses(t, responses)
	var out bytes.Buffer
	plan, _, err := collectOnboardingPlan(&out, inventory)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Prowlarr || plan.ProwlarrName != "prowlarr-alt" {
		t.Fatalf("plan=%+v", plan)
	}
	if !strings.Contains(out.String(), "Prowlarr instance: prowlarr-alt") {
		t.Fatalf("review omitted exact Prowlarr selection:\n%s", out.String())
	}
}

func TestCollectOnboardingPlanExistingManual(t *testing.T) {
	withResponses(t, planResponses("manual", "y"))
	var out bytes.Buffer
	plan, accepted, err := collectOnboardingPlan(&out, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if !accepted || plan.ArrContext != "existing" || plan.ArrConnection != "manual" {
		t.Fatalf("unexpected result: accepted=%t plan=%+v", accepted, plan)
	}
	if plan.BaseArrTopology || !plan.Aggregator || !plan.ReactiveCommit {
		t.Fatalf("feature choices not preserved: %+v", plan)
	}
	if !reflect.DeepEqual(plan.AcquisitionLanes, []string{"torrent", "http"}) {
		t.Fatalf("lanes=%v", plan.AcquisitionLanes)
	}
	text := out.String()
	if strings.Contains(text, "t1/t2/t3") || strings.Contains(text, "Arr context") {
		t.Fatal("normal setup exposed internal topology or fixture language")
	}
	if !strings.Contains(text, "No persistent changes have been made") {
		t.Fatal("review did not state the mutation boundary")
	}
}

func TestCollectOnboardingPlanIsForExistingArrs(t *testing.T) {
	withResponses(t, planResponses("discover", "n"))
	var out bytes.Buffer
	plan, accepted, err := collectOnboardingPlan(&out, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if accepted || plan.ArrContext != "existing" {
		t.Fatalf("unexpected result: accepted=%t plan=%+v", accepted, plan)
	}
	text := out.String()
	if !strings.Contains(text, "Sonarr and Radarr instances you already run") {
		t.Fatalf("existing Arr assumption missing:\n%s", text)
	}
	if strings.Contains(strings.ToLower(text), "fixture") {
		t.Fatalf("test-fixture language leaked into normal setup:\n%s", text)
	}
}

func TestDebridProviderSelectionIsExplicitAndPreservesFallbackOrder(t *testing.T) {
	withResponses(t, []string{"3,1,real-debrid"})
	var out bytes.Buffer
	plan := onboardingPlan{AcquisitionLanes: []string{"torrent"}}
	got := promptDebridProviderSelection(&out, plan)
	want := []string{"alldebrid", "torbox", "realdebrid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("providers=%v want=%v", got, want)
	}
	plan.DebridProviders = got
	printOnboardingPlan(&out, plan)
	if !strings.Contains(out.String(), "AllDebrid → TorBox → Real-Debrid") {
		t.Fatalf("review omitted fallback order:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "first provider that accepts a torrent owns that item afterward") {
		t.Fatalf("prompt omitted persistent winner boundary:\n%s", out.String())
	}
}

func TestSelectedTorrentRequiresAnExplicitProvider(t *testing.T) {
	withResponses(t, []string{"none", "premiumize"})
	var out bytes.Buffer
	plan := onboardingPlan{AcquisitionLanes: []string{"torrent"}}
	got := promptDebridProviderSelection(&out, plan)
	if !reflect.DeepEqual(got, []string{"premiumize"}) {
		t.Fatalf("providers=%v", got)
	}
	if !strings.Contains(out.String(), "Torrent was selected; choose at least one") {
		t.Fatalf("missing actionable source/provider correction:\n%s", out.String())
	}
}

func TestSelectedUsenetRequiresAnAccountOrDirectProvider(t *testing.T) {
	withResponses(t, []string{
		"y", "nntp", "none", "none", "custom-news", "recommended",
		"n", "n", "n", "n", "n", "n",
	})
	var out bytes.Buffer
	plan, err := collectOnboardingChoices(&out, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.UsenetProviders, []string{"custom-news"}) {
		t.Fatalf("plan=%+v", plan)
	}
	if !strings.Contains(out.String(), "Usenet was selected; choose at least one") {
		t.Fatalf("missing actionable source/provider correction:\n%s", out.String())
	}
}

func TestCollectOnboardingPlanReviewsNNTPOrderBeforeApproval(t *testing.T) {
	withResponses(t, []string{
		"y", "nntp", "torbox", "newshosting,torbox", "recommended",
		"n", "n", "n", "n", "n", "n", "y",
	})
	var out bytes.Buffer
	plan, accepted, err := collectOnboardingPlan(&out, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if !accepted || !reflect.DeepEqual(plan.UsenetProviders, []string{"newshosting", "torbox"}) {
		t.Fatalf("accepted=%t plan=%+v", accepted, plan)
	}
	text := out.String()
	if !strings.Contains(text, "NNTP providers:    newshosting → torbox") {
		t.Fatalf("complete review omitted NNTP fallback order:\n%s", text)
	}
	if strings.Index(text, "NNTP providers:    newshosting → torbox") > strings.Index(text, "Apply this complete plan?") {
		t.Fatalf("NNTP selection appeared after mutation approval:\n%s", text)
	}
}

func TestSourceSelectionHasNoImplicitDefault(t *testing.T) {
	withResponses(t, []string{"", "http"})
	var out bytes.Buffer
	lanes, err := promptLanes(&out, false)
	if err != nil || !reflect.DeepEqual(lanes, []string{"http"}) {
		t.Fatalf("lanes=%v err=%v", lanes, err)
	}
	if !strings.Contains(out.String(), "will not assume a provider or lane") {
		t.Fatalf("missing neutral-selection explanation:\n%s", out.String())
	}
}

func TestCollectOnboardingPlanSelectsDiscoveredArrSubset(t *testing.T) {
	inventory := testInventory()
	inventory.Arrs = []string{"sonarr-main", "sonarr-anime", "radarr"}
	responses := append([]string{"y", "2,3"}, planResponses("discover", "n")[1:]...)
	withResponses(t, responses)
	var out bytes.Buffer
	plan, _, err := collectOnboardingPlan(&out, inventory)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sonarr-anime", "radarr"}
	if !reflect.DeepEqual(plan.SelectedArrs, want) {
		t.Fatalf("selected=%v want=%v", plan.SelectedArrs, want)
	}
	if !strings.Contains(out.String(), "Arr instances:     sonarr-anime, radarr") {
		t.Fatalf("redacted review omitted selected subset:\n%s", out.String())
	}
}

func TestAggregatorDerivesTopologyAndAddsHTTPSource(t *testing.T) {
	withResponses(t, []string{"y", "torrent", "torbox", "recommended", "y", "y", "n", "n", "y", "n", "n", "n", "n"})
	var out bytes.Buffer
	plan, accepted, err := collectOnboardingPlan(&out, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if accepted || !plan.Aggregator || plan.ReactiveCommit || !plan.hasLane("http") || plan.Topology != "t2" || !plan.WrapperRepair {
		t.Fatalf("plan=%+v accepted=%t", plan, accepted)
	}
	if !strings.Contains(out.String(), "HTTP/stream support added") {
		t.Fatalf("dependency was not explained:\n%s", out.String())
	}
}

func TestManualArrEntryNeverEnablesConnectorWrapperRepair(t *testing.T) {
	withResponses(t, []string{"n", "torrent", "torbox", "recommended", "y", "n", "n", "y", "n", "n", "n", "n"})
	var out bytes.Buffer
	plan, _, err := collectOnboardingPlan(&out, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.BaseArrTopology || plan.WrapperRepair {
		t.Fatalf("plan=%+v", plan)
	}
	if !strings.Contains(out.String(), "manual installation is required") {
		t.Fatalf("manual repair consequence missing:\n%s", out.String())
	}
}

func TestDocumentedT3PlanReplayMatchesInteractive(t *testing.T) {
	responses := planResponses("manual", "y")
	withResponses(t, responses)
	var interactiveOut bytes.Buffer
	want, wantAccepted, err := collectOnboardingPlan(&interactiveOut, testInventory())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "answers.json")
	data := `{"responses":["n","torrent,http","torbox","recommended","n","n","n","n","y","y","n","n","y"]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := prepareAnswers(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	var replayOut bytes.Buffer
	got, gotAccepted, err := collectOnboardingPlan(&replayOut, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || gotAccepted != wantAccepted {
		t.Fatalf("replay differs: got=%+v/%t want=%+v/%t", got, gotAccepted, want, wantAccepted)
	}
}

func TestPrepareAnswersRejectsBroadPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "answers.json")
	if err := os.WriteFile(path, []byte(`{"responses":["existing"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAnswers(path); err == nil || !strings.Contains(err.Error(), "group/world accessible") {
		t.Fatalf("expected private-mode error, got %v", err)
	}
}

func TestPrepareAnswersRejectsSymlinkAndTrailingJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "answers.json")
	if err := os.WriteFile(target, []byte(`{"responses":["existing"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "answers-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAnswers(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected symlink refusal, got %v", err)
	}
	if err := os.WriteFile(target, []byte(`{"responses":["existing"]}{"responses":["manual"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAnswers(target); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("expected trailing-content refusal, got %v", err)
	}
}

func TestRunDeclinedPlanCreatesNoDatabaseOrConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	answers := filepath.Join(dir, "answers.json")
	data := `{"responses":["torrent","torbox","recommended","n","n","n","n","n","n"]}`
	if err := os.WriteFile(answers, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(dir, "config")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		ConfigDir:   configDir,
		DBPath:      filepath.Join(configDir, "darkharrbor.db"),
		AnswersFile: answers,
		Stdout:      &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("declined plan mutated config dir: %v", entries)
	}
	if !strings.Contains(out.String(), "Aborted — no changes made") {
		t.Fatal("missing cancellation result")
	}
}

func TestRunDeclinedNNTPPlanReviewsProviderOrderWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	answers := filepath.Join(dir, "answers.json")
	data := `{"responses":["nntp","none","custom-one,newshosting","recommended","n","n","n","n","n","n"]}`
	if err := os.WriteFile(answers, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(dir, "config")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), Options{
		ConfigDir:   configDir,
		DBPath:      filepath.Join(configDir, "darkharrbor.db"),
		AnswersFile: answers,
		Stdout:      &out,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("declined NNTP plan mutated config dir: %v", entries)
	}
	text := out.String()
	if !strings.Contains(text, "NNTP providers:    custom-one → newshosting") || !strings.Contains(text, "Aborted — no changes made") {
		t.Fatalf("NNTP review or cancellation result missing:\n%s", text)
	}
}

func TestRunHTTPPlanHasOneCompleteRedactedApprovalBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	answers := filepath.Join(dir, "answers.json")
	data := `{"responses":["http","recommended","n","n","n","n","n","n","n","media","stremio","https://secret.example/user/token","","","n"]}`
	if err := os.WriteFile(answers, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(dir, "config")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		ConfigDir:   configDir,
		DBPath:      filepath.Join(configDir, "darkharrbor.db"),
		AnswersFile: answers,
		Stdout:      &out,
		DialBackend: func(context.Context, config.HTTPBackend) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("declined complete plan mutated config dir: %v", entries)
	}
	text := out.String()
	if strings.Count(text, "Apply this complete plan?") != 1 || strings.Contains(text, "Include these HTTP sources") {
		t.Fatalf("wizard did not present one final approval:\n%s", text)
	}
	if strings.Contains(text, "secret.example") || !strings.Contains(text, "HARRBOR_HTTPBACKEND_MEDIA_URL=[hidden; will be sealed]") {
		t.Fatalf("complete review leaked or omitted hidden HTTP authority:\n%s", text)
	}
}

func TestNewAnswersReaderPreservesBlankResponses(t *testing.T) {
	r := newAnswersReader([]string{"existing", "", "t1"})
	got := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.TrimSuffix(line, "\n"))
	}
	if !reflect.DeepEqual(got, []string{"existing", "", "t1"}) {
		t.Fatalf("responses=%v", got)
	}
}

func TestSecretOutputPathHonorsConfiguredKeyVolume(t *testing.T) {
	t.Setenv("HARRBOR_SECRETS_KEY_FILE", "/run/darkharrbor-key/secrets.key")
	if got := secretOutputPath("HARRBOR_SECRETS_KEY_FILE", "/config/secrets.key"); got != "/run/darkharrbor-key/secrets.key" {
		t.Fatalf("key path=%q", got)
	}
}

func TestSetupOutcomeSeparatesStatesAndCommands(t *testing.T) {
	plan := onboardingPlan{
		ArrContext: "existing", ArrConnection: "manual", Topology: "t3",
		AcquisitionLanes: []string{"http"}, ReactiveCommit: true, Aggregator: true,
	}
	var out bytes.Buffer
	printSetupOutcome(&out, plan)
	text := out.String()
	for _, want := range []string{"enabled:", "skipped:", "degraded:", "failed:", "externally pending:", "validate after HTTP backend configuration and restart: darkharrbor doctor", "darkharrbor reconcile-arr"} {
		if !strings.Contains(text, want) {
			t.Fatalf("outcome missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "validate now: darkharrbor doctor") {
		t.Fatalf("HTTP outcome claimed doctor was immediately decisive:\n%s", text)
	}
	if strings.Contains(text, "ffprobe") {
		t.Fatalf("reactive-only outcome incorrectly required the optional ffprobe relay:\n%s", text)
	}
}
