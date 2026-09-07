---
name: darkharrbor-setup-guide
description: Helps a user install and set up DarkHarrbor — either by conversationally walking them through install.sh and the setup wizard stage by stage as they run it themselves, or by gathering their choices and running the whole thing autonomously using install.sh's --answers-file headless flag. Use when a user wants help installing, setting up, configuring, or troubleshooting DarkHarrbor, or asks what an install/setup prompt means.
---

# DarkHarrbor setup guide

Arr, a new crew member wants to get DarkHarrbor seaworthy. Two ways to do
this, and the user needs to actually pick one — don't default to either
silently.

## Step 0: always ask first

Before doing anything else, lay out the choice plainly and let the user
pick:

1. **Walk me through it** — you explain each stage as the user runs
   `install.sh` and the wizard themselves, in their own terminal. The user
   stays at the keyboard, types every command and every answer themselves.
   Zero risk of Claude getting something wrong against real credentials,
   since Claude never executes anything.
2. **Just install it** — the user tells you their choices (which
   providers, which Arrs, media path, etc.) and you gather the answers,
   show the user the complete plan for explicit confirmation, then run
   `install.sh --answers-file` to do the whole thing in one shot.

If they pick option 2, ask one more thing: do they want to **dictate
credentials in this chat** (simplest, but the values end up in this
conversation's transcript), or **stage them in a local file themselves**
first (you tell them the exact file shape needed; they fill it in with a
text editor, outside the chat; you only read it once, never echo its
contents back, and it gets deleted automatically once `install.sh`
consumes it). Offer both, let them choose — don't assume which they'd
prefer.

Never proceed past this point without an explicit choice from the user.

---

## Mode 1: Walk me through it

The authoritative, stage-by-stage script for this is
`docs/GETTING-STARTED.md` in the DarkHarrbor repo the user has checked
out — read it fresh each time rather than relying on a memorized summary,
since it documents the wizard's real source-verified behavior and both
can change. Follow its real order: `install.sh`'s own prompts (image
download, container/network detection, media path, install preview), then
the wizard's real running order (`S1, S2, S2b, S3, S3b, S4, S8, S5, S6,
S9, S7, S7c, S7b, S7d, S10, S10b, S11, S12` — the wizard prints this
itself, don't assume a different order).

As each real prompt comes up in the user's terminal:
- Explain in plain terms what it's asking and why, drawing on
  `GETTING-STARTED.md`'s explanation of that stage.
- If something looks wrong (an error, an unexpected prompt, a "degraded"
  or "externally pending" result), help them read it — don't guess at a
  fix without checking the actual message against `CONFIGURATION.md` or
  the relevant source under `src/internal/` if the doc doesn't cover it.
- Never ask the user to paste a secret (API token, password) into this
  chat in this mode. There's no reason to — they're typing it directly
  into their own terminal prompt, not through you.

End with `darkharrbor doctor` and, if they ran it, the S12 zero-account
demo, to confirm things actually work — not just that the wizard finished
without erroring.

---

## Mode 2: Just install it

This mode has real teeth: it creates Docker volumes and networks, pulls
images, writes credentials into a sealed store, and registers DarkHarrbor
with the user's real Sonarr/Radarr instances. Move carefully.

### 2.1 — Gather the plan

Through conversation, work out:
- Which acquisition lanes (TorBox, Real-Debrid, AllDebrid, Premiumize,
  direct NNTP, TorBox News Server) and their credentials, per whichever
  staging choice was made in Step 0.
- Whether to use Docker auto-discovery for Sonarr/Radarr/Prowlarr/Jellyfin
  (and which containers to approve) or manual entry (URL + API key per
  instance).
- The Docker network(s) to attach to, and the media path for `.strm`
  files (must already exist and be writable — same rule as a manual
  install).
- Acquisition-lane priority order, once it's clear which lanes are
  actually available.
- Performance profile (or let the wizard auto-select).
- Whether to set up play-to-commit (aggregator integration) — if yes,
  this forces the Stremio client edge stage on automatically, per
  `src/internal/wizard/onboarding_plan.go`'s `plan.StremioAddon = true`
  when `plan.Aggregator` is selected. Check current source if unsure;
  this is exactly the kind of detail that can drift from memory.
- Backups (target, cadence, retention) and the zero-account demo
  (recommended — it's free, real, and proves the pipeline works).

### 2.2 — Derive the exact prompt sequence from current source

**Do not use a hardcoded answer-sequence template.** The wizard's
conditional branching (which prompts appear, in what order, depends on
exactly which lanes/providers/features were selected) is real code that
can change between DarkHarrbor versions. Read the actual current source
in the user's checkout before building the answers file:

- `install.sh` itself, for its own pre-wizard prompts (image download
  confirm, container/network approval, media path, install preview) —
  these are NOT covered by `-answers` and must be piped separately; see
  2.5.
- `src/internal/wizard/wizard.go` for the real stage order and branching
  (`plan.hasProvider`, `plan.hasLane`, etc. gate which prompts fire).
- `src/internal/wizard/debrid_setup.go` (Real-Debrid/AllDebrid/Premiumize
  prompts), `performance.go` (performance-profile prompt), `stremio_edge.go`
  (S7c), `reactive_destinations.go` (S7d, only if play-to-commit),
  `onboarding_plan.go` (the plan-selection prompts that precede all of
  this, including which lanes/features are even offered).

Cross-check against `docs/GETTING-STARTED.md`'s walkthrough as a sanity
check — if the two disagree, trust the source, and treat the doc as
possibly stale (flag that to the user if it happens, it's worth fixing).

### 2.3 — Build the answers file

Format (per `src/internal/wizard`'s headless-replay support): a JSON
object `{"responses": ["answer1", "answer2", ...]}`, where each array
entry is fed as one line of stdin, in the *exact* order prompts actually
fire for the plan derived in 2.2 — including secret prompts, which take a
plain array entry too, not special handling.

Requirements, enforced by both `darkharrbor setup -answers` and
`install.sh --answers-file` itself (fails closed if violated, so get this
right):
- Write it to a path the user controls (suggest somewhere private, e.g.
  under their home directory, not inside the repo).
- `chmod 600` the file.
- Its parent directory must not be group- or world-writable.
- Absolute path, no embedded newline, not a symlink.

### 2.4 — Confirm the full plan before executing anything

Before running anything, show the user a plan summary in the same spirit
as `install.sh`'s own "Installation preview (no secrets)" screen —
everything EXCEPT secret values (name the providers/lanes selected, not
the tokens themselves) — and get an explicit go-ahead. This mirrors
`install.sh`'s own safety pattern and is not optional: never execute
`install.sh` in this mode without a clear, explicit confirmation of the
full plan first.

### 2.5 — Execute

`install.sh`'s own prompts (before the wizard) are plain, TTY-free `read
-r` prompts and can be piped directly; only the wizard itself needs
`--answers-file`. Combine both in one command:

```sh
printf '<install.sh prompt answers, one per line, in order>\n' | \
  ./install.sh --answers-file /absolute/path/to/answers.json
