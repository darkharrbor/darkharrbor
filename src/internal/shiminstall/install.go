// Package shiminstall installs the .strm-mode ffprobe wrapper into a
// discovered arr container (REDESIGN S10 Gate 3), using
// internal/dockerctl.Client.Exec as root -- no stdin-attach, no bind mount,
// no compose changes. Stdin-attach through the Docker Engine API needs raw
// socket hijacking that net/http doesn't expose cleanly, and isn't needed
// here anyway: the wrapper script is a few KB, well within exec argv limits
// once base64-encoded, so it travels as a Cmd argument instead.
//
// The wrapper script itself (assets/ffprobe-wrapper.sh) is embedded at build
// time via go:embed. It is still the interim direct-exec-ffprobe-full
// version, not yet the D-RELAY curl-POST version -- that content swap is
// Gate 6, sequenced after Gate 5 (the relay endpoint it will POST to)
// exists to test against. Nothing in this package's install logic changes
// when that swap happens; it installs whatever bytes are embedded.
package shiminstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/arrdiscovery"
	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
	"github.com/darkharrbor/darkharrbor/internal/dockerpolicy"
)

var wrapperScript = dockerpolicy.FFprobeWrapper

// wrapperMarker must match the marker string ffprobe-wrapper.sh writes at
// its own top (HARRBOR_STRM_WRAPPER) -- used to detect whether the file
// currently at the target path is already our wrapper vs. the arr's stock
// ffprobe.
const wrapperMarker = "HARRBOR_STRM_WRAPPER"

// Action describes what Install actually did.
type Action string

const (
	ActionAlreadyCurrent Action = "already-current"   // wrapped + checksum matched embedded script; no-op
	ActionInstalled      Action = "installed"         // was stock ffprobe; backed up + installed
	ActionReinstalled    Action = "reinstalled-drift" // was wrapped but checksum differed; reinstalled, no re-backup
)

// Result is the outcome of one Install call.
type Result struct {
	Action   Action
	Verified bool // true if the post-install `ffprobe -version` smoke test exited 0
}

// wrapperSHA256 is computed once from the embedded script and compared
// against each target's on-disk checksum to decide no-op vs. reinstall.
var wrapperSHA256 = func() string {
	sum := sha256.Sum256(wrapperScript)
	return hex.EncodeToString(sum[:])
}()

// Install installs the embedded wrapper into target.FFprobePath inside
// target.ContainerID, idempotently:
//   - not currently wrapped (stock binary)  -> back up (if no backup exists
//     yet), then install. Action=Installed.
//   - wrapped, checksum matches embedded    -> no-op. Action=AlreadyCurrent.
//   - wrapped, checksum differs (drift,
//     e.g. an older wrapper version)        -> reinstall, no new backup
//     (the original is already preserved from the first install).
//     Action=ReinstalledDrift.
//
// The write itself is atomic: content lands at "<path>.tmp" first, then
// `mv` replaces the live path in one step, so a concurrently-starting arr
// process never observes a partially-written wrapper.
//
// After install/reinstall, a smoke test runs `<path> -version` inside the
// container and reports whether it exited 0 (Result.Verified) -- this
// doesn't affect Result.Action, since a failed smoke test after a
// successful atomic write is a problem worth surfacing at the call site,
// not a reason to consider the install itself to not have happened.
func Install(ctx context.Context, client *dockerctl.Client, target arrdiscovery.ArrContainer) (*Result, error) {
	if target.FFprobePath == "" {
		return nil, fmt.Errorf("shiminstall: target %s has no resolved ffprobe path", target.Name)
	}
	path := target.FFprobePath

	wrapped, remoteSHA, err := probe(ctx, client, target.ContainerID, path)
	if err != nil {
		return nil, fmt.Errorf("shiminstall: probe %s in %s: %w", path, target.Name, err)
	}

	var action Action
	switch {
	case wrapped && remoteSHA == wrapperSHA256:
		action = ActionAlreadyCurrent
	case wrapped:
		action = ActionReinstalled
	default:
		action = ActionInstalled
		if err := backupIfNeeded(ctx, client, target.ContainerID, path); err != nil {
			return nil, fmt.Errorf("shiminstall: backup %s in %s: %w", path, target.Name, err)
		}
	}

	if action == ActionAlreadyCurrent {
		return &Result{Action: action, Verified: true}, nil
	}

	if err := writeAtomic(ctx, client, target.ContainerID, path); err != nil {
		return nil, fmt.Errorf("shiminstall: write %s in %s: %w", path, target.Name, err)
	}

	verified, err := verify(ctx, client, target.ContainerID, path)
	if err != nil {
		return nil, fmt.Errorf("shiminstall: verify %s in %s: %w", path, target.Name, err)
	}

	return &Result{Action: action, Verified: verified}, nil
}

// probe checks whether path currently holds our wrapper (marker string in
// the first 8KB, matching is_wrapped() in strm-init.sh) and, if so, its
// sha256, in a single exec round trip.
func probe(ctx context.Context, client *dockerctl.Client, containerID, path string) (wrapped bool, sha string, err error) {
	result, err := client.Exec(ctx, containerID, "root", dockerpolicy.ShimProbe(path))
	if err != nil {
		return false, "", err
	}
	if result.ExitCode != 0 {
		return false, "", fmt.Errorf("probe exited %d: %s", result.ExitCode, result.Output)
	}
	return parseProbeOutput(result.Output)
}

func parseProbeOutput(output string) (wrapped bool, sha string, err error) {
	line := strings.TrimSpace(output)
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return false, "", fmt.Errorf("unexpected probe output: %q", output)
	}
	for _, f := range fields {
		switch {
		case strings.HasPrefix(f, "WRAPPED="):
			wrapped = strings.TrimPrefix(f, "WRAPPED=") == "1"
		case strings.HasPrefix(f, "SHA="):
			sha = strings.TrimPrefix(f, "SHA=")
		}
	}
	return wrapped, sha, nil
}

// backupIfNeeded preserves the arr's original stock ffprobe at "<path>.real"
// exactly once. Only called from the not-currently-wrapped branch of
// Install, so a call here means the file at path is genuinely stock -- but
// the backup step itself still checks test -e first, defensively, in case a
// prior partial run already created it without completing the install (e.g.
// a crash between backup and write).
func backupIfNeeded(ctx context.Context, client *dockerctl.Client, containerID, path string) error {
	result, err := client.Exec(ctx, containerID, "root", dockerpolicy.ShimBackup(path))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("backup exited %d: %s", result.ExitCode, result.Output)
	}
	return nil
}

// writeAtomic removes the exact stale temporary entry, base64-decodes the
// embedded wrapper script to "<path>.tmp", marks it executable, then mv's it
// over path in one step. Removing first prevents redirection through a stale
// temporary symlink; the final replacement remains atomic.
func writeAtomic(ctx context.Context, client *dockerctl.Client, containerID, path string) error {
	result, err := client.Exec(ctx, containerID, "root", dockerpolicy.ShimWrite(path))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("write exited %d: %s", result.ExitCode, result.Output)
	}
	return nil
}

// verify runs "<path> -version" inside the container and reports whether it
// exited 0. -version isn't an -i/input flag, so the wrapper's own arg-scan
// treats it as a non-input option and falls through to passthrough against
// ffprobe.real -- a real invocation of the installed wrapper, not a stub
// check, satisfying the Gate 3 AC ("an in-container ffprobe -version then
// invokes the shim").
func verify(ctx context.Context, client *dockerctl.Client, containerID, path string) (bool, error) {
	result, err := client.Exec(ctx, containerID, "root", dockerpolicy.ShimVerify(path))
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}
