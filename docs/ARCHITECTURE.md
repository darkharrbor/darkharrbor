# Architecture

This is the mechanism-level document: how DarkHarrbor actually works
internally, not how to configure or run it. If you want settings, see
[CONFIGURATION.md](../CONFIGURATION.md); if you want to know what a feature
costs to adopt, see [FEATURE-MAP.md](FEATURE-MAP.md). This document exists
for readers who want to understand the machinery underneath those two —
contributors, operators debugging something non-obvious, or anyone curious
how a 500+ file Go codebase turns "play this" into bytes on screen without
ever downloading a full file until someone actually asks for it.

Citations are `package/file.go:line` against the current source tree.
Verify against the actual file before trusting an exact line number in a
future reading — source moves.

## The core model: a pointer, not a file

Every item DarkHarrbor manages is, on disk, a tiny `.strm` text file
containing one signed URL — never the media itself. `.strm` generation
(`internal/output`) writes this atomically (stage-then-rename, so an Arr
never scans a partial write). The URL embeds an HMAC-SHA256 token over
`itemID:fileID`, keyed by the deployment's `STREAM_SECRET`
(`output.go:157-161`) — **deliberately non-expiring**: a `.strm` written
today must still resolve in a year, since there's no mechanism to regenerate
it, and Sonarr/Radarr have no reason to ever re-import an unchanged file.

Playing that file is a standing order, not a receipt. The player requests
the URL; DarkHarrbor resolves it live against whichever acquisition lane
currently owns that item, fetches (or streams) the actual bytes, and caches
them for a fast rewatch. If the original provider goes away, DarkHarrbor can
silently re-home the item to a different lane carrying the same verified
content — see "Resilience ladder" below — without the `.strm` file on disk
ever changing. This is why losing DarkHarrbor's own database is a real
problem even though the `.strm` files survive: the files are pointers: what
they point *at*, and the verified identity behind that pointer, lives
entirely in the database, not in the file.

## Acquisition: four independent lanes, one preference system

DarkHarrbor pulls from four structurally different backends, each with its
own adapter:

1. **Torrent + debrid** — TorBox, Real-Debrid, AllDebrid, Premiumize. Each
   has an independent runtime adapter (`internal/torbox`, `realdebrid`,
   `alldebrid`, `premiumize`); the torrent walk itself is provider-neutral.
2. **NNTP/Usenet** — direct multi-provider NNTP (`internal/nntp`), or
   TorBox's own cached-NZB lane.
3. **HTTP-stream** — a fourth, independent acquisition backend
   (`internal/httpstream`), not a thin wrapper around the other three: four
   pluggable protocols (Internet Archive, OMSS, the Stremio addon SDK, and an
   operator-declared generic descriptor format), each with conservative
   title/digest-aware matching, SSRF-hardened outbound policy, and a
   predictive mid-stream re-resolve system that refreshes an expiring signed
   URL *before* playback reaches it.
4. **Generic declared sources** — the `generic` protocol above, specifically:
   typed descriptors (`progressive`/`hls`/`m3u`/`metalink`/`object`/`webdav`/
   `fixed`) the operator declares directly, re-resolved by a stable
   `source_id` rather than a search.

`HARRBOR_PREFERENCE` (which acquisition *lanes* to try, in what order) and
`HARRBOR_PROVIDERS` (which debrid *backends* to try, in what order) are
orthogonal axes that are commonly conflated — the first picks a lane, the
second picks who serves that lane's torrents.

### Three distinct fallback mechanisms

These are separate code paths solving separate problems, not one generic
retry loop:

- **Fresh-grab lane walk** (`cmd/darkharrbor/main.go`, `submitItem`/
  `submitViaLane`): tries `HARRBOR_PROVIDERS` in order at grab time. A
  non-retryable provider failure (Real-Debrid's `infringing_file`, for
  example) stops immediately rather than trying the next lane; a retryable
  one stops too, but reschedules in 30s.
- **Mid-stream ephemeral alt-source** (`streamaltprovider.go`): a one-shot
  alternate CDN source when the primary provider's chunk fetch fails
  *during playback*. Never persists or mutates the item's recorded provider
  — purely a live save.
- **Stream-failure repair re-homing** (`repair.go`): only on a genuine
  broken-source detection. Retries the same provider first, then walks every
  other configured lane; success permanently rebinds the item's provider.
  Total exhaustion blacklists the infohash and fails the item so the Arr
  re-searches from scratch.

