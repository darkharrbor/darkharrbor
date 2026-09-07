package wizard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/securefile"
)

func TestWriteSetupHandoffCreatesOnlyPrivateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "handoff")
	recovery := strings.Repeat("r", 64)
	mediaflow := strings.Repeat("m", 64)
	if err := writeSetupHandoff(dir, recovery, mediaflow); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"recovery.key", "mediaflow.env"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%o", name, info.Mode().Perm())
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "mediaflow.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "HARRBOR_MEDIAFLOW_PASSWORD="+mediaflow+"\n" {
		t.Fatal("MediaFlow handoff content mismatch")
	}
}

func TestWriteSetupHandoffClearsStaleOptionalFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "handoff")
	if err := writeSetupHandoff(dir, "old-recovery", "old-mediaflow"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stremio-install.url"), []byte("old-url\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSetupHandoff(dir, "new-recovery", ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mediaflow.env", "stremio-install.url", "connector-targets"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("stale %s remains: %v", name, err)
		}
	}
}

func TestWriteConnectorTargetsHandoffIsPrivateSortedAndBounded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "handoff")
	if err := securefile.PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := writeConnectorTargetsHandoff(dir, []string{"Sonarr-HD", "radarr", "radarr"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "connector-targets")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "Sonarr-HD\nradarr\n" {
		t.Fatalf("targets=%q", data)
	}
	if err := writeConnectorTargetsHandoff(dir, []string{"radarr\nattacker"}); err == nil {
		t.Fatal("newline-bearing target accepted")
	}
}

func TestRuntimeArrConnectorTargetsRequireDiscoveryAndBaseWiring(t *testing.T) {
	instances := []discoveredArrInstance{
		{Name: "sonarr", AppType: "sonarr"},
		{Name: "prowlarr", AppType: "prowlarr"},
		{Name: "radarr", AppType: "radarr"},
	}
	got := runtimeArrConnectorTargets(onboardingPlan{ArrConnection: "discover", BaseArrTopology: true, WrapperRepair: true}, instances)
	if strings.Join(got, ",") != "sonarr,radarr" {
		t.Fatalf("targets=%v", got)
	}
	if got := runtimeArrConnectorTargets(onboardingPlan{ArrConnection: "manual", BaseArrTopology: true}, instances); len(got) != 0 {
		t.Fatalf("manual targets=%v", got)
	}
	if got := runtimeArrConnectorTargets(onboardingPlan{ArrConnection: "discover", BaseArrTopology: false}, instances); len(got) != 0 {
		t.Fatalf("reactive-only targets=%v", got)
	}
}
