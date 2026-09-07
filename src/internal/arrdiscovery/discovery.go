// Package arrdiscovery finds which running containers are Sonarr/Radarr
// instances that need the .strm-mode ffprobe shim installed (REDESIGN S10
// Gate 2), and resolves each to the in-container ffprobe path the shim
// install routine (Gate 3) will replace.
//
// Only Sonarr and Radarr need the wrapper: they're the two arrs that run
// ffprobe against imported media. Prowlarr is a meta-indexer with no file
// import step, so it's deliberately excluded even though it's also an LSIO
// "arr" container DH talks to elsewhere (torznab). qBittorrent/SABnzbd are
// download clients, not media importers, and are excluded for the same
// reason. Getting this allowlist wrong in either direction is exactly what
// the Gate 2 acceptance criteria (REDESIGN S10) is checking for: "the
// selected set contains zero non-arr containers."
package arrdiscovery

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
	"github.com/darkharrbor/darkharrbor/internal/dockerpolicy"
)

// App identifies which arr a matched container is running.
type App string

const (
	AppSonarr App = "sonarr"
	AppRadarr App = "radarr"
)

// imageMatchers maps an image-name regex to the App it identifies. Matched
// against the repository portion only (before any ":tag"), so it's tag- and
// registry-digest-agnostic. Deliberately narrow: exact LSIO image names,
// not a broad "contains sonarr" substring match, to avoid false positives
// from unrelated images that happen to share a word.
var imageMatchers = []struct {
	pattern *regexp.Regexp
	app     App
}{
	{regexp.MustCompile(`(^|/)linuxserver/sonarr(:|$)`), AppSonarr},
	{regexp.MustCompile(`(^|/)linuxserver/radarr(:|$)`), AppRadarr},
}

// ArrContainer is one discovered, shim-eligible container.
type ArrContainer struct {
	ContainerID string
	Name        string // container name with leading "/" trimmed
	Image       string
	App         App
	FFprobePath string // e.g. "/app/sonarr/bin/ffprobe"; empty if not resolved
}

// MatchApp returns the App an image name identifies, or "" if it isn't a
// shim-eligible arr image. Exported so callers that already have a
// container's image from another source (e.g. an events-stream Actor, see
// internal/eventwatcher) can classify it without a full Discover() list call.
func MatchApp(image string) App {
	for _, m := range imageMatchers {
		if m.pattern.MatchString(image) {
			return m.app
		}
	}
	return ""
}

// Discover lists all containers via client, filters to running Sonarr/Radarr
// containers by image match, and resolves each one's in-container ffprobe
// path. It has no side effects (read-only list + inspect-via-exec-ls calls
// only) and is safe to call repeatedly -- each call is independent and
// reflects current container state, satisfying the Gate 2 "idempotent"
// acceptance criterion trivially, since there's no persisted install state
// here yet (that begins at Gate 3).
//
// A container matching the image allowlist but not currently running is
// skipped (exec would fail against a stopped container) and logged, not
// treated as an error -- the events watcher (Gate 4) picks it up once it
// starts. A matched+running container whose ffprobe path can't be resolved
// is also skipped and logged rather than aborting the whole discovery pass,
// since one arr's unusual layout shouldn't block installing into the others.
func Discover(ctx context.Context, client *dockerctl.Client) ([]ArrContainer, error) {
	containers, err := client.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("arrdiscovery: list containers: %w", err)
	}

	var matched []ArrContainer
	for _, c := range containers {
		app := MatchApp(c.Image)
		if app == "" {
			continue
		}
		name := primaryName(c.Names)
		if c.State != "running" {
			log.Printf("arrdiscovery: skip %s (%s): matched image %s but state=%s, not running", name, c.ID[:min(12, len(c.ID))], c.Image, c.State)
			continue
		}

		path, err := resolveFFprobePath(ctx, client, c.ID)
		if err != nil {
			log.Printf("arrdiscovery: skip %s (%s): could not resolve ffprobe path: %v", name, c.ID[:min(12, len(c.ID))], err)
			continue
		}

		matched = append(matched, ArrContainer{
			ContainerID: c.ID,
			Name:        name,
			Image:       c.Image,
			App:         app,
			FFprobePath: path,
		})
	}

	log.Printf("arrdiscovery: matched %d arr container(s): %s", len(matched), summarize(matched))
	return matched, nil
}

// ResolveTarget classifies and resolves a single container by ID and image,
// without listing every container on the host -- used by the events watcher
// (Gate 4) to react to one start event at a time instead of re-running
// Discover's full list+filter pass on every event.
//
// Returns (zero, false, nil) if image isn't a shim-eligible arr image at
// all -- not an error, just "nothing to do here". Returns (zero, true, err)
// if it IS an eligible image but the ffprobe path couldn't be resolved
// (e.g. exec'd too early, before the arr's /app directory is populated --
// expected to be transient; the caller is expected to retry with backoff,
// see eventwatcher's bounded retry loop).
func ResolveTarget(ctx context.Context, client *dockerctl.Client, containerID, image string, names []string) (ArrContainer, bool, error) {
	app := MatchApp(image)
	if app == "" {
		return ArrContainer{}, false, nil
	}
	path, err := resolveFFprobePath(ctx, client, containerID)
	if err != nil {
		return ArrContainer{}, true, err
	}
	return ArrContainer{
		ContainerID: containerID,
		Name:        primaryName(names),
		Image:       image,
		App:         app,
		FFprobePath: path,
	}, true, nil
}

// resolveFFprobePath execs a shell glob inside the container to find its
// ffprobe binary. LSIO arr images keep it at /app/<appname>/bin/ffprobe;
// the app-name path segment isn't assumed statically (it has drifted
// before, e.g. custom LSIO branch images) -- the glob discovers it live.
func resolveFFprobePath(ctx context.Context, client *dockerctl.Client, containerID string) (string, error) {
	result, err := client.Exec(ctx, containerID, "", dockerpolicy.ResolveFFprobe())
	if err != nil {
		return "", fmt.Errorf("exec ls: %w", err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("exec ls exited %d", result.ExitCode)
	}
	lines := strings.Fields(strings.TrimSpace(result.Output))
	switch len(lines) {
	case 0:
		return "", fmt.Errorf("no /app/*/bin/ffprobe found")
	case 1:
		return lines[0], nil
	default:
		return "", fmt.Errorf("ambiguous: %d ffprobe paths found (%s)", len(lines), strings.Join(lines, ", "))
	}
}

// primaryName returns the first Docker container name with its leading
// slash trimmed, or "?" if the list is empty (shouldn't happen in practice).
func primaryName(names []string) string {
	if len(names) == 0 {
		return "?"
	}
	return strings.TrimPrefix(names[0], "/")
}

func summarize(matched []ArrContainer) string {
	if len(matched) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(matched))
	for _, m := range matched {
		parts = append(parts, fmt.Sprintf("%s[%s]", m.Name, m.App))
	}
	return strings.Join(parts, ", ")
}
