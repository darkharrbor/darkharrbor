package dockerpolicy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllowedExec(t *testing.T) {
	arr := "lscr.io/linuxserver/sonarr:latest"
	prowlarr := "lscr.io/linuxserver/prowlarr:latest"
	jellyfin := "jellyfin/jellyfin:latest"
	path := "/app/sonarr/bin/ffprobe"
	tests := []struct {
		name  string
		image string
		user  string
		cmd   []string
		want  bool
	}{
		{"resolve arr ffprobe", arr, "", ResolveFFprobe(), true},
		{"read only selected arr config", arr, "", ReadArrConfig(), true},
		{"read prowlarr config", prowlarr, "", ReadArrConfig(), true},
		{"shim probe", arr, "root", ShimProbe(path), true},
		{"shim backup", arr, "root", ShimBackup(path), true},
		{"exact embedded shim write", arr, "root", ShimWrite(path), true},
		{"shim verify", arr, "root", ShimVerify(path), true},
		{"jellyfin fixed write", jellyfin, "", JellyfinWriteFFmpeg(), true},
		{"jellyfin media visibility", jellyfin, "", []string{"test", "-e", "/mnt/darkharrbor"}, true},
		{"arbitrary command", arr, "root", []string{"id"}, false},
		{"full config read", arr, "", []string{"cat", "/config/config.xml"}, false},
		{"wrong image", "postgres:latest", "", ReadArrConfig(), false},
		{"wrong user", arr, "", ShimWrite(path), false},
		{"different shim payload", arr, "root", []string{"sh", "-c", "echo ZWNobyBwd25lZA== | base64 -d > /app/sonarr/bin/ffprobe.tmp && chmod 0755 /app/sonarr/bin/ffprobe.tmp && mv /app/sonarr/bin/ffprobe.tmp /app/sonarr/bin/ffprobe"}, false},
		{"path traversal", arr, "root", ShimVerify("/app/sonarr/../bin/ffprobe"), false},
		{"jellyfin secret path probe", jellyfin, "", []string{"test", "-e", "/config/config/system.xml"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AllowedExec(tt.image, tt.user, tt.cmd); got != tt.want {
				t.Fatalf("AllowedExec()=%v, want %v; cmd=%q", got, tt.want, tt.cmd)
			}
		})
	}
}

func TestReadArrConfigReturnsOnlyRequiredFields(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.xml")
	raw := `<Config><Port>8989</Port><ApiKey>test-key</ApiKey><BindAddress>*</BindAddress><AuthenticationMethod>Forms</AuthenticationMethod></Config>`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	command := strings.ReplaceAll(ReadArrConfigCommand, "/config/config.xml", configPath)
	out, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	got := string(out)
	if got != "<Config><Port>8989</Port><ApiKey>test-key</ApiKey></Config>\n" {
		t.Fatalf("unexpected selected config output: %q", got)
	}
}

func TestShimWriteRemovesExactTemporaryEntryBeforeRedirect(t *testing.T) {
	cmd := ShimWrite("/app/sonarr/bin/ffprobe")
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("unexpected command shape: %q", cmd)
	}
	want := `rm -f "/app/sonarr/bin/ffprobe.tmp" && printf %s `
	if !strings.HasPrefix(cmd[2], want) {
		t.Fatalf("temporary path is not removed before decode: %q", cmd[2])
	}
	if strings.Contains(cmd[2], "echo ") {
		t.Fatalf("shell-dependent echo used for embedded payload: %q", cmd[2])
	}
}

func TestJellyfinCommandsRejectSymlinkPathsAndReplaceAtomically(t *testing.T) {
	for name, command := range map[string]string{
		"check wrapper": JellyfinCheckWrapperCommand,
		"mkdir wrapper": JellyfinMkdirCommand,
		"read bitrate":  JellyfinReadBitrateCommand,
		"set bitrate":   JellyfinSetBitrateCommand,
		"check system":  JellyfinCheckSystemCommand,
		"add bitrate":   JellyfinAddBitrateCommand,
	} {
		if !strings.Contains(command, "test ! -L") {
			t.Fatalf("%s lacks symlink refusal: %q", name, command)
		}
	}
	for name, command := range map[string][]string{
		"ffmpeg":  JellyfinWriteFFmpeg(),
		"ffprobe": JellyfinWriteFFprobe(),
	} {
		if len(command) != 3 || !strings.Contains(command[2], "test ! -L /config/ffmpeg-wrapper") ||
			!strings.Contains(command[2], "rm -f /config/ffmpeg-wrapper/"+name+".tmp") ||
			!strings.Contains(command[2], "mv /config/ffmpeg-wrapper/"+name+".tmp /config/ffmpeg-wrapper/"+name) {
			t.Fatalf("%s write is not guarded and atomic: %q", name, command)
		}
	}
}

func TestJellyfinShellCommandsRefuseAndRemoveSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	victimDir := filepath.Join(t.TempDir(), "victim")
	if err := os.Mkdir(victimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wrapperDir := filepath.Join(root, "ffmpeg-wrapper")
	if err := os.Symlink(victimDir, wrapperDir); err != nil {
		t.Fatal(err)
	}
	run := func(command string) error {
		command = strings.ReplaceAll(command, "/config", root)
		return exec.Command("sh", "-c", command).Run()
	}
	if err := run(JellyfinMkdirCommand); err == nil {
		t.Fatal("symlinked wrapper directory was accepted")
	}
	if err := run(JellyfinWriteFFmpeg()[2]); err == nil {
		t.Fatal("write through symlinked wrapper directory was accepted")
	}

	if err := os.Remove(wrapperDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(wrapperDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victimFile := filepath.Join(victimDir, "victim")
	if err := os.WriteFile(victimFile, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimFile, filepath.Join(wrapperDir, "ffmpeg.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := run(JellyfinWriteFFmpeg()[2]); err != nil {
		t.Fatalf("guarded wrapper write: %v", err)
	}
	if got, err := os.ReadFile(victimFile); err != nil || string(got) != "unchanged" {
		t.Fatalf("temporary symlink target changed: %q err=%v", got, err)
	}

	configDir := filepath.Join(root, "config")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimFile, filepath.Join(configDir, "system.xml")); err != nil {
		t.Fatal(err)
	}
	if err := run(JellyfinSetBitrateCommand); err == nil {
		t.Fatal("symlinked system.xml was accepted")
	}
	if got, err := os.ReadFile(victimFile); err != nil || string(got) != "unchanged" {
		t.Fatalf("system.xml symlink target changed: %q err=%v", got, err)
	}
}