```

Derive the exact pre-wizard answer sequence from `install.sh`'s current
source too (same discipline as 2.2) — it's a handful of prompts (image
download confirm, container approval, network approval, media path,
install preview confirm), all documented in `GETTING-STARTED.md`'s
"Weigh anchor" through "Reviewing the manifest" sections, but verify
against the actual script since flags/defaults can change.

`install.sh --answers-file` deletes the answers file itself once consumed
— on success or failure alike — so there's nothing to clean up
afterward on that front.

### 2.6 — After it runs

- If it succeeded: run `darkharrbor doctor` (the install already ran it
  once, but confirm current state), report what it found, and mention the
  private handoff directory (`recovery.key` etc.) needs to be copied to
  the user's own secure storage and deleted locally — see
  `GETTING-STARTED.md`'s "What you're handed at the end."
- If it failed: `install.sh` cleans up after itself safely (removes the
  incomplete project, volumes, and Compose file; never leaves a half-built
  deployment running) — explain what the error actually said rather than
  guessing, and don't blindly retry with the same inputs without
  understanding why it failed first.

---

## Either mode

Light pirate flavor in how you talk about this is fine and expected — "your fleet," "weigh anchor," "the hold" — but keep every actual command, prompt, flag, and file path completely literal. Never invent a flag or a file path; check the current source or `GETTING-STARTED.md`/`CONFIGURATION.md` if unsure. For deeper reference: `CONFIGURATION.md` (every setting) and `CONFIGURATION.md § Subcommands` (the full CLI, verbatim from `darkharrbor help`) cover anything this walkthrough doesn't.
