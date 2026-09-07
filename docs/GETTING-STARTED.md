# Getting Started

The full walkthrough for standing up DarkHarrbor from nothing, stage by
stage, following `install.sh`'s and the setup wizard's actual running
order — not a summary, the real prompts and real output. Prefer to have
an AI drive the whole thing instead? See
[.claude/skills/](../.claude/skills/).

## Before you start

You'll want:

- **Docker, and Compose v2, on the host running DarkHarrbor itself.**
  `install.sh` checks for both before it does anything else — `docker info`
  has to actually answer, and `docker compose version` has to report v2.
  No Docker, no voyage. (DarkHarrbor itself must run in Docker — this only
  covers where *it* runs, see below for the Arrs.)
- **A media path, already there.** Somewhere on this host for DarkHarrbor's
  `.strm` claim tickets to live — the installer will ask for it and refuses
  to sail if the directory doesn't already exist or isn't writable by its
  crew (UID/GID `1000:1000`).
- **Sonarr, Radarr, and/or Prowlarr — recommended, not required, and not
  even required to be in Docker.** If they're up *in Docker* when you run
  the installer, it finds them on its own by matching known images. If
  they're anywhere else reachable on the network — bare metal, a different
  host, whatever — you can still enter each one by hand (URL + API key,
  verified live before anything's stored); you just lose the
  auto-discovery, and one follow-up step (installing the ffprobe shim)
  becomes a manual copy-paste instead of automatic.

Nothing else. The installer doesn't ask for a single API key, password, or
account credential of any kind before you approve a deployment — it reads
your existing containers' own configuration for the ones it finds, and
everything else (TorBox, Real-Debrid, usenet providers, and so on) is
entered live, inside the setup wizard itself, once DarkHarrbor is actually
running.

**If you're planning to set up play-to-commit later** (watch something,
have it land in your library automatically — covered in
[docs/AGGREGATOR-INTEGRATION.md](AGGREGATOR-INTEGRATION.md), not this
doc): that needs a separate self-hosted Stremio-compatible aggregator,
AIOStreams being the tested one. Worth knowing now rather than later —
AIOStreams' own debrid layer defaults to a **public** StremThru instance,
and that public instance is currently down. A working AIOStreams +
DarkHarrbor play-to-commit setup needs its own **self-hosted** StremThru,
not the default. None of this blocks the walkthrough below; it's only
relevant once you get to that part.

## Weigh anchor

```sh
git clone https://github.com/darkharrbor/darkharrbor.git
cd darkharrbor
./install.sh
```

First words out of it:

```
Welcome to DarkHarrbor — the dark port for your media library.

Credentials are not requested or read by this installer.
```

Take that literally — nothing below asks for an account credential of any
kind. What it does ask for next:

```
This will download the main and scoped-connector images for release latest.
Continue with the image download? [Y/n]:
```

Say yes, and it pulls two images: the main `darkharrbor` image, and a
separate, much smaller `docker-proxy` image — a locked-down Docker-socket
connector DarkHarrbor uses only to talk to the containers it's allowed to
touch (more on that in a moment). Both get pinned to their exact digest
right after pulling, not just a tag, and the installer refuses to continue
if either image doesn't carry the expected schema label — a release too
old for this installer, or a tampered image, gets rejected here rather
than failing weirdly later.

Want a specific release instead of the latest? Set `DARKHARRBOR_VERSION`
before running. Running behind a private registry mirror, or on a
non-default port? `DARKHARRBOR_IMAGE_REPO` and `DARKHARRBOR_HOST_PORT`
(default `8382`) are there too — the install preview screen in a couple of
steps will show you exactly what it resolved before anything's created.

## Charting your fleet

Next it goes looking for who else is docked at this port. It scans your
*running* containers for known Sonarr, Radarr, Prowlarr, and Jellyfin
images (LinuxServer.io images for the first three, the official Jellyfin
image for the last) and lists whatever it finds — including more than one
of the same kind, if you run them. Multiple Sonarr and/or Radarr instances
are fully supported side by side (say, one for TV and a separate one for
anime, or however you split your library); each gets configured
independently later in the wizard.

