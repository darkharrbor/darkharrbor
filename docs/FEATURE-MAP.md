# Feature Map — What You Can Adopt, and What Each Costs

Depends on [`CONFIGURATION-INVENTORY.md`](CONFIGURATION-INVENTORY.md), which
classifies the individual settings; this document is about which **features**
you can take and what each one obliges you to run.

DarkHarrbor is not one feature. Most of it is separately adoptable, and you
should expect to run a subset. This document exists so that choosing a subset is
an informed decision rather than a discovery made three faults later.

---

## Topology tiers

`HARRBOR_TOPOLOGY` is the coarse switch. It names an exact base capability
profile; promotion is independently optional inside `t3` and setup writes its
reviewed enable switch.

| Tier | Aggregated discovery | Universal proxy | Reactive commit | Promotion |
|---|---|---|---|---|
| `t1` | — | — | — | — |
| `t2` | yes | yes | — | — |
| `t3` | yes | yes | yes | available (opt-in) |

An unknown value falls back to `t1` with a startup warning rather than failing.

---

## Feature catalogue

### Arr integration (the classic workflow)
**Tier:** any. **Requires:** Sonarr and/or Radarr; the three protocol-locked
indexers and two client shims the wizard registers at S9.
**Standalone:** yes — this works with nothing else on this page.
**Declining it:** you lose the search-in-Sonarr workflow entirely. Also note the
routing consequence below.

### Arr ffprobe metadata relay
**Tier:** any. **Requires:** base Arr search/grab topology; S10 can auto-install
it only through the scoped Docker proxy. It lets an Arr obtain real media
metadata from a DarkHarrbor `.strm` pointer. It is NOT either of the qBittorrent
or SABnzbd download-client shims registered at S9.
**Reactive-only:** not required. Current Sonarr/Radarr recognize `.strm` as a
media extension, and the direct `ManualImport` path can record the pointer
without ffprobe metadata. The documented disposable Radarr fixture therefore
skips S9, S10, and the Docker proxy.

### NNTP / usenet lane
**Tier:** any. **Requires:** at least one usenet provider account and
`nntp_nzb` in `HARRBOR_PREFERENCE`.
**Standalone:** yes.
**Costs:** a connection budget you must divide deliberately. Per-provider pools
are set with `HARRBOR_PROVIDER_<NAME>_*` and **override the global defaults**.
`doctor` reports the global defaults and each effective per-provider pool
separately.
**First run:** no provider is preselected. A selected Usenet source requires a
direct provider or selected TorBox account before the sole mutation approval.
Enter Newshosting, selected-account TorBox News Server, and/or custom names once
in desired failover order and verify that order in the complete review. After
approval, only those providers produce credential prompts; TorBox News Server
must still pass verified-plan capability checking. If verification leaves no
cached-NZB or direct-NNTP capability, setup fails rather than silently removing
the approved Usenet source.
**Declining it:** no usenet sources. Nothing else breaks.

### Torrent / debrid lane
**Tier:** any. **Requires:** one supported debrid account.
**Standalone:** yes.
**Declining it:** no torrent sources; the aggregator's debrid results become
unresolvable.

The preference tokens `torbox_torrent`, `uncached_torrent`, and
`uncached_torrent_derank` have legacy names, but the runtime torrent walk is
provider-neutral across `HARRBOR_PROVIDERS`. TorBox, Real-Debrid, AllDebrid, and
Premiumize have independent runtime adapters. Only `torbox_nzb` and TorBox
plan/slot/News features are actually TorBox-specific.

Setup now validates all selected debrid providers before S5. Any verified
TorBox, Real-Debrid, AllDebrid, or Premiumize account unlocks the
provider-neutral torrent choice; only TorBox plan/News features remain
TorBox-specific. TorBox and Real-Debrid have each passed a real-account
independent acceptance run; AllDebrid and Premiumize remain experimental
pending an available account to run the same path.

