# Aggregator Integration Guide

Use this guide when choosing t2 aggregated playback or t3 play-to-add. If you
only want the standard t1 Arr workflow, you do not need AIOStreams or StremThru.

## Start here

If AIOStreams and StremThru already work, keep them. Do not deploy the example
bundle, replace their images, copy their databases, or rebuild the saved user.
The normal installation is:

1. Run `darkharrbor setup` and select the existing Arr plus t2 or t3.
2. Start DarkHarrbor with `docker compose up -d darkharrbor`.
3. Put setup's one-time MediaFlow credential in AIOStreams' private environment,
   add the printed Library manifest to the existing saved user, and save it.
4. Run `darkharrbor configure --existing-aiostreams` inside the running
   DarkHarrbor container and enter the resulting Direct Manifest URL at its
   hidden prompt.
5. Install that saved AIOStreams manifest in the viewing client and verify normal
   playback. For t3, play beyond the chosen threshold and confirm the Arr adds
   the title.

AIOStreams must privately reach `http://darkharrbor:8381`, and DarkHarrbor must
reach the internal AIOStreams origin. The viewing client reaches only the
restricted client-facing origin configured during setup, normally HTTPS on the
8382 listener. Never publish DarkHarrbor's primary 8381 API.

If you do not already operate AIOStreams and StremThru, use the optional locked
bundle later in this guide. Maintenance and release-testing procedures are
clearly labeled and are not installation steps.

This guide covers the external half of play-to-add that DarkHarrbor cannot own.

The Arr connection and reactive destination are documented in `README.md` and
collected by wizard S8/S7d. Wizard S9 separately registers the three indexers
and two download-client shims only when base Arr topology is selected; a
reactive-only t3 journey does not require S9. The aggregator and shared tier
remain externally managed because DarkHarrbor does not own them. The normal
existing-stack handoff uses `darkharrbor configure --existing-aiostreams`;
section 2's guarded saved-config helper is optional and only replicates a full
AIOStreams user configuration. Without the external half, aggregated playback
and reactive capture cannot fire.

**Substitute your own addresses throughout.** Every host, port and account here
is from the reference deployment and is `LOCAL` in the inventory's terms.

**Optional new-stack bundle.** `examples/aggregator-tier/` contains four files you
use directly. `docker-compose.yml` deploys the aggregator and shared tier;
`compatibility.lock` records their supported immutable images and dashboard
contract. `.env.example` lists the credential values that Compose interpolates
-- copy it to `.env` in the same directory, mode `0600`, and never inline a
live value in the YAML. `credentials.example.json` is used only by the OPTIONAL
helper in section 2. Substitute your own addresses and accounts before
deploying, but change the locked images only through the maintainer upgrade
procedure.

---

## 1. What DarkHarrbor requires — the contract

Written as a contract, not as one product's settings, so a different aggregator
can satisfy it.

An aggregator is correctly integrated when:

1. **It proxies playback through DarkHarrbor** using the MediaFlow-compatible
   protocol, pointed at DarkHarrbor's **private** listener.
2. **It proxies every service you want to commit.** Coverage is recorded only
   for bytes DarkHarrbor delivers. A named service filter silently limits commit
   to those services; an empty filter proxies everything.
3. **Its playback URLs are fetchable by whoever fetches them** — see §4.
4. **It does not rate-limit DarkHarrbor**, which will call it far more often
   than a human client.

A generic aggregator satisfying that contract needs no DarkHarrbor-owned
account credential, and DarkHarrbor never writes its configuration. The tested
AIOStreams path has two additional correlation/bootstrap requirements described
below: AIOStreams must query the **DarkHarrbor Library** addon for the Stremio
coordinate, and DarkHarrbor's AIO backend must use that saved user's complete
addon base URL rather than the bare AIOStreams origin.

### The DarkHarrbor side of the contract

