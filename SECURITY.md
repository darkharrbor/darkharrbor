# Dark Harrbor Security Policy

## Supported Versions

The latest release is supported for security updates.

## Reporting a Vulnerability

Please report security vulnerabilities responsibly:

1. **Do not** open a public issue
2. Contact the maintainers directly
3. Allow reasonable time for a fix before public disclosure

## Security Model

### Provider Secrets

Provider credentials (TorBox, NNTP, qBittorrent, SABnzbd) are never stored in
plaintext in the repository or container image. They are:

- Sealed at rest using an argon2id KDF + AES-256-GCM
- Loaded at startup from the sealed store
- Never logged or exposed in API responses

**Sealing secrets.** Use the `darkharrbor seal` subcommand to encrypt
env-style `KEY=VALUE` lines into the sealed store:

    darkharrbor seal --file /config/secrets.sealed < secrets.env

The sealed store is configured and unlocked at startup via these variables:

- `HARRBOR_SECRETS_FILE` — path to the sealed store (e.g. `/config/secrets.sealed`)
- `HARRBOR_SECRETS_UNLOCK` — unlock mode: `env`, `keyfile`, or `prompt`
- `HARRBOR_SECRETS_KEY_FILE` — path to the key file when unlock mode is `keyfile` (new installer deployments use `/run/darkharrbor-key/secrets.key`)

In `env` mode the password is supplied via `HARRBOR_SECRETS_PASSWORD`; in
`keyfile` mode it is read from the file named by `HARRBOR_SECRETS_KEY_FILE`
(store it with `0600` permissions and never commit it); in `prompt` mode it is
read interactively at startup. Interactive credential prompts disable terminal
echo through the terminal API and fail before reading if echo cannot be safely
controlled; redirected input remains available for deliberate automation. See
`.env.example` for a complete example.

Secret and key paths are fail-closed. DarkHarrbor rejects symlinks (including
symlinked parent components), non-regular files, unexpected ownership,
group/world-readable files, writable parent directories, empty files, and
oversized files. Reads use `O_NOFOLLOW`; setup and updates use mode-`0600`,
fsync-backed atomic replacement. Setup normalizes its application-owned config
and key directories to mode `0700`.

Automated `setup --answers` files may contain credentials and therefore use the
same bounded, owner-checked, `O_NOFOLLOW` read boundary. Loose permissions,
unsafe or symlinked parents, ownership mismatches, oversized content, and file
replacement races are rejected before any answer is replayed.

Generic HTTP/WebDAV descriptor catalogs use that same private-file boundary.
They may contain signed or otherwise sensitive source URLs even when no URL is
stored in runtime tuning. Keep them under `/config` with owner-only permissions;
never place them in Compose, Git, logs, chat, or a publicly mounted directory.
Catalog publication rejects unknown or case-aliased fields, duplicate keys,
non-object entries, and non-array top-level values before replacing the prior
file, preventing ambiguous JSON from changing which source URL is authoritative.
Enumerated M3U, Metalink, and WebDAV representations are pinned by opaque
content-derived selectors, not mutable list offsets. Reordering an upstream
listing cannot substitute another file behind an existing stable pointer;
legacy positional selectors fail closed because their original member identity
was never persisted and cannot be inferred safely. A descriptor whose selected
identity is duplicated abstains during search; duplication introduced after a
grab remains an exact-match ambiguity and also fails closed.

An optional bounded `source_id` lets a generic descriptor rotate an expiring
URL, CDN, mirror, or provider without putting that location in its persisted
identity. It is unique within the catalog and remains bound to the descriptor's
declared media identity. Named M3U entries and Metalink metadata similarly
separate media identity from transport. Cross-lane fallback still requires
verified byte evidence—security hardening must not disable fallback, but
fallback must never mean accepting an unproven different file.

