// Package dockerpolicy defines the complete command and container policy
// enforced by harrbor-dockerproxy. Keep privileged Docker exec operations
// here so callers and the security boundary use the same exact commands.
package dockerpolicy

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed assets/ffprobe-wrapper.sh
var FFprobeWrapper []byte

const (
	ResolveFFprobeCommand = "ls -1 /app/*/bin/ffprobe 2>/dev/null"
	ReadArrConfigCommand  = "p=$(sed -n 's:.*<Port>\\([^<]*\\)</Port>.*:\\1:p' /config/config.xml | head -n1); k=$(sed -n 's:.*<ApiKey>\\([^<]*\\)</ApiKey>.*:\\1:p' /config/config.xml | head -n1); printf '<Config><Port>%s</Port><ApiKey>%s</ApiKey></Config>\\n' \"$p\" \"$k\""

	JellyfinCheckWrapperCommand = "test ! -L /config/ffmpeg-wrapper && test ! -L /config/ffmpeg-wrapper/ffmpeg && cat /config/ffmpeg-wrapper/ffmpeg 2>/dev/null"
	JellyfinMkdirCommand        = "test ! -L /config/ffmpeg-wrapper && mkdir -p /config/ffmpeg-wrapper && test -d /config/ffmpeg-wrapper && test ! -L /config/ffmpeg-wrapper"
	JellyfinCheckEnvCommand     = "printenv JELLYFIN_FFMPEG 2>/dev/null"
	JellyfinReadBitrateCommand  = "test -d /config/config && test ! -L /config/config && test -f /config/config/system.xml && test ! -L /config/config/system.xml && grep -o '<RemoteClientBitrateLimit>[^<]*</RemoteClientBitrateLimit>' /config/config/system.xml 2>/dev/null"
	JellyfinSetBitrateCommand   = "test -d /config/config && test ! -L /config/config && test -f /config/config/system.xml && test ! -L /config/config/system.xml && sed -i 's|<RemoteClientBitrateLimit>[^<]*</RemoteClientBitrateLimit>|<RemoteClientBitrateLimit>0</RemoteClientBitrateLimit>|' /config/config/system.xml"
	JellyfinCheckSystemCommand  = "if test -d /config/config && test ! -L /config/config && test -f /config/config/system.xml && test ! -L /config/config/system.xml; then echo yes; else echo no; fi"
	JellyfinAddBitrateCommand   = "test -d /config/config && test ! -L /config/config && test -f /config/config/system.xml && test ! -L /config/config/system.xml && sed -i 's|</ServerConfiguration>|  <RemoteClientBitrateLimit>0</RemoteClientBitrateLimit>\\n</ServerConfiguration>|' /config/config/system.xml"
)

const JellyfinFFmpegWrapper = `#!/bin/bash
# DarkHarrbor ffmpeg wrapper — installed by wizard
ENCODING_XML="/config/config/encoding.xml"
if grep -q '<string>mkv</string>' "$ENCODING_XML" 2>/dev/null; then
    sed -i 's|[[:space:]]*<string>mkv</string>||g' "$ENCODING_XML"
fi
ARGS=()
SKIP=0
for arg in "$@"; do
    if [[ $SKIP -eq 1 ]]; then SKIP=0; continue; fi
    if [[ "$arg" == "-probesize" ]]; then ARGS+=("-probesize" "10M"); SKIP=1
    elif [[ "$arg" == "-analyzeduration" ]]; then ARGS+=("-analyzeduration" "5M"); SKIP=1
    elif [[ "$arg" == "-readrate" ]]; then SKIP=1
    else ARGS+=("$arg"); fi
done
exec /usr/lib/jellyfin-ffmpeg/ffmpeg "${ARGS[@]}"
`

const JellyfinFFprobePassthrough = `#!/bin/bash
exec /usr/lib/jellyfin-ffmpeg/ffprobe "$@"
`

var (
	arrImage      = regexp.MustCompile(`(^|/)linuxserver/(sonarr|radarr)(:|@|$)`)
	prowlarrImage = regexp.MustCompile(`(^|/)linuxserver/prowlarr(:|@|$)`)
	jellyfinImage = regexp.MustCompile(`(^|/)jellyfin/jellyfin(:|@|$)`)
	ffprobePath   = regexp.MustCompile(`/app/[A-Za-z0-9._-]+/bin/ffprobe`)
)

func shell(command string) []string { return []string{"sh", "-c", command} }