```
Detected supported running containers (names and images only):
  1. sonarr (linuxserver/sonarr:latest)
  2. sonarr-anime (linuxserver/sonarr:latest)
  3. radarr (linuxserver/radarr:latest)
  4. prowlarr (linuxserver/prowlarr:latest)
Approve connector access by number, "all", or "manual" [all]:
```

This is the only vote the `docker-proxy` connector ever gets. Whatever you
approve here — by number, comma-separated, `all`, or `manual` to skip it
entirely — is the *entire, permanent* allowlist of containers it's allowed
to reach. Nothing discovered later can widen it; the installer itself
checks that at the end and refuses to continue if anything tries.

(If you run more than one Sonarr and/or Radarr, note that picking which
one is the *default* for play-to-commit — where a watched title lands when
more than one could take it — isn't decided here. That choice comes later
in the wizard, once each instance is already configured with its own root
folder and quality profile.)

No supported containers running? You'll see "No supported running
containers were detected; the wizard will use manual Arr entry" instead,
and that's fine — see [Before you start](#before-you-start) above.

Right after, if you approved anything, it looks at which Docker networks
those containers actually sit on and offers to attach DarkHarrbor to the
same ones:

```
Detected user-defined networks for the approved containers: <your-network-name>
Attach DarkHarrbor to these networks? [Y/n]:
```

Whatever that network's actually called on your host is what shows up
there — there's no fixed or expected name. Say no (or nothing was
detected) and it falls back to showing you every user-defined network on
the host and asking you to type the name(s) yourself, comma-separated.
DarkHarrbor needs at least one user-defined network to continue — the
default `bridge` network doesn't count and won't be offered.

## Choosing your berth

```
Host media path for DarkHarrbor .strm files [/mnt/darkharrbor]:
```

This is the one directory on this host where DarkHarrbor's `.strm` claim
tickets actually live — not your media library itself, just the tiny
pointer files. Press enter for the default, or type your own absolute
path. Two rules, both enforced before anything else happens:

- **It has to already exist.** DarkHarrbor won't create it for you — "does
  not exist; create and permission it deliberately before installation"
  is the installer being deliberately unhelpful here, on purpose. Create
  it yourself first if it isn't there.
- **It has to actually be writable by DarkHarrbor's own crew, not just
  root.** The installer doesn't take your word for it — it spins up a
  disposable, fully locked-down container (read-only root filesystem, no
  network, every capability dropped, running as DarkHarrbor's real
  non-root UID/GID `1000:1000`) and tries to write into your path for
  real. If that fails, the install stops right here rather than limping
  along and failing later, mid-download.

## Reviewing the manifest

Before anything gets created, you get the full plan laid out — no secrets
in it, because none have been asked for yet:

```
Installation preview (no secrets):
  main image:      ghcr.io/darkharrbor/darkharrbor@sha256:...
  connector image: ghcr.io/darkharrbor/darkharrbor-dockerproxy@sha256:...
  compose file:    ./.darkharrbor/compose.yml (mode 0600)
  media path:      /mnt/darkharrbor
  networks:        <your-network-name>
  approved targets: sonarr, sonarr-anime, radarr, prowlarr
  generic catalog: none
  runtime access:  narrowed after setup to only selected managed services
  published port:  127.0.0.1:8382 only
Create this deployment and run the first-use wizard? [y/N]:
```

Two things worth actually reading before you answer:

- **`published port: 127.0.0.1:8382 only`.** By default, DarkHarrbor's
  client-facing listener is bound to loopback on this host — it's not
  reachable from your LAN or the internet until you deliberately expose
  it (a reverse proxy, Tailscale, whatever fits your setup). Nothing
  leaks out unpublished.
- **This prompt defaults to No**, unlike the earlier ones. Everything up
  to here has been read-only reconnaissance; saying `y` is the actual
  point of no return — it creates the Compose deployment, the Docker
  volumes, and kicks off the first-run setup wizard next.