The public installer stores `secrets.sealed` and `secrets.key` on separate
Docker volumes. Both are mounted only into DarkHarrbor and its one-shot setup
container. The canonical manual Compose file retains `/config/secrets.key` for
backward compatibility until an explicit migration workflow is shipped.
Installer setup sends generated recovery, MediaFlow, and tokenized Stremio
values only through mode-`0600` files in a mode-`0700` handoff directory. After
health succeeds, the host copies only the three exact allowlisted names—not the
directory—and rejects absent required recovery material, empty or oversized
files (`recovery.key` is capped at 4 KiB), symlinks, non-regular objects, or
unexpected ownership. Files are staged
under a private temporary directory and the complete validated directory is
atomically published before the exact temporary sources are deleted from the
key volume. It reports paths, never values. Operators must move retained
recovery material to an independent vault and delete the local handoff copies
after use.

Installer upgrades recognize only a regular, same-user, mode-`0600`,
non-symlinked Compose file with the fixed installer schema marker. The upgrade
rewrites only the two image fields to verified immutable digests and validates
the complete candidate before atomic replacement; it never imports an arbitrary
Compose project. Recovery material must be confirmed first. A failed health
wait stops the candidate instead of automatically launching an older image
against state the candidate may already have migrated.

`DARKHARRBOR_PRELOADED_IMAGES=1` is an explicit local-provenance boundary for
air-gapped/source-build use. It never pulls, accepts only both expected local
tags with the required installer and connector-policy labels, and converts the
mutable tags to immutable daemon-local image IDs before Compose publication.
It does not claim registry authenticity; default installs still require
registry-resolved digests.

Tag-driven registry publication has a separate fail-closed verification job
with read-only repository permission. It must pass release/installer security
invariants, formatting, build, vet, ordinary repository tests, and the full
race suite before the image job receives package-write permission. The
checked-in CycloneDX inventory is
compared exactly with `go list -mod=readonly -m all`; any missing, stale, or
version-mismatched module prevents publication. The tag without its leading
`v` must also equal the SBOM application version. Published installer inputs
are still resolved to immutable registry digests before generated Compose is
written.
CI, release verification, image-build checkout, and scheduled aggregator
compatibility also disable persisted checkout credentials. Only the image job
receives package-write permission, used by its explicit registry login.

The Docker build context excludes `.git`, `.env`, and every `.env.*` file.
Release verification pins those exclusions and rejects a broad `COPY .` in the
Dockerfile. Images copy only declared source/module inputs, preventing a
developer's ignored credential files from entering an intended release layer.

The confirmation covers only the independently retained unlock key. The
installer does not trust an operator assertion that a database backup exists:
it invokes `darkharrbor backup snapshot` against the current running release and
refuses to rewrite the Compose file or restart a service unless the online,
atomically published snapshot succeeds. Candidate images may already have been
downloaded and inspected, but that does not mutate the deployed application.

Scheduled `/backup` snapshots contain the database and already-encrypted
`secrets.sealed`, but never the unlock key. The database snapshot itself is not
an encrypted export artifact and must remain local/private. `darkharrbor backup
export` converts a complete snapshot into a mode-`0600` `.dhbackup` bundle using
Argon2id plus chunked AES-256-GCM under the existing recovery key. The key is
never included or accepted as a command-line value. Header fields, every chunk,
and the terminal marker are authenticated; import rejects wrong keys, tampering,
truncation, appended data, unsafe files, unbounded content, and unexpected
archive paths before publishing a validated local snapshot.

Local restore applies the same private-file boundary to both snapshot members:
owner-only regular files, safe non-symlinked parents, bounded reads,
`O_NOFOLLOW`, and opened-file identity verification. It copies both members to
private staging and runs SQLite integrity checks against the staged database,
so pathname replacement cannot make restore validate one database and install
another. The shared integrity checker also holds a securely opened identity
guard for encrypted export/import validation. Any restore rejection occurs
before either live config file is replaced.

Advisory lock files use the same parent, owner, type, mode, `O_NOFOLLOW`, and
opened-file identity checks. This covers the daemon/restore exclusion lock,
manual-versus-scheduled snapshot serialization, and paced monitor-flip
serialization. A symlink or group/world-accessible pre-created lock fails closed
instead of redirecting or splitting the protected operation.

