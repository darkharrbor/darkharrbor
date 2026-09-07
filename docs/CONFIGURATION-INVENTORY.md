# Configuration Inventory — Required vs Deployment-Specific

Reconstructed from the project's own durable-precondition history, NOT from
the reference deployment's compose files. That distinction is the point of
this document: the compose records *what* a setting is, and only this record
keeps *why*. A value whose reason is lost is a value someone eventually
"cleans up".

Every entry is classified:

| Class | Meaning |
|---|---|
| **REQUIRED** | The integration does not work without it. Not a preference. |
| **CONDITIONAL** | Required only for a named feature or topology tier. |
| **LOCAL** | This host's addresses, paths, accounts. Substitute your own. |

Entries also record **how the failure presents**, because most of these fail
opaquely — at the player, naming nothing.

---

## 1. Aggregator → DarkHarrbor proxy contract

These make the aggregator route playback through DarkHarrbor. Without them the
reactive lane cannot fire at all: coverage is recorded **only** for bytes
DarkHarrbor itself delivers.

| Setting | Class | Why it exists | Failure if wrong |
|---|---|---|---|
| `FORCE_PROXY_ENABLED=true` | REQUIRED | Turns on aggregator-side proxying | No stream reaches DarkHarrbor; reactive lane silently never fires |
| `FORCE_PROXY_ID=mediaflow` | REQUIRED | Selects the MediaFlow-compatible protocol DarkHarrbor implements | Aggregator speaks a protocol DarkHarrbor does not answer |
| `FORCE_PROXY_URL` | REQUIRED (value LOCAL) | Points at DarkHarrbor's **private** listener | Wrong host: streams bypass DarkHarrbor. Wrong port: exposes the primary API |
| `FORCE_PROXY_PROXIED_SERVICES` | REQUIRED | **Empty list = every lane can commit.** A named list silently limits commit to those services | Named list looks healthy; unlisted lanes simply never commit |
| `FORCE_PROXY_DISABLE_PROXIED_ADDONS=true` | CONDITIONAL | Clears the per-addon allowlist so routing is governed by service alone | Per-addon list silently overrides intent |
| `FORCE_PROXY_CREDENTIALS` | REQUIRED (value LOCAL) | The aggregator sends this as `api_password` on every MediaFlow call. It must equal DarkHarrbor's `HARRBOR_MEDIAFLOW_PASSWORD`. Supplied via `env_file`, never inline | Unset or mismatched: `/proxy/ip` and `/generate_urls` answer `503`/`403`, no stream is ever proxied, and the reactive lane silently never fires |

> When aggregator integration is selected, S6 generates and S7 seals
> `HARRBOR_MEDIAFLOW_PASSWORD`. Setup prints that literal exactly once so the
> operator can place the SAME value in the aggregator bundle's private
> `FORCE_PROXY_CREDENTIALS`; it remains rejected from `darkharrbor.conf`.
> Both halves must be set or neither works.

**Two lanes can never commit regardless of these settings**, because the
aggregator hardcodes them to bypass any external proxy: its own built-in usenet
engine, and NzbDAV/AltMount unless the aggregator's *built-in* proxy is used. If
you want usenet to commit, use a shared-tier or debrid usenet service instead.

---

## 2. Settings that exist because a shipped default is the failure mode

The most dangerous class. Each of these looks like an arbitrary local override
and is not. **Restoring any of them to its default re-breaks a fault that took
days to find.**

Most entries are operator-applied settings — environment variables or dashboard
values the operator must explicitly set. One entry (`mediaFlowMaxURLs`) is a
**compiled-in invariant** already set in DarkHarrbor's source and requires no
operator action; it is retained here because restoring the old constant
re-breaks the fault, and its heading is marked accordingly.

### `BUILTIN_STREMTHRU_URL` / `STREMTHRU_TORZ_URL` / `STREMTHRU_STORE_URL`
**REQUIRED** (values LOCAL). The shipped default points at a *public* shared-tier
instance. When that instance went down, **every debrid source lost resolution at
once** — the failure is total, not partial, because the shared tier is the
resolution path for all of them. Self-hosting is required, not advised.

For the documented torrent/debrid path, the account credential belongs to the
AIOStreams saved user's **Services** configuration. AIOStreams service wrapping
uses that selected service with the self-hosted StremThru endpoints; the example
bundle has no debrid-token environment variable and the operator does not repeat
the account in a StremThru dashboard. StremThru dashboard provider entries in
this integration are NNTP servers for the usenet path. Standalone StremThru
Store/Torz provider setup is outside this topology.

