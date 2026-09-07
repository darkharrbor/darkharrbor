// Package jellyfinsetup implements wizard S10b: Jellyfin ffmpeg-wrapper
// installation and RemoteClientBitrateLimit configuration. Discovers a
// running Jellyfin container via docker-proxy, writes the DH ffmpeg wrapper
// into its /config volume, sets RemoteClientBitrateLimit=0 via direct
// system.xml edit, and verifies the JELLYFIN_FFMPEG env var is set.
package jellyfinsetup

import (
	"context"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
	"github.com/darkharrbor/darkharrbor/internal/dockerpolicy"
)

// jellyfinImageRe matches the official jellyfin/jellyfin image (any tag/registry).
var jellyfinImageRe = regexp.MustCompile(`(^|/)jellyfin/jellyfin(:|@|$)`)

// IsSupportedImage reports whether image names the official Jellyfin image by
// tag, digest, or an optional registry prefix.
func IsSupportedImage(image string) bool { return jellyfinImageRe.MatchString(strings.ToLower(image)) }

// Result reports what happened.
type Result struct {
	ContainerName     string
	WrapperInstalled  string // "created", "already-present", "updated"
	BitrateConfigured string // "set-to-zero", "already-zero", "skipped"
	FFmpegEnvOK       bool
	Warnings          []string
	Err               error
}

// Setup discovers Jellyfin, installs the wrapper, and configures bitrate.
func Setup(ctx context.Context, out io.Writer, dockerClient *dockerctl.Client, targetName string) (*Result, error) {
	// Step 1: Discover Jellyfin container
	target, err := discoverJellyfin(ctx, dockerClient, targetName)
	if err != nil {
		return nil, fmt.Errorf("jellyfin discovery: %w", err)
	}
	if target == nil {
		return nil, nil // no Jellyfin found — not an error
	}

	r := &Result{ContainerName: target.name}

	// Step 2: Install ffmpeg wrapper via exec
	action, err := installWrapper(ctx, dockerClient, target.id)
	if err != nil {
		r.Err = fmt.Errorf("install wrapper: %w", err)
		return r, nil
	}
	r.WrapperInstalled = action

	// Step 3: Check JELLYFIN_FFMPEG env
	r.FFmpegEnvOK = checkFFmpegEnv(ctx, dockerClient, target.id)
	if !r.FFmpegEnvOK {
		r.Warnings = append(r.Warnings,
			"JELLYFIN_FFMPEG env var not set — add JELLYFIN_FFMPEG=/config/ffmpeg-wrapper/ffmpeg to Jellyfin's compose environment")
	}

	// Step 4: Set RemoteClientBitrateLimit=0 via system.xml edit
	action, err = configureBitrateLimit(ctx, dockerClient, target.id)
	if err != nil {
		// Non-fatal — wrapper is the important part
		r.BitrateConfigured = "skipped"
		r.Warnings = append(r.Warnings, fmt.Sprintf("could not set RemoteClientBitrateLimit: %v — set manually to 0 in Jellyfin server config", err))
	} else {
		r.BitrateConfigured = action
	}

	return r, nil
}

type jellyfinTarget struct {
	id   string
	name string
}

// discoverJellyfin finds a running jellyfin/jellyfin container.
func discoverJellyfin(ctx context.Context, client *dockerctl.Client, targetName string) (*jellyfinTarget, error) {
	containers, err := client.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var matches []*jellyfinTarget
	for _, c := range containers {
		if !IsSupportedImage(c.Image) {
			continue
		}
		if c.State != "running" {
			log.Printf("jellyfinsetup: skip %s: image matches but state=%s", primaryName(c.Names), c.State)
			continue
		}
		target := &jellyfinTarget{
			id:   c.ID,
			name: primaryName(c.Names),
		}
		if targetName != "" && target.name != targetName {
			continue
		}
		matches = append(matches, target)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if targetName != "" {
		return nil, fmt.Errorf("selected Jellyfin container %q is not running", targetName)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("multiple running Jellyfin containers require an explicit selection")
	}
	return nil, nil
}

// installWrapper writes the ffmpeg wrapper scripts into /config/ffmpeg-wrapper/
// inside the Jellyfin container via exec.
func installWrapper(ctx context.Context, client *dockerctl.Client, containerID string) (string, error) {
	// Check if wrapper already exists with correct content
	checkRes, err := client.Exec(ctx, containerID, "", []string{
		"sh", "-c", dockerpolicy.JellyfinCheckWrapperCommand,
	})
	existedBefore := err == nil && checkRes.ExitCode == 0
	if existedBefore {
		existing := strings.TrimSpace(checkRes.Output)
		if strings.Contains(existing, "DarkHarrbor ffmpeg wrapper") {
			return "already-present", nil
		}
	}

	// Create directory
	if err := execPolicy(ctx, client, containerID, []string{"sh", "-c", dockerpolicy.JellyfinMkdirCommand}); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}

	// Write ffmpeg wrapper
	if err := execPolicy(ctx, client, containerID, dockerpolicy.JellyfinWriteFFmpeg()); err != nil {
		return "", fmt.Errorf("write ffmpeg: %w", err)
	}

	// Write ffprobe passthrough
	if err := execPolicy(ctx, client, containerID, dockerpolicy.JellyfinWriteFFprobe()); err != nil {
		return "", fmt.Errorf("write ffprobe: %w", err)
	}

	if existedBefore {
		return "updated", nil
	}
	return "created", nil
}

