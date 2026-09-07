# DarkHarrbor `.strm` mode — ffprobe wrapper + installer

Status: **scripts implemented and proven end-to-end** (see "Verification" below).
Remaining: DarkHarrbor Go-side bootstrap to inject these into arr containers
automatically (manual/one-shot install works today via `strm-init.sh`).

## What this is

In `.strm` (no-mount) delivery mode, an Arr's library entry is a `.strm` text
file whose contents are a DarkHarrbor `/stream` URL. Current Sonarr/Radarr
recognize `.strm` as a media extension and can record the pointer without media
metadata, which is why reactive-only direct `ManualImport` does not require
this integration. Stock Arr `ffprobe` still cannot parse the pointer's HTTP(S)
target. These scripts provide real media metadata where the base Arr topology
selects that integration:

- **`ffprobe-wrapper.sh`** — a drop-in `ffprobe` placed at `/app/<app>/bin/ffprobe`.
  When the probe input is a real `.strm`, it extracts the first http(s) URL and
  reroutes the probe to a network-capable ffprobe (`ffprobe-full`) with
  `-protocol_whitelist file,http,https,tcp,tls` (+ probesize/analyzeduration/
  rw_timeout tunables). For any other input it passes straight through to the
  arr's original ffprobe (backed up as `ffprobe.real`). Never uses `set -e`;
  propagates the child exit code.
- **`strm-init.sh`** — root installer, idempotent, always exits 0. Provisions a
  network-capable `ffprobe-full`, then marker-guarded replaces each
  `/app/*/bin/ffprobe` with the wrapper (backing up the original once;
  re-backs-up after an image update restores the stock binary). Designed to run
  via LinuxServer.io `/custom-cont-init.d` at every boot, or on demand via
  `docker exec`.

The arr only ever talks to **DarkHarrbor** (the `.strm` URL points at DH, never
TorBox), so probing is cheap and never resets debrid retention: DH serves the
**cached header bytes** and the real ffprobe produces the JSON. This is the
thing a bare `.strm` could not do, and it keeps the probe faithful (real
ffprobe output, not synthetic JSON).

## D-LICENSE decision (resolved)

DarkHarrbor is **MIT**. The reference for this technique, STRMProbe
(`github.com/cosmicflow2512/STRMProbe`), is **GPL-3.0**; copying its source into
DarkHarrbor would force DH's entire distribution to GPL-3.0, contradicting the
MIT license, the all-permissive dependency slate, and the project NOTICE.

**Decision: do not copy STRMProbe. These scripts are a clean-room
reimplementation written from a behavioral specification only** (the technique —
read a URL from a `.strm`, run a network-capable ffprobe against it — is an
unprotectable idea; only literal code is copyrightable, and a shim this small
has essentially one obvious form). No third-party source was incorporated. DH
stays MIT.

`ffprobe-full` is **not bundled** (it is GPL/LGPL ffmpeg). The installer
provisions it at deploy time and never commits it to the repo:
1. `$HARRBOR_FFPROBE_FULL` if set → 2. staged `$STAGE/ffprobe-full` →
3. one already on `$PATH` → 4. native package manager (`apk` on Alpine/musl
arr images, `apt` on Debian/glibc) → 5. best-effort static download (BtbN).
The arr containers here are LinuxServer.io **Alpine/musl**, so path 4 (`apk add
ffmpeg`) is what runs, yielding a libc-matched network-capable ffprobe.

## Deployment (intended)

DarkHarrbor, after arr discovery, mounts/copies this `strm-mode/` dir into each
arr container as `$HARRBOR_STRM_STAGE` (default `/opt/harrbor-strm`), drops
`strm-init.sh` into the container's `/custom-cont-init.d/` for persistence, and
runs it once via `docker exec` for immediate effect. (DH has host/docker access
via the homelab connector.) Jellyfin needs no wrapper — it understands `.strm`
and its bundled ffmpeg is already network-capable.

