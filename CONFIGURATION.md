# DarkHarrbor Configuration

## Which parts apply to you

DarkHarrbor is adopted in layers. Most deployments need only the BASE.

**BASE — the standard Arr workflow.** Sonarr/Radarr automation with DarkHarrbor
as the download client and indexer. No aggregator, no reactive commit. This is
`HARRBOR_TOPOLOGY=t1` and it is a complete, supported end state.

> Prerequisites · Running a second, isolated instance · Automated / scripted
> setup · Wizard Modes · Configuration Sources · Acquisition Preference ·
> Core Settings · Sealed Secrets · Diagnostics · Backup and Restore · Release
> Upgrades · Installer Uninstall and Data Retention · Reverse Proxy and Bind
> Safety · TorBox Provider · NNTP Providers · Cache Configuration · CDN /
> Stream Tuning · Selection Mode · Arr Notification · Strm Mode / Docker
> Proxy · Wizard Stages · Tuning Reference · Environment-Only Settings ·
> Subcommands

**OPTIONAL — aggregated playback and play-to-commit.** This is independent of
base Arr search/grab wiring: `t2` provides aggregated playback without commit,
while `t3` can add played content through an existing Arr's API and manual
import without registering the base indexers or download clients. The documented
AIOStreams path requires a self-hosted aggregator plus shared tier, neither of
which DarkHarrbor manages. Setup for that external half is in
[docs/AGGREGATOR-INTEGRATION.md](docs/AGGREGATOR-INTEGRATION.md).

> HTTP Stream Sources · Schema 45 — reactive destination capture · Reactive
> Commit Destinations · the watch-to-commit part of Prerequisites

If you are not running an aggregator, you can skip every OPTIONAL section and
still have a fully working deployment. Declining the optional layer is a
supported choice, not an omission. See
[docs/FEATURE-MAP.md](docs/FEATURE-MAP.md) for what each feature costs and what
is lost by declining it. For how any of this actually works underneath, see
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Prerequisites

Every requirement applicable to the features you choose must be true BEFORE
`darkharrbor setup` is run. The wizard verifies what it can in S1 and fails
loudly rather than proceeding on a broken assumption, but it cannot create
these for you.

The published image deliberately runs as non-root UID/GID `1000:1000`. The
public installer automatically verifies the selected host media directory with
that exact identity before publishing Compose. Its digest-pinned probe has no
network, drops all capabilities, enables `no-new-privileges`, uses a read-only
container root, and runs only `test -w` against the bind mount. It does not read
or modify directory contents. If the check fails, deliberately grant that
identity write/traverse access according to your host filesystem policy and
rerun; the installer does not invoke `sudo`, `chown`, or broaden permissions.
Before that probe, release schema and connector-policy labels are inspected on
the resolved immutable image identities, never on their movable tag aliases.

On a new Compose installation, run setup before the daemon:

```sh
docker compose run --rm -it darkharrbor setup
docker compose up -d darkharrbor
```

The first setup surface is a goal plan, not a credential prompt. It performs a
read-only discovery preview through the scoped connector and then asks in this
order:

1. use discovered Sonarr/Radarr instances or authenticated manual entry; when
   several are discovered, select the exact subset DarkHarrbor may configure;
2. choose any combination of torrent/debrid, Usenet/NNTP, and HTTP/stream
   sources; there is no default lane;
3. select only existing debrid providers, in submission fallback order; no
   provider is preselected, a selected torrent source requires at least one,
   and the first provider that accepts a torrent owns that item afterward;
4. when Usenet/NNTP was selected, choose direct NNTP providers in fallback
   order; no provider is preselected, and Usenet requires a direct provider or
   selected TorBox account. TorBox News Server remains contingent on later
   account-capability verification;
5. choose a Recommended, Low memory, or High throughput performance profile;
6. enable or skip normal Arr search/grab wiring, discovered Prowlarr, discovered
   Jellyfin, and recommended local backups;
7. optionally connect an existing self-hosted AIOStreams deployment;
8. optionally enable play-to-commit, which automatically includes the addon
   required for safe title identity, plus optional promotion; and
9. review the redacted plan—including both provider orders—and confirm it.

The review occurs before setup opens/migrates the database, probes `/config`
with a write, seals credentials, or changes an Arr. Internal `t1`/`t2`/`t3`
topology is derived from those outcomes and is not an ordinary first-run
question. Normal installation always connects Arrs the user already operates;
disposable fixtures belong only to external acceptance tests.

Prowlarr is selected independently from Sonarr/Radarr. If several supported
instances are discovered, setup requires exactly one name or number before the
connector reads configuration. Its fixed command returns only `Port` and
`ApiKey`; setup authenticates `/api/v1/system/status`, then seals the selected
URL/key. It never reads credentials from an unselected Prowlarr. Manual mode
likewise accepts at most one authenticated Prowlarr because runtime selection
mode has one upstream endpoint.

### Optional play-to-commit setup with an existing Arr

Enable play-to-commit when you want a title played through your existing
AIOStreams user to be added to an existing Sonarr or Radarr. The relevant
first-run choices are:

| Order | Wizard prompt | Response | Effect |
|---:|---|---|---|
| 1 | Read-only existing-service discovery | No credential response | Shows only supported running service names and types from sanitized connector metadata |
| 2 | `Use the discovered Arr instances? [Y/n]` and, when needed, `Arr instances ... [all]` | Usually `y`, then the intended names/numbers | Uses scoped discovery; only the selected subset has API keys extracted and later receives registration or wrapper changes. Choose `n` for private manual entry |
| 3 | `Select one or more ... no default` | Your source families | Requires an explicit choice; `http` is added automatically if AIOStreams is selected later |
| 4 | `Debrid providers ... [none]` | Existing providers in desired submission fallback order | No provider is assumed; torrent requires at least one explicit selection, the first provider that accepts an item remains its owner, and `none` remains valid when only direct NNTP/HTTP is wanted |
| 5 | `NNTP providers ... [none]` when Usenet was selected | Existing direct providers in desired fallback order | The order joins the complete preview; `none` is accepted only with a selected TorBox account, whose News Server/cached-NZB capabilities remain subject to verification |
| 6 | `Performance profile [recommended]` | Usually Enter | Uses a reviewed preset; advanced tuning remains available later |
| 7 | `Use DarkHarrbor for normal Sonarr/Radarr searches and grabs? [Y/n]` | Your choice | Choose `n` for optional playback/play-to-commit without base Arr client/indexer wiring |
| 8–10 | Discovered Prowlarr, discovered Jellyfin, and local backups | Your choice | Prowlarr and backups are recommended; media-server management remains optional |
| 11 | `Connect an existing self-hosted AIOStreams setup ... [y/N]` | `y` | Enables the optional aggregator handoff and derives internal aggregate topology |
| 12 | `Enable play-to-commit and its required DarkHarrbor addon? [y/N]` | `y` | Enables safe playback identity correlation and derives internal reactive topology |
| 13 | Promotion and public-domain demo | Usually `n` | Optional follow-on behavior |
| 14 | `Apply this complete plan? [y/N]` | `y` | The sole mutation approval; both provider orders, selected HTTP settings, and hidden-secret markers are already included in this review |

The profiles write only existing allowlisted, non-secret tuning keys and merge
with unrelated settings. All profiles keep one stream readahead worker to avoid
turning a convenience preset into provider request amplification:

| Profile | Cache modes | Shared disk ceiling | NNTP window | Stream chunk / minimum buffer | Workers |
|---|---|---:|---:|---:|---:|
| Recommended | readahead / readahead | 2048 MB | 16 segments | 16 MB / 2 chunks | 1 |
| Low memory | readahead / readahead | 1024 MB | 8 segments | 8 MB / 1 chunk | 1 |
| High throughput | disk / disk | 8192 MB | 32 segments | 16 MB / 4 chunks | 1 |

High throughput assumes at least 8 GiB of application-owned cache storage.
Setup reads only the container's cgroup memory ceiling and free blocks on the
application-owned `/data` filesystem. A real memory limit below 2 GiB defaults
to Low memory. Less than 10 GiB free disables High throughput, preserving 2 GiB
of headroom beyond its 8 GiB cache ceiling. Missing/unknown resource data keeps
the balanced default, and abundant resources never automatically select High
throughput. The review still shows the choice before mutation. Use
`darkharrbor configure` for a deliberate custom policy.

Before final plan approval—and still before persistent mutation—an HTTP selection runs
the existing secure backend configurator. It asks for named backend types,
hidden base URLs or a private generic descriptor file, exact redirect origins,
and an explicit private/container-network opt-in when required. Each source
must pass a bounded dial test. The wizard adds non-secret settings plus
`[hidden; will be sealed]` markers to the complete plan review. Decline its one
final approval to exit without opening the database or writing `/config`.

Setup then prompts for:

1. Credentials only for the acquisition providers selected in the reviewed
   plan. A selected provider requires a credential; unselected providers never
   produce credential prompts.
2. Any Arr details that discovery did not supply. Manual API keys use hidden
   prompts, and setup verifies the connection and application type.
3. Acquisition preference among the providers you successfully connected. The
   prompt uses names such as `cached-torrent`, `cached-nzb`, and
   `direct-usenet`; compatibility tokens are written internally.
4. Stremio edge mode and, unless `internal`, its client-reachable HTTPS origin.
   A remote viewing device normally uses `tailscale` or `custom`.
5. Commit mode (`auto` or `supervised`), playback threshold, episode/movie
   monitoring, each displayed Arr root-folder and
   quality-profile choice, and the default movie Arr. A sole usable root folder
   or quality profile is selected automatically.

Setup then prints the Library manifest and one-time MediaFlow credential needed
by the existing AIOStreams deployment. Save that integration in the existing
AIOStreams user, start DarkHarrbor, and run
`darkharrbor configure --existing-aiostreams`. For ordinary HTTP selections,
first run already writes the validated named backend configuration and seals
sensitive URLs. The focused AIOStreams command remains necessary when its
Direct Manifest URL is produced later by the operator-controlled dashboard.

The setup subcommand is dispatched before normal configuration loading and can
therefore create `/config/secrets.sealed` on an empty volume. Starting the
daemon first exits on that missing file and the restart policy retries it; run
setup and let S7 create the sealed files.

**Host and runtime**

| Requirement | Why |
|---|---|
| Docker Engine with the Compose v2 plugin (`docker compose`, not `docker-compose`) | The canonical deployment is Compose-native and uses `profiles:` |
| An application-owned `/config` volume | After plan approval S1 rejects symlinks/unexpected ownership and normalizes it to mode `0700` before writing |
| An external Docker network named `arr-net` | Both `docker-compose.yml` and `docker-compose.example.yml` declare it `external: true`; create it with `docker network create arr-net` if it does not exist |
| Sonarr and/or Radarr reachable on that network | Required for S8 discovery, S9 registration, and all reactive commit |

**Optional, feature-gating**

| Requirement | Unlocks |
|---|---|
| `docker-proxy` sidecar (with an explicit `HARRBOR_DOCKERPROXY_TARGETS` list before setup) | Scoped S8 discovery and S10 shim install; manual S8 entry and API-based S9 registration remain available without it |
| Jellyfin reachable | S10b (ffmpeg wrapper, bitrate limit, env verification) |
| An ID-capable HTTP backend and reachable Radarr at S12 | The optional public-domain demo; other acquisition lanes do not satisfy its identity search |

Official `jellyfin/jellyfin` images are recognized by tag or immutable digest.
S10b refuses symlinked wrapper directories, wrapper files, configuration
directories, and `system.xml`; wrapper updates use guarded temporary files and
atomic replacement.
If several supported instances are visible, the wizard requires one explicit
name/number selection and includes that exact name in the complete review
before S10b may change it.

**Accounts**

| Account | Status | Wizard stage |
|---|---|---|
| TorBox | Optional; when supplied, plan level and slot capabilities are detected rather than assumed. Any plan tier is accepted, including Free | S2 |
| Real-Debrid | Optional, independent provider lane. **Account must be Premium** | S2b |
| AllDebrid | Optional. **Account must be Premium** | S3 |
| Premiumize | Optional. **Account must be Premium** (checked as current, non-expired at the moment of setup, not just a flag) | S3b |
| NNTP/usenet provider | Optional; required only if `nntp_nzb` appears in `HARRBOR_PREFERENCE` | S4 |

Real-Debrid, AllDebrid, and Premiumize each call their own account endpoint
live and refuse the key outright -- `S2b: Real-Debrid account is not Premium`
and equivalents -- if the account comes back anything other than an active
Premium subscription. This is a hard rejection at credential-entry time, not
a degraded mode: there is no free-tier path through any of these three.
TorBox is the one exception -- it is the only debrid provider that accepts a
non-Premium (including Free) account, with its own separate conservative
limits applied instead of a rejection.

Both provider pickers preselect nothing and run before the complete-plan review.
Setup refuses a selected torrent source without a debrid provider and refuses
a selected Usenet source without either a direct NNTP provider or selected
TorBox account, so a guaranteed-unusable source cannot cross approval.
The plan records explicitly selected TorBox, Real-Debrid, AllDebrid, and
Premiumize entries plus one ordered direct-NNTP list containing `newshosting`,
selected-account `torbox`, and/or custom provider names. After approval, S4
requests credentials only for those names and uses the reviewed order as NNTP
failover priority; it never preselects Newshosting or TorBox News Server. A
selected TorBox News Server must still pass verified-plan capability checking
before its one-time credential is obtained. Setup verifies the resulting
providers before S5. If an approved Usenet source has neither verified cached
NZB capability nor a validated direct NNTP provider, setup stops explicitly
instead of silently dropping that source from the later priority list. Any
verified debrid account therefore unlocks the provider-neutral torrent choice;
the persisted token is still named `torbox_torrent` for compatibility.

**Acceptance status:** TorBox and Real-Debrid have each completed a real-account
independent source-family acceptance run. AllDebrid and Premiumize are
implemented as full provider-neutral adapters but remain experimental — no
account has been available to run either through the same acceptance path.
Treat both as unverified for unattended production use until independently
proven. NNTP
prompts appear only when the goal plan selected NNTP. TorBox is available for
torrent or NNTP; the other debrid providers appear only with torrent.

Provider account responses are capability input, not permission to guess. For
TorBox, a recognized active plan enables only its table-owned capabilities. An
active plan code this DarkHarrbor build does not recognize is displayed as
`Unknown (plan=N)`: runtime admission uses conservative Free-level limits, but
setup does not call the account Free, recommend Free-tier policy, enable News
Server/usenet, or invent AirLock quota. An unavailable account response is
likewise unknown, not proof of a Free account. An expired or explicitly
unsubscribed account is a verified non-paid state. `darkharrbor doctor` warns on
the unknown-plan fallback without printing account identity or credentials.

**Watch-to-commit (reactive lane) additionally requires**

> Preserve an existing working AIOStreams/StremThru deployment. Setup prints
> the MediaFlow credential for AIOStreams' forced-proxy environment and the
> Library manifest for its saved user; after saving,
> `darkharrbor configure --existing-aiostreams` derives, verifies, and seals
> DarkHarrbor's backend settings from the Direct Manifest URL. It never
> rewrites AIOStreams or StremThru. Users without that stack may deploy the
> immutable optional bundle in `examples/aggregator-tier/`.

Reactive commit observes bytes DarkHarrbor itself delivers. Nothing that
bypasses DarkHarrbor can ever commit, so the aggregator stack is a hard
prerequisite for this feature and not an optional integration.

| Requirement | Why |
|---|---|
| A self-hosted aggregator (for example AIOStreams) | It is what routes playback through DarkHarrbor's proxy in the first place |
| A self-hosted debrid abstraction tier (for example StremThru) | Its failure is TOTAL, not isolated -- every source loses resolution at once. A public default instance doing exactly that is why self-hosting is required, not advised |
| The aggregator's proxy pointed at DarkHarrbor | `FORCE_PROXY_URL` (or equivalent) must target DarkHarrbor's private listener |
| The shared tier base URL reachable from its actual fetcher | With selective proxying, viewing devices fetch it and need a client-reachable URL; with the documented universal empty filter, DarkHarrbor fetches it and the container-network URL is correct |

The aggregator's proxied-service filter decides which lanes can commit. If it
names specific services, only those services' streams pass through DarkHarrbor
and only those can reach the threshold; an empty filter proxies everything.
P2P/magnet sources can never be observed, because there is no HTTP body to
count. See "Supported Aggregator Dependency Posture" below for the full
failure model and the `doctor` checks that name an aggregator-side fault.

**To make EVERY lane commit, the proxied-service filter must be EMPTY.** A named
list is the common default and silently limits commit to those services. Two
lanes remain excluded no matter what you set, because the aggregator hardcodes
them: its OWN built-in usenet engine, and NzbDAV/AltMount unless the aggregator's
built-in proxy is in use. If you want usenet to commit, use a shared-tier or
debrid usenet service rather than the aggregator's internal engine.

Emptying that filter puts DarkHarrbor in the byte path for ALL playback. Accept
two consequences: a DarkHarrbor restart interrupts any stream in flight, and the
shared tier's base URL requirement moves from client-reachable to
container-reachable — see the reachability constraint below, and change both in
one window.

**Recovery and one-time operator handoff**