func ResolveFFprobe() []string { return shell(ResolveFFprobeCommand) }
func ReadArrConfig() []string  { return shell(ReadArrConfigCommand) }

func ShimProbe(path string) []string {
	return shell(fmt.Sprintf(
		`if head -c 8192 %q 2>/dev/null | tr -d '\\0' | grep -q HARRBOR_STRM_WRAPPER; then w=1; else w=0; fi; s=$(sha256sum %q 2>/dev/null | cut -d' ' -f1); printf 'WRAPPED=%%s SHA=%%s\\n' "$w" "$s"`,
		path, path,
	))
}

func ShimBackup(path string) []string {
	return shell(fmt.Sprintf(`test -e %q || cp -a %q %q`, path+".real", path, path+".real"))
}

func ShimWrite(path string) []string {
	encoded := base64.StdEncoding.EncodeToString(FFprobeWrapper)
	tmp := path + ".tmp"
	return shell(fmt.Sprintf(`rm -f %q && printf %%s %s | base64 -d > %q && chmod 0755 %q && mv %q %q`, tmp, encoded, tmp, tmp, tmp, path))
}

func ShimVerify(path string) []string { return []string{path, "-version"} }

func JellyfinWriteFFmpeg() []string {
	return shell(fmt.Sprintf("test -d /config/ffmpeg-wrapper && test ! -L /config/ffmpeg-wrapper && rm -f /config/ffmpeg-wrapper/ffmpeg.tmp && cat > /config/ffmpeg-wrapper/ffmpeg.tmp << 'WRAPPEREOF'\n%sWRAPPEREOF\nchmod 0755 /config/ffmpeg-wrapper/ffmpeg.tmp && mv /config/ffmpeg-wrapper/ffmpeg.tmp /config/ffmpeg-wrapper/ffmpeg", JellyfinFFmpegWrapper))
}

func JellyfinWriteFFprobe() []string {
	return shell(fmt.Sprintf("test -d /config/ffmpeg-wrapper && test ! -L /config/ffmpeg-wrapper && rm -f /config/ffmpeg-wrapper/ffprobe.tmp && cat > /config/ffmpeg-wrapper/ffprobe.tmp << 'WRAPPEREOF'\n%sWRAPPEREOF\nchmod 0755 /config/ffmpeg-wrapper/ffprobe.tmp && mv /config/ffmpeg-wrapper/ffprobe.tmp /config/ffmpeg-wrapper/ffprobe", JellyfinFFprobePassthrough))
}

// AllowedExec is fail-closed: only fixed operations on recognized images pass.
// The proxy calls this after independently inspecting the target container.
func AllowedExec(image, user string, cmd []string) bool {
	switch {
	case arrImage.MatchString(image):
		return allowedArr(user, cmd)
	case prowlarrImage.MatchString(image):
		return user == "" && equal(cmd, ReadArrConfig())
	case jellyfinImage.MatchString(image):
		return user == "" && allowedJellyfin(cmd)
	default:
		return false
	}
}

func allowedArr(user string, cmd []string) bool {
	if user == "" && (equal(cmd, ResolveFFprobe()) || equal(cmd, ReadArrConfig())) {
		return true
	}
	if user != "root" {
		return false
	}
	if len(cmd) == 2 && cmd[1] == "-version" && validFFprobePath(cmd[0]) {
		return true
	}
	path := firstFFprobePath(cmd)
	return path != "" && (equal(cmd, ShimProbe(path)) || equal(cmd, ShimBackup(path)) || equal(cmd, ShimWrite(path)))
}

func allowedJellyfin(cmd []string) bool {
	fixed := [][]string{
		shell(JellyfinCheckWrapperCommand), shell(JellyfinMkdirCommand),
		JellyfinWriteFFmpeg(), JellyfinWriteFFprobe(), shell(JellyfinCheckEnvCommand),
		shell(JellyfinReadBitrateCommand), shell(JellyfinSetBitrateCommand),
		shell(JellyfinCheckSystemCommand), shell(JellyfinAddBitrateCommand),
	}
	for _, want := range fixed {
		if equal(cmd, want) {
			return true
		}
	}
	return len(cmd) == 3 && cmd[0] == "test" && cmd[1] == "-e" && safeMediaPath(cmd[2])
}

func firstFFprobePath(cmd []string) string {
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		return ""
	}
	return ffprobePath.FindString(cmd[2])
}

func validFFprobePath(path string) bool { return ffprobePath.FindString(path) == path }

func safeMediaPath(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
		return false
	}
	for _, root := range []string{"/data", "/media", "/mnt"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
