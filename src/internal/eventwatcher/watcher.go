// Package eventwatcher keeps the .strm-mode ffprobe shim installed into
// arr containers as they come and go, without polling (REDESIGN S10 Gate
// 4). It has two mechanisms:
//
//   - Startup reconcile: one Discover+Install pass over every currently
//     running arr container, covering anything that started or drifted
//     while DH itself was down (events are only delivered while connected).
//   - Live event stream: subscribes to the proxy's /events endpoint and
//     reacts to container "start" events for images arrdiscovery
//     recognizes, reconnecting with backoff on any stream error.
//
// "create" events are observed and logged but never trigger an install
// attempt: a just-created container isn't running yet, so any exec against
// it would simply fail, and the "start" event that (almost) always follows
// immediately after is the one that actually matters. This is a deliberate
// narrowing of the literal Gate 4 acceptance criteria wording ("start"/
// "create" event triggers...) to what's actually actionable -- documented
// here rather than left as a silent gap.
package eventwatcher

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/arrdiscovery"
	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
	"github.com/darkharrbor/darkharrbor/internal/shiminstall"
)

// reconnectBackoff is how long Run waits before reopening the event stream
// after it drops (socket reset, proxy restart, etc.).
const reconnectBackoff = 3 * time.Second

// installRetryInterval / installRetryAttempts bound the retry loop for a
// container that just reported "start" but whose /app directory (and thus
// ffprobe path) isn't populated yet -- LSIO's s6-overlay init can lag a
// beat behind the Docker-level "start" event on a container's very first
// boot. Five attempts one second apart lands within the Gate 4 AC's ~5s
// target for the common case (already-initialized container restarting)
// while giving a brand-new container a few extra beats too.
const (
	installRetryInterval = 1 * time.Second
	installRetryAttempts = 5
)

// Run blocks until ctx is canceled: it performs one startup reconcile pass,
// then watches for live container-start events, reconnecting with backoff
// on any stream error. It is safe to run in its own goroutine; on ctx
// cancellation it returns ctx.Err() once any in-flight Next() call unblocks
// -- no goroutines are left running afterward (see dockerctl's
// TestEventsContextCancelUnblocksNext, which guards the primitive this
// relies on).
func Run(ctx context.Context, client *dockerctl.Client, allowedNames []string) error {
	allowed := nameAllowlist(allowedNames)
	reconcile(ctx, client, allowed)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err := watchOnce(ctx, client, allowed); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("eventwatcher: event stream error, reconnecting in %s: %v", reconnectBackoff, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnectBackoff):
		}
	}
}

// reconcile runs one full Discover+Install pass. Errors are logged, not
// returned -- a reconcile failure (e.g. the proxy briefly unreachable at
// startup) shouldn't prevent Run from proceeding to the event-watch loop,
// which will pick up any missed containers on their next start event, or on
// the next reconcile if Run itself is restarted.
func reconcile(ctx context.Context, client *dockerctl.Client, allowed map[string]bool) {
	targets, err := arrdiscovery.Discover(ctx, client)
	if err != nil {
		log.Printf("eventwatcher: startup reconcile: discover failed: %v", err)
		return
	}
	for _, target := range targets {
		if !nameAllowed(allowed, target.Name) {
			continue
		}
		result, err := shiminstall.Install(ctx, client, target)
		if err != nil {
			log.Printf("eventwatcher: startup reconcile: install failed for %s: %v", target.Name, err)
			continue
		}
		log.Printf("eventwatcher: startup reconcile: %s action=%s verified=%v", target.Name, result.Action, result.Verified)
	}
}

// watchOnce opens one event-stream connection and processes events from it
// until the stream errors or ctx is canceled. Returning nil only happens via
// ctx cancellation (Next() has no other way to return cleanly on a stream
// that's meant to run forever); any other return is an error the caller
// should reconnect after.
func watchOnce(ctx context.Context, client *dockerctl.Client, allowed map[string]bool) error {
	stream, err := client.Events(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	for {
		event, err := stream.Next()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		handleEvent(ctx, client, event, allowed)
	}
}

// handleEvent reacts to one container event. Only Type=="container" &&
// Action=="start" triggers work; everything else (including "create", see
// package doc comment) is a no-op, logged at a lower level for
// create/other-action visibility without noise on every unrelated event.
func handleEvent(ctx context.Context, client *dockerctl.Client, event dockerctl.Event, allowed map[string]bool) {
	if event.Type != "container" {
		return
	}
	image := event.Actor.Attributes["image"]
	name := event.Actor.Attributes["name"]
	if !nameAllowed(allowed, name) {
		return
	}

	switch event.Action {
	case "create":
		if arrdiscovery.MatchApp(image) != "" {
			log.Printf("eventwatcher: observed create for arr-image container %s (%s); awaiting start event", name, image)
		}
		return
	case "start":
		// fall through to install below
	default:
		return
	}

	if arrdiscovery.MatchApp(image) == "" {
		return // not an arr image; ignore silently, this is the common case for every non-arr container start
	}

	result, installErr := installWithRetry(ctx, client, event.Actor.ID, image, []string{name})
	if installErr != nil {
		log.Printf("eventwatcher: install failed for %s (%s) after retries: %v", name, event.Actor.ID[:min(12, len(event.Actor.ID))], installErr)
		return
	}
	log.Printf("eventwatcher: %s action=%s verified=%v", result.Name, result.action, result.verified)
}

func nameAllowlist(names []string) map[string]bool {
	if names == nil {
		return nil
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	return allowed
}

func nameAllowed(allowed map[string]bool, name string) bool {
	return allowed == nil || allowed[name]
}

// installResult bundles what handleEvent needs to log after a successful
// installWithRetry, without exporting shiminstall.Result fields redundantly.
type installResult struct {
	Name     string
	action   shiminstall.Action
	verified bool
}

// installWithRetry resolves and installs the shim for one just-started
// container, retrying ResolveTarget on failure (see installRetryInterval/
// installRetryAttempts) since the ffprobe path may not exist for the first
// beat after a "start" event on a fresh container.
func installWithRetry(ctx context.Context, client *dockerctl.Client, containerID, image string, names []string) (installResult, error) {
	var lastErr error
	for attempt := 1; attempt <= installRetryAttempts; attempt++ {
		target, matched, err := arrdiscovery.ResolveTarget(ctx, client, containerID, image, names)
		if !matched {
			// Shouldn't happen (caller already checked MatchApp), but treat
			// as a non-error no-op rather than a retry-worthy failure.
			return installResult{}, errors.New("eventwatcher: container no longer matches an arr image")
		}
		if err != nil {
			lastErr = err
			select {
			case <-ctx.Done():
				return installResult{}, ctx.Err()
			case <-time.After(installRetryInterval):
			}
			continue
		}

		result, err := shiminstall.Install(ctx, client, target)
		if err != nil {
			return installResult{}, err
		}
		return installResult{Name: target.Name, action: result.Action, verified: result.Verified}, nil
	}
	return installResult{}, lastErr
}