Installer-managed restore adds orchestration, not a second restore parser. It
accepts only the canonical snapshot basename grammar and exact matching
confirmation, never a host/container path or credential. Before any runtime
mutation, a one-shot `backup verify` validates the exact snapshot database and
uses the mounted owner-private key file to authenticate and parse its sealed
store. It reports only a fixed OK or generic failure and clears mutable key and
plaintext buffers after use. Wrong-key, tamper, format, private-file, or
database failure therefore occurs before a safety snapshot or stop. When the
daemon is running, an online safety snapshot must then succeed before any stop.
Main and the Docker-socket connector are stopped before the one-shot existing
restore command runs against the exact `/backup/<name>` directory. Restore
repeats its checks, restore rejection never starts services, and success reuses
pinned-image, connector-aware, health-gated `--start`. The installer never
prints or replaces the unlock key and does not replace non-secret tuning, images,
or the media library.

Snapshot discovery is a separate read-only boundary. `backup list` validates a
real non-symlink backup root, accepts only canonical timestamp directory names,
ignores symlink, foreign, and incomplete entries, and emits basenames only in
sorted order. It dispatches before configuration/key loading and exposes no
paths, file metadata, database contents, or validation errors. A listed name is
only a structural candidate; restore still owns all permission, identity, size,
and SQLite-integrity decisions. `backup verify` is the separate, explicitly
key-aware preflight: it accepts the recovery material only as a private file,
never as a flag value, and exposes no snapshot path, key, secret, decrypted
field, or detailed cryptographic failure.

Snapshots do not contain the media library. Restore preserves the stable item
IDs and exact authoritative pointer URLs in SQLite; it does not infer provider
state from library filenames or pointer IDs. `readopt` only compares a mounted
read-only library with restored authority and emits sanitized relative paths and
item IDs. Its SQLite connection uses `mode=ro` plus `query_only`; it never prints
pointer bodies, contacts pointer hosts, or invents missing database rows.
Filesystem opens are confined beneath the supplied root, discovered symlink
entries are skipped, and each `.strm` is opened with `O_NOFOLLOW` plus an
identity recheck. The standard deployment does not mount final media libraries
into DarkHarrbor, so audit them only through an explicit temporary read-only
bind. This keeps total-loss recovery from turning permanent bearer URLs or broad
persistent mounts into unsafe reconstruction channels.

### Stream Tokens

`.strm` URLs contain deterministic HMAC-signed bearer tokens. Tokens are:

- generated using `HARRBOR_STREAM_SECRET` (minimum 32 characters);
- bound to one item/file identity and validated on every request; and
- intentionally non-expiring so permanent library `.strm` files remain playable.

They are not single-use credentials. Anyone who obtains a complete `.strm` URL
can replay it while that stream secret remains active. Rotating the stream
secret invalidates every existing `.strm` token, so perform rotation only with
a planned library-pointer regeneration/re-adoption procedure.

### Container Hardening

- Non-root user (uid 1000)
- Read-only rootfs
- All capabilities dropped
- No-new-privileges security option
- Primary API restricted to the internal network; only the separate, restricted
  client listener is mapped to loopback host port 8382 by default. The installer
  may select a different loopback host port without changing that exposure.

### Scoped Arr Automation