| Setting | Reference value | Meaning |
|---|---|---|
| `HARRBOR_SERVER_ADDRESS` | `:8381` | Private API. **Never publish this.** |
| `HARRBOR_SERVER_BASE_URL` | `http://darkharrbor:8381` | How the aggregator reaches DarkHarrbor |
| `HARRBOR_STREMIO_CLIENT_ADDRESS` | `:8382` | Restricted, client-facing listener |
| `HARRBOR_MEDIAFLOW_BASE_URL` | `http://<client-reachable>:8382` | Origin embedded in playback URLs handed to clients |
| `HARRBOR_MEDIAFLOW_PASSWORD` | generated and sealed by setup when aggregator integration is selected | Shared secret the aggregator must send as `api_password`. Copy the one-time S7 output into the aggregator's private `FORCE_PROXY_CREDENTIALS`. Empty disables the whole MediaFlow surface |

The two listeners are separate on purpose. `8381` carries the full API and stays
inside the container network; `8382` is the only surface a viewing device
touches. Publishing `8381` exposes the primary API.

---

## 2. Aggregator configuration

Using AIOStreams as the worked example.

```yaml
FORCE_PROXY_ENABLED: "true"
FORCE_PROXY_ID: "mediaflow"
FORCE_PROXY_URL: "http://darkharrbor:8381"     # private listener
FORCE_PROXY_PROXIED_SERVICES: "[]"             # EMPTY = every lane can commit
FORCE_PROXY_DISABLE_PROXIED_ADDONS: "true"
FORCE_PROXY_CREDENTIALS: "${FORCE_PROXY_CREDENTIALS}"  # == HARRBOR_MEDIAFLOW_PASSWORD
DISABLE_RATE_LIMITS: "true"
NODE_OPTIONS: "--dns-result-order=ipv4first"
```

Credentials go in an `env_file`, never inline.

Two of these are not preferences:

- **`DISABLE_RATE_LIMITS`** — DarkHarrbor can make more stream requests than
  AIOStreams' human-oriented rate limit permits.
- **`--dns-result-order=ipv4first`** — required wherever the container network
  has IPv6 disabled but the host has working IPv6; it prevents Node from choosing
  an unreachable IPv6 result for outbound provider requests.

**`FORCE_PROXY_PROXIED_SERVICES` is the setting that decides which lanes can
commit.** A named list is the common default and looks entirely healthy.

Also raise the aggregator's max-addons ceiling if you add usenet indexers —
Search Mode `both` instantiates *two* addons per indexer, and hitting the
ceiling refuses the whole config save.

### Existing working AIOStreams/StremThru

This is the normal installation path. Do not deploy the example bundle, change
image tags, copy databases, or rewrite a working Torznab configuration. Run
DarkHarrbor setup, add its printed MediaFlow credential to AIOStreams'
forced-proxy environment, and recreate AIOStreams without changing its image or
data volume. Add the printed Library manifest to the existing saved user, retain
that user's working service and StremThru settings, and save. Start DarkHarrbor,
then run:

```sh
docker compose exec darkharrbor darkharrbor configure --existing-aiostreams
```

Enter the existing internal AIOStreams origin, the Direct Manifest URL at the
hidden prompt, and the reviewed DarkHarrbor MediaFlow client origin. The command
derives the internal saved-user backend path, performs a bounded read-only dial
test, seals the sensitive URL, and enables the backend. It does not read or
write StremThru configuration. It restarts DarkHarrbor automatically. Run
`darkharrbor doctor`, install the same saved manifest in the viewing client, and
confirm playback. With t3, play beyond the configured threshold and confirm the
selected Arr adds the title.

### New optional bundle deployment order

Use this path only when the user does not already have a working
AIOStreams/StremThru deployment. Do these in order; the aggregator cannot start
with its final proxy credential until DarkHarrbor setup has generated it:

1. Make the target existing Arr reachable. Run `darkharrbor setup` and copy its one-time
   `HARRBOR_MEDIAFLOW_PASSWORD` directly into a private handoff file or password
   manager. Never place it in shell arguments or tracked files.
2. Copy `examples/aggregator-tier/.env.example` to `.env`, set mode `0600`, and
   fill the AIOStreams and StremThru values. Put the one-time MediaFlow literal
   in `FORCE_PROXY_CREDENTIALS`. Ordinary installations leave
   `AGGREGATOR_NETWORK` as `arr-net`.
