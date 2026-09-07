# DarkHarrbor

Arr, mateys! Welcome to DarkHarrbor, the dark port for your media library. No
matter which seas ye sail for treasure: torrents, usenet, the open web, or yer
own private stash, this is where it all comes ashore, gets logged in the
harbor book, and waits quietly in port 'til ye come collect it.

DarkHarrbor sits between your Arr fleet (Sonarr, Radarr, Prowlarr) and
whatever sources you point it at. It's the download client, the indexer, and
the harbor master all at once: grabs come in and show up in your library as a
lightweight claim ticket (a tiny `.strm` file) instead of the full cargo.

That ticket isn't a receipt for something already unloaded, though. It's a
standing order to the harbor master. Play it, and DarkHarrbor fetches the real
bytes on the spot and streams them straight to your player, no waiting for a
slow ship to dock first. Watch it again later and it's cached for a quick
rewatch, but the ticket itself never goes stale: lose a provider, and
DarkHarrbor quietly finds another ship carrying the same cargo without you
lifting a finger. Nothing ever needs a full hold's worth of storage waiting
below deck just in case you might want it someday.

This crew keeps no logbook for anyone but you: no telemetry, no signals sent
back to some distant admiralty. Where a pirate gets his booty is his business.
The only ships that leave this port are the ones you sent out yourself.

## What's in the hold

Bring whatever you've got. DarkHarrbor doesn't care which flag you sail
under. Point it at any combination of:

- **Torrent + debrid**: TorBox, Real-Debrid, AllDebrid, or Premiumize, with
  automatic fallback across whichever ones you've set up.
- **Usenet**: direct NNTP with multi-provider failover, or TorBox's own
  cached NZB lane.
- **The Internet Archive**: public-domain treasure, no account required.
- **Self-hosted streaming backends**: OMSS-compatible sources like Cinepro.
- **Your own declared sources**: hand it a typed list of direct links you
  already trust — plain HTTP, HLS, M3U playlists, metalink, S3-style
  objects, or a
  [WebDAV collection](CONFIGURATION.md#private-cloud-drives-through-webdav)
  served by your own rclone (Google Drive, Dropbox, wherever). No
  searching, no account, no upstream credentials touching DarkHarrbor
  itself: you declare exactly what's there.

TorBox and Real-Debrid have earned their sea legs through real testing.
AllDebrid, Premiumize, and the private-cloud lane are built and working, just
newer to open water: think of them as crew that just signed on, not
battle-tested yet, but perfectly seaworthy.

However you mix it, sources fail over to each other automatically, and
DarkHarrbor never hands you a lookalike file for the real thing: recovery
across providers is only ever trusted when the bytes themselves prove it's
the same cargo.

## It finds your Arrs and does the paperwork itself

Run `darkharrbor setup`, the wizard, and it goes looking for your existing
Sonarr, Radarr, and Prowlarr on its own: no manual pointing, no copying API
keys by hand. It reads each one's configuration straight out of its own
container and verifies the key itself before it ever asks you for anything.
(Want the full stage-by-stage play-by-play, or would rather have an AI drive
it for you? See [Getting Started](docs/GETTING-STARTED.md) below.)

Once it's found them, it pulls your actual root folder and quality profile
straight from each Arr's own live settings instead of asking you to retype
anything, then registers the indexers and download clients for you. To your
Arrs, DarkHarrbor looks like qBittorrent and SABnzbd, so there's no special
client type to configure.

Hand it a TorBox account and it reads your plan tier itself: what you're
allowed, how many slots you get, and sets its own limits to match. No
guessing, no manual tuning. Same idea for usenet: give it your NNTP
credentials and it verifies up front that they actually support what you're
asking for (cached NZB, direct NNTP, or both) instead of letting a bad
credential surface as a mystery failure three steps later.

There's also a choice of three performance profiles (Recommended, Low
Memory, High Throughput) that the wizard picks sensibly based on your
hardware, how much memory you've got and how much room's left in the hold,
or you can choose one yourself if you know better.

Season packs get split into per-season releases so Sonarr can actually
import them, instead of rejecting the pack because it "already has" a
lower-scoring single episode. An optional cache-aware mode sits in front of
Prowlarr and only shows your Arrs results that are actually ready to grab
right now.

## A crew that watches the tide for you

Behind the scenes, a governor keeps a weather eye on how hard DarkHarrbor
leans on each provider, so a burst of grabs never gets your account
rate-limited or flagged. A stalled download doesn't walk the plank just
because it's having a slow day: DarkHarrbor gives it real time to recover
before writing it off, since swarms and slow segments come back to life more
often than you'd think. Nothing gets marked dead on a hunch, on any lane.