**Adapter comparison, as last verified against each provider's own API**
(not a promise — see [CONFIGURATION-INVENTORY.md](CONFIGURATION-INVENTORY.md)'s own caution that the
wizard derives no quota/concurrency/tier behavior from anything but a live
account call; your account's own numbers are the real authority, not this
table):

| | Cache oracle before submit | Slot tracking | Client rate limit |
|---|---|---|---|
| TorBox | Yes (torrent + NZB) | Yes (live + 30s cache) | 300/min + 60/hr uncached + 10/min burst |
| Real-Debrid | No (disabled provider-side) | Yes (100-slot ceiling on Premium) | 250/min |
| AllDebrid | No | Yes (fixed 30-slot ceiling) | 12/sec + 600/min |
| Premiumize | Yes | No (falls back to rolling-window governor budgeting) | None published — server-fed `Retry-After` holds only |

### HTTP stream acquisition lane
**Tier:** any. **Requires:** at least one explicitly configured Internet
Archive, OMSS, typed generic HTTP/WebDAV, or Stremio-compatible backend.
**Standalone:** yes. At `t1` it participates in ordinary Arr search/grab/import
without an aggregator, debrid account, or NNTP account. Generic first setup can
stage its private mode-`0600` catalog with `install.sh --catalog`; other HTTP
backends use the wizard's hidden URL input. **Declining it:** torrent and NNTP
acquisition remain available when selected; aggregator playback and reactive
commit have no HTTP route.

### Aggregator proxy lane
**Tier:** `t2`+. **Requires:** a self-hosted aggregator and, for the documented
AIOStreams arrangement, a self-hosted shared tier. See the reachability rules —
they change with the proxy posture. **Standalone:** no. This optional lane is
the substrate for reactive commit; it is not required by ordinary HTTP stream
acquisition.

### Reactive commit (play-to-library)
**Tier:** `t3`. **Requires:** the aggregator proxy lane, the setup-generated
`HARRBOR_MEDIAFLOW_PASSWORD` mirrored into the aggregator
(empty disables the proxy surface entirely), `HARRBOR_REACTIVE_ENABLED=true`,
a commit mode other than `off`, at least one Arr with a captured destination and
read access to DarkHarrbor's reported `.strm` path, **and the aggregator proxying
the lanes you care about**.

For the documented AIOStreams path, also install the DarkHarrbor Library addon
inside AIOStreams and configure DarkHarrbor's AIO backend from that saved user's
Direct Manifest URL (without `/manifest.json`), not from the bare AIOStreams
origin. The addon observation supplies the playback identity used for safe Arr
routing.
**Standalone:** no.

> **This is the feature that only LOOKS optional in its parts.** Coverage is
> recorded solely for bytes DarkHarrbor delivers, so the aggregator's
> proxied-service filter is not a tuning preference — it decides which lanes can
> commit at all. A named filter looks perfectly healthy and silently limits
> commit to those services. Empty means everything.
>
> Two lanes can never commit regardless: an aggregator's own built-in usenet
> engine, and NzbDAV/AltMount outside the aggregator's built-in proxy. Both are
> hardcoded to bypass external proxies.
>
> P2P/magnet can never commit either — there is no HTTP body to observe.

**Declining it:** everything else still works. This is the top of the stack.

### Promotion
**Tier:** `t3`. **Requires:** reactive commit; explicit
`HARRBOR_REACTIVE_PROMOTION_ENABLED=true`; free-space floor via
`HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT`.
**Declining it:** committed entries stay pointers and are re-resolved on each
play.

### First-party Stremio addon
**Tier:** any. **Requires:** a client-reachable restricted listener and an
install token.
**Standalone:** yes for direct library playback. **AIOStreams t3 exception:**
it is also the identity-observation hop and must be installed as an AIOStreams
addon even if the operator does not intend to browse DarkHarrbor's existing
library directly. Declining it outside that path changes nothing; declining it
inside the documented AIOStreams reactive path leaves playback routes unbound
and prevents a safe commit proposal.