## Resilience ladder: never guess, always verify

DarkHarrbor's recovery story is layered, and every layer shares one rule —
abstain rather than serve unverified bytes.

1. **Per-segment multi-provider failover** (NNTP). `ProviderHealth` tracks
   consecutive real failures per provider with exponential backoff (2s base,
   doubling, capped at 2 minutes — engaging only after two or more
   consecutive failures; a single definitive "missing" response is treated
   as *evidence the provider is healthy*, not counted against it).
2. **PAR2 Reed-Solomon reconstruction** (`internal/nntp/par2repair.go`). A
   full from-scratch GF(2^16) solver: measures the exact damaged byte span
   from surviving segments, determines every whole PAR2 input block the
   damage touches, solves for the unknowns, then verifies every
   reconstructed block against its own native PAR2 IFSC hash before
   returning anything. Any failure returns an explicit abstain error; the
   caller falls back to the original error rather than serving unverified
   bytes. Bounded by a hard byte-and-wallclock budget.
3. **Cross-lane recovery** (`internal/crosslane`, `par2crosslane.go`). If
   PAR2 also fails, DarkHarrbor can pull the identical, hash-proven byte
   range from a *different acquisition lane entirely* — torrent or HTTP —
   verified block-by-block against the same proof. Supports BitTorrent v1
   SHA-1, BitTorrent v2 Merkle, PAR2 IFSC MD5, and HTTP flat-digest SHA-256
   proof formats (`internal/contentproof`). Confirmed wired with real
   production call sites in both directions (`nntp_crosslane.go`,
   `torrent_crosslane.go`), not merely available-but-unused.

A shared abstraction, `internal/ladder`, is the rung-walk/budget/lease
contract every lane plugs its own concrete rungs into — it's pure plumbing,
but it's the reason the three lanes share one failure philosophy instead of
three independently-invented ones.

### Verification doesn't stop at grab time

- **`torrentmeta`** verifies torrent pieces against the torrent's own
  BitTorrent v1/v2 hash tree *during actual playback*, not just at grab
  time. A piece-hash mismatch penalizes that CDN host's reputation and falls
  through to the next fetch rung — corrupted bytes never reach the viewer.
- **The continuity ledger** SHA-256-hashes every fetched HTTP-lane byte
  range and checks it against durable history for that exact range. If the
  *same range* now hashes differently than previously observed — a CDN
  silently serving different content — the chunk is rejected and the ladder
  falls through, rather than serving a mid-stream content swap.
- **9 proof provenances across 4 trust tiers** (authoritative > same-origin
  > trust-on-first-use > hint): torrent piece/Merkle, PAR2 IFSC, Internet
  Archive digest, Metalink hash, HTTP flat digest, same-origin ETag,
  playback-observed, and candidate-hint. `internal/contentproof` is the
  shared graph all of these feed.

## Media identity: what it's supposed to be vs. what it actually is

Two deliberately separate concerns, feeding one comparison:

- **`mediaidentity`** — pure data. What the Arr *says* a release should be
  (TVDB/TMDB/IMDB IDs, runtime, year). Shape validation only; never touches
  file bytes.
- **`mediatruth`** — the file-bytes analyzer. Derives real facts
  (codec/resolution/duration/audio languages) from actually-probed bytes,
  lane-neutral — the same code path for NNTP, torrent, and HTTP.
- **`identitycheck`** compares the two. Four independently-abstaining checks
  (year, runtime tolerance = `max(12 minutes, 12%)`, audio language, and
  conflicting source tags like CAM+BluRay in one release title) — every
  check abstains on missing input rather than guessing. Default strictness
  is `warn` (log only, non-destructive); `enforce` suppresses, blacklists at
  source, fails the item, and asks the Arr to re-search. A mismatch is
  deliberately never handed to the repair mechanism above — repair would
  just re-fetch the same wrong content.

A separate, slower background pass — the identity audit — re-checks already-
`Ready` library items on rotation, least-recently-audited first. It never
suppresses or blacklists anything; it only ever produces a findings report,
since touching a Ready item is judged materially riskier than grab-time
enforcement.

## Output and storage