Two sub-findings worth preserving:

- The Torz addon carries its **own** `url` field independent of
  `builtins.stremthru.url`, and the preset resolves `options.url || DEFAULT_URL`
  — so the environment variable seeds *new* configs only and a saved addon keeps
  pointing at the dead host. **The env var appears to work and does nothing.**
- In the locked AIOStreams v2.31.1 contract it is a **Torznab** preset, not a
  Stremio-addon preset. Its single URL field requires the complete endpoint
  `http://stremthru:8080/v0/torznab/api`; `…/stremio/torz` produces
  `GET /stremio/torz?t=caps` → 404. If a candidate changes the URL shape or
  controls, it requires a new compatibility-lock promotion, not an inferred
  edit during installation.

### `DISABLE_RATE_LIMITS=true`
**REQUIRED.** The aggregator enforces per-IP limiters *even when self-hosted*
(default false; stream API 5 requests/10s). Measured rejections before the fix:
DarkHarrbor 765, Prowlarr 320, SABnzbd 15. Zero after.

### `NODE_OPTIONS=--dns-result-order=ipv4first`
**CONDITIONAL** — required wherever the container network has IPv6 disabled while
the host has working IPv6. Node/undici happy-eyeballs does not fall back cleanly
and produces **corrupt TLS reads**, surfacing as `packet length too long` and
~2,951 failed debrid library calls. `curl` on the same host falls back fine, so
the network looks healthy under manual testing.

### `mediaFlowMaxURLs` (raised 200 → 4096) — compiled-in, no operator action
**REQUIRED invariant — compiled into DarkHarrbor, not operator-configured.** The
source constant `mediaFlowMaxURLs` in `internal/api/mediaflow.go` is set to 4096
at the tested revision. It is not an environment variable, dashboard setting,
tuning-allowlist key, sealed key, or operator-supplied value. Operators must not
attempt to configure it; it is satisfied by running a conforming DarkHarrbor
build.

An aggregator posts an **entire result set in one call**. Real batches of 539 and
631 URLs were observed against the original 200 cap, and DarkHarrbor rejected the
whole batch with a bare `400` — discarding *every* stream for that title.
Boundary established by binary search against the live service: 200 → OK,
201 → 400. The fix is the source constant; a deterministic boundary test
(`TestMediaFlowAuthBoundsAndPublicIP`) asserts rejection at `mediaFlowMaxURLs+1`.
Restoring the old 200 value re-breaks the fault.

---

## 3. Reachability — the requirement that moves

`STREMTHRU_BASE_URL` and `HARRBOR_MEDIAFLOW_BASE_URL` are **REQUIRED**, values
**LOCAL**, and their correctness depends on **who fetches the URL**:

| Proxy posture | Who fetches shared-tier URLs | Base URL must be reachable from |
|---|---|---|
| Selective (named services) | Viewing devices | Every client device |
| Universal (empty list) | DarkHarrbor only | The container network |

**These conflict, and one variable cannot satisfy both.** Moving between postures
silently moves the burden. Treat the proxy posture and the base URL as one
coupled change.

`HARRBOR_MEDIAFLOW_BASE_URL` exists because DarkHarrbor ignores the
aggregator-supplied `mediaflow_proxy_url` and handed clients a container name
they could not resolve. It presented as VLC's *"Multiple media cannot be played"*
— naming neither DarkHarrbor nor the cause.

The documented AIOStreams path has a second, different URL contract:
`HARRBOR_HTTPBACKEND_AIOSTREAMS_URL` is the saved user's complete Stremio addon
base (Direct Manifest URL without `/manifest.json`) rewritten to a
container-reachable origin. The bare AIOStreams origin proves only that the web
app answers; it carries neither the user nor encrypted configuration coordinate
and cannot perform DarkHarrbor's deterministic lookup. The complete path is a
credential. Enter it only at `darkharrbor configure`'s hidden backend-URL
prompt; it is updated in `secrets.sealed` and never written to the tuning file.