### Jellyfin integration
**Tier:** any. **Requires:** reachable Jellyfin (wizard S10b).
**Standalone:** yes. **Declining it:** skip S10b; nothing else changes.
Official tagged and digest-pinned images are recognized. Automated wrapper and
bitrate configuration fails closed on symlinked Jellyfin configuration paths.
Multiple instances require one exact pre-mutation selection.

### Prowlarr meta-indexer (cache-aware selection mode)
**Tier:** any. **Optional.** Note this is DarkHarrbor's **own** use of Prowlarr.
It is NOT how the Arrs get indexers — Prowlarr app-sync must stay **disabled**,
because it re-pushes proxied indexers that conflict with the protocol-locked
topology.

With it configured, DarkHarrbor fans a search out to your own Prowlarr
indexers, then filters the results down to only what it can actually serve —
cached, not blacklisted, not suppressed, season-correct — before Sonarr/Radarr
ever sees a result, so the Arr can never grab something DarkHarrbor can't
fulfill. This includes: real multi-season-pack splitting (detects a
"S01-S05"/complete-series pack and synthesizes a legitimate single-season
release so Sonarr can import one season out of it cleanly); retention-based
deranking of NZBs older than your configured providers' retention window
(shown, not hidden, so you know it's stale); the same derank-not-hide
treatment for uncached torrents, tagged `DH-UNCACHED`; and a text-search
fallback when a structured TV-param search returns nothing, since some
indexers ignore season/episode parameters entirely.

### Scheduled backups
**Tier:** any. Wizard S7b. Snapshots use the SQLite online-backup API
(crash-consistent against a live WAL, not a raw file copy), so they do not
carry stale plaintext. A separate, portable `.dhbackup` export bundle exists
for carrying a backup off-box: it's additionally encrypted (`backup/bundle.go`,
AES-256-GCM with an Argon2id-derived key), reusing the same recovery password
as sealed secrets rather than requiring a second one to remember.

---

## Always-on, not separately adoptable

These aren't features you opt into — they run as part of the base deployment
regardless of tier, with no switch to turn them off. Listed because "what
does DarkHarrbor actually do under the hood" is a real question, not because
any of them belong in an adoption decision.

- **`darkharrbor doctor`'s full sweep.** Every check calls the real production
  code path, not a parallel probe: live authenticated calls to every
  configured provider, Arr reachability/key validity, a path-mapping check
  that writes a marker file and asks each Arr whether it can see it at the
  expected location (catches volume/bind-mount mismatches automatically),
  clock skew, and more.
- **The PAR2 + cross-lane NNTP recovery ladder.** A damaged or missing Usenet
  segment isn't just retried against other providers (rungs 1-3) — DarkHarrbor
  has its own from-scratch GF(2^16) Reed-Solomon PAR2 solver (rung 4) that
  reconstructs the exact damaged span and verifies every reconstructed block
  against its native PAR2 hash before serving it, and if that fails too, can
  pull the identical, hash-proven byte range from a *different acquisition
  lane entirely* — torrent or HTTP — for the same release (rung 5). Never
  guesses: any failure abstains to the original error rather than risking
  unverified bytes.
- **Priority-fair-share admission (`accountgov`) and AIMD connection tuning.**
  A shared lease system orders playback above recovery, grab, repair, and
  audit work across every lane, with session fair-share on top. NNTP
  connection pools additionally self-tune: a real connection-cap rejection
  halves the effective ceiling, which grows back one connection every 30
  rejection-free seconds.
- **Self-measured suppression (`suppress`) and search-budget limiting
  (`searchbudget`).** After DarkHarrbor itself observes a release fail for a
  deterministic reason (an unsupported/encrypted archive, a confirmed-dead
  Usenet post), it won't re-grab that same effective release — even reposted
  under a different hash or indexer — for a configurable TTL. Never imports a
  shared/crowd-sourced blocklist; this is purely DarkHarrbor's own
  observation. Search budget separately caps repeated no-result searches per
  catalog identity so a title that keeps coming back empty stops hammering
  your indexers.