`.strm` writes are atomic (stage-then-rename). Naming prioritizes a
TorrentMeta-verified release name over a provider-reported one when a
persisted manifest exists. Archive detection (RAR/ZIP/7z bundled alongside
a release) uses a "payload dominance" heuristic — combined archive-volume
bytes must exceed the largest direct video file, with at least two volumes
— specifically to avoid mistaking a video release that just bundles a small
subtitle RAR for an actual scene-release RAR set.

The store layer (`internal/store`) mixes two persistence strategies
deliberately: targeted JSON-blob fields for data that's read as a whole
(`items.metadata_json`, updated via `json_set` rather than a full-column
overwrite, so concurrent writers can't clobber each other) and dedicated
relational tables for anything needing an indexed lookup — torrent
manifests, content proofs (a bounded LRU), continuity history, the
reactive-commit state machine, playback coverage. Playback coverage resets
entirely on a declared-size change, since a different release now answering
the same identity shouldn't have its byte coverage miscounted against the
old one.

### Archive extraction — real, but narrower than it might sound

RAR and 7z support is real and non-trivial, and currently **NNTP/Usenet-lane
only** — the type system recognizes the same archive kinds on the HTTP-
stream lane (a Stremio addon returning `rarUrls`, for instance) but member
resolution there isn't implemented yet.

- **RAR** (RAR3 + RAR5): store/uncompressed volumes only — a
  real-compressed RAR fails closed with a typed error (usenet video releases
  are essentially always store-mode already, so this isn't a practical
  limitation). **Password-protected RAR is fully supported**: RAR3's
  classic SHA-1 KDF and RAR5's PBKDF2-style HMAC-SHA256 KDF, both with full
  CBC decryption on the fly per range request, including block-aligned
  partial reads across NNTP segment boundaries. Nested-archive detection
  refuses to proceed if a decoded payload is itself another archive
  signature.
- **7-Zip**: a real from-scratch container parser, not a library wrapper —
  but copy/store coder only (no compression, no solid blocks), and
  AES-encrypted 7z is explicitly rejected outright (no password support,
  unlike RAR). Multi-volume `.7z.001/.002/...` is supported via correct raw
  concatenation.
- **Obfuscation**: whole-archive-family detection (RAR vs. 7z vs. ZIP vs.
  raw media) is magic-byte-based and survives a fully obfuscated release
  name. Identifying *which individual files are the RAR volumes*, though,
  still depends on the part filenames actually containing `.rar`/`.rNN` — a
  release where every per-volume filename is renamed to something
  RAR-unrecognizable fails with "no RAR parts found." Resilient to an
  obfuscated release name; not resilient to obfuscated per-volume filenames.

## Reactive commit: play-to-library

The engine (`reactivecommit`, `reactivequeue`, `reactivedispatch`) is
aggregator-agnostic in code — it operates purely on a representation-ID /
playback-coverage abstraction and has no reference to any specific
aggregator brand anywhere in it. The maintained, documented integration path
(AIOStreams + StremThru) is aggregator-*specific* at the setup/handoff layer,
not in the engine itself — see
[AGGREGATOR-INTEGRATION.md](AGGREGATOR-INTEGRATION.md) for that boundary.

Full state machine: `committing → committed | parked → undoing → undone`,
idempotent on re-commit, with full undo (including deleting materialized
`.strm` blobs). Ambiguous cases land in a genuine operator review queue,
keyed by reason (`positive_mismatch`, `weak_evidence`, `routing_ambiguous`,
`routing_unresolved`, `supervised_review`), each with its own valid
resolution set. A per-provider-ID default-Arr assignment is remembered, so
once you've resolved one ambiguous case for a given show, future instances
of it pre-resolve themselves. An Arr instance missing a captured root
folder or quality profile stays manually-routable-only — it's never
auto-dispatched to until that's filled in.

**Promotion** (turning a re-resolved pointer into fully-owned, permanently
cached content) is an explicit, authenticated action, never
threshold-triggered automatically. The free-space floor
(`total_filesystem_bytes * floorPercent / 100`, default 10%) is enforced by
evicting cache entries — including normally-exempt pinned hot-head
entries — to make room; if the floor still can't be satisfied, the
promotion is refused and rolled back. The original `.strm` is never touched
until the content is fully staged, checksum-verified against an
authoritative content-proof (refuses to proceed without one), and only then
atomically cut over, with a full rollback path if anything fails
post-cutover.

## The HTTP surface: two routers, by design

DarkHarrbor runs two genuinely separate routers, not one router with
conditional auth:

- **The primary router** (`:8381`, container-internal only — never publish
  this) carries everything: qBittorrent-compat and SABnzbd-compat endpoints,
  the three protocol-locked Torznab/Newznab feeds, health/metrics/debug/
  doctor, identity-audit, reactive-promotion, and the sidecar resource
  server.
- **The client router** (`:8382`, the only surface a viewing device ever
  touches) is deliberately narrower: `/healthz`, stream/MediaFlow playback
  routes, sidecar, and the DarkHarrbor Library Stremio addon — no qBit/SAB,
  no WebDAV, no metrics, no debug, no admin surface of any kind. It's also
  deliberately excluded from request-path logging, since the addon path
  embeds the install credential. A fixed 64-concurrent-request semaphore
  (503 + `Retry-After` when exhausted) is its only dedicated rate limiter;
  all other admission control is the governor lease system described below,
  which throttles provider/CDN calls, not raw client request rate.

There is no web UI or dashboard anywhere in either router — the only HTML
DarkHarrbor ever emits is one trivial hardcoded fragment some SABnzbd
clients probe for capability detection. Every real endpoint is API-shaped.

### MediaFlow proxy protocol

A distinct capability from DarkHarrbor's own native HTTP-stream lane: an
implementation of the MediaFlow Proxy protocol (`/proxy/ip`,
`/generate_urls`, `/proxy/stream`) that Stremio-ecosystem aggregators like
AIOStreams already speak, letting DarkHarrbor act as a MediaFlow-compatible
sink for them. Playback tickets are AES-GCM AEAD-sealed — the real
destination URL, headers, and representation identity are never exposed to
the client in plaintext. Opt-in only: requires both a MediaFlow password and
a stream secret configured, not enabled just by running DarkHarrbor. A
self-origin loop guard unwraps a nested self-generated URL exactly once and
refuses a second wrap.

The identity-binding problem this solves is non-obvious: an aggregator can
emit a batch of dozens of playback URLs where only a fraction carry a
correlatable metadata ID per-URL. DarkHarrbor binds an entire batch to one
identity observed within a short time window instead of trying to correlate
per-URL; genuine ambiguity abstains to a random ID rather than guessing
wrong.

### HLS

A full live/VOD-aware playlist rewriter. Every URI-bearing tag — variant
streams, audio/video/subtitle renditions, the DRM key URI, the init
segment, ordinary segments, and low-latency parts/preload-hints/rendition-
reports — gets rewritten so no real upstream URL ever reaches the client.
Live and VOD are handled differently (live: sliding window plus
media-sequence tracking plus delta updates; VOD: an exact presentation
timeline computed from summed durations). Content Steering is genuinely
implemented, not just tolerated.

The "resumable session graph" is precise, not marketing: the *identity
graph* (which upstream, which byte range, which media-sequence coordinates)
is durable in the database and survives a restart. The manifest text and
resolved upstream URLs themselves are never cached — every request
re-derives and re-fetches live — but because the identity graph is already
durable, that re-derivation is deterministic and reproduces the identical
rewrite. That's what lets a player's already-held URL survive a restart
without needing a fresh master-manifest fetch.

## Security and access control

### The Docker proxy layer

`harrbor-dockerproxy` plus `internal/dockerpolicy` is a default-deny
allowlist proxy, not a generic socket-forwarding shim. It cannot stop,
restart, kill, or delete anything; cannot pull images; cannot touch volumes
or networks. Only four read-only endpoints are allowed (container list,
events, version — every response sanitized; container list drops labels
entirely) plus a fully-mediated three-step exec protocol (create → start →
inspect-once-then-forget) restricted to an exact allowlist of roughly a
dozen hardcoded, non-interactive, one-shot commands against exactly three
image-matched container types (Sonarr/Radarr, Prowlarr, Jellyfin). There is
no free-form command execution surface anywhere in it. An unrecognized
container image is unconditionally refused. This is substantially narrower
than a raw Docker socket mount, and it's fair to describe it that plainly.

### Secrets and config layering

Load order: hardcoded defaults → `.env` file (never overwrites an
already-set env var) → `HARRBOR_*` env vars → sealed secrets (sealed wins
over plaintext env for roughly 15 known credential keys) →
`darkharrbor.conf` tuning file (wins over both sealed and env, but only for
its own strict, non-credential-bearing allowlist — see
[CONFIGURATION.md § Tuning Reference](../CONFIGURATION.md#tuning-reference)
for the exact current key count). Decrypted secrets are held in-process
only, never exported via `os.Setenv`, so a compromised child process (like
an ffprobe wrapper invocation) cannot inherit them even in principle.
Startup hard-refuses the shipped default credentials if still in place, and
refuses any unresolved `${...}`-shaped placeholder secret rather than
silently treating the literal placeholder as a real value.

### Backups

Scheduled snapshots use genuine SQLite online backup — crash-consistent
against a live WAL, not a raw file copy — exclusive-flock-serialized, with
atomic publish via rename. The separate, portable `.dhbackup` export format
adds its own encryption layer (AES-256-GCM with an Argon2id-derived key,
`internal/backup/bundle.go`), deliberately reusing the same recovery
password as sealed secrets rather than asking the operator to remember a
second one — a restore already needs that key regardless. Restore refuses
to run against a live daemon, runs a SQLite integrity check before
installing anything, and has a full rollback path on any partial failure.

## Admission control and self-protection

None of what follows is something you enable — it's part of the base
deployment and runs regardless of tier.

- **`accountgov`** (distinct package from `internal/governor`, despite the
  similar name) implements a shared priority-lease admission system across
  every lane, with a fixed priority order — Playback > Recovery > Grab >
  Repair > Audit > Prewarm — plus session fair-share on top. A separate AIMD
  mechanism self-tunes NNTP connection pool ceilings: a real connection-cap
  rejection halves the effective ceiling, which grows back one connection
  every 30 rejection-free seconds.
- **`suppress`** stops DarkHarrbor from re-grabbing a release it has itself
  already observed fail for a deterministic reason (an unsupported or
  encrypted archive, a confirmed-dead Usenet post) — even if that release
  reappears under a different hash or indexer, for a configurable TTL. This
  is purely DarkHarrbor's own first-hand observation; it never imports a
  shared or crowd-sourced blocklist.
- **`searchbudget`** caps repeated no-result searches per stable catalog
  identity (never raw title text), so a title that keeps coming back empty
  stops hammering your indexers.
- **Torrent blacklist / NZB dead-letter** apply the same 3-strike/7-day-
  window concept to each lane — the NZB variant exists because NZBs have no
  infohash to key a blacklist on the way torrents do.
- **Keep-warm janitor** proactively re-adds recently-played torrent items to
  the debrid provider before an assumed provider-side cache-retention window
  expires (a derived estimate per provider, not a live query — and it
  abstains entirely for a provider with no known retention assumption).
- **Decay audit** rotates a background sampler over already-`Ready` items'
  live remote-cache presence. On a conclusive miss, it hands off to the
  existing repair chain rather than acting directly — and a single audit
  pass is never enough to trigger action: a segment once flagged as
  possibly decayed must be reconfirmed on a second, independently-sampled
  pass before anything fires, specifically because a segment flagged
  missing has been observed to fully recover on the very next pass.

## Diagnostics

`darkharrbor doctor` reuses real production code paths end to end rather
than a parallel probe mechanism — every check is a live call against the
actual configured provider, Arr, or path, not a simulated one. See
[CONFIGURATION.md § Diagnostics](../CONFIGURATION.md#diagnostics) for the
full, current check list; this document won't duplicate a table that drifts
independently of prose.

Prometheus `/metrics` is secret-free by construction: every label comes
from a closed vocabulary, never a raw title or URL. It covers per-provider
NNTP connection-pool acquire counts, per-source health/latency/token-
lifetime scores, governor admission denials by lane/priority/reason, item
counts by lifecycle state, an error-journal breakdown by failure class, and
reactive-commit/promotion volume, among others.

## What's real but not wired (don't market these as active)

- `experience.NextEpisodePrewarm` — a speculative next-episode prewarm,
  fully implemented and unit-tested, with no live call site today. Everything
  else in `experience` (hot-head prewarm, seek-target prefetch, adaptive
  readahead sizing) is live.
- `identitycheck`'s audio-language check — one of its four checks is wired
  to a hardcoded empty expected value in production, so it currently never
  actually fires, even though the mechanism exists.