// checkFFmpegEnv verifies JELLYFIN_FFMPEG is set in the container's env.
func checkFFmpegEnv(ctx context.Context, client *dockerctl.Client, containerID string) bool {
	res, err := client.Exec(ctx, containerID, "", []string{
		"sh", "-c", dockerpolicy.JellyfinCheckEnvCommand,
	})
	if err != nil || res.ExitCode != 0 {
		return false
	}
	return strings.TrimSpace(res.Output) == "/config/ffmpeg-wrapper/ffmpeg"
}

// configureBitrateLimit sets RemoteClientBitrateLimit=0 in Jellyfin's
// system.xml via direct file edit inside the container. This avoids Jellyfin
// API auth requirements entirely — the config volume is writable from exec.
// Jellyfin reads system.xml at startup; a restart picks up the change.
func configureBitrateLimit(ctx context.Context, client *dockerctl.Client, containerID string) (string, error) {
	// Read current value
	res, err := client.Exec(ctx, containerID, "", []string{
		"sh", "-c", dockerpolicy.JellyfinReadBitrateCommand,
	})
	if err != nil {
		return "", fmt.Errorf("read system.xml: %w", err)
	}

	const configPath = "/config/config/system.xml"

	if res.ExitCode == 0 && strings.TrimSpace(res.Output) != "" {
		// Tag exists — check current value
		if strings.Contains(res.Output, ">0<") {
			return "already-zero", nil
		}
		// Replace existing value with 0
		if err := execPolicy(ctx, client, containerID, []string{"sh", "-c", dockerpolicy.JellyfinSetBitrateCommand}); err != nil {
			return "", fmt.Errorf("sed replace: %w", err)
		}
		return "set-to-zero", nil
	}

	// Tag doesn't exist — check if system.xml exists at all
	checkRes, err := client.Exec(ctx, containerID, "", []string{
		"sh", "-c", dockerpolicy.JellyfinCheckSystemCommand,
	})
	if err != nil || strings.TrimSpace(checkRes.Output) != "yes" {
		return "", fmt.Errorf("system.xml not found at %s — Jellyfin may not have started yet", configPath)
	}

	// Insert the tag before closing </ServerConfiguration>
	if err := execPolicy(ctx, client, containerID, []string{"sh", "-c", dockerpolicy.JellyfinAddBitrateCommand}); err != nil {
		return "", fmt.Errorf("sed insert: %w", err)
	}
	return "set-to-zero", nil
}

func execPolicy(ctx context.Context, client *dockerctl.Client, containerID string, cmd []string) error {
	res, err := client.Exec(ctx, containerID, "", cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exited %d: %s", res.ExitCode, strings.TrimSpace(res.Output))
	}
	return nil
}

func primaryName(names []string) string {
	if len(names) == 0 {
		return "jellyfin"
	}
	return strings.TrimPrefix(names[0], "/")
}

// VisibilityResult reports whether a running Jellyfin container exists and,
// if so, whether it can see DH's data path at the given container-internal
// path -- read-only, via the same docker-exec mechanism installWrapper
// already uses. Does not check library-folder registration inside
// Jellyfin's own configuration (no stored Jellyfin API key exists to query
// that); it verifies the filesystem precondition a library folder would
// depend on.
type VisibilityResult struct {
	ContainerName string
	Found         bool // a running jellyfin/jellyfin container was discovered
	PathVisible   bool // the container's own filesystem sees dataPath
	Err           error
}

// CheckVisibility discovers a running Jellyfin container (S10b's own
// discovery logic) and, if found, runs a bounded read-only `test -e` against
// dataPath inside it. A missing Jellyfin container is reported via
// Found=false, not an error -- Jellyfin is optional infrastructure the same
// way it is throughout jellyfinsetup.Setup.
func CheckVisibility(ctx context.Context, dockerClient *dockerctl.Client, dataPath string) VisibilityResult {
	target, err := discoverJellyfin(ctx, dockerClient, "")
	if err != nil {
		return VisibilityResult{Err: fmt.Errorf("jellyfin discovery: %w", err)}
	}
	if target == nil {
		return VisibilityResult{Found: false}
	}
	r := VisibilityResult{ContainerName: target.name, Found: true}
	if err := execPolicy(ctx, dockerClient, target.id, []string{"test", "-e", dataPath}); err != nil {
		r.Err = err
		return r
	}
	r.PathVisible = true
	return r
}