Manual one-shot (any arr container):
```
docker exec --user root <arr> mkdir -p /opt/harrbor-strm
docker cp strm-mode/. <arr>:/opt/harrbor-strm/
docker exec --user root <arr> bash /opt/harrbor-strm/strm-init.sh
```

## Verification (2026-06-27)

Proven in a throwaway Alpine/musl container on `arr-net`, against the live DH
`/stream` for a real item (Skyfall, 4K HEVC MKV, 64 streams), with **no
production arr container mutated**:
- installer apk-sourced a musl network-capable ffprobe, wrapped the target,
  backed up stock → `.real`, exit 0;
- wrapped ffprobe on the `.strm` returned **duration=8590.262000,
  nb_streams=64** (exact ground truth) via `-i`, via bare positional, and in
  FFMpegCore-style JSON form;
- non-`.strm` input correctly passed through to `ffprobe.real`.

This depends on the rangecache throughput fix (cached header served at CDN
speed); before that fix a probe through DH would have stalled.

## Update (2026-06-27)

- **Production-proven in Radarr** (uid 1000): wrapped ffprobe on a real library
  `.strm` returns exact duration+streams via DH `/stream`. Install is currently
  ephemeral (reverts on container recreate).
- **`ffprobe-full` chosen production source: a single host-staged musl-static
  ffprobe** (one file, no apk, persistent, instant). `apk add ffmpeg` works but
  is heavy (157 MiB) and re-pulls every ephemeral boot, so it becomes a fallback.
  The installer already prefers `$HARRBOR_FFPROBE_FULL` / `$STAGE/ffprobe-full`.
- **Install needs `--user root`** on arr containers whose app runs as non-root
  (e.g. Radarr = uid 1000); Sonarr exec is already root. The custom-cont-init.d
  boot path runs as root regardless.
- Full session log + open items: `../docs/SESSION-2026-06-27-throughput-strm-mode.md`.

## Update (2026-06-28) — ffprobe-full finalized

- **`ffprobe-full` is now a single host-staged fully-static ffprobe**
  (`mwader/static-ffmpeg`, ffprobe 8.1.2, `static-pie`), regenerable via
  `fetch-ffprobe-full.sh` (the binary is gitignored). No apk, runs on Alpine/musl,
  resolves DNS. Production Radarr was cut over (apk ffmpeg removed, 157 MB
  reclaimed); the `.strm` probe still returns exact metadata.
- The installer already prefers `$STAGE/ffprobe-full`, so mounting `strm-mode/`
  into an arr container as `/opt/harrbor-strm` is all that's needed to use it
  (no apk/download). That mount + running `strm-init.sh` at boot is the remaining
  persistence step.

## Update (2026-06-29) — persistent boot install (implemented)

Persistence is wired for the sonarr trio via **compose bind-mounts + an LSIO
`/custom-cont-init.d` launcher** (`strm-cont-init.sh`, new in this dir). Per arr
container:

```yaml
volumes:
  # shared, read-only — one 134 MB host copy serves all arr containers
  - /srv/docker/homelab/darkharrbor/strm-mode:/opt/harrbor-strm:ro
  # boot hook dir (sonarr already mounts this; radarr needed it added)
  - /srv/docker/homelab/configs/<arr>/.../custom-cont-init.d:/custom-cont-init.d
```

Then drop `strm-cont-init.sh` into that host `custom-cont-init.d` dir as
`10-harbor-strm-init.sh` (executable). At every start LSIO runs it as root, it
execs `/opt/harrbor-strm/strm-init.sh`, which links the staged `ffprobe-full` and
installs the wrapper — idempotent, survives recreate.

Notes:
- LSIO `init-custom-files` does **not** require root ownership of the hook script
  (only the execute bit), but it runs as PID1's uid — so a service pinned to
  `user: "1000:1000"` runs the hook as 1000 and the root-level install is skipped.
  **radarr's `user:` override was removed** (PUID/PGID=1000 still drops the app);
  the sonarr trio already starts as root.
- prowlarr ships no `/app/*/bin/ffprobe` → not wired.
- Compose lives outside this repo at `/srv/docker/homelab/compose/docker-compose.yml`.