Public installer setup does not print generated credentials or tokenized addon
URLs. It copies exactly `recovery.key` plus selected `mediaflow.env` and
`stremio-install.url`; it never copies the container directory or an unknown
entry. Each copied object must be nonempty, bounded, current-user-owned, regular,
and non-symlink. The installer stages them under a new mode-`0700` directory,
sets each to mode `0600`, atomically renames that directory to
`.darkharrbor/handoff/`, removes the exact temporary source files from the key
volume, and reports only the paths. Move the required files to an independent
secure vault and delete the local handoff directory. `recovery.key` is required
and capped at 4 KiB; absence or an excessive size fails the installation before
publication and enters full incomplete-project cleanup.

Public installer deployments also keep the active boot key in a separate
`darkharrbor-keys` volume at
`/run/darkharrbor-key/secrets.key`; canonical manual Compose keeps
`/config/secrets.key` for backward compatibility. Both boot in `keyfile` mode,
so you do **not** type the recovery key on every normal boot. Manual setup
without `--handoff-dir` retains the attended, exactly-once terminal fallback.
The retained recovery value is required if the key volume is lost or you
deliberately switch to `env` or `prompt` unlock mode.

Do not re-run `darkharrbor setup --force` merely because the recovery key is
not visibly present at boot; that command rotates the sealed store and every
generated credential.

## Running a second, isolated instance

The shipped `docker-compose.yml` describes ONE deployment per host. It pins
`container_name: darkharrbor`, the global image tag `darkharrbor:latest`, host
port `8382`, a bind mount of `/mnt/darkharrbor`, and the external `arr-net`
network. Every one of those is a hard collision if you bring up a second copy —
for a test, a staging rehearsal, or a clean-room reproduction — alongside a
running instance. Image tags in particular are global to the Docker daemon, so
an unmodified second build OVERWRITES the running deployment's `latest` tag.

Use a project-scoped override. Never edit the canonical file for this.

```yaml
# isolated.override.yml
services:
  darkharrbor:
    image: darkharrbor:<unique-tag>
    container_name: dh-<unique-suffix>
    ports: !override
      - "18382:8382"
    volumes: !override
      - darkharrbor-config:/config
      - darkharrbor-backups:/backup
      - dh-isolated-data:/data
    networks: !override
      - isolated

networks:
  isolated:
    name: <unique-project>_isolated

volumes:
  dh-isolated-data:
```

Bring it up under its own project name, which scopes the container, network and
volume names:

```sh
docker compose -f docker-compose.yml -f isolated.override.yml \
  -p <unique-project> up -d darkharrbor
```

**Verify the image resolution BEFORE building**, because this is the step that
protects the running deployment:

```sh
docker compose -f docker-compose.yml -f isolated.override.yml \
  -p <unique-project> config --images
```

Every line must carry your unique tag and none may say `darkharrbor:latest`.

Notes that are not optional:

- The `!override` list syntax requires Compose v2.24 or newer; it REPLACES the
  canonical list instead of appending to it. Without it, `volumes:` and
  `networks:` merge and the production bind mount and `arr-net` survive.
- `volumes: !override` is what removes the production
  `/mnt/darkharrbor` bind mount. An isolated instance must not share a library
  path with a live one. It still needs a NEW shared path with its disposable
  Arr: mount `dh-isolated-data` at `/data` in DarkHarrbor and read-only at the
  reported prefix (normally `/mnt/darkharrbor`) in that Arr. If the Arr is in a
  separate Compose project, declare the cold volume by exact external name:

  ```yaml
  services:
    radarr:
      volumes:
        - dh-data:/mnt/darkharrbor:ro
  volumes:
    dh-data:
      name: <unique-project>_dh-isolated-data
      external: true
  ```

  Reactive commit asks Arr to import the `.strm` file from that reported path.
  If Arr cannot see it, automatic dispatch repeatedly fails with `manual import
  candidate is absent or ambiguous`; a healthy container and a generated
  pointer are not enough.
- **Do not use the `strm` profile on a shared daemon.** `docker-proxy` mounts
  `/var/run/docker.sock`, which grants the container control of the whole host
  daemon including the production instance. Choose `manual` at S8 instead: it
  authenticates the isolated Arr, persists its non-secret metadata in
  `arr_instances`, seals the API key, and lets S7d capture its live destination.
  `HARRBOR_ARR_NAMES` remains a legacy/process-environment fallback, not a step
  required by the supported manual wizard path.
- `HARRBOR_REPORTED_PATH_PREFIX` stays `/mnt/darkharrbor` unless you override
  it. It is only the path written INTO `.strm` content, so it touches no
  production file, but set it to match whatever the isolated Arr actually mounts.
- The isolated instance needs its own Sonarr/Radarr on the isolated network.
  Pointing it at the production Arrs defeats the isolation.
- Compose keeps the service-name DNS alias `darkharrbor` on the isolated network
  even though `container_name` is unique. Keep
  `HARRBOR_SERVER_BASE_URL=http://darkharrbor:8381` and the aggregator's
  `FORCE_PROXY_URL=http://darkharrbor:8381` together unless you deliberately
  replace that alias; never substitute the published client port for private
  port 8381.
- The sample maps the restricted listener to host port `18382`. S7c's printed
  Tailscale/Funnel command assumes the canonical host port `8382`; for this
  isolated sample replace only its target with `http://127.0.0.1:18382`. The
  client HTTPS origin remains the origin entered at S7c. Do not expose 8381.

## Automated / scripted setup

For scripted deployments (Ansible, other IaC, or simply repeating an install),
`darkharrbor setup --answers FILE` replays prompts from a JSON object whose
sole field is an ordered string array named `responses`. Empty strings accept
defaults. Because the file can contain credentials, it must be a regular
owner-matched, non-symlink file with mode `0600` or stricter beneath safe
non-writable, non-symlinked parent directories. Setup reads it through the same
`O_NOFOLLOW` secure-file boundary as sealed credentials, caps it at 1 MiB,
rejects unknown fields, suppresses secret echo, renders the same plan review,
and still requires an explicit response to `Apply this complete plan?`.

`darkharrbor setup --answers` only replays the wizard stage; it does nothing
about `install.sh`'s own pre-wizard prompts (image download confirm,
container/network approval, media path, install preview), and `install.sh`'s
embedded `docker compose run --rm -it darkharrbor setup ...` call hard-requires
a real TTY -- piped/non-interactive stdin fails it closed with "the input
device is not a TTY" rather than hanging. `./install.sh --answers-file
/absolute/private/answers.json` solves both: it swaps that one invocation for
`-T` (no pseudo-TTY) plus `-answers`, bind-mounts the file read-only for that
single invocation only, and deletes it once consumed -- on success or failure
alike. The file itself is validated the same way as `--catalog` (regular,
non-symlink, owner-matched, mode `0600`, non-writable parent directory) except
for its size cap, which matches the wizard's own 1 MiB limit exactly, not the
catalog's 2 MiB. `install.sh`'s own pre-wizard prompts still need to be piped
separately -- they are plain `read -r` prompts with no TTY requirement -- so a
full headless run combines both: `printf '<answers>\n' | ./install.sh
--answers-file /path/to/answers.json`.

## Wizard Modes

DarkHarrbor deliberately has two interactive configuration paths with separate
ownership:

- `darkharrbor setup` is the first-run/bootstrap wizard. It walks a new user
  through provider and Arr setup, generates DarkHarrbor's internal credentials,
  and writes the protected `secrets.sealed`/key material under `/config`.
  Re-run it with `--force` when credentials or discovered integrations must be
  rotated or reconciled.
- `darkharrbor configure` is the runtime tuning wizard. It writes non-secret
  values to `/config/darkharrbor.conf`; its one credential-bearing surface is
  the hidden HTTP-backend URL prompt, which atomically updates that URL in the
  existing sealed store without displaying it or rewriting unrelated secrets.

`darkharrbor setup` records both scoped-discovered and authenticated
manually entered Arr instances in the `arr_instances` table with a
sealed-secrets `api_key_ref` rather than a key. Reactive commit destination
values -- per-instance root folder and quality profile, and any per-kind default
destination -- belong to the wizard and are captured from each instance's OWN
real root folders and quality profiles, so a user picks from a list rather than
typing an id. See "Reactive Commit Destinations" below.

This capture step SHIPPED at schema 45 (RD-34 D8). Stage S7d queries each
accepted instance's live `/api/v3/rootfolder` and
`/api/v3/qualityprofile`, presents the real choices, and persists the
selection to `arr_instances`. Declining `docker-proxy` does not disable this:
choose manual entry at S8 and setup authenticates and persists the Arr before
S7d. Only when no reachable Arr was accepted (for example S8 was skipped or
manual authentication failed) does S7d write
`HARRBOR_REACTIVE_ENABLED=false` and `HARRBOR_REACTIVE_COMMIT_MODE=off`
without prompting. Add or repair the Arr, then rerun setup/configure rather than
assuming environment variables register it.
`darkharrbor configure` re-enters the same capture, so a destination can be
changed after bootstrap without touching credentials.

`arr_instances` is the router's authoritative target source for both
discovered and manually entered Arrs. The environment-declared
`HARRBOR_ARR_NAMES` surface remains a FALLBACK used only when that table has no
usable instance, so neither surface silently overrides the other. An instance
whose sealed `api_key_ref` cannot be resolved is SKIPPED rather than contacted
unauthenticated, and its name is
reported without printing the reference value.

Consolidated-plan row `HR7.1` completed the secret-safe IA, OMSS,
Stremio/AIOStreams, and typed-generic source editor. It preserves untouched
advanced tuning, supports source disablement, and requires a successful bounded
protocol health test for every enabled source before saving.

### Existing AIOStreams HTTP follow-up

After setup, preserve the existing AIOStreams/StremThru deployment. Add the
MediaFlow credential from the private handoff (or attended manual fallback) to
AIOStreams' private container environment and
recreate only that container without changing its image or data volume. Add the
printed DarkHarrbor Library manifest to the existing saved user, retain its
already-working service and Torznab settings, and save. Start DarkHarrbor with
`docker compose up -d darkharrbor`, then run:

```sh
docker compose exec darkharrbor darkharrbor configure --existing-aiostreams
```

The focused command asks for three values:

1. the AIOStreams origin reachable from DarkHarrbor (normally
   `http://aiostreams:3000`);
2. the saved user's Direct Manifest URL at a hidden prompt;
3. the client-reachable DarkHarrbor MediaFlow origin, defaulting to the reviewed
   Stremio client origin when one was configured during setup.

It verifies the saved-user endpoint before writing, preserves the complete
credential-bearing `/stremio/...` path while replacing only its origin, seals
that backend URL, enables the `aiostreams` Stremio backend, and restarts the
daemon when invoked through `docker compose exec`. It never changes an
AIOStreams or StremThru container, image, volume, user, credential, addon, or
Torznab setting. Run `darkharrbor doctor` after restart, then complete genuine
viewing-client playback. Use the full `darkharrbor configure` wizard only for
advanced HTTP sources or unrelated tuning.

## Configuration Sources (precedence, highest first)

1. `/config/darkharrbor.conf` — strict, allowlisted non-secret runtime
   configuration
2. `secrets.sealed` — credentials and legacy sealed settings
3. Process environment — including values explicitly supplied by Compose
4. A working-directory `.env` — direct-binary legacy input only, and only for
   keys not already present in the process environment
5. Built-in defaults

This order is the one implemented by `config.Load`: defaults are created
first, a local `.env` fills missing process variables, process values are
applied, sealed values override them, and allowlisted tuning is applied last.
`Config.Lookup` follows the same tuning → sealed → process order.

`darkharrbor configure` manages source 1 and can update sensitive HTTP backend
URLs in source 2 without echoing or displaying them; `darkharrbor setup`
creates source 2. Known credential keys are rejected from source 1. Sensitive
or token-bearing backend URLs must stay out of the tuning file. Enter them at
the hidden `configure` prompt; the command preserves unrelated sealed values
and rewrites the existing sealed store atomically. Canonical Compose neither
copies nor mounts a host `.env`,
declares no `env_file:`, and does not interpolate provider/configuration keys. A
repository-side `.env` is therefore not DarkHarrbor runtime configuration.

Normal deployments do not require a `docker-compose.override.yml`;
installation-specific provider/source settings belong under `/config`, not in
Compose. `docker-compose.example.yml` remains a complete credential-free
new-user template/reference; it is not an override or an installation-specific
configuration store.

For a separate Compose-managed service whose shipped file explicitly references
plaintext runtime credentials, keep each value in a same-directory, git-ignored
`.env` owned by the operator and mode `0600`; interpolate it as `${NAME}`.
Never inline a live credential in tracked YAML. Before adding a previously
excluded Compose file, verify that the `.env` is ignored, render the
configuration, and inspect the staged file for inline password, token, secret,
API-key, credential, and URL-userinfo values. Such a `.env` is runtime input,
not a versioned backup.

The tuning allowlist also accepts non-secret HTTP-source metadata and the
`.strm` installer enablement flag. Omit a token-bearing or otherwise sensitive
HTTP backend URL from `darkharrbor.conf`; resolution can then select a sealed
value or private process-environment value by the precedence order above.

## Acquisition Preference

`HARRBOR_PREFERENCE` is the single canonical control for what DarkHarrbor
acquires and in what priority. Ordered comma-separated list of lanes.
Position = priority; presence = enabled; omission = disabled.
This chooses candidate/acquisition order for new work; it does not migrate an
already-bound item between providers or substitute an unproven source during
playback.

The torrent tokens retain legacy TorBox-branded names, but provider selection is
a separate axis. The wizard seals `HARRBOR_PROVIDERS` from whichever of TorBox,
Real-Debrid, AllDebrid, and Premiumize you actually configured, and the torrent
walk builds an independent lane for each provider in that order. Do not infer
that TorBox is required from the token name.

| Lane | Meaning |
|---|---|
| `torbox_torrent` | Legacy name for the provider-neutral cached/ready torrent lane across `HARRBOR_PROVIDERS` |
| `torbox_nzb` | TorBox's cached NZB/usenet service; genuinely TorBox-specific |
| `nntp_nzb` | NZB streamed directly over configured NNTP providers |
| `uncached_torrent` | Provider-neutral uncached torrent add, governed per provider |
| `uncached_torrent_derank` | Same uncached path, with authoritative misses tagged `DH-UNCACHED` for Arr-native deranking |

When no explicit provider list exists, no provider is selected. Setup writes
only the providers the operator selected and verified; credentials alone never
activate TorBox or any other provider.
All selected debrid providers are validated before S5, so a non-TorBox-only
torrent deployment uses the same provider-neutral preference without a
post-setup edit.

Uncached policy is tri-state: omit both uncached lanes to deny cache misses; use
`uncached_torrent` to allow them without title scoring; or use
`uncached_torrent_derank` to keep misses visible with the stable title token.
The two uncached values are mutually exclusive.

**Recommended full-stack order** (cached-first, NNTP floor, uncached derank last):

```
HARRBOR_PREFERENCE=torbox_torrent,torbox_nzb,nntp_nzb,uncached_torrent_derank
```

Default when unset: no acquisition lane. The first-run wizard writes the chosen
lanes in the operator's priority order.

An intentional HTTP-only installation writes `HARRBOR_PREFERENCE=none` and
`HARRBOR_PROVIDERS=none`. `none` is an explicit opt-out, not a lane or
provider. An omitted setting also selects nothing, while `none` records the
wizard's intentional no-provider choice. A selected TorBox lane still
requires a valid TorBox credential and fails closed without one.

The setup wizard idempotently installs one `DarkHarrbor Uncached` release-title
custom format with score `-1` in every Arr profile. It never changes a profile's
quality ordering or minimum custom-format score. A profile whose
`minFormatScore` is greater than `-1` will reject an otherwise-unscored uncached
release instead of merely deranking it; setup reports the count of incompatible
profiles. Resolve that Arr-owned policy explicitly before activating derank mode.

Legacy booleans `HARRBOR_REQUIRE_CACHED` and `HARRBOR_NZB_CACHE` are derived from
`HARRBOR_PREFERENCE` when it is set, and synthesize a preference when it is not.

## Core Settings

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_SERVER_ADDRESS` | `127.0.0.1:8381` | Standalone listen address; canonical Compose explicitly uses `:8381` only inside its unpublished `arr-net` network |
| `HARRBOR_SERVER_BASE_URL` | `http://localhost:8381` | Base URL for .strm generation and selection-mode loop-safety; canonical Compose explicitly uses `http://darkharrbor:8381` on `arr-net` |
| `HARRBOR_STREMIO_EDGE_MODE` | `internal` | Restricted client edge: `internal`, `lan`, `tailscale`, `custom`, or explicit public `funnel` |
| `HARRBOR_STREMIO_CLIENT_ADDRESS` | `:8382` | Dedicated client listener; it never exposes qBit, SAB, WebDAV, metrics, debug, or administrative routes |
| `HARRBOR_STREMIO_CLIENT_BASE_URL` | unset | Exact client-facing HTTP(S) origin; non-localhost origins require HTTPS and only replace the origin of an already-validated signed stream URL |
| `HARRBOR_DATA_ROOT` | `/data` | Data directory (container path) |
| `HARRBOR_DATABASE_PATH` | `/config/darkharrbor.db` | SQLite database path |
| `HARRBOR_REPORTED_PATH_PREFIX` | `/data` | Path prefix in `.strm` file content; canonical Compose explicitly uses `/mnt/darkharrbor` for the Arr-visible mount |
| `HARRBOR_LOG_LEVEL` | `INFO` | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `HARRBOR_TOPOLOGY` | `t1` | Deployment profile: `t1` baseline, `t2` adds aggregated discovery + universal proxy, `t3` adds reactive commit + promotion. Unknown values are ignored with a startup warning and fall back to `t1` |
| `HARRBOR_TOPOLOGY_ACHIEVED_STALENESS_HOURS` | `48` | How long an achieved-topology observation stays fresh before it is recomputed |
| `HARRBOR_GOVERNOR_UNCACHED_BUDGET` | `20` | Rolling uncached-submission budget; distinct from TorBox's ten simultaneous slots, which count only actively downloading uncached TorBox torrents. Cached/ready torrents and all NZB, NNTP, HTTP, and other-provider work use no TorBox torrent slot. |