`HARRBOR_HTTPBACKEND_<NAME>_REDIRECT_ORIGINS` is **OPTIONAL** and
**INSTALLATION-DERIVED**. Most sources leave it empty. When a source's published
metadata flow redirects to another authority, it contains only the reviewed
comma-separated exact origins (maximum eight), never the complete redirect
URL, path, query, token, wildcard, or provider credential. The approval applies
only to that named backend's metadata client and does not widen media-source
address policy or any other backend. The bounded IA/OMSS/Stremio dial test can
derive this origin from a blocked redirect and request explicit approval without
following, displaying, or saving the full redirect URL. Generic descriptor
health is file-only, so generic origins are entered explicitly when required.

> A new setting must be registered at **all four** config points — struct field,
> env binding, `tuningKeys` membership, and tuning validation/apply. Omitting the
> `tuningKeys` entry is exactly what silently dropped a previous variable.

### Provider account capability discovery

Provider account facts are runtime-discovered, not installation constants.
TorBox keeps two states deliberately separate: the effective conservative
policy and the verified account tier. A recognized active plan owns its slots,
size ceiling, usenet/News Server gates, and AirLock quota. An active unknown
plan code gets Free-level runtime limits and no paid capabilities, but remains
labelled unknown and does not trigger a Free-tier setup recommendation. The
sanitized `provider_caps` wizard record exposes this distinction as
`free_tier`, `conservative_free_policy`, and `unknown_plan_fallback`; it contains
no token, account ID, email, or provider URL. Doctor reports the same shared
resolution and warns on unknown fallback.

Real-Debrid, AllDebrid, and Premiumize are enabled only after their account API
confirms the paid state DarkHarrbor requires. Published ceilings that those APIs
do not return remain release-validation facts; the wizard must not derive new
quota, concurrency, or tier behavior from account names or local deployment
values.

### `HARRBOR_STRM_INSTALLER_ENABLED`
**CONDITIONAL.** This enables S10's ongoing Arr-side ffprobe metadata relay and
requires the scoped Docker proxy. It belongs to base Arr search/grab topology;
it is not one of the qBittorrent/SAB download-client shims and is not required
for reactive-only direct `ManualImport`. A clean reactive-only Radarr can record
the `.strm` with empty media metadata, so the disposable t3 acceptance fixture
leaves this false and does not mount the Docker socket.

### `HARRBOR_DOCKERPROXY_TARGETS`

**CONDITIONAL, COMPOSE-SIDE SECURITY BOUNDARY.** A comma-separated list of exact
container names the scoped connector may execute its fixed operations inside.
The empty default permits only sanitized discovery and denies all execs. This
value is not a credential and must contain names, never API keys or URLs.

---

## 4. Not environment-lockable — the settings that revert

Recorded because an installer **cannot own these**. It can set them once; it
cannot keep them set.

| Setting | Where it lives |
|---|---|
| Installed addon URLs and per-addon options | Encrypted user config |
| Per-addon filters, sort, formatter | Encrypted user config |
| Shared-tier usenet servers | Shared-tier dashboard; no API (`/v0/usenet/server` → 404) |
| Torz addon `url` | Saved addon config, overrides env |
| DarkHarrbor Library addon instance | Saved addon config; required by the tested AIOStreams t3 path to observe the Stremio identity before MediaFlow batching |
| AIOStreams Direct Manifest URL | Generated from the encrypted saved-user config; its `/stremio/<user>/<opaque>` path is credential-bearing and is also the source for DarkHarrbor's internal backend base |

The aggregator applies environment forcing **at request time**, so its *saved*
configuration can disagree with its *effective* configuration. Reading the saved
value proves nothing — inspect a real stream lookup instead.

---

## 5. Genuinely LOCAL — substitute your own

Host addresses and ports, `/config` `/data` `/backup` paths, the shared
DarkHarrbor-output mount visible to each Arr, Arr instance names and their root
folders and quality profiles, all provider accounts, client-facing HTTPS
origins, the tailnet address, Newshosting connection split (95 total = 56 demand + 39 readahead),
TorBox News connections (**account-wide cap is 10**, shared with anything else
using it), and the `arr-net` network name.

---

## 6. Correction to an earlier claim

An earlier note held that the shared-tier base URL was **not** settable from
the environment and required a dashboard edit. That is **wrong**:
`BUILTIN_STREMTHRU_URL` is declared in the aggregator's builtin config schema,
and its settings store rejects any database write for an env-backed field with
`Setting <key> is overridden by <ENV>`. The posture is environment-lockable
exactly as `FORCE_PROXY_*` is.

The Torz addon's own `url` field is a **separate** setting and is *not*
env-lockable. Conflating the two is what produced the original error.