3. Deploy the bundle with an explicit project name, then inspect status:

   ```sh
   cd examples/aggregator-tier
   docker compose -p <deployment>-aggregator up -d
   docker compose -p <deployment>-aggregator ps
   ```

   The bundle deliberately has no fixed `container_name` or top-level project
   name. Containers and volumes are project-scoped, while the stable service
   aliases `aiostreams`, `stremthru`, and `darkharrbor` resolve on the selected
   external network. Do not run `docker compose config` after entering secrets;
   its rendered output contains the interpolated values.
4. Start the DarkHarrbor daemon. Complete the AIOStreams dashboard steps below,
   create the saved user, and install the two required addon entries. Copy the
   tokenized DarkHarrbor Library manifest directly from the interactive S7c
   output into AIOStreams' manifest field; do not redirect or preserve that
   credential-bearing output in a transcript. If it was missed, run
   `darkharrbor configure`, accept the current values, and copy the install URL
   it reprints after save.
5. Run `darkharrbor configure --existing-aiostreams`, enter the internal
   AIOStreams origin and Direct Manifest URL at its hidden prompt, accept the
   reviewed DarkHarrbor HTTPS origin for MediaFlow, and save. The command
   restarts DarkHarrbor automatically.
6. Install the saved AIOStreams manifest in the viewing client, confirm
   playback, then run doctor. With t3, play past the configured threshold and
   confirm the selected Arr adds the title.

**Debrid credential boundary.** For a torrent/debrid installation, the chosen
TorBox, Real-Debrid, AllDebrid, or Premiumize credential is entered in the
AIOStreams saved user's **Services** page. Service wrapping resolves Torznab
magnet results through that selected service and the self-hosted StremThru URL.
There is no second debrid credential in the example `.env`, no DarkHarrbor-owned
copy of the AIOStreams credential, and no separate StremThru dashboard provider
step for this path. StremThru dashboard provider entries in this guide are NNTP
servers for the usenet path; standalone StremThru Store/Torz configuration is a
different topology.

### First-time AIOStreams dashboard setup