## Sealed Secrets

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_SECRETS_FILE` | `/config/secrets.sealed` | Path to sealed secrets file |
| `HARRBOR_SECRETS_UNLOCK` | source default `env`; canonical Compose sets `keyfile` | Unlock method: `env`, `keyfile`, or interactive `prompt` |
| `HARRBOR_SECRETS_KEY_FILE` | installer: `/run/darkharrbor-key/secrets.key`; canonical manual Compose: `/config/secrets.key` | Required only in `keyfile` mode |
| `HARRBOR_SECRETS_PASSWORD` | unset | Plain unlock value required only in `env` mode; never put it in tracked Compose or evidence |

Credentials sealed by the wizard include:
`HARRBOR_TORBOX_API_TOKEN`, `HARRBOR_RD_API_TOKEN`,
`HARRBOR_ALLDEBRID_API_KEY`, `HARRBOR_PREMIUMIZE_API_KEY`,
`HARRBOR_PROVIDERS`, `HARRBOR_QBIT_PASSWORD`, `HARRBOR_SAB_API_KEY`,
`HARRBOR_STREAM_SECRET`, selected `HARRBOR_MEDIAFLOW_PASSWORD`,
`HARRBOR_STREMIO_INSTALL_TOKEN`, NNTP credentials per
provider, Prowlarr URL + API key, Arr API keys + URLs, and
`HARRBOR_PREFERENCE`.

Both secret files must be owner-matched regular files with no group/world
access. Symlinks, symlinked parent components, writable parent directories,
empty/oversized files, and ownership mismatches are rejected. Reads are bounded
and use `O_NOFOLLOW`; setup/configure writes are atomic and fsync-backed. Setup
normalizes only its application-owned config and key directories to mode
`0700`, after the operator approves the plan.

Scheduled backups contain `darkharrbor.db` plus `secrets.sealed`, both mode
`0600`, and intentionally exclude `secrets.key`. They are private local
snapshots: the database is not encrypted. Keep `/backup` local. Before an
off-host or cloud copy, use `darkharrbor backup export` to create an
authenticated encrypted `.dhbackup` bundle. The bundle reuses but never
contains the sealed-store recovery key; a snapshot created before key rotation
therefore still requires its original recovery key.

The source default is `env` when `HARRBOR_SECRETS_UNLOCK` is omitted. The
canonical Compose file explicitly selects `keyfile`, and S7 writes the key file,
so normal Compose boots do not consume the recovery key. `prompt`
is valid for an attended process; terminal echo suppression is fail-closed.
Redirected input is accepted only as an explicit automation path. `password` is
not a mode name.

When aggregator integration is selected, S6 generates
`HARRBOR_MEDIAFLOW_PASSWORD` and S7 seals it. Public installer setup places the
literal only in private `mediaflow.env`; manual setup without a handoff uses the
attended terminal fallback. Copy it into the aggregator's private
`FORCE_PROXY_CREDENTIALS`, then delete or vault the handoff. It is a credential
key and is therefore rejected from `/config/darkharrbor.conf`.

This matters more than its absence suggests. `/proxy/ip` and `/generate_urls`
are gated on a NON-EMPTY `HARRBOR_MEDIAFLOW_PASSWORD` together with
`HARRBOR_STREAM_SECRET`. Leave it unset and the entire MediaFlow surface answers
`503`, the aggregator can never route playback through DarkHarrbor, and reactive
commit can never fire -- with nothing in the DarkHarrbor log naming the cause.

The setup wizard generates the opaque Stremio install token. Configure may
change mode and client origin but never reads or rewrites that token. Port 8381
must remain private; reverse proxies and Tailscale target only port 8382. Funnel
is public and must be selected explicitly. Returning to `internal` disables the
client listener without changing internal Arr/Jellyfin `.strm` authorities.

Treat the complete Stremio install URL as a credential: its token can be used to
query the managed library and obtain signed playback URLs. If it is exposed,
rotate `HARRBOR_STREMIO_INSTALL_TOKEN` in the sealed secrets and update every
installed copy of the addon URL in the same maintenance window, including an
aggregator custom-addon entry and direct clients such as Nuvio. The old URL must
then return not found. Regenerate or remove any saved install-URL artifact; do
not print it to verify rotation. `configure` does not rotate this token.

### Provider Credentials at Rest

Some provider capabilities discover credentials that CANNOT be re-fetched at
boot. A News Server password, for example, is minted by the provider, revealed
once at first provisioning, and returned masked on every later call — so it must
be persisted when it is revealed, and it is not operator-supplied static config.
Those live in the `provider_credentials` table rather than the sealed-secrets
file.

They are nonetheless SEALED AT REST, using the same envelope as sealed secrets
and the same key. A leaked database or backup is therefore not a credential
leak. Notes:

- Sealing is active whenever sealed secrets are configured. With sealing off,
  behaviour is unchanged and rows stay plaintext, so an upgrade cannot break a
  deployment that never adopted sealing.
- Existing plaintext rows are re-sealed IN PLACE on first read. No migration
  step, no downtime, no operator action.
- A wrong or missing key FAILS CLOSED with an error naming the provider. It
  never returns ciphertext as if it were the credential.
- The wizard writes this credential unsealed at S4, because the seal does not
  exist until S7. The daemon seals it on first read.
- Sealing does not scrub history from the live SQLite file — stale bytes remain
  in freelist pages and the WAL. Published backup snapshots use the SQLite
  online-backup API and are clean. `VACUUM` the live file if you want it
  scrubbed too.

## HTTP Stream Sources
> **OPTIONAL ACQUISITION FAMILY.** Enable this for Internet Archive, OMSS,
> typed generic HTTP/WebDAV sources, or an aggregator. It is not inherently an
> aggregator feature and can participate in the standard Arr workflow.


HTTP source definitions are installation-specific and intentionally absent from
the public Compose file.

| Variable | Description |
|---|---|
| `HARRBOR_HTTP_STREAM_ENABLED` | Enable the HTTP-stream lane |
| `HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES` | Allow trusted private/CGNAT source addresses when required by a self-hosted backend |
| `HARRBOR_HTTP_BACKENDS` | Ordered comma-separated backend IDs |
| `HARRBOR_HTTPBACKEND_<NAME>_TYPE` | `ia`, `omss`, `stremio`, or `generic` |
| `HARRBOR_HTTPBACKEND_<NAME>_URL` | Backend base URL for `ia`, `omss`, and `stremio`; `generic` uses only its descriptor file and does not need a dummy URL. Sensitive/token-bearing URLs belong in `secrets.sealed` instead |
| `HARRBOR_HTTPBACKEND_<NAME>_PUBLIC_URL` | Optional client-facing origin, without path/query/credentials, when it differs from the internal backend origin |
| `HARRBOR_HTTPBACKEND_<NAME>_REDIRECT_ORIGINS` | Optional comma-separated additional exact origins that this backend's metadata requests may follow; at most eight, with no path/query/fragment/credentials |
| `HARRBOR_HTTPBACKEND_<NAME>_DESCRIPTORS_FILE` | Absolute descriptor-file path for `generic` backends |
| `HARRBOR_TMDB_API_KEY` | A free TMDB API key. Required for `omss` backends to produce a real, Arr-parseable release title (`Movie.Name.Year...`); without it, OMSS falls back to an ID-only stub (`TMDB.<id>...`) that Sonarr/Radarr correctly reject as "Unable to parse release," so grabs never complete. Sensitive; belongs in `secrets.sealed` |

Backend metadata can therefore live in `/config/darkharrbor.conf` while a
sensitive URL for the same backend remains sealed. `darkharrbor configure`
displays sealed URLs only as `[sealed]`, never copies them into the non-secret
file, and dial-tests enabled sources sequentially with a five-second timeout.
If an IA, OMSS, or Stremio health probe encounters an unapproved cross-origin
redirect, it stops before contacting the target and asks whether to approve the
sanitized exact origin. The full redirect path and query are never displayed or
persisted. Approval updates only that named backend and reruns the bounded test.
Declining leaves configuration unapplied. Generic descriptor health performs no
network request, so any required generic metadata origin remains explicit input.

Outbound redirects do not implicitly widen configured authority. A configured
backend may redirect within its own exact origin. If its published metadata API
genuinely redirects elsewhere, `darkharrbor configure` accepts up to eight
additional exact origins for that backend only. Do not enter a complete URL,
wildcard, provider-wide domain, signed URL, token, path, or query. Each backend
gets a separate metadata client, so one source's approval cannot authorize
another. DarkHarrbor removes copied credentials, custom headers, cookies, and
`Referer` before every approved cross-origin hop while retaining bounded
range/encoding headers. HTTPS never redirects down to plaintext HTTP.

This setting governs backend **metadata** redirects only. Media-source/CDN
redirects continue through the separate source-trust policy, and changing it
does not change provider ordering, refresh, fallback, proof-gated cross-lane
recovery, or `.strm` self-healing behavior. Leave it blank unless the backend's
published behavior requires a second origin; the wizard can derive the
credential-free origin from a blocked IA/OMSS/Stremio health redirect, but it
never approves or follows that origin without the operator's explicit `yes`.

### Private cloud drives through WebDAV

DarkHarrbor already has two different WebDAV roles. `/dav/` is its private
output/compatibility server for owned media; it is not a cloud-drive importer.
Cloud input uses a `generic` HTTP source whose private typed-descriptor file can
declare `webdav` collections.

The supported minimum path is:

```text
cloud provider -> operator-managed rclone serve webdav -> private container network
               -> DarkHarrbor generic descriptor -> stable DarkHarrbor .strm
```

Keep the provider login entirely in rclone. Do not publish its credential-free
WebDAV listener to the host or an untrusted network. DarkHarrbor currently does
not send WebDAV Basic/OAuth credentials and does not claim a direct Google
Drive, PikPak, or other provider API adapter.

Each `webdav` URL grants access to that collection only. DarkHarrbor performs a
bounded, non-recursive `PROPFIND` (`Depth: 1`) and accepts video members only
when their normalized URL remains on the descriptor's exact origin and beneath
its declared collection path. Cross-origin, parent-path, and sibling-path hrefs
returned by a server are ignored. Live playback still passes through the shared
source-address, redirect, and bounded-range controls.

M3U, Metalink, and WebDAV members use opaque content-derived selectors rather
than list positions. Reordering a live listing therefore cannot silently point
an existing DarkHarrbor item at different media. Positional `#N` selectors from
older builds cannot be reconstructed safely and fail closed; remove and re-grab
an affected generic-source item once to install its stable selector. If two
current entries produce the same stable identity, DarkHarrbor does not advertise
the descriptor until the catalog distinguishes them; it never chooses one by
position.

The repository's optional rclone integration check starts a read-only
`rclone serve webdav` over a test-owned local directory on a non-loopback
private address. It verifies real WebDAV listing and content lengths, generic
search/resolve, a nonzero-offset bounded range read through DarkHarrbor's byte
source, insertion of an earlier-sorting member, rclone restart, and a fresh
handler instance. Run it with:

```sh
cd src
go test -v ./internal/httpstream/generic \
  -run TestRcloneWebDAVSearchResolveRangeReorderAndRestart -count=1
go test -v ./internal/api \
  -run TestGenericWebDAVResolvePersistsStableStrmAndPlaysAfterHandlerRestart \
  -count=1
```

The test skips when rclone or a private IPv4 fixture address is unavailable and
deletes its temporary server/files on completion. It proves the credential-free
local WebDAV contract. The companion API test proves the production HTTP resolve
transition, URL-free persisted file list, signed DarkHarrbor-only `.strm`, fresh
handler registry, listing insertion, and ranged playback. These are composable
fixtures, not a claim that a real Arr or media server participated. They do not
prove a cloud provider's authentication refresh, throttling, VFS cache, external
Arr import, full-process restart, or genuine media-server playback behavior.

**Acceptance status:** this lane is experimental. No session has had a real
cloud-provider account (Google Drive, Dropbox, or similar behind
operator-managed rclone) available to run the same real-account Arr
search/grab/import/playback/fail-closed acceptance path that Internet Archive,
generic HTTP, TorBox, NZB/NNTP, OMSS, and Real-Debrid have each passed. Treat
private cloud/WebDAV input as unverified beyond the protocol-level rclone
fixtures above until a real account becomes available for that run.

Generic re-resolution pins media identity, not transport location. Give any
descriptor whose URL may expire or move a unique `source_id` of 1–64
characters: a lowercase letter or digit first, then any mix of lowercase
letters, digits, dots, underscores, or hyphens. DarkHarrbor then keeps the same
descriptor identity when its URL, signed query, CDN, mirror, or provider
changes, while still binding it to the declared movie/episode metadata. Named
M3U entries likewise keep identity across URL/mirror changes; unnamed entries
use their path and fail closed when no stable media label exists. Metalink uses
its strongest declared digest, then name plus size, but currently selects only
the best declared URL on each resolve rather than retaining all Metalink URLs
as independent runtime mirrors. WebDAV uses the confined member path while
rclone owns provider failover and token refresh behind that stable private
origin. Ordering multiple HTTP backends prioritizes search results; it does not
permit an already-grabbed item to jump to another backend.

Create a local `cloud-descriptors.json`; do not try to edit the Docker volume
directly. Keep the local file mode `0600`. A minimal catalog entry is:

```json
[
  {
    "source_id": "example-show-s01e01",
    "kind": "webdav",
    "title": "Example Show",
    "tvdb_id": "12345",
    "season": 1,
    "episode": 1,
    "url": "http://rclone-webdav:8080/tv/example-show/season-01/"
  }
]
```

Each entry uses this provider-neutral contract:

| Field | Requirement | Meaning |
|---|---|---|
| `kind` | required | One of `progressive`, `fixed`, `hls`, `m3u`, `metalink`, `object`, or `webdav` |
| `url` | required | Absolute HTTP(S) location owned by this descriptor; credential-bearing URLs still belong only in the private catalog |
| `source_id` | recommended | Stable 1–64 character lowercase identity (must start with a letter or digit) that survives URL, mirror, or provider changes |
| `title`, `imdb_id`, `tvdb_id`, `tmdb_id` | at least one required | Exact media identity; episodes also require both `season` and `episode` |
| `year`, `name`, `content_type`, `quality` | optional | Operator-declared matching or presentation metadata; `quality` must be a supported label |
| `size` | optional except for `fixed` | Exact byte count. `fixed` requires a positive value; negative and zero fixed sizes are rejected before setup |

