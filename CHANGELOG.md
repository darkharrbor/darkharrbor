# DarkHarrbor Changelog

## v1.0.0 — 2026-09-07

First public release.

DarkHarrbor sits between an Arr stack (Sonarr, Radarr, Prowlarr) and whatever
media sources an operator points it at. It acts as indexer and download client
at once: a grab lands in the library immediately as a small `.strm` pointer,
and the real bytes are fetched and streamed only when the title is played,
then cached for rewatches.

### Acquisition

- Provider-neutral source selection with explicit operator-declared priority
  and automatic fallback between configured providers.
- Torrent via debrid: TorBox, Real-Debrid, AllDebrid, Premiumize.
- Usenet: direct NNTP with multi-provider failover, and TorBox's cached NZB
  lane.
- Internet Archive public-domain acquisition, no account required.
- OMSS-compatible self-hosted streaming backends.
- Typed operator-declared generic sources: plain HTTP, HLS, M3U, metalink,
  S3-style objects, WebDAV collections, and fixed URLs. No search and no
  upstream credentials are involved; the operator declares exactly what exists.
- Multi-season pack splitting into synthetic per-season releases, so a season
  pack an Arr would otherwise reject as a duplicate imports cleanly.
- Optional cache-aware selection mode fans searches out to the operator's own
  Prowlarr indexers and returns only releases DarkHarrbor has a lane to
  fulfill. The default remains fulfillment-only.

### Identity and recovery

- `.strm` pointers carry a permanent, self-healing identity that does not
  depend on any upstream URL remaining valid.
- Cross-provider recovery is accepted only when the bytes themselves prove the
  content is identical, never on a title or size resemblance.
- Cross-lane repair reconstructs damaged or missing data mid-stream, including
  from the recovery data bundled with real usenet releases, verified before any
  byte is served.
- Transient backend outages and rate limits fail closed without being treated
  as permanent decay or arming destructive re-search.

### Setup and integration

- `darkharrbor setup` discovers existing Sonarr, Radarr, Prowlarr, and Jellyfin
  instances, reads each one's configuration directly from its own container,
  and verifies credentials before use.
- Root folders and quality profiles are read from each Arr's live settings;
  indexers and download clients are registered automatically. DarkHarrbor
  presents as qBittorrent and SABnzbd, so no special client type is needed.
- TorBox plan tier is read from the account and concurrency limits are set to
  match. NNTP credentials are verified up front against the lanes requested.
- Three performance profiles (Recommended, Low Memory, High Throughput),
  selected from detected hardware or chosen explicitly.
- `install.sh` resolves digest-pinned images from the public organization
  namespace, validates their security-policy labels, and fails closed on
  ambiguous targets, unsafe paths, origins, redirects, or secret storage.
  Supports `--answers-file` headless setup, `--catalog` first-install catalog
  staging, `--upgrade`, and `--restore`.
- The wizard installs and maintains the ffmpeg wrapper Jellyfin needs. The
  playback mechanism is not Jellyfin-specific: anything that reads `.strm`
  files and HTTP range requests works.

### Reactive library

- With a self-hosted Stremio-compatible aggregator, playing a title that is not
  yet in the library adds it and hands ownership to the appropriate Arr once a
  configurable watch threshold is passed.
- Frequently replayed titles can be promoted from a re-resolved pointer to a
  fully downloaded, owned copy.
- Ships a first-party Stremio addon for browsing an existing library directly.

### Operations

- A provider governor paces request volume so bursts do not trigger provider
  rate limiting. Stalled transfers are given real recovery time before being
  written off, on every lane.
- A keep-warm janitor asks providers to keep recently watched copies ready, and
  handles cleanup when they age out.
- `darkharrbor doctor` checks every configured provider, Arr, and path mapping.
- Prometheus `/metrics` exposes connection pool health and per-source
  reliability.
- Crash-consistent backup snapshots plus a separate portable encrypted bundle.
  The recovery key is deliberately never included in either.
- Multi-architecture amd64 and arm64 images with published SBOMs.

### Security

- Provider credentials are sealed at rest with argon2id and AES-256-GCM, and
  are never present in the image, the Compose file, or git. Keys never leave
  the sealed store, so a compromised helper process cannot exfiltrate them.
- HMAC-signed stream tokens.
- Hardened container: read-only root filesystem, all capabilities dropped,
  runs as a non-root user.
- Same-origin guards on provider redirects and preflight, path-traversal
  prevention, SSRF mitigation on outbound requests, and integer overflow guards
  on archive parsing.
- No telemetry. The only outbound connections are to the providers and services
  the operator configured.