DarkHarrbor never receives the Docker socket. The optional connector exposes a
small allowlisted API and accepts execution only for explicitly approved
container names and exact policy-owned operations. The first discovery preview
prints only supported service names and types. When several Sonarr/Radarr
instances are visible, setup records an explicit subset before extracting API
keys; registration, sealed Arr entries, wrapper installation, startup repair,
and container-event repair are restricted to those configured names. Manual
authenticated Arr entry remains available with the connector disabled.
Exec start requests are bounded to 1 KiB, reject unknown or trailing JSON, and
must remain attached with TTY disabled; invalid starts do not consume the
proxy-issued one-use exec ID.
Sanitization of the Docker container-list response is capped at 8 MiB before
JSON decoding, so an unexpectedly large daemon response fails closed rather
than causing unbounded connector memory growth.
The fixed wrapper-write operation removes only its exact temporary entry before
decoding, so shell redirection cannot follow a stale temporary symlink; it then
atomically replaces the approved ffprobe path.
Optional Jellyfin automation applies the same rule to its ffmpeg/ffprobe
wrappers and rejects symlinked wrapper/configuration parents or files before
reading or changing `system.xml`.
Multiple running Jellyfin instances require an explicit reviewed selection;
discovery order never chooses a mutation target, and an absent selected target
fails rather than falling back to another instance.

Wrapper repair is a reviewed plan outcome, not an inference from connector
reachability. It is enabled only when the operator selected scoped discovery
and normal Arr search/grab wiring. The same flag controls initial S10 mutation,
runtime event repair, and the narrowed connector target handoff. Manual entry
forces it off, preventing an empty selected-name list from expanding an install
to every discoverable Arr.

Prowlarr selection is a separate one-instance boundary. With multiple visible
instances, the operator chooses exactly one before any Prowlarr exec. The
connector's fixed command emits only `Port` and `ApiKey`, setup verifies the key
against `/api/v1/system/status`, and only the chosen URL/key enter the sealed
store. Unselected Prowlarr configuration is not read; manual mode also rejects a
second Prowlarr before requesting its credential.

Public installation treats the broader pre-wizard target approval as temporary
setup authority. After setup, the installer stops the connector before it
processes the private bounded target handoff, intersects every returned name
with the operator's original approval, rejects attempted expansion, atomically
narrows the generated Compose allowlist, and deletes the handoff. A connector
with no remaining managed target is removed. This limits long-running Docker
execution authority to services whose selected features still require it.
Fresh install first rejects existing exact project containers, the internal
project network, named config/backup/key volumes, and a pre-existing handoff
path. A shared post-publication failure guard removes the incomplete project and
its secret-bearing volumes after wizard, validation, startup, signal, or
handoff-copy failure. It deletes only current installer-owned paths; if Docker
cleanup fails, it retains Compose and reports the residue instead of silently
orphaning secrets.

Before publication, the installer verifies the operator-selected media bind
using the already resolved immutable image and its fixed non-root
`1000:1000` identity. The ephemeral probe has no network, a read-only root,
no capabilities, `no-new-privileges`, and a fixed `test -w` command. It reads no
media file and creates nothing. Failure precedes Compose and volume creation;
the installer never escalates privileges or automatically changes host
ownership or modes to force access.

The final first-install start is a credential-publication boundary. Compose
must report DarkHarrbor healthy before the installer copies recovery or optional
integration handoff files from the key volume to the host. An unhealthy or
timed-out service triggers project-and-volume cleanup, produces no host handoff,
and cannot be reported as a completed installation.

Uninstall is equally fail-closed. Its default is cancellation, and the retain
path never passes `-v`. Permanent state deletion requires the exact
`DELETE DARKHARRBOR` phrase and targets only an inventory-derived set of the
three expected volumes after both Compose project and volume-key labels match.
External networks, bind-mounted media, images, and private handoff files are
outside automatic deletion. Any ownership, inventory, runtime-removal, or
volume-removal error preserves generated Compose so remaining state stays
identifiable and recoverable.

Retained-state start does not infer connector need from discovered containers
or silently enable Docker-socket authority. It parses exactly one private
generated allowlist line: empty starts only DarkHarrbor, while a valid non-empty
list enables the scoped profile. Invalid or duplicate state starts nothing.
Images must already exist locally (`--pull never`), health is mandatory, and a
failed attempt stops only services that invocation newly started.

### Network Posture