**Exact bounds** (`src/internal/httpstream/generic/generic.go`), all fixed,
not operator-tunable, and NOT uniform in how they fail: byte-size ceilings
are hard rejections, count ceilings silently truncate instead. The
descriptor file itself is capped at 2 MiB (`install.sh --catalog`'s own
validation matches this) and rejected outright over that size; more than
5000 descriptor entries in a valid file is also a hard rejection ("exceeds
entry bound"). Within a descriptor, an `m3u` playlist body over 1 MiB, a
`metalink` body over 1 MiB, or a `webdav` PROPFIND response over 2 MiB are
each a hard rejection of that fetch. But once a body is successfully read,
its internal counts are capped by silently keeping only the first N and
discarding the rest, no error: 4000 M3U lines, 500 resolved M3U entries, 50
metalink files, 20 URLs per metalink file, 2000 WebDAV members. A directory
with 2500 WebDAV members is not an error -- it quietly becomes 2000.

Use `progressive` for an ordinary direct HTTP media object when DarkHarrbor
should obtain its current size and content type with a bounded HEAD request.
Use `object` for the same single-object discovery contract when the source is
conceptually object storage. Use `fixed` only when the producer already knows
and supplies the exact positive byte size; it deliberately performs no metadata
probe. `hls` names an HLS manifest. `m3u` and `metalink` expand their bounded
manifests, while `webdav` performs a confined bounded collection listing.
DarkHarrbor never infers a kind from a URL.

A minimal direct-HTTP movie entry is therefore:

```json
[
  {
    "source_id": "example-movie-primary",
    "kind": "progressive",
    "title": "Example Movie",
    "tmdb_id": "12345",
    "url": "https://operator-controlled.example/media/example.mp4"
  }
]
```

Catalog import validates this contract before the first-use wizard. In
particular, an unusable zero-size `fixed` entry now fails before an Arr can see
or grab it. This does not change fallback: replacing the URL while retaining
the same `source_id` and media identity keeps the existing pointer eligible to
resolve the new location.

For a fresh installation, stage it before the wizard with:

```sh
./install.sh --catalog "$PWD/cloud-descriptors.json"
```

The installer accepts only an absolute, current-user-owned, non-symlink regular
file at mode `0600`, bounds it to 1 byte–2 MiB, and shows only
`selected (private; contents hidden)` in the mutation preview. After approval
it opens and revalidates a pinned descriptor only for passing the content over
stdin to a one-shot catalog validator,
which atomically installs `/config/cloud-descriptors.json` before setup. The
file content and source URLs never enter Compose, environment values, command
arguments, or installer output. Invalid input aborts setup and removes the
incomplete project and its volumes. In setup, select type `generic` and enter
the fixed container path `/config/cloud-descriptors.json`.

For an existing installation, import a replacement through stdin so neither
source URLs nor credentials appear in shell arguments and the container writes
it with the correct ownership and mode:

```sh
docker compose -f .darkharrbor/compose.yml exec -T darkharrbor \
  darkharrbor catalog import < cloud-descriptors.json
```

The command bounds and validates the complete catalog before atomically writing
`/config/cloud-descriptors.json` as mode `0600`; an invalid replacement leaves
the prior catalog untouched. Each descriptor must be a JSON object using only
the documented lowercase field names; unknown fields, case aliases, duplicate
keys, `null`, symlinks, loose permissions, unexpected ownership, unsafe
parents, oversized files, and replacement races are rejected.

Then run `darkharrbor configure`, enable HTTP stream sources, choose a source
name, select type `generic`, and enter `/config/cloud-descriptors.json`. No
backend URL is requested because every live source is owned by the descriptor
catalog. The bounded setup dial test validates the installed file before
saving. A catalog producer may refresh a direct URL without changing an
existing pointer only when that descriptor has a stable `source_id`; without
one, the URL remains part of its backward-compatible identity and a changed URL
requires a re-grab. The private rclone endpoint is preferred because rclone
owns provider token refresh behind a stable internal URL.

This mechanism complements rather than replaces other fallback layers.
Configured debrid providers retain ordered submission fallback and direct NNTP
providers retain lane-local retry/failover. A selected debrid provider remains
bound to the item after submission.
Proof-gated cross-lane continuity may recover identical bytes through HTTP,
torrent, or NNTP when enabled. DarkHarrbor never changes to a merely
similar-looking file: missing stable identity or missing byte proof abstains.

### Media-server support boundary

DarkHarrbor’s durable output is a plain `.strm` file containing a stable signed
DarkHarrbor HTTP URL. Its playback endpoint implements the player-neutral
GET/HEAD/range behavior used for probing, seeking, and transcoding. Jellyfin is
currently the only media server with verified automatic wrapper, bitrate, and
path-visibility setup. Emby may use the core `.strm` contract when configured
manually, but automated Emby mutation is not yet claimed. Plex `.strm` behavior
and automated Plex configuration remain unverified and are not advertised as a
working integration.

**AIOStreams is the important sensitive-URL case.** DarkHarrbor's `stremio`
handler needs the saved user's addon base URL, not the bare AIOStreams origin.
Run `darkharrbor configure --existing-aiostreams` and enter the dashboard's
complete **Direct Manifest URL** at its hidden prompt. The command removes only
the final `/manifest.json`, replaces only the client-facing origin with the
operator-supplied internal origin, preserves the complete
`/stremio/<user>/<opaque>` path, and verifies it before saving. DarkHarrbor
appends `/manifest.json` for health and `/stream/<type>/<id>.json` for lookups.
A bare `http://aiostreams:3000` can pass a socket probe while returning no
playable sources.

That path is a credential. Do not put it in `darkharrbor.conf`, chat, logs, or
evidence. The focused command writes it to `secrets.sealed`; only the
non-secret backend name, type, and public origin are written to the tuning file.

### Stable AIOStreams Route Refresh

DarkHarrbor supports the owned playback-route capability in official stable
AIOStreams; no fork or version allowlist is required. Configure AIOStreams with
its standard service wrapper enabled (`serviceWrap.enabled=true`) and its
file-info store disabled (`builtins.debrid.fileinfoStore=false`; this setting
was not found via settings search on locked AIOStreams v2.31.1 during live
testing and may have been renamed or removed upstream). The latter is
required because stored file information becomes a finite cache key, whereas
durable refresh requires the self-contained inline descriptor. AIOStreams must
also have its normal resolver/debrid service configured and reachable.

### Supported Aggregator Dependency Posture

An aggregator such as AIOStreams depends on two CATEGORICALLY DIFFERENT kinds
of upstream, and the difference decides how their failures behave.

**Shared infrastructure.** A debrid abstraction tier (for example StremThru)
sits between the aggregator and every debrid service. Its failure is TOTAL:
every configured source loses resolution simultaneously, and the client sees
only that media cannot be played. On 2026-08-16 a default PUBLIC instance of
this tier went down and did exactly that.

**Content sources.** Individual addons and indexers. Their failure is ISOLATED:
the aggregator skips the failing one and returns results from the rest.
Playback continues.

RECOMMENDATION: self-host the shared tier. DarkHarrbor makes no recommendation
about content sources, does not manage, deploy, proxy for, or health-check
third-party addons, and takes no dependency on any specific shared-tier
service.

#### The reachability constraint (load-bearing)

A self-hosted shared tier generates playback URLs, and **WHO FETCHES THEM
DEPENDS ON THE PROXY POSTURE.** Get this backwards and playback fails opaquely,
naming nothing. The base URL — `STREMTHRU_BASE_URL` for StremThru — must be
reachable by whichever party actually fetches it.

**Selective proxying (aggregator proxies only some services).** Streams from
unproxied services reach the CLIENT as shared-tier URLs, so the base URL must be
an address reachable from every viewing device and CANNOT be `localhost` or a
container-internal name. An address correct from the server and wrong from the
client fails at the player.

**Universal proxying (aggregator proxies everything).** Every stream is
rewritten to DarkHarrbor before it reaches a client, so no client ever fetches a
shared-tier URL. The only consumer is DarkHarrbor, from inside the container
network, and the base URL must be reachable FROM THERE. A container-internal
name is then not merely acceptable but usually correct.

THE TWO REQUIREMENTS CAN CONFLICT, and one variable cannot satisfy both. This is
not hypothetical. On the reference deployment, moving to universal proxying
broke every previously-working shared-tier stream: the base URL was the host's
tailnet address, published to the tailnet interface only, and Docker's DNAT rule
for that port carries `! -i <bridge>`, so a packet from the container bridge
addressed to the tailnet IP is never rewritten and simply times out. The symptom
was HTTP 502 at a flat 10.004s with NO request reaching the shared tier and
nothing logged by DarkHarrbor. A sibling container on the same bridge was
unaffected and looked fine, because its traffic routes directly and needs no
DNAT — so one service working proves nothing about another.

Verify from the party that fetches. Establish which that is FIRST, by inspecting
what a real stream lookup returns: if every client-facing URL points at
DarkHarrbor, the client is not a consumer and only the container network
matters. Do not infer it from configuration.

CHANGING THE PROXY POSTURE CHANGES THIS REQUIREMENT. Moving between selective
and universal proxying silently moves the reachability burden between the client
and the container network, so the base URL must be re-evaluated in the same
maintenance window. Treat them as one coupled change.

#### Diagnosis

`doctor` reports two checks under `deployment-topology` so an aggregator-side
failure is NAMED rather than presenting as an unexplained playback error:

- `aggregator resolution` — deliberately does NOT perform a lookup, and never
  reports anything but `SKIP`. An earlier version ran one real stream lookup
  against the configured backend; that was removed because the lookup is not
  read-only -- it can call the installed DarkHarrbor Library addon while
  resolving the probe title, which seeds reactive identity state in the
  running daemon just from running `doctor`. Its detail explains exactly
  this: "full catalog lookup is deliberately excluded because it can call the
  installed Library addon and alter reactive identity state." The two checks
  actually relied on for aggregator health are `aggregator egress` (socket
  reachability, see below) and `aggregator delivery`, next.
- `aggregator delivery` — read from durable playback observation, which does
  carry it, because a resolution failure stops bytes from ever transiting
  DarkHarrbor. It distinguishes three states: nothing has EVER been delivered
  (the posture never worked — check client reachability of the playback base
  URL); delivery previously worked and has STOPPED (an aggregator-side failure
  downstream of discovery, typically the shared tier, since a content-source
  outage does not stop delivery); and recent delivery (healthy).

The corresponding metrics are `darkharrbor_aggregator_delivery_observed` and
`darkharrbor_aggregator_delivery_age_seconds`.

#### Lockable versus unlockable aggregator settings

Required aggregator posture should be ENVIRONMENT-LOCKED, so that a
configuration save cannot silently revert it. Not every setting supports that,
and the difference is operationally important.

**Lockable.** An environment variable overrides the aggregator's database, and
a save against the setting is rejected (`Setting <key> is overridden by
<ENV>`). Set these in the aggregator's compose file, where they are readable,
diffable and version-controllable:

    FORCE_PROXY_ENABLED, FORCE_PROXY_ID, FORCE_PROXY_URL,
    FORCE_PROXY_PROXIED_SERVICES, FORCE_PROXY_DISABLE_PROXIED_ADDONS,
    BUILTIN_STREMTHRU_URL, STREMTHRU_TORZ_URL, STREMTHRU_STORE_URL

**Unlockable.** The saved user configuration wins. For an installed addon
instance the preset resolves `options.url || DEFAULT_URL`, so the environment
variable seeds NEW configurations only and the stored value always takes
precedence. This covers installed addon instance URLs and options, per-addon
filters, and user filter/sort/formatter configuration.

RESIDUAL RISK, stated plainly because it cannot be engineered away from this
side: an unlockable setting can change without leaving a readable before/after,
because the user configuration is AES-encrypted under the account password. A
required setting can therefore be silently reverted while validation still
reports healthy. That is not hypothetical — it is what happened on 2026-08-14
and again on 2026-08-16, and it is why an unlockable setting should be treated
as a standing operational risk rather than a settled configuration.

MITIGATION: track change DETECTION even where the value is unreadable. The
reference deployment runs an hourly snapshot that records, per user, a SHA-256
of the encrypted configuration blob and its `updated_at`, alongside the VALUES
of every lockable setting and the NAMES of the unlockable ones. A change to an
unlockable setting is then detectable from that history alone: the hash and
timestamp move, telling you WHEN a save occurred and therefore which interval
to attribute a behaviour change to. It does NOT tell you WHICH setting changed.
That limit is the residual risk, and it is the reason to prefer the lockable
mechanism wherever a setting supports it.

Verified 2026-08-17: a real addon-URL edit moved the recorded hash and
`updated_at`, and was attributable from the history without decrypting the
configuration or reading any credential.

### Client-Facing MediaFlow Origin

`HARRBOR_MEDIAFLOW_BASE_URL` sets the origin used to build the playback URLs
DarkHarrbor hands to an aggregator, which the aggregator then hands to VIEWING
DEVICES.

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_MEDIAFLOW_BASE_URL` | *(empty)* | Client-facing origin for generated MediaFlow playback URLs. Empty falls back to `HARRBOR_SERVER_BASE_URL`. |
| `HARRBOR_MEDIAFLOW_PUBLIC_IP` | *(empty)* | Optional literal public egress address reported by `/proxy/ip`. When empty, DarkHarrbor resolves it at runtime. An explicit literal always wins. |

Both keys are accepted in the strict `/config/darkharrbor.conf` tuning file.
That file is applied after sealed values and process environment, so its value
wins over any Compose environment value. Canonical Compose intentionally leaves
both keys unset: deployment-specific literals belong in the tuning file.

This is the MediaFlow-direction counterpart to
`HARRBOR_HTTPBACKEND_<NAME>_PUBLIC_URL`, and exists for the same reason: an
address that is correct from the server's perspective can be wrong from the
client's, and it fails opaquely.

WHY IT IS REQUIRED. `HARRBOR_SERVER_BASE_URL` is normally a container-internal
name such as `http://darkharrbor:8381`. MediaFlow playback URLs are fetched by
client devices, not by DarkHarrbor, and a client cannot resolve a container
name. Without this variable the aggregator hands clients an unreachable origin
and playback fails at the PLAYER with a generic "media cannot be played",
naming neither DarkHarrbor nor the cause. Set this on any `T2`/`T3` instance
whose clients are not on the container network. Single-network deployments may
leave it empty; behaviour is then unchanged.

The value is an origin only. It must carry no path, query, or credentials, and
must be reachable from every viewing device.

The public-IP lookup is bounded and uses multiple external address services so
no single service is required. DarkHarrbor caches the last successful address,
refreshes it periodically, and continues serving that last known good value if
a refresh fails. `darkharrbor doctor` reports a failed lookup with no cached
value as a failure and a stale last known good value as a warning. The residual
risk is unavoidable: after a WAN-address change, clients can receive the old
address until the next refresh succeeds. Set `HARRBOR_MEDIAFLOW_PUBLIC_IP` only
when a stable literal override is preferable to automatic discovery.

PORT EXPOSURE. Point this at the RESTRICTED client listener on port 8382, not
at the primary API on 8381. Only `/proxy/stream` is fetched by clients, and it
is served by both listeners; the aggregator-facing `/proxy/ip` and
`/generate_urls` are served by the primary router ONLY and are called over the
container network. Serving playback therefore does not require publishing 8381,
which must remain private as stated above: it carries the qBit, SAB, WebDAV,
metrics, debug and administration surfaces on a daemon that reaches sealed
credentials.

The playback route authenticates from the AEAD-sealed ticket in its own query
string, so it is self-contained on the restricted listener and adds no trust
surface there. Verify after configuring: `/proxy/stream` on 8382 answers (403
without a valid ticket), while `/generate_urls` and `/proxy/ip` on 8382 return
404. Never publish 8381 on `0.0.0.0` or behind Funnel.

Configure the backend as type `stremio`. If AIOStreams emits an origin that
clients use instead of its internal `arr-net` origin, set the corresponding
`HARRBOR_HTTPBACKEND_<NAME>_PUBLIC_URL`; this is an origin only and is managed by
`darkharrbor configure`. The MediaFlow password is generated and sealed by
setup when aggregator integration is selected (see Sealed Secrets above).

`HARRBOR_NNTP_CROSSLANE_SPLICE` is **not required** for ordinary MediaFlow
playback, coverage capture, or reactive commit. Despite its legacy `NNTP_` name,
it gates proof-backed cross-lane recovery and durable owned-route admission for
NNTP, torrent, and HTTP sources. Enable it only when adopting that additional
route-refresh/recovery behavior; leaving it false is a supported minimal t3
subset.

Admission is fail-closed and proof-gated. DarkHarrbor accepts only the exact
configured AIOStreams origin and owned route shape with a complete inline
torrent or usenet descriptor. A unique ready native candidate must match the
release key, size, descriptor metadata, and delivered-byte proof before the
URL-free route becomes durable. Cache-key routes, arbitrary external URLs,
ambiguous candidates, malformed descriptors, contradictory geometry, and
canceled requests never gain durable status. If a future stable release changes
the capability, playback can remain ephemeral but durable admission abstains.

## Diagnostics

`darkharrbor doctor` is the fastest way to find out why something is not
working. It exits `1` if any check FAILs, so it is usable in a health probe or
a cron job, and `-json` emits the whole report for machine consumption.

Two check families are worth knowing about specifically, because they cover
failures that are otherwise invisible.

**`aggregator egress: <backend>`** fetches each configured HTTP backend FROM
DARKHARRBOR'S OWN NETWORK POSITION. This is not the same question as "is the
backend up". A backend can be perfectly reachable from every viewing device and
unreachable from inside the container network — a published port bound to a
single host interface does exactly that, because Docker's DNAT rule excludes the
container bridge, so a packet from inside is never rewritten and simply times
out. Under universal proxying DarkHarrbor is the ONLY fetcher of those URLs, so
client reachability is not sufficient.

The check reports the transport cause — DNS, TIMEOUT, REFUSED, NO ROUTE, TLS —
rather than a single opaque failure, and names the address it tried with any
path, query and userinfo stripped. ANY HTTP status is a pass: the property under
test is only the socket path, not the application response, so a `404` from a
reachable host is reported OK. Its PASS detail therefore says `socket-path
check only`: the separate full-resolution lookup may still time out or fail at
the application layer without contradicting egress. A PASS detail now points to
`aggregator delivery` (below) for that deeper signal, not to a second lookup of
its own -- there is no separate full-resolution lookup anymore; see the next
paragraph.

```
[OK  ] aggregator egress: aiostreams   reachable from DarkHarrbor at
                                       http://aiostreams:3000 (HTTP 404;
                                       socket-path check only -- ...)
[FAIL] aggregator egress: aiostreams   UNREACHABLE from DarkHarrbor's own network
                                       position at http://host-only:8600
                                       (TIMEOUT: no response before the deadline)
```

**`nntp: connection pools`** reports the DEFAULT pools first, then each
provider's own override. Per-provider `HARRBOR_PROVIDER_<NAME>_*` values are
what the pool is actually built from — read those, not the defaults. Until
2026-08-22 this check printed only the defaults under a name promising
effective values, and was wrong on any deployment using per-provider pools.

**`aggregator resolution`** performs no lookup at all and always reports
`SKIP`. It once ran one real stream lookup against the configured backend;
that was removed because the lookup isn't read-only -- it can call the
installed DarkHarrbor Library addon while resolving the probe title, which
seeds reactive identity state in the running daemon just from running
`doctor`. Its detail states this reasoning directly. `aggregator egress`
(socket reachability, above) and `aggregator delivery` (below, real
observed playback) are what doctor actually relies on for aggregator
health now.

**`reactive: commit-capable lanes`** states which lanes can reach commit at all.
Coverage is recorded only for bytes DarkHarrbor delivers, so a lane commits if
and only if the aggregator proxies it here. The check reflects the two-stage
toggle honestly: capture disabled and commit-mode `off` are different states and
neither commits.

Verify the proxy posture by INSPECTING A REAL STREAM LOOKUP, not the
aggregator's saved configuration. Environment forcing is applied at request
time, so the saved value can disagree with the effective one. If every returned
stream URL points at DarkHarrbor, every lane can commit; any that do not,
cannot.

## Backup and Restore

DarkHarrbor creates a consistent SQLite online backup immediately after startup
and then on the configured cadence. Each atomically published snapshot contains
`darkharrbor.db` and `secrets.sealed`; incomplete snapshots are ignored. The
base Compose file stores snapshots in a separate `darkharrbor-backups` volume.
Bind-mount `HARRBOR_BACKUP_TARGET` to independent storage for host-loss recovery.
That storage must remain local/private. The SQLite snapshot itself is not
encrypted; only a `.dhbackup` file produced by `darkharrbor backup export` is
safe to copy off-host.

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_BACKUP_ENABLED` | `true` | Enable scheduled snapshots |
| `HARRBOR_BACKUP_TARGET` | `/backup` | Absolute snapshot target directory |
| `HARRBOR_BACKUP_INTERVAL_HOURS` | `24` | Hours between snapshots |
| `HARRBOR_BACKUP_KEEP` | `7` | Complete snapshots retained |

The sealed key/password is intentionally not copied beside `secrets.sealed`.
Keep that recovery material independently.

Create an immediate local snapshot with the same SQLite online-backup and
atomic-publication path used by the scheduler:

```sh
docker compose exec -T darkharrbor darkharrbor backup snapshot
```

This works even when scheduled backups are disabled. It requires the live
database and private sealed-secrets file, honors `HARRBOR_BACKUP_TARGET` and
`HARRBOR_BACKUP_KEEP`, and prints only the new snapshot name. The published
snapshot remains local plaintext and does not contain its unlock key. Manual and
scheduled snapshot writers serialize on a private target-local lock.

Create an authenticated encrypted bundle from the newest snapshot:

```sh
docker compose exec darkharrbor darkharrbor backup export --from /backup --to /backup/exports
```

The command uses the already-loaded sealed-store recovery key, writes a private
mode-`0600` `.dhbackup` file atomically, and never prints or accepts the key as a
flag. Copy only that `.dhbackup` file out of `/backup/exports` and off-host;
for example, `docker compose cp darkharrbor:/backup/exports/. ./darkharrbor-exports/`.
The format uses Argon2id and
chunked AES-256-GCM; every header, chunk, and end marker is authenticated, so a
wrong key, corruption, truncation, appended data, or an unexpected archive path
fails closed. The unlock key is not embedded in the bundle.

To bring a bundle back, keep its independently retained recovery key in the
installer-managed key volume (or supply another private key file). Import
creates a validated normal snapshot; it does not modify the live database:

```sh
chmod 600 ./darkharrbor-example.dhbackup
docker compose run --rm --no-deps \
  -v "$PWD/darkharrbor-example.dhbackup:/incoming/backup.dhbackup:ro" \
  darkharrbor backup import \
  --file /incoming/backup.dhbackup \
  --to /backup
```

Both input files must be owned by the container user and mode `0600`; their
immediate parents must be owned by that user or root, non-writable by group/world,
and reached without symlinks. Import accepts the key only from that private
file, authenticates the entire bundle, bounds file sizes, rejects every path
except `darkharrbor.db` and `secrets.sealed`, validates SQLite integrity, and
removes incomplete staging data on failure. The default key path comes from
`HARRBOR_SECRETS_KEY_FILE`; `--key-file` is needed only for separately mounted
recovery material.

For an installer-managed deployment, restore one exact local or imported
snapshot from the trusted checkout:

```sh
./install.sh --snapshots
./install.sh --restore
```

`--snapshots` runs the read-only `darkharrbor backup list --from /backup`
primitive in a one-shot container and prints only sorted canonical basenames for
structurally complete candidates. It does not load application configuration,
unseal secrets, inspect the recovery key, print container paths, or stop/start
services. Structural completeness is not a promise that restore will succeed:
permissions, no-follow identity, bounds, and SQLite integrity are deliberately
validated again by the restore primitive after selection.

Enter only its canonical directory name, for example
`darkharrbor-20260830T120000.000000000Z`; paths and aliases are rejected. The
matching recovery key must already be installed or independently available.
After exact `RESTORE <snapshot>` confirmation, the installer first runs a
read-only `backup verify` one-shot against that exact directory and the mounted
private key file. It validates SQLite and authenticates and parses the sealed
store while emitting only `OK` or a generic failure. A wrong key, tampered
store, malformed plaintext, unsafe file, or invalid database aborts before a
safety snapshot or service stop. After preflight, a running daemon must publish
a new safety snapshot. The installer then stops both DarkHarrbor and the scoped
connector, invokes the existing restore command, and calls health-gated
`--start` only on success. Safety-snapshot failure stops nothing. Restore
failure leaves services stopped and preserves the pre-restore live files for
inspection. Restore repeats its own safety and integrity validation rather than
trusting the earlier preflight.

Manual Compose deployments may invoke the same primitive directly:

```sh
docker compose stop darkharrbor && docker compose run --rm --no-deps darkharrbor restore --from /backup/darkharrbor-YYYYMMDDTHHMMSS.NNNNNNNNNZ
```

The restore command validates SQLite integrity, refuses to run while the daemon
holds the config-volume lock, and restores both the database and sealed secrets.
Pass an exact snapshot directory to `--from` to restore an older retained copy.
Snapshot inputs must be owner-only regular files beneath safe non-symlinked
parents. Restore opens them with `O_NOFOLLOW`, copies them into private
mode-`0600` staging files, and validates SQLite integrity on that exact staged
database before replacing either live file. A failure preserves the pre-restore
database and sealed store. The daemon/restore and snapshot-writer locks are
owner-only regular files opened with `O_NOFOLLOW`; unsafe pre-created locks are
rejected. Published snapshots remain immutable, so the backup volume may be
mounted read-only during a restore rehearsal.

Installer restore uses the currently pinned image and leaves
`darkharrbor.conf`, the unlock-key volume, Compose, external networks, images,
and bind-mounted media unchanged. It is not automatic image rollback. When a
release rollback also needs an older image or matching historical tuning file,
restore those reviewed artifacts separately before starting that older release.

The tested total-config-loss boundary is exact: a retained snapshot plus its
separately retained unlock key restores item identities, authoritative `.strm`
URLs, and sealed credentials. On startup, database-authoritative reconciliation
can rematerialize unconsumed download-side pointers under `/data`. It
deliberately does not recreate consumed pointers that Arr already moved into a
media library. Audit those permanent pointers against the restored database by
mounting the actual host library read-only:

```sh
docker compose run --rm --no-deps \
  -v "/path/to/media/library:/audit:ro" \
  darkharrbor readopt --library /audit
```

The normal `/data` mount is DarkHarrbor's Arr-import staging tree, not
necessarily the final library after Sonarr/Radarr moves a pointer. Repeat the
command for every relevant library root. `readopt` opens SQLite in `mode=ro`
with `query_only`, is bounded, confines file opens beneath the supplied root,
skips discovered symlinks, and never contacts a URL. An orphan pointer does not
contain enough provider/source authority to reconstruct a missing database row
safely. The backup volume also does not back up the operator's media library or
the separate unlock key; protect those independently.

## Release Upgrades and Rollback

Database schema 14 is the oldest state this release can upgrade from. Startup
applies the embedded forward migrations through schema 45; restarting the same
release is idempotent. A supported prior
`darkharrbor.conf` remains valid, and unknown or retired tuning keys are
ignored with key-only warnings instead of preventing startup.

Before upgrading, separately preserve the exact `darkharrbor.conf` plus the
sealed-secret unlock key or password. The installer creates the complete local
snapshot itself while the current release is still running.
Scheduled snapshots contain only `darkharrbor.db` and `secrets.sealed`;
unlock material is deliberately excluded.

Installer-managed deployments use:

```sh
./install.sh --upgrade
```

The command fails closed unless `.darkharrbor/compose.yml` is a regular,
same-user, mode-`0600` file whose first line is the installer schema `v1`
marker. It resolves both release images to immutable digests, changes only the
main and scoped-connector image fields, validates the full Compose model, and
publishes it atomically. Existing networks, paths, connector targets, volumes,
settings, and secret storage are preserved. A connector that was stopped stays
stopped. After the operator confirms the independently held recovery key, the
installer runs `darkharrbor backup snapshot` against the current service and
refuses to change the deployment if it fails. Pulling and inspecting candidate
images can happen before that checkpoint, but the Compose file and running
services do not change. A failed health wait stops the new DarkHarrbor service;
it does not start the prior image against possibly migrated state.

Manual Compose deployments have no equivalent wrapper — the installer's
digest-pinning and automatic health-gated rollback are installer-only. The
same discipline applies by hand:

1. Preserve `darkharrbor.conf` and the retained recovery key/password, as
   above.
2. With the current release still running, snapshot first:
   `docker compose exec darkharrbor darkharrbor backup snapshot`.
3. Pull the new image and recreate only the DarkHarrbor service:
   `docker compose pull darkharrbor && docker compose up -d --force-recreate darkharrbor`.
   This changes only the image; the same named volumes stay attached, so the
   database, sealed secrets, and unlock key are untouched.
4. Verify before calling it done: `docker compose exec darkharrbor darkharrbor
   doctor`, plus `/healthz`, schema version, and Arr reconciliation.
5. If the new image does not come up healthy, follow the rollback procedure
   below using the direct restore primitive documented under Backup and
   Restore (`docker compose run --rm --no-deps darkharrbor restore --from
   ...`) rather than `install.sh --restore`, which only accepts an installer
   schema `v1` deployment.

There is no automatic rollback here — recreating the service and checking its
health afterward is the operator's own responsibility, not something Compose
verifies for you.

Rollback is snapshot-based:

1. Stop DarkHarrbor.
2. Restore the pre-upgrade snapshot with the restore command.
3. Restore the matching prior `darkharrbor.conf` when it changed.
4. Start the prior image and verify `/healthz`, schema, providers, and Arr
   reconciliation.

Never start an older image against a database already migrated by a newer
release, and never use an in-place Goose down migration on production state.
This preserves a single rollback owner and avoids relying on reversibility of
historical schema changes.

## Installer Uninstall and Data Retention

Run `./install.sh --uninstall` only from the trusted checkout that owns the
mode-`0600`, same-user, schema-v1 generated Compose file. The command pulls no
images and reads no credentials. Its action prompt defaults to cancellation.

- `retain` runs project-scoped Compose down without `-v`. Runtime containers
  and the internal control network are removed; Compose, config/database/sealed
  secrets, backups, unlock-key volume, private handoff, external Arr networks,
  bind-mounted media, and images remain.
- `delete` inventories Docker volumes first and accepts only the exact three
  installer names whose Compose project and volume-key labels match. It shows
  those names and the generated Compose path, requires `DELETE DARKHARRBOR`,
  removes runtime objects, deletes the validated volumes, then removes Compose.
  A foreign label, inventory/inspection failure, runtime-removal failure, or
  volume-removal failure stops the operation and retains Compose for recovery.

Neither path deletes external networks, the host media tree, pulled images, or
`.darkharrbor/handoff`. The media tree may contain signed `.strm` pointers and
the handoff may contain recovery credentials or tokenized addon URLs; archive
or delete them deliberately after deciding they are no longer needed.

Resume retained state with `./install.sh --start`. The command reuses the
installer Compose ownership/mode/schema checks, then requires exactly one
generated `HARRBOR_DOCKERPROXY_TARGETS` line with an empty or strict
comma-separated container-name value. It never prints the value or reads
secrets. Empty starts only DarkHarrbor; non-empty starts the `strm` profile so
the previously narrowed connector remains available. `--pull never` prevents a
resume from changing pinned images, and Compose must report healthy within 120
seconds. Failure stops only services that were not running before this start
attempt and leaves all retained data unchanged.

MediaFlow playback tickets created by newer images may carry an optional
sealed Stremio coordinate. Older images reject that newer ticket payload
strictly. After rolling back to an older image, refresh the affected stream
list in AIOStreams or the viewing client so it receives tickets minted by the
running version; already-issued playback URLs can fail until they are refreshed.

## Reverse Proxy and Bind Safety

The standalone default listens only on loopback. Canonical Compose overrides
that address to `:8381` so Sonarr, Radarr, Jellyfin, and the scoped proxy can
reach DarkHarrbor on `arr-net`. It publishes only the restricted client listener
as `8382:8382`; it never publishes the primary API on 8381. The setup wizard
warns whenever an explicit wildcard address is used.

Do not publish port 8381 directly to an untrusted LAN or the internet. The
qBittorrent, SABnzbd, Torznab, WebDAV, health, metrics, and diagnostic surfaces
assume a trusted operator network; several compatibility endpoints cannot add
interactive authentication without breaking Arr clients. If remote access is
required:

1. keep DarkHarrbor on its private container network;
2. publish only a TLS reverse proxy;
3. restrict the proxy by VPN, firewall allowlist, or authenticated operator
   network; and
4. preserve request paths, Range headers, and streaming responses without
   logging query strings or authorization headers.

Stream URLs contain deterministic non-expiring bearer tokens so permanent
library pointers remain playable. Treat them as credentials and never place
complete `.strm` contents or proxy access logs in support bundles.

## TorBox Provider

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_TORBOX_API_TOKEN` | (sealed) | TorBox API token |
| `HARRBOR_TORBOX_BASE_URL` | `https://api.torbox.app/v1` | API base URL |
| `HARRBOR_TORBOX_USENET` | `auto` | Usenet capability: `auto`/`on`/`off` |

## NNTP Providers

Multiple NNTP providers are supported as peers with failover. The pre-approval
plan picker preselects no provider. Enter provider names once in fallback
order; `newshosting` uses the legacy `HARRBOR_NNTP_*` fields, `torbox` can be
selected only with the TorBox account and is accepted after approval only when
the verified plan exposes News Server, and every other valid name uses the
generic family below. The complete review shows this order. Empty input or
`none` configures no direct NNTP provider and consumes no direct-provider
credential prompts; it is valid for a selected Usenet source only when the
TorBox account was selected for later capability verification.

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_USENET_PROVIDERS` | — | Comma-separated selected provider names (order = failover priority); omission selects none |
| `HARRBOR_NNTP_VERIFY_CRC` | `true` | Verify yEnc `pcrc32`/`crc32`; set false only as an emergency compatibility bypass |

Per-provider settings use the pattern `HARRBOR_PROVIDER_<NAME>_*`:

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_PROVIDER_<N>_HOST` | — | NNTP server hostname |
| `HARRBOR_PROVIDER_<N>_PORT` | `563` | NNTP server port |
| `HARRBOR_PROVIDER_<N>_TLS` | `true` | Use TLS |
| `HARRBOR_PROVIDER_<N>_USERNAME` | (sealed) | NNTP username |
| `HARRBOR_PROVIDER_<N>_PASSWORD` | (sealed) | NNTP password |
| `HARRBOR_PROVIDER_<N>_TOTAL_CONNECTIONS` | provider max | Total connection pool size |
| `HARRBOR_PROVIDER_<N>_DEMAND_CONNECTIONS` | 60% of total | Demand (playback) connections |
| `HARRBOR_PROVIDER_<N>_READAHEAD_CONNECTIONS` | 40% of total | Readahead (prefetch) connections |
| `HARRBOR_PROVIDER_<N>_MAX_CONNS_PER_FILE` | unset = uncapped | Per-file demand concurrency cap. Left unset, there is no per-file limit at all -- `16` is only the value `darkharrbor configure` suggests interactively if you touch this setting (capped at the provider's own demand-connection count), not a silent runtime default |

`HARRBOR_READAHEAD_MAX_SEGMENTS` is the runtime readahead-window control for all
providers. The legacy Newshosting-specific
`HARRBOR_PROVIDER_NEWSHOSTING_READAHEAD_MAX_SEGMENTS` is read only as a seed when
`darkharrbor configure` imports old configuration; it is not a runtime
per-provider setting. Configure auto-calculates the 60/40 demand/readahead split
when a total is changed; unchanged totals preserve the current split.

## Cache Configuration

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_NNTP_CACHE_MODE` | `readahead` (inherited) | NNTP override: `none`, `readahead`, `disk`, or `full`; unset inherits `HARRBOR_CACHE_MODE` |
| `HARRBOR_STREAM_CACHE_MODE` | `readahead` (inherited) | CDN/torrent override: `none`, `readahead`, `disk`, or `full`; unset inherits `HARRBOR_CACHE_MODE` |
| `HARRBOR_DISK_CACHE_SIZE_MB` | `2048` | Shared disk-cache size in MiB |
| `HARRBOR_DISK_CACHE_TTL_MIN` | `60` | Fresh cache-entry TTL in minutes for `disk` mode |
| `HARRBOR_FULL_EVICT_TTL_MIN` | `120` | Full/restored entry TTL in minutes when full-mode eviction is `ttl` |
| `HARRBOR_FULL_EVICT_MODE` | `ttl` | Full-mode eviction: `ttl`, `manual`, or `never` |

The shipping cache mode is `readahead`. Choose `disk` or `full` when persistent
rewatch caching justifies disk use; NNTP and stream entries share the configured
size budget under separate namespaces.

## CDN / Stream Tuning

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_STREAM_CHUNK_SIZE_MB` | `16` | CDN fetch window size (MB) |
| `HARRBOR_STREAM_MIN_BUFFER_SEGMENTS` | `2` | Segments buffered before first byte emitted |
| `HARRBOR_STREAM_READAHEAD_WORKERS` | `1` | CDN readahead parallelism (keep at 1 to avoid 429s) |
| `HARRBOR_READAHEAD_MAX_SEGMENTS` | `16` | Global readahead window |
| `HARRBOR_NNTP_CROSSLANE_SPLICE` | `false` | Proof-gated cross-lane recovery and durable owned-route admission across NNTP, torrent, and HTTP; not required for ordinary playback capture/commit |

### Performance profiles, exact values

The wizard's `recommended` / `low-memory` / `high-throughput` prompt is a
bundle-apply of exactly the cache and stream keys above
(`src/internal/wizard/performance.go`) -- there is no separate mechanism, and
nothing here that manual `darkharrbor.conf` editing can't reach directly.

| Key | `low-memory` | `recommended` (default) | `high-throughput` |
|---|---|---|---|
| `HARRBOR_NNTP_CACHE_MODE` | `readahead` | `readahead` | `disk` |
| `HARRBOR_STREAM_CACHE_MODE` | `readahead` | `readahead` | `disk` |
| `HARRBOR_DISK_CACHE_SIZE_MB` | `1024` | `2048` | `8192` |
| `HARRBOR_READAHEAD_MAX_SEGMENTS` | `8` | `16` | `32` |
| `HARRBOR_STREAM_CHUNK_SIZE_MB` | `8` | `16` | `16` |
| `HARRBOR_STREAM_MIN_BUFFER_SEGMENTS` | `1` | `2` | `4` |
| `HARRBOR_STREAM_READAHEAD_WORKERS` | `1` | `1` | `1` |

The wizard recommends (but does not force) `low-memory` when the container's
cgroup memory limit is below 2 GiB, and removes `high-throughput` from the
choices entirely -- not merely from the recommendation -- when the media
path's free space is below 10 GiB. Applying a profile always re-runs the same
validation manual tuning goes through (`config.ValidateTuning`); it is not a
separate, less-checked path.

## Selection Mode (optional Prowlarr meta-indexer)

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_PROWLARR_BASE_URL` | (sealed) | Prowlarr URL (enables selection mode) |
| `HARRBOR_PROWLARR_API_KEY` | (sealed) | Prowlarr API key |
| `HARRBOR_PROWLARR_INDEXER_IDS` | (all) | Optional indexer ID filter |

When both are set, DarkHarrbor fans searches to Prowlarr, prefilters by cache
status, and returns only releases that an enabled lane can fulfill. The arr
never sees ineligible releases.

First-run setup can derive these values only from an explicitly selected,
supported LSIO Prowlarr container through the scoped connector. It validates
the key against Prowlarr's v1 status endpoint and `darkharrbor doctor` repeats
that authenticated check. Other images and multiple simultaneous Prowlarr
upstreams are not inferred; use one manual endpoint when connector extraction
is unavailable.

**Topology:** register DarkHarrbor as `Torznab` at `/torznab/torrent-only` and
`Newznab` at `/torznab/usenet-only` in each arr (not a single combined feed —
see the protocol-lock requirement in README). Set Prowlarr app-sync to
**Disabled** for every arr.

## Arr Notification

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_ARR_NAMES` | (sealed) | Comma-separated arr instance names for queue refresh |
| `HARRBOR_ARR_<NAME>_URL` | (sealed) | Per-arr API URL |
| `HARRBOR_ARR_<NAME>_APIKEY` | (sealed) | Per-arr API key |
| `HARRBOR_ARR_<NAME>_ROOTFOLDER` | unset | Root folder used when reactive commit ADDS content here |
| `HARRBOR_ARR_<NAME>_QUALITYPROFILE` | unset | Quality profile id used when reactive commit ADDS content here |

DarkHarrbor notifies arrs to refresh their queue after cleaning a failed
download. Falls back to the arr's own ~90s polling if not configured.

`_ROOTFOLDER` and `_QUALITYPROFILE` are required TOGETHER before automatic
reactive commit may ADD content to an instance. With either missing the instance
can still be routed to manually, but automatic dispatch parks. Unset is the
shipping default.

**These two keys are the FALLBACK surface, not the intended one.** They are
read only for instances declared through `HARRBOR_ARR_NAMES`. A deployment that
accepted discovered or manually entered Arrs through `darkharrbor setup` has
them in the `arr_instances` table instead, and that table is AUTHORITATIVE: the
reactive router builds its target list from stored instances and falls back to
the environment-declared Arrs ONLY when the table has no usable target
(`internal/reactivequeue.ResolveTargets`). On a wizard-managed deployment these
keys are inert; on a legacy process-environment deployment with no stored target
they are the runtime fallback. Neither silently overrides the other. The wizard
is the intended owner of stored targets; see RD-34 D8 in the consolidated plan.

**If you decline the `docker-proxy` sidecar, choose manual Arr entry in the goal
plan.** S8 asks for each existing instance's type, name, base URL, and API key;
it authenticates to `/api/v3/system/status` before accepting the target, stores
only non-secret metadata plus the API-key reference in `arr_instances`, and
seals the key at S7. S9 and S7d then use the same target model as discovery.
The remaining manual work is:

1. In each clean Sonarr/Radarr, create its media root first under **Settings →
   Media Management → Root Folders**. The directory must be writable by that
   Arr. A brand-new Arr has no usable destination and DarkHarrbor never creates
   one.
2. Copy the Arr API key from **Settings → General → Security** and enter it only
   at S8's non-echoing prompt. Do not inline it in YAML, `darkharrbor.conf`, a
   command argument, or evidence.
3. Select the root folder and quality profile from S7d's live per-instance list.
4. Mount DarkHarrbor's data output into the Arr at exactly
   `HARRBOR_REPORTED_PATH_PREFIX`. Canonical Compose reports
   `/mnt/darkharrbor`; the Arr therefore needs read access to that same path.
   The isolated named-volume example above shows the equivalent cold-run mount.
5. If base search/grab topology was selected, install and maintain S10's
   ffprobe metadata relay in each Arr container; S10 cannot perform container
   exec without the scoped proxy. Reactive-only direct `ManualImport` needs
   neither that relay, the three base indexers, nor the two download-client
   shims.

These keys resolve through the normal precedence order; a private `env_file` is
process-environment input and needs no sealed-store entry. `arr_instances` uses
a different, wizard-owned model: its `api_key_ref` stores a sealed key **name**,
not the credential itself.

## Schema 45 — reactive destination capture
> **OPTIONAL LAYER.** Only applies when reactive commit is enabled.


Schema 45 adds `root_folder` and `quality_profile` to `arr_instances` so the
reactive router can source wizard-managed discovered or manual instances rather
than relying on environment-declared ones. Existing rows take `''` and
`0`, which read as NOT CAPTURED, so an upgrade cannot by itself cause anything
to be added to any Arr.

Rolling back to a pre-45 image is expected to work, because the added columns
are read by name and ignored by older builds, but the columns themselves remain
in the database. Take a database backup before upgrading regardless; the online
`.backup` path is WAL-safe and does not require stopping the container.

## Reactive Commit Destinations
> **OPTIONAL LAYER.** Only applies when reactive commit is enabled.


Reactive commit adds content to an arr when playback crosses the configured
threshold. Which instance receives it is determined, never inferred.

**The switch is two-stage, by design.** `HARRBOR_REACTIVE_ENABLED` gates
CAPTURE; `HARRBOR_REACTIVE_COMMIT_MODE` gates what happens to what was
captured. Both default to off, so an upgrade never starts committing on its
own.

| `ENABLED` | `COMMIT_MODE` | Behaviour |
|---|---|---|
| `false` | (any) | No capture at all. No coverage is recorded, nothing parks |
| `true` | `off` | Observes and records coverage; never adds to an Arr |
| `true` | `supervised` | Every candidate parks for a human decision |
| `true` | `auto` | Determined destinations commit; undetermined ones still park |

`HARRBOR_TOPOLOGY` must be `t3`, which is the profile that declares reactive
commit and promotion as available features. Capture also requires the HTTP
stream lane to be enabled.

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_REACTIVE_ENABLED` | `false` | Master switch for the reactive lane; invalid values are ignored with a startup warning and fall back to `false` |
| `HARRBOR_REACTIVE_COMMIT_MODE` | `off` | One of `off`, `supervised`, `auto`. `off` observes only; `supervised` parks every candidate for a human; `auto` commits determined destinations without prompting |
| `HARRBOR_REACTIVE_THRESHOLD` | `0.5` | Fraction of playback that must elapse before commit; must be greater than 0 and at most 1 |
| `HARRBOR_REACTIVE_PROMOTION_ENABLED` | `true` for legacy configurations; setup writes the reviewed choice | Enables materialized-file promotion. Declining it leaves committed entries as pointers |
| `HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT` | `10` | Free-space floor (1-99). Promotion stops when the destination filesystem drops below this |
| `HARRBOR_REACTIVE_MONITOR_EPISODES` | `true` | Monitor the played episode after adding a series |
| `HARRBOR_REACTIVE_MONITOR_MOVIES` | `true` | Monitor movies on add; set `false` to add them unmonitored like series |
| `HARRBOR_REACTIVE_DEFAULT_SERIES_ARR` | unset | Optional instance name to receive first-seen SERIES |
| `HARRBOR_REACTIVE_DEFAULT_MOVIE_ARR` | unset | Optional instance name to receive first-seen MOVIES |

Routing rules:

- **Exactly one instance of a kind** (one Radarr, or one Sonarr) -- determined.
  Commits automatically with no further configuration.
- **More than one instance of a kind** -- NOT determined. Nothing in a Stremio
  coordinate maps to a private split such as 4K/HD/kids or anime/classic/modern,
  and DarkHarrbor does not interpret what your instance labels mean. These park
  for a human decision. This is symmetric: multiple Radarr instances behave
  exactly like multiple Sonarr instances.
- **A named default** resolves the multi-instance case, because it is a
  destination you chose rather than an inference. A default naming an instance
  that is not configured, or that lacks a root folder and quality profile, is
  IGNORED so a stale name parks rather than misfiles.

A named default means captured content LANDS THERE UNLESS YOU MOVE IT. It is not
routing and does not attempt to respect your curation. Because a series is added
unmonitored a misplacement cannot trigger grabs, and the `.strm` is a stable
DarkHarrbor pointer, so relocating it to another instance later is safe.

Series are always added with `monitored=false` and only the played episode
monitored. Movie monitoring follows `HARRBOR_REACTIVE_MONITOR_MOVIES`, so movies
can be added unmonitored exactly like series if preferred.

Parked entries have no HTTP surface yet; they are reached through the
`reactive-pending` CLI. Multi-instance deployments should expect to use it
until a UI exists.

```sh
darkharrbor reactive-pending -action list          # what is parked, and why
darkharrbor reactive-pending -action destinations  # valid -root-folder / -quality-profile values
darkharrbor reactive-pending -action assign -representation <id> -arr sonarr-modern
```

`list` prints each entry's title, kind, year, park time, reason, and the exact
actions that reason permits. An entry whose authoritative identity is
unavailable prints `title=<unknown>` rather than a guess -- that is itself the
signal that it cannot be routed. `destinations` reads each Arr's live root
folders and quality profiles, so the values required when adding absent content
never have to be looked up by hand.

## Strm Mode / Docker Proxy

The recommended public installation path is `./install.sh`. It discovers only
non-secret container/network metadata, requires explicit approval, resolves
release tags to immutable image digests, validates the hardened connector policy
label, and writes `.darkharrbor/compose.yml` mode `0600`. The generated file is
gitignored and contains no credentials. It binds the restricted client listener
to `127.0.0.1:8382`; remote access requires an explicit later network decision.
The default registry repository is `ghcr.io/darkharrbor/darkharrbor`, matching
the release workflow; `DARKHARRBOR_IMAGE_REPO` is an explicit advanced override
for a mirror or operator-built registry namespace.

Set `DARKHARRBOR_HOST_PORT` to another decimal port from `1` through `65535`
before running the installer when `8382` is already occupied. The override
changes only the loopback host side of the mapping; the restricted container
listener remains `8382`, and the primary API remains unpublished.

Fresh installation uses the fixed Compose project `darkharrbor` and first proves
that its exact project-labelled containers, `darkharrbor_control` network, and
three named volumes do not exist. The install directory must be current-user
owned, real, and not group/world writable; an existing handoff path is rejected.
Once Compose is published, one shared failure guard owns cleanup for setup,
runtime-target validation, service startup, credential handoff, and signals. It
runs project-scoped `down -v --remove-orphans`, deletes only generated temporary,
Compose, and known handoff files, and retains pulled images. If Docker cleanup
fails, it preserves Compose and reports the recovery path so remaining volumes
are not hidden or orphaned silently.

For an intentional air-gapped or local source build, preload both
`$DARKHARRBOR_IMAGE_REPO:$DARKHARRBOR_VERSION` and its `-dockerproxy` companion,
then set `DARKHARRBOR_PRELOADED_IMAGES=1`. The installer skips `docker pull`,
requires both images, validates the same schema/policy labels, resolves their
immutable local image IDs, and pins those IDs. This does not confer registry
provenance; the operator is responsible for how the local images were built or
transported. The default remains registry pull plus digest pinning.

First-install completion is health-gated. After setup and permanent connector
narrowing, the installer runs Compose `up --wait` for DarkHarrbor and does not
copy recovery or optional integration handoff files to the host until the
service is healthy. Timeout or unhealthy state enters the same incomplete-install
cleanup and cannot print the success message.

Manual Compose installation remains supported below.

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_STRM_INSTALLER_ENABLED` | source/manual default `false`; public wizard enables it for discovered Arrs using normal search/grab | Enable selected-Arr startup/event ffprobe-wrapper repair |
| `HARRBOR_DOCKER_PROXY_URL` | `http://docker-proxy:2375` | Docker proxy URL for discovery/exec |
| `HARRBOR_DOCKERPROXY_TARGETS` | empty | Compose-side, comma-separated container names approved for fixed connector operations; empty denies every exec |

Before first setup, start only the `strm` profile's docker-proxy sidecar:

```sh
export HARRBOR_DOCKERPROXY_TARGETS=sonarr,radarr
docker compose --profile strm up -d docker-proxy
```

Replace the example names with the exact existing containers selected by the
operator. The allowlist is enforced again inside the connector after it
independently inspects the target image. Supported exec requests are fixed:
selected-field Arr configuration read, ffprobe resolution/install/verification,
the documented Jellyfin setup operations, and media-path visibility checks.
Arbitrary commands, unapproved targets, stopped targets, unknown request fields,
replayed or forged exec IDs, and direct container-inspect requests are denied.
Container listings discard labels and mounts; event streams retain only the
container ID, image, name, event type/action, and timestamp.

The connector is deliberately small and default-deny, but it is still a trusted
component with access to the Docker socket. Operators who do not accept that
residual host-level trust should leave the profile disabled and choose manual
Arr entry; DarkHarrbor itself never mounts the socket.
Keep the non-secret target variable set for any later Compose operation that
could recreate the connector. The public installer will persist it in a
generated, untracked deployment after the user confirms the initial targets.
That initial list is temporary setup authority. Setup emits a private, bounded
list containing only selected Arrs that need wrapper repair and the selected
managed Jellyfin instance. Before the daemon starts, the installer stops the
discovery connector, rejects any name not present in the operator's original
approval, atomically narrows `HARRBOR_DOCKERPROXY_TARGETS`, deletes the internal
handoff, and recreates the connector only if the narrowed list is non-empty.
Manual and reactive-only installations therefore do not retain a needless
socket helper unless another selected managed service requires it.
Any failure after generated Compose publication removes the entire incomplete
installer project and its new volumes, not only the temporary connector. This
includes a failed setup, a runtime target outside the original approval, daemon
startup, signal, or private-handoff copy. Cleanup is safe because exact project
objects were proven absent before publication; a cleanup failure retains the
Compose file for explicit recovery.

The reviewed onboarding plan records `wrapper-repair` separately from base Arr
wiring. Scoped discovery plus normal search/grab enables it by default and
writes `HARRBOR_STRM_INSTALLER_ENABLED=true` through the validated non-secret
tuning file. S10 and the runtime handoff use the same flag, so an empty selected
list can never mean “all Arrs.” Manual entry writes the flag false, performs no
connector-based S10 mutation, and reports wrapper installation/maintenance as
externally pending. Doctor reports any plan/runtime mismatch.

After S7 has created the sealed store, a profile-wide `docker compose --profile
strm up -d` is safe.

## Wizard Stages

`darkharrbor setup` finishes with separate `enabled`, `skipped`, `degraded`,
`failed`, and `externally pending` lines plus exact doctor and reconciliation
commands. It never describes an intentionally skipped integration as passed.
Lettered stages are full prompts, not sub-steps, and each one can be skipped
where marked.

**Stage IDs are stable labels, not a running order.** They are fixed references
used across the project's records, so they are not renumbered when the sequence
changes. The wizard prints the actual order at S1; as of 2026-08-25 it is:

```
S1, S2, S2b, S3, S3b, S4, S8, S5, S6, S9, S7, S7c, S7b, S7d, S10, S10b, S11, S12
```

Arr DISCOVERY (S8) runs early so every later stage knows which instances exist.
Arr REGISTRATION (S9) cannot: it needs the credentials generated at S6.

| Stage | Name | Notes |
|---|---|---|
| S1 | Preflight | `/config` write probe, listen-address warning, existing-config detection |
| S2 | TorBox + plan self-config | Optional — press Enter to skip; when supplied, detects plan level, capabilities, and slots |
| S2b | Real-Debrid | Optional -- press Enter to skip |
| S3 | AllDebrid | Optional -- press Enter to skip |
| S3b | Premiumize | Optional -- press Enter to skip |
| S4 | Usenet providers | Credentials and validation for the pre-approved Newshosting, selected-account TorBox News Server, and/or custom order; TorBox remains verified-plan contingent |
| S5 | Acquisition preference | Interactive lane ordering; writes `HARRBOR_PREFERENCE` |
| S6 | Internal secrets | Generates qBit password, SAB API key, stream secret, Stremio install token |
| S7 | Seal + write config | Atomically writes `secrets.sealed` plus the configured key-file path; PRINTS THE SEAL PASSWORD ONCE |
| S7b | Scheduled backups | |
| S7c | Stremio client edge | Sets `HARRBOR_STREMIO_EDGE_MODE` and client origin |
| S7d | Reactive library | Enables the lane and captures per-instance destinations from live Arr data; when no reachable Arr is found (including when S8 was skipped), writes `HARRBOR_REACTIVE_ENABLED=false` and `HARRBOR_REACTIVE_COMMIT_MODE=off` to the tuning file without prompting |
| S8 | Arr connection + key extraction | Scoped discovery or authenticated manual Sonarr/Radarr entry; optional Prowlarr is selected independently |
| S9 | Arr auto-registration | Wires only selected lanes: up to two download clients and three direct-lane indexers; reactive-only use skips this stage. The public installer defers this mutation until DarkHarrbor is healthy, then requires `reconcile-arr` to succeed so Arr's live client test reaches the service. |
| S10 | Shim install | Installs the ffprobe metadata relay into exactly the selected discovered Arrs and enables persistent repair; manual base topology reports this externally pending, while reactive-only direct `ManualImport` skips it |
| S10b | Jellyfin setup | ffmpeg wrapper, bitrate limit, env verification |
| S11 | Validate + finalize | |
| S12 | Public-domain acceptance demo | Explicit opt-in; requires an ID-capable HTTP backend and Radarr. Re-runnable as `darkharrbor demo` |

Doctor diagnostics run automatically after S12, before the
`bootstrap_complete` marker is written.

## Tuning Reference

`darkharrbor configure` writes the fields it manages interactively and preserves
other valid allowlisted values. These are non-secret runtime settings; defaults
are correct for most deployments.

**The table below is a selected reference, NOT the allowlist.** It documents the
keys that have no other home in this file. The complete allowlist is larger, and
most of its keys are documented in their own sections above -- including every
key required by the OPTIONAL `t2`/`t3` layer. If you are looking for where to
set topology, the HTTP stream lane, or reactive commit, the answer is that all
of them are accepted here, in `/config/darkharrbor.conf`.

### The complete allowlist

48 literal keys, plus two per-instance families. Anything not on this list is
ignored on read with a key-only warning, which is also how credential keys such
as `HARRBOR_MEDIAFLOW_PASSWORD` are kept out of this file -- they are not
allowlisted, so they never take effect from it. The file uses `KEY=VALUE`
format (one entry per line; blank lines and `#` comments are ignored).
`darkharrbor configure` is the intended editor; direct edits are accepted
provided every key is on the allowlist. Source of truth: `tuningKeys`,
`providerTuningSuffixes` and `httpBackendTuningSuffixes` in
`src/internal/config/tuning.go`.

**Topology** -- `HARRBOR_TOPOLOGY`, `HARRBOR_TOPOLOGY_ACHIEVED_STALENESS_HOURS`

**Reactive commit (`t3`)** -- `HARRBOR_REACTIVE_ENABLED`,
`HARRBOR_REACTIVE_COMMIT_MODE`, `HARRBOR_REACTIVE_THRESHOLD`,
`HARRBOR_REACTIVE_MONITOR_EPISODES`, `HARRBOR_REACTIVE_MONITOR_MOVIES`,
`HARRBOR_REACTIVE_DEFAULT_SERIES_ARR`, `HARRBOR_REACTIVE_DEFAULT_MOVIE_ARR`,
`HARRBOR_REACTIVE_PROMOTION_ENABLED`, `HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT`

**HTTP / aggregator lane (`t2`+)** -- `HARRBOR_HTTP_STREAM_ENABLED`,
`HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES`, `HARRBOR_HTTP_BACKENDS`,
`HARRBOR_MEDIAFLOW_BASE_URL`, `HARRBOR_MEDIAFLOW_PUBLIC_IP`

**Acquisition** -- `HARRBOR_PREFERENCE`, `HARRBOR_GOVERNOR_UNCACHED_BUDGET`,
`HARRBOR_GOVERNOR_STALL_TIMEOUT_MIN`,
`HARRBOR_GOVERNOR_DEAD_SENTINEL_GRACE_MIN`,
`HARRBOR_TORRENT_SUBMIT_BUDGET_MIN`, `HARRBOR_TORRENT_KEEPWARM_DAYS`,
`HARRBOR_SUPPRESSION_TTL_HOURS`, `HARRBOR_SEARCH_BUDGET_CAP`,
`HARRBOR_SEARCH_BUDGET_SUPPRESS_DAYS`

**NNTP** -- `HARRBOR_NNTP_CONNECTIONS`, `HARRBOR_NNTP_STRIPE`,
`HARRBOR_NNTP_CROSSLANE_SPLICE`, `HARRBOR_NNTP_PREWARM_BYTES`,
`HARRBOR_NNTP_CACHE_MODE`

**Cache and streaming** -- `HARRBOR_STREAM_CACHE_MODE`,
`HARRBOR_DISK_CACHE_SIZE_MB`, `HARRBOR_DISK_CACHE_TTL_MIN`,
`HARRBOR_FULL_EVICT_MODE`, `HARRBOR_FULL_EVICT_TTL_MIN`,
`HARRBOR_STREAM_CHUNK_SIZE_MB`, `HARRBOR_STREAM_MIN_BUFFER_SEGMENTS`,
`HARRBOR_STREAM_READAHEAD_WORKERS`, `HARRBOR_READAHEAD_MAX_SEGMENTS`,
`HARRBOR_READAHEAD_SEEK_WIDEN_MS`

**Client edge and strm** -- `HARRBOR_STREMIO_EDGE_MODE`,
`HARRBOR_STREMIO_CLIENT_BASE_URL`, `HARRBOR_STRM_INSTALLER_ENABLED`

**Backups and logging** -- `HARRBOR_BACKUP_ENABLED`, `HARRBOR_BACKUP_TARGET`,
`HARRBOR_BACKUP_INTERVAL_HOURS`, `HARRBOR_BACKUP_KEEP`, `HARRBOR_LOG_LEVEL`

**Per-instance families.** `HARRBOR_HTTPBACKEND_<name>_` accepts the suffixes
`TYPE`, `URL`, `PUBLIC_URL`, `REDIRECT_ORIGINS` and `DESCRIPTORS_FILE`. `HARRBOR_PROVIDER_<name>_`
accepts `TOTAL_CONNECTIONS`, `DEMAND_CONNECTIONS`, `READAHEAD_CONNECTIONS`,
`MAX_CONNS_PER_FILE`, `CONNECTIONS` and `RETENTION_DAYS`. A token-bearing or
otherwise sensitive backend URL must be omitted here and entered through
`darkharrbor configure` so the sealed value is selected.

**These keys are also settable from the process environment**, and the wizard
writes several of them itself -- `HARRBOR_TOPOLOGY`, `HARRBOR_REACTIVE_ENABLED`
and `HARRBOR_REACTIVE_COMMIT_MODE` are set by stage S7d. Remember the precedence
order at the top of this document: this file wins over the process environment,
so a value set in Compose is overridden by the same key here.

### Selected reference

| Variable | Default | Description |
|---|---|---|
| `HARRBOR_MEDIAFLOW_BASE_URL` | *(empty)* | Client-facing HTTP(S) origin for generated MediaFlow playback URLs; enter an origin only, with no path, query, fragment, or credentials |
| `HARRBOR_MEDIAFLOW_PUBLIC_IP` | *(empty)* | Optional literal public egress IP for `/proxy/ip`; omit it to use bounded runtime discovery |
| `HARRBOR_GOVERNOR_STALL_TIMEOUT_MIN` | `30` | Minutes an uncached torrent may sit in a provider-reported stalled/no-swarm state. Never blacklists -- later swarm recovery stays possible |
| `HARRBOR_GOVERNOR_DEAD_SENTINEL_GRACE_MIN` | `5` | Minutes TorBox's own dead-sentinel state is tolerated before it is treated as terminal. Deliberately shorter than the stall timeout |
| `HARRBOR_TORRENT_SUBMIT_BUDGET_MIN` | `15` | Minutes a submitted source may go without transferring ANY bytes before it is treated as blacklist-worthy. Stronger signal than a stall, so it is a separate budget |
| `HARRBOR_TORRENT_KEEPWARM_DAYS` | `20` | Recent-play window making a Ready torrent a keep-warm candidate. `0` disables the keep-warm janitor entirely |
| `HARRBOR_SUPPRESSION_TTL_HOURS` | `72` | How long a suppressed release stays suppressed |
| `HARRBOR_SEARCH_BUDGET_CAP` | `5` | Maximum searches per item before the budget suppresses further attempts |
| `HARRBOR_SEARCH_BUDGET_SUPPRESS_DAYS` | `7` | Days an item stays suppressed after exhausting its search budget |
| `HARRBOR_NNTP_CONNECTIONS` | `8` | Default per-provider NNTP connection count when no per-provider override is set |
| `HARRBOR_NNTP_STRIPE` | `false` | Round-robin healthy NNTP providers per segment (NS-8.4) |
| `HARRBOR_NNTP_PREWARM_BYTES` | `16777216` | Leading-byte prewarm budget after manifest resolution (16 MiB) |
| `HARRBOR_READAHEAD_SEEK_WIDEN_MS` | `0` | Milliseconds NNTP readahead stays widened to the pool ceiling after a seek. `0` keeps width pinned at the ceiling always, identical to pre-NS-5.5 behaviour |

## Environment-Only Settings

The keys below are read **only from the environment**. They are not on the
`darkharrbor.conf` allowlist documented above, so putting them in that file
does nothing at all — they are ignored on read with a key-only warning, and the
default silently stays in force. That failure is quiet, which is exactly why
they are listed here.

Set them in the `environment:` block of your deployment's Compose file
(`.darkharrbor/compose.yml` for an installer deployment). `install.sh
--upgrade` rewrites only the two `image:` lines and passes every other line
through unchanged, so anything you add here survives upgrades.

Defaults are correct for almost every deployment, and the three performance
profiles already pick sensible values for the pool sizes. Reach for these when
you have a specific symptom, not as routine tuning — several of them trade
playback latency against background work, and the wrong value is worse than the
default.

### NNTP connection pool

The pool is split: `TOTAL` is the whole budget, and `DEMAND` and `READAHEAD`
divide it between playback and prefetch. Your provider's own concurrent
connection limit is the real ceiling — exceeding it gets connections refused,
not queued.

- **`HARRBOR_NNTP_TOTAL_CONNECTIONS`** — default `16`
  Total NNTP connection pool size, demand plus readahead. Raise it only as far
  as your provider actually allows; a plan permitting 50 connections is the
  argument for raising it, a slow stream is not.
- **`HARRBOR_NNTP_DEMAND_CONNECTIONS`** — default `8`
  Reserved for on-demand playback fetches. This is the one that governs how
  fast a seek recovers, since a seek cannot be served by prefetch.
- **`HARRBOR_NNTP_READAHEAD_CONNECTIONS`** — default `8`
  Used by the prefetch goroutine. Raising it makes sequential playback smoother
  on high-latency providers; raising it at the expense of `DEMAND` makes seeks
  worse. Tune the pair together, not individually.
- **`HARRBOR_NNTP_WARM_CONNS`** — default `1`, `0` disables
  Connections kept authenticated and idle per configured provider, so the first
  segment of a play skips a fresh dial, TLS handshake and `AUTHINFO`. The cost
  is one held connection per provider even while idle; set `0` if you are at
  your provider's connection limit and would rather spend that slot on a fetch.

### NNTP transport

- **`HARRBOR_NNTP_PIPELINE_DEPTH`** — default `4`, `1` disables pipelining
  Maximum `BODY` commands in flight on a single authenticated connection.
  Values above `32` are clamped with a startup warning. Pipelining is the
  cheapest latency win against a distant provider; set `1` if a provider
  mishandles pipelined requests, which usually presents as corrupt or
  interleaved segment data rather than an error.
- **`HARRBOR_NNTP_NEGCACHE_TTL_MIN`** — default `60`, `0` disables
  How long a segment known to be missing is remembered, so repeat demand for a
  dead segment short-circuits instead of spending another `BODY` on a certain
  failure. In-memory and per-process: a restart forgets, costing one re-fetch
  per segment. Lower it only if you expect a provider to backfill articles it
  previously reported missing.
- **`HARRBOR_NNTP_HOST`**, **`HARRBOR_NNTP_PORT`**, **`HARRBOR_NNTP_TLS`**
  Connection settings for the primary direct-NNTP provider. The wizard collects
  these and seals them, so setting them by hand is a deliberate override; see
  [Configuration Sources](#configuration-sources-precedence-highest-first) for
  which source wins.

### NNTP provider ordering

- **`HARRBOR_NNTP_LAGMODEL_YOUNG_HOURS`** — default `24`, `0` or negative disables
  Article age below which the lag model orders providers by observed
  age-at-first-success rather than static preference. Propagation delay is real
  and provider-specific, so for very fresh posts the fastest provider is not
  the one you would have picked. Disabling reverts to static preference order.
- **`HARRBOR_NNTP_LAGMODEL_MIN_SAMPLES`** — default `3`, values below `1` clamp to `1`
  Observed samples a provider needs before its average is trusted for that
  ordering. Raise it if a provider's early samples are unrepresentative and you
  are seeing it promoted or demoted too eagerly.

### NNTP stream-time repair

Both budgets bound the same operation: rebuilding a damaged region mid-stream
from recovery data instead of failing the play. They exist because that repair
is an unbounded read behind a live stream if nothing stops it.

- **`HARRBOR_NNTP_REPAIR_BUDGET_MB`** — default `256`, `0` disables repair
  Byte ceiling for one stream-time repair. Setting `0` restores the older
  behaviour where exhausting the recovery ladder simply errors.
- **`HARRBOR_NNTP_REPAIR_BUDGET_SECONDS`** — default `45`, below `1` disables
  Wallclock ceiling for one repair. Exceeding it abandons the attempt and
  returns the original error unchanged. Lower it if you would rather fail fast
  than have a player stall while a repair runs.

### Background audits

Three separate passes, all sampling rather than fully verifying — a complete
verification would cost as much as downloading the library. `grab health` runs
once at grab time on a tight budget, `health audit` sweeps the Ready library
slowly and forever, and `identity audit` is a cheap database-only pass.

Turning any of them off is a legitimate choice on a constrained connection
budget; you lose early warning, not correctness.

- **`HARRBOR_HEALTHAUDIT_ENABLED`** — default `true`
  Gates the whole periodic pass to a no-op.
- **`HARRBOR_HEALTHAUDIT_INTERVAL_SEC`** — default `60`
  Cadence between audited items — one per minute. Combined with your Ready NNTP
  library size this sets the full-sweep period; the design target is 7 days or
  longer. Raise it if the audit's background `STAT`s compete with playback on a
  tight connection budget.
- **`HARRBOR_HEALTHAUDIT_SAMPLE_SIZE`** — default `8`
  `STAT` samples drawn per audited item. Fewer samples is cheaper but noisier,
  and noise here has consequences — see `RESEARCH_ENABLED` below.
- **`HARRBOR_HEALTHAUDIT_COMPLETENESS_THRESHOLD`** — default `0.95`
  Fraction of conclusively-sampled segments that must confirm present for an
  item to stay healthy. The default sits below `1.0` deliberately, to leave
  headroom for PAR2 recovery: an item missing a little is still repairable and
  should not be called dead.
- **`HARRBOR_HEALTHAUDIT_MAX_DEAD_REGIONS`** — default `64`
  Caps how many distinct dead regions are persisted per item, so a pathological
  all-dead item cannot grow its map without bound across many passes.
- **`HARRBOR_HEALTHAUDIT_RESEARCH_ENABLED`** — default `true`
  **This is the consequential one.** When a pass finds an item newly decayed,
  DarkHarrbor blocklists that NZB's content key, records a suppression
  fingerprint, and marks the item failed so the owning Arr re-searches it. Set
  `false` to make decay detection purely measurement — the audit still records
  what it found, but nothing is blocklisted or re-searched. Worth doing if you
  suspect sampling noise is condemning healthy items.
- **`HARRBOR_HEALTHAUDIT_RESEARCH_COOLDOWN_MIN`** — default `60`
  Minimum minutes before the same item may trigger another re-search. Mostly
  defence in depth: a failed item leaves the audit rotation on its own, so under
  normal operation this is never tested.

- **`HARRBOR_GRABHEALTH_ENABLED`** — default `true`
  Gates the at-grab-time sample. This is what stops an already-dead release
  being accepted, queued, and left permanently unimported.
- **`HARRBOR_GRABHEALTH_SAMPLE_SIZE`** — default `4`
  Deliberately half the periodic pass's `8`, because this one runs inside the
  grab accept path where the budget is roughly a second, not the periodic
  pass's looser tolerance.
- **`HARRBOR_GRABHEALTH_TIMEOUT_MS`** — default `1500`
  Bounds the whole sample pass, leaving headroom under the accept handler's own
  2-second ceiling. A timeout mid-sample degrades to whatever conclusive samples
  were already collected — never a hard failure — so lowering it trades
  confidence for accept latency rather than risking a rejected grab.

- **`HARRBOR_IDENTITYAUDIT_ENABLED`** — default `true`
  Gates the periodic identity pass. The on-demand HTTP trigger ignores this
  flag: asking for a pass explicitly is always honoured.
- **`HARRBOR_IDENTITYAUDIT_INTERVAL_SEC`** — default `300`
  Slower than the health audit's 60s on purpose — this pass is in-memory and
  database-only, so there is no provider cost pressure forcing a faster cadence
  and no benefit to one.

### Prewarm

Prewarm fetches leading bytes before anyone presses play, so the first seconds
of a title are already local. It is the single biggest lever on
time-to-first-frame, and the single easiest way to waste provider quota on
titles nobody watches.

- **`HARRBOR_PREWARM_ENABLED`** — default `true`
  Gates the whole service to a no-op across every lane.
- **`HARRBOR_PREWARM_HOTHEAD_MB`** — default `32`
  Leading megabytes of a just-resolved item pinned at grab time, ahead of any
  real play request. Raising it buys a faster start on titles you do watch and
  costs bandwidth on titles you do not; the honest question is what fraction of
  your grabs actually get played.
- **`HARRBOR_PREWARM_TIMEOUT_SEC`** — default `45`
  Bounds one prewarm or seek-target prefetch call regardless of the caller's own
  context, so a slow provider cannot leave prewarm running indefinitely behind
  the scenes.
- **`HARRBOR_PREWARM_MIN_READAHEAD_WORKERS`** / **`HARRBOR_PREWARM_MAX_READAHEAD_WORKERS`**
  — defaults `1` and `4`
  Bound the adaptive readahead recommendation. Paired with the torrent CDN
  capacity default below; raising the maximum without raising that capacity
  just produces workers that queue.
- **`HARRBOR_PREWARM_NEXTEP_THRESHOLD`** — default `0.85`
  Playback fraction past which next-episode prewarm would fire. **Currently
  inert**: no lane yet reaches an end-of-playback position signal into this
  code path, so the value is exercised only by tests. Documented because it is
  settable and reads as if it works; changing it has no effect today.

### Readahead

- **`HARRBOR_READAHEAD_ENABLED`** — default `true`
  Toggles prefetching. Ignored in `full` cache mode, which always prefetches.
- **`HARRBOR_READAHEAD_MIN_BUFFER_SEGMENTS`** — default `4`
  Segments to prefetch before playback is allowed to start. This is a direct
  trade: raising it delays the first frame but reduces the chance of an early
  stall on a slow provider.
- **`HARRBOR_READAHEAD_TAIL_EVICT`** — default `true`
  Evicts buffered segments that fall behind the lookback window. Disabling it
  keeps everything already fetched, which helps repeated short seeks backwards
  at the cost of unbounded buffer growth on a long title.
- **`HARRBOR_READAHEAD_TAIL_LOOKBACK`** — default `4`
  Segments retained behind the current position when tail eviction is on.
  Raise it if your player makes frequent small backward seeks.

### Governor

The governor paces work against providers so a burst never trips rate limiting
or account flags. Capacities here are concurrency ceilings, not rate limits.

- **`HARRBOR_GOVERNOR_HTTP_CAPACITY`** — default `0` (unbounded)
  Bounds concurrent HTTP-lane operations across the three classes that share
  it: live playback, grab-time preflight, and startup prewarm. Set a positive
  value if an HTTP source rate-limits you. The catch is that all three then
  share that one budget, so too low a value throttles playback in order to
  protect prewarm.
- **`HARRBOR_GOVERNOR_TORRENT_CDN_CAPACITY`** — default `4`
  Concurrent debrid-CDN fetch operations. The paired default for the prewarm
  worker bounds above.
- **`HARRBOR_GOVERNOR_ALTPROVIDER_FAILOVER`** — default `true`
  Allows bounded mid-stream failover to an alternate provider holding the same
  content hash, rather than failing the play. Set `false` if you do not want a
  second provider account touched by playback recovery at all — even bounded
  and read-mostly.
- **`HARRBOR_GOVERNOR_REPAIR_ENABLED`** — default `true`
  Gates torrent-lane self-repair: same-provider re-add, then alternate-provider
  re-add, then blocklist and re-search. Set `false` to make a stream-time
  exhaustion terminal immediately, with no automatic second attempt.
- **`HARRBOR_GOVERNOR_REPAIR_COOLDOWN_MIN`** — default `30`
  Minimum minutes before the same torrent identity may be repaired again. This
  exists to stop a readahead storm — many concurrent chunk failures on one
  broken item — from re-triggering repair or re-blocklisting over and over.

### HTTP stream lane

- **`HARRBOR_HTTP_IA_FILE_PREFERENCE`** — default `derivative`
  Which Internet Archive file variant to prefer. `derivative` picks the
  transcoded copy, which is smaller and far more likely to play directly;
  choose the original only if you specifically want the source encode and can
  live with formats a player may refuse. An unrecognised value falls back to
  `derivative`.
- **`HARRBOR_HTTP_PREDICTIVE_RESOLVE_ENABLED`** — default `true`
  Resolves the next upstream URL slightly before it is needed, so a redirect or
  token refresh does not stall playback at the moment of use.
- **`HARRBOR_HTTP_PREDICTIVE_RESOLVE_MIN_LEAD_MS`** /
  **`HARRBOR_HTTP_PREDICTIVE_RESOLVE_MAX_LEAD_MS`** — defaults `5000` and `60000`
  How far ahead that resolve may run. Too small and the resolve lands too late
  to help; too large and you burn resolves on positions the viewer never
  reaches, which matters when the upstream rate-limits resolution.
- **`HARRBOR_HTTP_SELFHEAL_ENABLED`** — default `true`
  Lets the HTTP lane re-resolve a source that has gone bad mid-stream instead of
  failing the play outright.
- **`HARRBOR_HTTP_SELFHEAL_COOLDOWN_MIN`** — default `60`
  Minimum minutes before the same source is self-healed again, so a permanently
  broken source cannot drive a re-resolve loop.

### ffprobe relay

The relay runs a network-capable `ffprobe` on behalf of media servers that need
to inspect a `.strm` target. It is a real fan-out risk — an Arr retrying an
import can hammer it — so every bound here exists to contain that.

- **`HARRBOR_RELAY_CONCURRENCY`** — default `8`
  Simultaneous `ffprobe-full` invocations. Excess requests queue rather than
  fanning out unbounded.
- **`HARRBOR_RELAY_TIMEOUT_SEC`** — default `45`
  Maximum duration of a single invocation.
- **`HARRBOR_RELAY_QUEUE_TIMEOUT_SEC`** — default `20`
  How long a request waits for a concurrency slot before being rejected with
  `503`. Rejecting is deliberate: a queued probe that outlives its caller is
  pure waste.
- **`HARRBOR_RELAY_MAX_BODY_BYTES`** — default `65536`
  Request body cap, enforced before the concurrency semaphore is touched.
- **`HARRBOR_RELAY_NEGATIVE_TTL_MIN`** — default `10080` (7 days), `0` disables
  How long a *deterministic* probe failure — ffprobe ran and exited non-zero —
  is replayed from cache with no upstream byte movement. This is what bounds
  the provider cost of an Arr's infinite import-retry against a broken release.
  Failures where the process could not run at all are never cached.
- **`HARRBOR_FFPROBE_FULL_PATH`** — default `/usr/local/bin/ffprobe-full`
  Where the network-capable static binary is staged inside the image. Change
  only if you have restaged it yourself.

### Item lifecycle

- **`HARRBOR_CLEANUP_HOURS`** — default `8`
- **`HARRBOR_RECONCILE_INTERVAL_SEC`** — default `300`
  Cadence of the reconcile pass that re-checks state against the Arrs.
- **`HARRBOR_READY_AUTO_REMOVE_SEC`** — default `1800`
  How long a Ready item lingers before it is automatically removed from the
  download-client view. Too short and an Arr may not have polled yet.
- **`HARRBOR_NEVER_PLAYED_UNCACHED_HOURS`** — default `24`
  How long an uncached item that has never been played is kept before cleanup
  reclaims it.
- **`HARRBOR_FAILED_RETENTION_MIN`** / **`HARRBOR_FAILED_RETENTION_HOURS`**
  — default `5` minutes
  How long failed items are retained before pruning, in whichever unit you
  prefer. Raise it if you want longer to inspect failures via `deadletter`.
- **`HARRBOR_FAILED_PRUNE_INTERVAL_SEC`** — default `120`
  How often that prune runs.

### Cache and probing

- **`HARRBOR_DISK_CACHE_PATH`**
  Where the disk cache lives inside the container. Relevant only in `disk` or
  `full` cache mode.
- **`HARRBOR_PINNED_BUDGET_MB`** — default `1024`
  Ceiling on pinned (hot-head and keep-warm) bytes. This is the budget prewarm
  and keep-warm compete for; raising `PREWARM_HOTHEAD_MB` without raising this
  simply causes earlier eviction.
- **`HARRBOR_PROBE_BYTES`** — default `1048576` (1 MiB)
  Bytes fetched for a media probe. Lower it only if probes are expensive on
  your provider and you accept less reliable format detection.

### Torrent availability

- **`HARRBOR_TORRENT_AVAILABILITY_TTL_HOURS`** — default `72`
  How long a recorded availability observation is treated as current. Provider
  cache state changes over time, so a stale observation should not be trusted;
  lower it if you see grabs accepted against content that has since fallen out
  of the provider's cache.
- **`HARRBOR_TORRENT_DECAYAUDIT_ENABLED`** — default `true`
  Gates the torrent-side decay audit.

### Provider plan overrides

- **`HARRBOR_PROVIDER_TORBOX_PLAN_SLOTS`**, **`HARRBOR_PROVIDER_TORBOX_PLAN_MAX_BYTES`**
  Override the concurrency slots and size ceiling normally discovered from your
  TorBox plan. Detection is reliable, so these exist for the case where it is
  wrong or unavailable — an override that is *higher* than your real plan will
  produce provider-side rejections, not extra capacity.

### Connector and strm mode

- **`HARRBOR_DOCKERPROXY_LISTEN`**, **`HARRBOR_DOCKERPROXY_SOCKET`**
  Listen address and Docker socket path for the scoped connector. Defaults suit
  the generated Compose; change them only alongside it.
- **`HARRBOR_STRM_WRAPPER`**
  Marker used to identify the installed ffmpeg wrapper, so DarkHarrbor can
  recognise and maintain its own shim rather than overwriting someone else's.
- **`HARRBOR_STUB_AUTHORITY`** — default `db`
  Which side is authoritative for on-disk stub and `.strm` state. `db` means
  the database wins and the startup consistency sweep restores files from it.
  Changing this disables that healing.

### Diagnostics and miscellaneous

- **`HARRBOR_METRICS_ENABLED`** — default `true`
  Serves the Prometheus `/metrics` endpoint.
- **`HARRBOR_ERRORJOURNAL_MAX_ENTRIES`** — default `20`
  Entries kept in the error journal surfaced by `/healthz`.
- **`HARRBOR_IDENTITY_STRICTNESS`** — default `warn`
  How hard identity mismatches are enforced. `warn` records without blocking.
- **`HARRBOR_DOCTOR_TIMEOUT_SECONDS`**
  Overall bound on a `doctor` run. Raise it if you have many providers and Arrs
  and the run is being cut short.
- **`HARRBOR_DEMO_TIMEOUT_SECONDS`**
  Bound on the `demo` command's end-to-end run.
- **`HARRBOR_TUNING_FILE`**
  Overrides the path to `darkharrbor.conf`. Exists mainly for tests and unusual
  mounts; normal deployments should leave it alone.
- **`HARRBOR_SAB_NZB_KEY`**
  A sealed credential rather than a tuning value — the SABnzbd-compatibility
  NZB key. Set it through the wizard, not by hand.

### Retired keys

- **`HARRBOR_DIRECT_STREAM`** — **removed and ignored.** Setting it produces a
  startup warning and nothing else. DarkHarrbor always proxies stream bytes
  now, so there is no direct-stream mode to select.

## Subcommands

Every command below runs inside the container. The installer offers to place a
`darkharrbor` command in `~/.local/bin` so you can invoke them directly —
`darkharrbor doctor` rather than the full
`docker compose -f ./.darkharrbor/compose.yml exec darkharrbor darkharrbor doctor`.
The deployment path is baked in when it is generated, so it works from any
directory, and it falls back to a disposable container when the deployment is
stopped. Arguments are forwarded verbatim, including flags and pipes.

Set `DARKHARRBOR_INSTALL_CLI=1` (or `0`) to decide without a prompt, which is
what headless `--answers-file` runs need; `DARKHARRBOR_CLI_DIR` overrides the
destination. A non-interactive run that sets neither installs nothing. For a
hand-written Compose deployment, `scripts/darkharrbor` in the repository is the
same wrapper — point it at your file with `DARKHARRBOR_COMPOSE`. Install-level
operations (`--upgrade`, `--restore`, `--snapshots`, `--uninstall`) belong to
`install.sh`, not this command.

Verbatim output of `darkharrbor help` (also `-h`, `--help`; source:
`printCLIUsage`, `src/cmd/darkharrbor/main.go`), reproduced here rather than
paraphrased so this table can't quietly drift from what the binary actually
prints:

```
DarkHarrbor -- media automation bridge

usage: darkharrbor <command> [flags]
       darkharrbor            (no command: run the daemon)

Setup and configuration
  setup                 Interactive bootstrap wizard (S1-S12); --force rotates credentials
  catalog import        Validate and atomically install a private generic-source catalog from stdin
  configure             Runtime editor; --existing-aiostreams connects an existing stack
  seal                  Seal KEY=VALUE lines from stdin into an encrypted secrets file
  reconcile-arr         Re-apply DarkHarrbor client/indexer registration to the Arrs

Diagnostics
  doctor                Full health report; -json for machine output. Exit 1 on any FAIL
  demo                  Zero-account demo: grab and play one public-domain movie

Reactive library
  reactive-pending      List and decide parked entries (-action list|destinations|
                        approve|assign|jellyfin-only|remove|acknowledge|undo)
  reactive-commit       Commit one representation directly
  reactive-undo         Undo a previous reactive commit
  reactive-promote      Promote a representation to a materialized path, or remove it
  monitor-flip          Flip Arr monitoring for committed content

Maintenance
  backup                List/create snapshots, or export/import encrypted backup bundles
  restore               Restore from a DarkHarrbor backup
  readopt               Audit existing .strm pointers against restored state
  deadletter            Inspect the dead-letter queue (list|requeue ITEM_ID|clear)

Run `darkharrbor <command> -h` for that command's flags.
```

`backup`, `restore`, and `readopt` each cover more ground than one line can
— see [Backup and Restore](#backup-and-restore) above for the individual
`backup snapshot`/`list`/`verify`/`export`/`import` subcommands, the
`restore` primitive, and `readopt`'s exact guarantees, rather than a second,
separately-maintained description here.

Run `darkharrbor <command> -h` yourself for any command's exact current
flags — this index is stable, but flags are not documented separately here
and can be added without a corresponding CONFIGURATION.md update.

An unknown subcommand prints this index to stderr and exits 2; it no longer
falls through to daemon startup. A bare `darkharrbor` with no argument is still
the daemon entrypoint.