- **Live in-stream verification (`torrentmeta`, `identitycheck`).** Torrent
  pieces are verified against the torrent's own BitTorrent v1/v2 hash tree
  *during actual playback*, not just at grab time — a mismatch penalizes that
  CDN host and falls through to the next source rung, so corrupted bytes
  never reach the viewer. Separately, a resolved release is checked against
  what the Arr actually expected (year, runtime tolerance, conflicting
  source tags) before it's trusted; default `warn` mode logs rather than
  acts, `enforce` blacklists and asks the Arr to re-search.
- **HLS.** A full live/VOD-aware playlist rewriter — every URI-bearing tag
  (variants, renditions, DRM key URIs, init segments, LL-HLS parts/preload
  hints) gets rewritten so no upstream URL ever reaches the client. The
  session/resource identity graph is durable across a restart; the actual
  manifest text and upstream URLs are never cached, so a restart-resumed
  session re-derives the identical rewrite rather than needing a fresh
  master-manifest fetch.
- **Prometheus `/metrics`.** Secret-free by construction — every label comes
  from a closed vocabulary, never a raw title or URL. Covers per-provider NNTP
  connection-pool acquire counts, per-source health/latency scores, governor
  admission denials by lane/priority/reason, item counts by lifecycle state,
  and reactive-commit/promotion volume, among others.

---

## Consequences worth deciding deliberately

**Your Arrs are the routing table.** `Resolve` sends content to the instance
that already owns it, and remembers that pairing permanently. With one Arr this
is invisible. With several, declining Arr integration means every first-seen
title falls to the named default instead.

**Universal proxying puts DarkHarrbor in the byte path for all playback.** A
restart interrupts any stream in flight, and every upgrade becomes a
playback-affecting event rather than a background one.

**Commit is not acquisition.** What lands is a pointer, not a file. "My library"
means "things I can re-resolve", which is exactly right for streaming-first and
worth knowing before an outage teaches it.

**The base URL requirement moves with the proxy posture.** Selective proxying
needs it client-reachable; universal proxying needs it container-reachable.
These conflict, and one variable cannot satisfy both. Change them together.

---

## Minimal adoptions

| Goal | Take | Skip |
|---|---|---|
| Classic Arr automation | Arr integration + one or more selected acquisition families, `t1` | aggregator, reactive, promotion |
| HTTP-only Arr automation | Arr integration + one or more HTTP stream backends, `t1` | debrid, NNTP, aggregator, reactive, promotion |
| Streaming-first, no Arr searching | `t3`, an existing Arr destination, aggregator, reactive commit | base Arr indexers and both download-client shims |
| Usenet only | NNTP lane + Arr integration, `t1` | debrid, aggregator, reactive |
| Aggregated playback, no auto-library | `t2` | reactive commit, promotion |
| Normal play-to-library adoption | `t3`, one existing Sonarr or Radarr entered manually or discovered, any supported debrid provider, AIOStreams + StremThru, DarkHarrbor Library addon, auto or supervised commit | base Arr indexers/clients are optional for reactive-only use; also skip NNTP, Jellyfin, Prowlarr, backups, direct Stremio-library use, and promotion when unwanted |
| Disposable play-to-library acceptance fixture | isolated `t3`, one operator-created clean Radarr, one supported debrid provider, AIOStreams + StremThru, DarkHarrbor Library addon, auto commit | never presented as the normal installation and never installed by DarkHarrbor |

For either t3 row, first run configures and validates ordinary selected HTTP
backends before mutation. The operator-controlled aggregator dashboard remains
**externally pending** because its Direct Manifest URL does not exist until the
saved user is updated. Complete the chronological handoff in
`AGGREGATOR-INTEGRATION.md`, run `darkharrbor configure --existing-aiostreams`,
restart, then treat `darkharrbor doctor` as decisive.