A keep-warm janitor keeps a keen eye on what you've been watching lately and
asks the provider to keep those copies ready, so a rewatch never means
waiting on a cold fetch again. When it's actually time to clear something
out, the same crew handles that quietly too, swabbing the deck without you
having to ask.

Usenet posts get more than a simple fetch-and-hope. Segments are read ahead
of where you actually are so seeking never stutters, and if one comes back
damaged or missing, DarkHarrbor can reconstruct it on the fly from the
recovery data bundled with real releases, live, mid-stream, checking its own
work twice before it ever hands you a byte. Whatever sea you're sailing, it
all adds up to the same thing: smooth sailing, all the way to the credits.

## Play it, and it just shows up in your library

This is the part that feels like real treasure the first time you see it:
hook DarkHarrbor up to a self-hosted aggregator, any Stremio-compatible one
you've already got running (AIOStreams is the one we've charted a course
through, but the mechanism itself doesn't care whose flag it flies), and
just watch something. Nothing has to be searched or grabbed in Sonarr/Radarr
first. Play it from any real Stremio-compatible client, and once you've
watched past a threshold you set, DarkHarrbor quietly adds it to your
library and hands ownership to the right Arr behind the scenes. No
searching, no grabbing, no babysitting a download. You just watched a movie
and it's yours now.

Titles you keep coming back to can also be *promoted*: instead of staying a
lightweight claim ticket re-resolved every time you press play, a promoted
title gets fully downloaded and owned outright, once you've got the space
for it.

Prefer to browse your existing library straight from a Stremio client
instead of Jellyfin? DarkHarrbor ships its own first-party Stremio addon
for exactly that.

Setting this part up lives outside DarkHarrbor itself, since the aggregator
is your own separate deployment: see
[docs/AGGREGATOR-INTEGRATION.md](docs/AGGREGATOR-INTEGRATION.md) for the
full setup.

## Built right, no surprises

- **Nothing gets hauled ashore 'til you actually want it.** Grabs write a
  tiny pointer file the moment they land; the real media is only fetched
  the first time it's played, then cached for rewatches.
- **Every pointer heals itself.** Lose a provider, and DarkHarrbor quietly
  finds another one carrying the exact same cargo, byte-proven, not just
  a similar-looking file. It'll even patch a damaged spot in one source
  using verified bytes pulled from a completely different one, torrent,
  usenet, or the open web, whichever one's actually got it.
- **Multi-season packs, actually usable.** Grab one cached season out of a
  full-series pack, or import a season pack Sonarr would normally reject as
  a duplicate, split cleanly per season on the way in. No other download
  client does this.
- **Your keys live in the captain's own chest, and the captain is you.**
  Sealed and locked (argon2id + AES-256-GCM), never in the image, compose
  file, or git. Not even the crew below decks gets a look inside: the keys
  never leave the chest to begin with, so a compromised helper process
  (like ffprobe) can't walk off with them either.
- **The ship itself is hardened.** Read-only root filesystem, every
  capability dropped, runs as a non-root user.
- **One command tells you what's wrong.** `darkharrbor doctor` checks every
  configured provider, Arr, and file path mapping for real, so you're never
  left guessing why something isn't working
  ([Diagnostics](CONFIGURATION.md#diagnostics)). Point a Prometheus/Grafana
  setup at it too: connection pool health, per-source reliability, and
  more are all there for the watching.
- **Jellyfin gets looked after automatically.** The wizard installs and
  maintains the ffmpeg wrapper it needs. (The core playback mechanism
  isn't Jellyfin-exclusive either: anything that reads `.strm` files and
  HTTP range requests works.)
- **Backups you can actually trust.** Real crash-consistent snapshots, not
  a raw file copy, plus a separate encrypted bundle you can carry off-box
  whenever you like ([Backup and Restore](CONFIGURATION.md#backup-and-restore)).
  The recovery key is deliberately never bundled in either one.

## Weigh anchor

```sh
git clone https://github.com/darkharrbor/darkharrbor.git
cd darkharrbor
./install.sh
```

The wizard takes it from there. For the full step-by-step, or if you'd
rather hand the whole thing to an AI assistant and let it drive, see:

- [docs/GETTING-STARTED.md](docs/GETTING-STARTED.md) — the complete
  walkthrough, stage by stage.
- **AI-assisted setup** — copy-paste a prompt into Claude (or Claude Code)
  and have it walk you through the install, or run the whole thing for you.
  See [.claude/skills/](.claude/skills/).
- [CONFIGURATION.md](CONFIGURATION.md) — every setting, tuning knob, and
  advanced option.
- [CONFIGURATION.md § Subcommands](CONFIGURATION.md#subcommands) — every
  CLI command, from `doctor` and `backup` to the reactive-library tools.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — how it actually works
  underneath, for anyone who wants the mechanism-level detail this README
  deliberately doesn't carry.

## License

MIT. See [LICENSE](LICENSE).