Use the official upstream
[AIOStreams deployment guide](https://github.com/Viren070/AIOStreams/blob/main/packages/docs/content/docs/getting-started/deployment.mdx),
[AIOStreams configuration reference](https://github.com/Viren070/AIOStreams/blob/main/packages/docs/content/docs/configuration/options.mdx),
and [StremThru repository](https://github.com/MunifTanjim/stremthru) to deploy
those products. The example does **not** follow their moving `latest` tags. Its
defaults are the immutable references in
`examples/aggregator-tier/compatibility.lock`; `docker compose pull` therefore
cannot silently change this dashboard contract. The path below is for the
locked AIOStreams v2.31.1 contract. Upstream installation docs do not supply
DarkHarrbor's cross-product values. A new user completes these steps:

1. Expose AIOStreams through an origin reachable by the viewing device. For a
   remote Stremio client, use trusted HTTPS (for example a private Tailscale
   Serve origin), not a container name or raw localhost URL. Open the dashboard,
   choose **Advanced** setup, and create the first saved user configuration
   (Advanced is required later in step 5; starting there avoids a mode switch
   partway through). Keep its UUID and password private.
2. Under **Services**, enable the selected debrid account and enter
   its credential. The tested example uses TorBox, but AIOStreams and
   DarkHarrbor independently support TorBox, Real-Debrid, AllDebrid, and
   Premiumize. Using the same provider in both products is easier to reason
   about; it is not a runtime requirement and it does not create a
   second StremThru credential.
3. Under **Addons → Marketplace**, install the **Torznab** preset for the
   self-hosted StremThru endpoint. For the example bundle use:

   | Field | Recommended value |
   |---|---|
   | Name | `StremThru Torznab` (operator label) |
   | Torznab URL | `http://stremthru:8080/v0/torznab` |
   | API key | blank |
   | Timeout | `30000` ms |
   | Paginate Results | disabled |

   Use 30000 ms so a cold self-hosted lookup has time to complete. Do not add
   a trailing `/api` — the addon appends `/api?t=caps` itself, and a
   `.../torznab/api` value produces a double-`/api` path that 404s.
   The locked dashboard accepts one complete Torznab endpoint URL; it has no
   separate API-path, media-type, or search-mode controls.
4. Under **Addons → Installed**, use the dashboard's direct/custom manifest
   installer to add the **DarkHarrbor Library** addon. Its credential-bearing
   manifest shape is:

   ```text
   <HARRBOR_STREMIO_CLIENT_BASE_URL>/stremio/<HARRBOR_STREMIO_INSTALL_TOKEN>/manifest.json
   ```

   The token is generated at wizard S6. For AIOStreams reactive commit this
   addon is not merely an optional library source: AIOStreams queries it before
   `/generate_urls`, and that observation binds the playback batch to the IMDb
   coordinate. Without it, playback may work but routes remain unbound and no
   safe Arr proposal can be formed.
5. Use the settings search to set `serviceWrap.enabled=true` and
   `builtins.debrid.fileinfoStore=false` (the latter was not found via
   settings search on locked v2.31.1 during live testing — it may have been
   renamed or removed upstream; if search doesn't find it, that's a known gap
   in this doc, not something you're doing wrong). Verify the **Proxy** page shows
   MediaFlow Proxy enabled, the private URL from `FORCE_PROXY_URL`, and the same
   shared credential as `HARRBOR_MEDIAFLOW_PASSWORD`. Environment-forced URL and
   credential fields may render encrypted/opaque; do not copy their displayed
   ciphertext elsewhere.
6. Create/save the configuration. AIOStreams then shows a **Direct Manifest
   URL**. Treat the complete URL as a credential: do not paste it into chat,
   logs, or tracked files. Install that HTTPS manifest in the viewing
   client's Stremio app.
7. DarkHarrbor's `stremio` backend must use the same saved user. Run
   `docker compose exec darkharrbor darkharrbor configure
   --existing-aiostreams`. Enter the internal AIOStreams origin, then enter the
   complete Direct Manifest URL at the hidden prompt. The command removes only
   the final `/manifest.json`, replaces only the origin, preserves the complete
   `/stremio/<user>/<opaque>` path, performs a bounded dial test, and seals the
   result. Accept the reviewed DarkHarrbor HTTPS MediaFlow origin and save. A
   bare `http://aiostreams:3000` is only a reachable website, not this user's
   Stremio backend.

   Setup leaves this step externally pending because the Direct Manifest URL
   does not exist until after the AIOStreams dashboard save. The focused command
   writes the enable flag and non-secret source metadata to
   `darkharrbor.conf` and the credential-bearing URL only to
   `secrets.sealed`. It does not rewrite AIOStreams or StremThru.

Remote playback uses **two** client-reachable HTTPS origins: AIOStreams for the
installed aggregate manifest, and DarkHarrbor's restricted listener on 8382 for
proxied playback and the DarkHarrbor Library manifest. Port 8381 remains private
between containers.

### Maintainers: controlled upstream upgrades

Ordinary deployments leave `AIOSTREAMS_IMAGE` and `STREMTHRU_IMAGE` unset and
use the immutable Compose defaults. To evaluate an update, resolve its digest;
never pass `latest` to the gate:

```sh
./scripts/verify-aggregator-compatibility.sh clean \
  ghcr.io/viren070/aiostreams@sha256:<candidate>
./scripts/verify-aggregator-compatibility.sh upgrade \
  ghcr.io/viren070/aiostreams@sha256:<candidate>
```

The clean gate drives Chromium through the real first-user Torznab dialog and
checks the StremThru caps endpoint. The upgrade gate starts the locked version,
then the candidate against the same initialized data volume and repeats the UI
contract. Both use unique containers, network, volumes, loopback-only random
ports, private temporary environment files, no host mounts, and no Docker
socket; teardown is automatic.

These zero-credential gates deliberately do not invent a debrid account or
claim playback success. Before promotion, privately back up the real
AIOStreams data volume, upgrade an isolated copy containing a saved user, and
complete the genuine aggregator-originated playback-to-Arr acceptance proof.
Only then update the two Compose defaults, `compatibility.lock`, these field
instructions, and the sanitized acceptance evidence in one commit. A database
migration makes an image-only rollback unsafe; restore the pre-upgrade volume
snapshot together with the previous image.

### OPTIONAL: guarded saved-config helper

`scripts/configure-aggregator.py` automates only AIOStreams' authenticated
saved-user-config `GET`/`PUT` route. **It cannot bootstrap a clean install.** A
`desired.json` is one COMPLETE user config, and the documented way to author one
is to edit a snapshot of an instance that is ALREADY configured; on a fresh
aggregator the snapshot is an empty default, and this guide does not publish the
config document's schema. Installing addons and configuring the debrid service
for the first time is therefore a DASHBOARD task -- expect to need a browser
once. The helper's purpose is reproducing or re-asserting a config you have
already authored, not creating one. It does not change compose, deploy either
tier, or configure shared-tier providers. It is optional and is not a
prerequisite for installing or reproducing DarkHarrbor.

AIOStreams replaces the complete user config on `PUT`; it has no partial-update
or conditional-write API. The helper therefore uses an explicit baseline:

1. If live already equals desired, it sends no write.
2. If live differs from both baseline and desired, it refuses with exit 3.
3. If live equals baseline, it reads a second time, sends one `PUT`, and verifies
   the complete read-back.

The second read narrows, but cannot eliminate, the API's lack of an atomic
compare-and-swap. Do not save from the dashboard while the command is running.

AIOStreams re-encrypts environment-forced proxy values on every `PUT`, so their
ciphertext changes even when the effective value does not. The reference command
explicitly declares `/proxy/url` and `/proxy/credentials` as server-normalized.
A declared path must exist and be identical in baseline and desired or the
helper refuses before contacting the API; the option therefore cannot conceal
an intended edit. Omit these declarations where the server does not rewrite the
fields, and never add a path merely to silence unexplained drift.

From the repository root:

```sh
cp examples/aggregator-tier/credentials.example.json \
  examples/aggregator-tier/credentials.json
chmod 600 examples/aggregator-tier/credentials.json

python3 scripts/configure-aggregator.py snapshot \
  --credentials examples/aggregator-tier/credentials.json \
  --output examples/aggregator-tier/baseline.json

cp examples/aggregator-tier/baseline.json \
  examples/aggregator-tier/desired.json
# Edit desired.json as one COMPLETE AIOStreams user config.

python3 scripts/configure-aggregator.py apply \
  --credentials examples/aggregator-tier/credentials.json \
  --baseline examples/aggregator-tier/baseline.json \
  --desired examples/aggregator-tier/desired.json \
  --server-normalized-path /proxy/url \
  --server-normalized-path /proxy/credentials
```

All three local JSON files are git-ignored. The baseline and desired files are
the decrypted user-config document and can contain addon tokens, indexer API
keys, proxy credentials, or parent-config credentials. The helper requires mode
`0600`, never prints their contents or the credentials-file values, and creates
snapshots at `0600`. Keep the complete files out of tickets and support
bundles. Basic authentication travels on every API call, so use HTTPS or a
trusted private network; never send it over an untrusted plaintext path.

Every successful apply or no-op prints the work it cannot perform: compose
settings, shared-tier provider creation/restart/pool verification, and the real
lookup/playback gate. Shared-tier Usenet servers remain dashboard-only in the
tested API (`/v0/usenet/server` returns 404).

---

## 3. Shared tier

Required, not advised. The shipped default points at a **public** instance, and
when that instance goes down **every debrid source loses resolution at once** —
the shared tier is the resolution path for all of them.

```yaml
BUILTIN_STREMTHRU_URL: "http://stremthru:8080"
STREMTHRU_TORZ_URL:    "http://stremthru:8080/v0/torznab"
STREMTHRU_STORE_URL:   "http://stremthru:8080/stremio/store"
```

Two traps:

- **The Torz addon carries its own `url` field**, independent of the builtin
  setting. The preset resolves `options.url || DEFAULT_URL`, so the environment
  variable seeds *new* configs only and a saved addon keeps pointing at the dead
  host. **The env var appears to work and does nothing.** Fix it in the addon's
  own configuration.
- **It is the dashboard's Torznab preset**, not a Stremio-addon preset. Set its
  complete Torznab URL to `http://stremthru:8080/v0/torznab` (no trailing
  `/api` — the addon appends `/api?t=caps` itself, and adding one produces a
  double-`/api` 404) and leave the API key blank. The locked dialog has no
  separate API-path control, so a `…/stremio/torz` value produces
  `GET /stremio/torz?t=caps` → 404. The environment variable seeds a new
  preset; the saved addon URL wins afterward.

For usenet, the shared tier also needs dashboard auth and a vault:

```yaml
STREMTHRU_AUTH: "user:password"      # also the aggregator's auth token
STREMTHRU_VAULT_SECRET: "<random>"   # REQUIRED for usenet
```

**The vault secret is one-way.** Lose it and every stored NNTP credential
becomes unreadable — same class of irreversibility as the DarkHarrbor seal
password.

---

## 4. Reachability — the part that bites

The shared tier's base URL must be reachable **by whoever fetches it**, and that
changes with your proxy posture:

| Posture | Who fetches | Must be reachable from |
|---|---|---|
| Selective (named services) | Viewing devices | Every client device |
| Universal (empty filter) | DarkHarrbor only | The container network |

**These conflict, and one variable cannot satisfy both.** Changing posture
silently moves the burden — treat posture and base URL as one change.

A published port bound to a single host interface is the classic failure:
Docker's DNAT rule excludes the container bridge, so a packet from inside is
never rewritten and times out. It presents as a flat ~10s `502` with no request
arriving upstream. A sibling container on the same bridge is unaffected and
looks healthy, so **one service working proves nothing about another**.

`darkharrbor doctor` catches this now — `aggregator egress: <backend>` reports
UNREACHABLE with the transport cause and the address tried.

---

## 5. Usenet

Indexers go in the **aggregator**, one addon per indexer. Providers go in the
**shared tier's dashboard**. Do not configure indexers in the shared tier — that
is for standalone use.

Which playback service you choose decides whether usenet can commit:

| Service | Commits? |
|---|---|
| Shared-tier newz | yes |
| Debrid usenet | yes |
| Aggregator's own built-in engine | **never** — hardcoded to bypass any proxy |
| NzbDAV / AltMount | **never**, outside the aggregator's built-in proxy |

Pick a provider before the pool exists and the pool caches empty — the shared
tier builds its NNTP pool lazily. **A green ping in the dashboard proves the
credential, not the pool.** Restart after adding servers and confirm the log
reports a non-zero server count.

Mind account-wide connection caps shared with DarkHarrbor's own NNTP ladder.

---

## 6. Verify

Do not infer from configuration. The aggregator applies environment forcing at
request time, so its **saved** config can disagree with its **effective** one.
Use one title that is absent from the target Arr and record the Arr's empty
baseline first.

1. Request a real stream lookup through the installed AIOStreams manifest. The
   DarkHarrbor Library addon must be queried, and the returned AIOStreams source
   URLs must all point at DarkHarrbor. Any source that bypasses it cannot commit.
2. Play one of those AIOStreams sources long enough to cross
   `HARRBOR_REACTIVE_THRESHOLD`. A generated URL, HEAD/200 probe, direct curl,
   or playback that bypasses the aggregator is not evidence.
3. Confirm the selected existing Arr now owns the title and reports an imported
   file (`hasFile=true`). For an acceptance run only, this may instead be the
   disposable clean Radarr. Its reactive-only direct `ManualImport` does not
   require S10; empty ffprobe media metadata is acceptable, but a missing file
   record is not. Then inspect `darkharrbor reactive-pending -action list`: a
   `weak_evidence` entry offering `acknowledge,undo` means the Arr commit
   completed but remains queued for review; it is not a failed write.
4. Run `darkharrbor doctor` after the playback evidence. Routine doctor no
   longer performs the invasive full AIOStreams catalog lookup that used to
   seed a second reactive identity; it truthfully skips that check while keeping
   bounded backend egress and durable delivery validation. Every selected
   aggregator, MediaFlow, HTTP-backend, reactive, Arr-reachability, and path
   check must be OK (intentional opt-outs may be SKIP).

---

## Non-goals

DarkHarrbor does **not** manage, deploy, proxy for, or health-check third-party
addons, and takes no dependency on any specific aggregator or shared tier.
This guide is documentation and example configuration, never runtime
ownership.

The optional helper performs one guarded, operator-authorized saved-config
replacement. It is not a one-click installer or a controller: a meaningful
share of the settings above are not environment-lockable, so automation can set
them once but cannot keep them set. See `CONFIGURATION-INVENTORY.md` §4.