(The `generic catalog` line only shows up as anything other than `none`
if you ran `install.sh --catalog /path/to/catalog.json`. This isn't OMSS
or the aggregator — it's DarkHarrbor's own fourth acquisition lane: an
operator-declared, typed list of direct sources (plain HTTP, HLS, M3U
playlists, metalink, S3-style objects, WebDAV, or a fixed URL) rather than
a searched backend. The WebDAV private-cloud support mentioned in the
[README](../README.md#whats-in-the-hold) is actually one entry kind inside
this same system. Not something you need for a first install; see
[CONFIGURATION.md](../CONFIGURATION.md#private-cloud-drives-through-webdav)
if you want it later.)

## The wizard itself

Once the deployment's created, `darkharrbor setup` runs automatically,
right there in your terminal. Heads up: the stages below are numbered
`S1`–`S12` in the code, but they don't run in that numeric order — the
wizard prints its real running order up front (`S1, S2, S2b, S3, S3b, S4,
S8, S5, S6, S9, S7, S7c, S7b, S7d, S10, S10b, S11, S12`) so you're never
guessing what's next. This walkthrough follows that real order.

Nothing here is graded pass/fail against a script — every account you give
it gets checked live, right then, against the real provider, not just
saved on faith.

### Filling the hold: your sources

**S2 — TorBox.** If you selected it, you're prompted for an API token.
Give it one and the wizard calls TorBox's own account API on the spot and
shows you exactly what it found:

```
── S2  TorBox + Plan Self-Config ─────────────────────
  TorBox API token: [hidden]
  ✓ TorBox account verified.
    Plan:          Standard
    Active slots:  5
    Max bandwidth: 500 GB
    Usenet ingest: yes
    News server:   yes
```

No token entered? That's fine — TorBox is entirely optional — but you'll
see a note that plan detection, News Server, and AirLock are unavailable,
and a reminder that *some* acquisition lane (debrid or direct NNTP) is
still required before setup will let you finish.

**S2b/S3/S3b — Real-Debrid, AllDebrid, Premiumize.** Same pattern for
whichever of these you selected: paste the key, the wizard calls that
provider's own account endpoint, and it refuses to continue if the account
comes back anything other than an active Premium account —
`Real-Debrid account is not Premium` (or AllDebrid's, or Premiumize's
fair-use-aware equivalent) stops the wizard right there rather than
silently accepting a free-tier key that won't actually work.

**S4 — Usenet.** For each direct NNTP provider you add, the wizard opens a
real connection and does an actual `AUTHINFO` handshake against it before
accepting the credentials — a bad username/password surfaces here, not
three steps later as a mystery grab failure:

```
── S4  Usenet Providers ───────────────────────────────
  ✓ eweka validated (news.eweka.nl:563, tls=true, connections=20)
```

If TorBox is configured and its plan includes a News Server, that lane can
be added automatically too, no separate credentials needed — it's derived
straight from the TorBox account you already verified in S2.

### Finding your fleet, for real this time

**S8 — Arr connection.** If you approved Docker containers back at
[Charting your fleet](#charting-your-fleet), this is where they're
actually queried: DarkHarrbor pulls the API key straight out of each
container's own config through the connector, live, no copy-pasting. Went
manual instead? You're prompted per instance for a URL and API key, each
one verified with a real authenticated status call before it's accepted.
Either way, you'll see a confirmation of exactly which named instances
everything downstream will use:

```
── S8  Arr Connection + Key Extraction ────────────────
  ℹ Later stages will use these instances: sonarr, sonarr-anime, radarr
```

**S5 — Acquisition preference.** Now that every provider you gave it has
been verified, the wizard shows you *only the lanes that are actually
usable* — no point offering `cached-nzb` if nothing you configured
supports it — and asks you to rank them:

```
── S5  Acquisition Preference ─────────────────────────
  Available source priorities (based on verified providers):
    1  cached-torrent          — cached torrent through any configured debrid provider
    3  direct-usenet           — direct NNTP streaming
    4  uncached-torrent        — download an uncached torrent (uses a provider slot)
    5  uncached-torrent-last   — same path, ranked behind cached results
  Enter priority order by name or number [cached-torrent]:
```

Two guardrails worth knowing about: if your TorBox account came back
free-tier in S2, the wizard hard-recommends `RequireCached=true` and
defaults straight to `cached-torrent` — burning through a free plan's
limited uncached-torrent budget by accident isn't something it'll let
happen quietly. And any uncached-torrent lane needs your explicit
agreement to rank it at all; it's never silently on by default for anyone.

**Performance profile.** A little later in the flow — not separately
numbered, but around the same point as the Stremio client edge setup —
the wizard looks at your container's actual memory limit and the free
space on your media path, then recommends one of three profiles:
`recommended`, `low-memory`, or `high-throughput`. Under 2 GiB of
container memory nudges you toward `low-memory`; under 10 GiB free on
your data path takes `high-throughput` off the table entirely, not just
off the recommendation — you can still hand-pick between whatever remains
available if you know your own hardware better than the wizard does.

### Locking the strongbox

**S6 — Internal secrets.** Fully automatic, nothing to answer: DarkHarrbor
generates its own internal credentials (the qBittorrent-compat password,
the SABnzbd-compat API key, the stream secret, and a couple more depending
on what you selected), each a fresh 64-character random value. You'll just
see a scroll of `✓` confirmations.

**S9 — Arr auto-registration.** Also automatic. Using the credentials S6
just generated, DarkHarrbor writes itself into each connected Sonarr/Radarr
as a qBittorrent-compatible download client and a SABnzbd-compatible one,
plus three indexers, straight through each Arr's own API — each one
locked to exactly one acquisition lane, so Sonarr/Radarr can't accidentally
route a search down a lane you didn't configure:

- `darkharrbor` (Torznab) — the torrent lane
- `darkharrbor-usenet` (Newznab) — the direct-usenet lane
- `darkharrbor-http-stream` (Torznab) — the HTTP-stream/generic-source lane

Skipped entirely for a reactive-only (play-to-commit-only) setup with no
base Arr topology — there's nothing to register a download client into.

**S7 — Seal.** Every secret gathered so far — TorBox, other debrid keys,
NNTP credentials, the S6-generated internal ones, each Arr's extracted API
key — gets concatenated and sealed into one file with argon2id +
AES-256-GCM, using a freshly generated recovery password. The wizard then
immediately *unseals the file it just wrote* and checks the result — a
genuine round-trip self-test, not just "the write call didn't error."

This is where your **recovery key** is born, and it's worth being precise
about where it actually lives, since it's not as simple as "encrypted, end
of story": that same recovery password is also written to disk in
**plaintext**, as a separate boot key file (`/run/darkharrbor-key/secrets.key`,
its own Docker volume) — that's what lets the container unseal its own
secrets automatically on every restart without you typing anything in.
This is a deliberate tradeoff, not an oversight — it's the same pattern as
an encrypted disk with a keyfile instead of a passphrase prompt at every
boot. Running through `install.sh` (the normal path), the value itself is
never *printed* to your terminal — it goes straight into a private handoff
directory instead, and `install.sh` copies it out to `recovery.key` for
you afterward. That copy matters because the boot key file alone isn't a
backup: lose the `darkharrbor-keys` volume and `recovery.key` is the only
way back in. More on exactly what you get and what to do with it in
[What you're handed at the end](#what-youre-handed-at-the-end), below.

**S7c — Stremio client edge.** This publishes DarkHarrbor's own first-party
Stremio addon — and it's not purely optional the way it might look:
**if you're setting up play-to-commit (AIOStreams), the wizard force-enables
this stage for you automatically**, because AIOStreams has to query
DarkHarrbor's own Library addon to correlate what's playing back to a
title it can commit. Skip this and play-to-commit has nothing to
correlate against. (If you're *not* using play-to-commit, it's genuinely
optional — just a way to browse your existing library from a Stremio
client instead of Jellyfin, and you're asked plainly whether you want it.)

Either way, you're asked to pick a mode — `internal` (off), `lan`,
`tailscale`, a `custom` HTTPS origin, or Tailscale `funnel` (opt-in
public) — and, for anything but `internal`, the client-facing HTTPS
address it should be reachable at. The generated install URL goes straight
into the same private handoff, never printed.

**S7b — Scheduled backups.** If you enabled backups in your plan, you're
asked for a target directory, a cadence in hours, and how many snapshots
to keep, each with a sane default. Skip backups entirely and none of these
three prompts even appear.

**S7d — Reactive library defaults.** Only relevant if you're using
play-to-commit. Each Sonarr/Radarr instance you connected gets its own
root folder and quality profile chosen here (auto-selected without asking
if an instance only has one option). If you're running more than one
Sonarr and/or Radarr, this is also where you pick the **default** one for
each — "Default series Arr" and "Default movie Arr" — the instance a
watched title lands in when more than one could technically take it.

### Fitting out, and one last shakedown cruise

**S10 — Shim install.** For every Sonarr/Radarr instance discovered in
Docker, DarkHarrbor installs its ffprobe wrapper automatically through the
connector — the piece that lets `.strm`-mode imports actually resolve
against a stream instead of a real file on disk. Manually-entered Arrs
skip this and get flagged "externally pending" — you'll need to install
that shim by hand, per the documented steps.

**S10b — Jellyfin.** Same idea, for Jellyfin: if a Jellyfin container was
detected and approved, DarkHarrbor installs its own ffmpeg wrapper and
bitrate config automatically. No Jellyfin detected? Skipped cleanly, no
harm done. Docker proxy unavailable? You get the exact manual steps
printed right there instead of a dead end.

Right after this, your connector access gets narrowed one last time — down
to only the specific containers this run actually touched, not the
broader set you originally approved back in
[Charting your fleet](#charting-your-fleet).

**S11 — Validate.** Fully automatic: a final consistency pass over
everything the wizard just did, before it calls itself finished.

**S12 — The zero-account demo.** This is the fun one, and it's real, not
staged. If you have a Radarr connected and at least one working
acquisition lane, DarkHarrbor grabs one specific, curated, public-domain
film from the Internet Archive, hands it to Radarr, imports it, and plays
it back with a real HTTP range/seek check — the entire add → search →
grab → import → play pipeline, exercised end to end, using nothing but a
title that needs no account and costs nothing. If that title already
exists in your library for real, it just verifies playback of what's
already there instead — it will never re-grab, duplicate, or delete real
content. Anything the demo itself added, it offers to clean up afterward.
Skip it during setup and you can always run it later: `darkharrbor demo`.

**Doctor, automatically.** Setup finishes by running the same full sweep
`darkharrbor doctor` runs on demand — every configured provider, Arr, and
path mapping, checked for real, secrets redacted. Whatever it reports here
is the actual state of your new deployment, not a hopeful summary.

## What you're handed at the end

```
DarkHarrbor installation completed.
Deployment file: ./.darkharrbor/compose.yml
Private recovery handoff: ./.darkharrbor/handoff/recovery.key
Private AIOStreams handoff: ./.darkharrbor/handoff/mediaflow.env
Private Stremio handoff: ./.darkharrbor/handoff/stremio-install.url
Move retained handoff files to your secure vault, then delete the local copies.
Run diagnostics: docker compose -f ./.darkharrbor/compose.yml exec darkharrbor darkharrbor doctor
```

Everything under `handoff/` is written mode `0600`, owned by you, and
never logged or printed anywhere else. What you actually get depends on
what you set up:

- **`recovery.key`** — always present, always required. This is the S7
  seal password, in plaintext, one time. It's your only independently-held
  copy of it; see [Locking the strongbox](#locking-the-strongbox) above
  for exactly why that matters and what it unlocks. Move it off this host
  entirely — a password manager, a printed copy in a drawer, anywhere that
  isn't this same disk.
- **`mediaflow.env`** — only if you set up aggregator integration. One
  line, `HARRBOR_MEDIAFLOW_PASSWORD=...`, the shared secret your
  aggregator needs to authenticate proxied playback requests.
- **`stremio-install.url`** — only if you published DarkHarrbor's own
  Stremio addon in a mode other than `internal`. The tokenized manifest
  URL for installing it in a real Stremio client.

This directory is plaintext and won't be regenerated for you. Copy
`recovery.key` (and whichever of the other two exist) to your own secure
storage — a password manager, an encrypted note, anywhere off this host —
then delete `./.darkharrbor/handoff/`. Currently a manual step, both
halves of it; there's no automatic cleanup yet.

## The `darkharrbor` command

Right at the end, the installer offers to put a `darkharrbor` command in
`~/.local/bin`. Say yes and every CLI example in these docs shortens to
what it actually reads like:

```sh
darkharrbor doctor
darkharrbor backup snapshot
darkharrbor reactive-pending -action list
```

It works from any directory — the deployment path is written into the
command when it's generated — and it keeps working when the container is
stopped, which is exactly when you want diagnostics. Arguments pass
through untouched, so `darkharrbor doctor -json | jq` behaves.

Declining costs you nothing; the full Compose form below always works, and
this page shows it throughout so both paths read the same. If you already
run DarkHarrbor from a hand-written Compose file, `scripts/darkharrbor` in
the repository does the same job — point it at your deployment with
`DARKHARRBOR_COMPOSE=/path/to/docker-compose.yml`.

It reaches the in-container CLI only. Upgrades, restores, snapshots and
uninstall stay with `install.sh`, which owns the deployment itself.

## First checks

Setup already ran `darkharrbor doctor` once at the very end, but it's
worth knowing you can run it again anytime, on demand, with nothing to
set up:

```sh
darkharrbor doctor
# or, without the installed command:
docker compose -f ./.darkharrbor/compose.yml exec darkharrbor darkharrbor doctor
```

It checks every configured provider, Arr, and file-path mapping for real —
live calls, not cached assumptions — and tells you exactly what's wrong if
anything is. Add `-json` for machine-readable output if you're scripting
against it.

If you ran the S12 demo during setup, you've already got real, working,
end-to-end proof: a title was actually grabbed, imported, and played.
Skipped it? Run `darkharrbor demo` now, or just grab something real
through Sonarr/Radarr and press play once it shows up. Either way, that's
the actual test — everything before this point was setup, this is the
part that proves it works.

## Upgrading later

When a new release comes out, you don't reinstall — you run the same
`install.sh` you already have, with one flag:

```sh
./install.sh --upgrade
```

It shows you exactly what's about to change before touching anything:

```
Upgrade preview (no secrets):
  deployment:      ./.darkharrbor/compose.yml
  main image:      ghcr.io/darkharrbor/darkharrbor@sha256:...
  connector image: ghcr.io/darkharrbor/darkharrbor-dockerproxy@sha256:...
  preserved:       networks, paths, targets, volumes, settings, and secrets
The current release must create a complete snapshot before deployment changes.
Its unlock key remains separate and must already be retained for rollback.
Confirm the separate recovery key is retained, then create the snapshot? [y/N]:
```

Everything you set up — your database, your sealed secrets, your networks,
your media path, your connector targets — carries over untouched. Two
things worth knowing:

- **It forces a real crash-consistent snapshot before it changes anything**,
  and refuses to proceed until you confirm your recovery key is somewhere
  safe. This is the moment that key you tucked away back in
  [What you're handed at the end](#what-youre-handed-at-the-end) actually
  matters.
- **If the new image doesn't come up healthy, it stops there and tells you
  to follow the documented snapshot rollback** rather than leaving you
  half-upgraded. It won't silently limp forward on a broken image.

This only works on a deployment `install.sh` itself created — it checks for
its own schema marker in the compose file first. If you followed this guide,
that's exactly what you have. Running a manual Compose deployment instead?
See [CONFIGURATION.md § Release Upgrades and Rollback](../CONFIGURATION.md#release-upgrades-and-rollback)
for the by-hand equivalent.

## Where to go next

- [docs/AGGREGATOR-INTEGRATION.md](AGGREGATOR-INTEGRATION.md) — setting up
  play-to-commit with AIOStreams (or another Stremio-compatible
  aggregator), covered in [Before you start](#before-you-start).
- [CONFIGURATION.md](../CONFIGURATION.md) — every setting, tuning knob,
  and advanced option, including the generic-source catalog mentioned in
  [Reviewing the manifest](#reviewing-the-manifest).
- [CONFIGURATION.md § Subcommands](../CONFIGURATION.md#subcommands) — the
  full CLI reference, verbatim from `darkharrbor help`. `doctor`, `demo`,
  and `reconcile-arr` already came up above; there's a lot more —
  `backup`, `deadletter`, the whole reactive-library toolset.
- **AI-assisted setup** — if you'd rather have Claude walk you through
  this, or drive the whole install for you, see
  [.claude/skills/](../.claude/skills/).
- [docs/ARCHITECTURE.md](ARCHITECTURE.md) — the mechanism-level detail
  behind everything this walkthrough covered, for anyone curious how it
  actually works underneath.