- Plain HTTP on internal Docker network (arr-net)
- TLS for provider egress (TorBox HTTPS, NNTP TLS)
- Authenticated provider account data is treated as untrusted capability input.
  Unknown plan codes fail closed to conservative limits and a warning; they do
  not unlock paid features or become a guessed Free-tier recommendation.
- Reverse proxy recommended for external access
- Credential-free rclone/WebDAV inputs must remain on a dedicated trusted
  container network; DarkHarrbor does not make an unauthenticated listener safe
  to publish
- Generic WebDAV listings are bounded to `Depth: 1`; returned media hrefs must
  remain on the descriptor's exact origin and beneath its normalized collection
  path. A listing cannot widen its operator-declared network/path authority.
- The optional real-rclone integration fixture binds only a test-selected
  non-loopback private address, serves a temporary directory read-only, uses no
  provider credentials, and terminates the child process during cleanup. It is
  not evidence that an unauthenticated WebDAV listener is safe to publish.
- The companion generic-WebDAV resolve test asserts that neither the upstream
  origin nor its coordinates enter `items.file_list` or the `.strm`; only an
  opaque selector/file ID and a signed DarkHarrbor stream URL persist. Its
  in-process WebDAV server is test mechanics, not a trusted-network exception.

## Threat Model

The trust boundary is the Docker network and host. DarkHarrbor protects provider
credentials, Arr credentials, signed stream URLs, source locations, NZB/message
identities, media metadata, and the integrity of bytes returned to a player.
It assumes the host, container runtime, configured Arr/Jellyfin services, and
private `arr-net` peers are trusted. A malicious indexer result, provider
response, archive, manifest, media file, or unauthenticated network client is
not trusted.

Primary controls are sealed secrets, bounded parsers and reads, outbound SSRF
policy, redirect refusal where authority could change, proof-gated cross-source
recovery, HMAC-signed stream URLs, log/output redaction, non-root execution,
read-only rootfs, and dropped capabilities. These controls do not protect a
compromised Docker host, a stolen seal key, a trusted peer that records bearer
URLs, provider-side account compromise, or traffic exposed through an
incorrectly configured public reverse proxy.

Configured backend redirects are same-origin by default, preventing a trusted
backend URL from transferring its private-network authority elsewhere. A
backend may receive a bounded, per-instance allowlist of additional exact
metadata origins; entries cannot contain credentials, paths, queries,
fragments, or wildcards, and authority is never shared between backend clients.
During configuration, a network-probed backend redirect is blocked first; only
its scheme and authority are surfaced for explicit approval. The path, query,
userinfo, and signed URL never enter output or the tuning file. Declining does
not contact the target. Generic catalogs do not receive pretend discovery from
their file-only health check and require explicit origins when needed.
DarkHarrbor strips credentials, cookies, custom headers, and `Referer` before
every allowed cross-origin hop; only bounded range and content-negotiation
headers survive. The separate media-source policy may follow a CDN origin under
the same header hygiene and dial-time source-address validation.
HTTPS-to-HTTP downgrade is always denied.

### Unauthenticated Compatibility Surface

Torznab, qBittorrent/SAB compatibility, WebDAV, health, metrics, and diagnostic
routes are designed for trusted service-to-service use. Some are deliberately
unauthenticated because the corresponding Arr protocol cannot carry a separate
DarkHarrbor login. Canonical Compose publishes only the restricted client
listener as 8382:8382; it never publishes the primary API on 8381. Do not map
port 8381 directly to an untrusted network.

For remote operator access, keep DarkHarrbor private and expose only a TLS
reverse proxy restricted by VPN, firewall allowlist, or an authenticated
operator network. Preserve byte-range streaming and never log query strings,
authorization headers, cookies, or `.strm` bodies. Stream tokens are persistent
bearer credentials until the stream secret is rotated; TLS prevents passive
collection but does not make a leaked token harmless.

### Telemetry

DarkHarrbor contains no phone-home analytics, crash reporter, or hosted
telemetry exporter. Local health and Prometheus metrics remain inside the
operator's deployment. Outbound connections are limited to sources and
services the operator configures.
