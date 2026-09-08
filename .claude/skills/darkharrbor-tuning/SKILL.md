---
name: darkharrbor-tuning
description: Measure and tune DarkHarrbor playback performance against a live deployment. Use when asked to make playback faster, diagnose slow starts or seeks, investigate 503s on library items, compare environment-knob settings, or interpret the Prometheus /metrics endpoint. Runs repeatable before/after experiments rather than guessing at hot paths.
---

# DarkHarrbor performance tuning

The point of this skill is that **nothing here is inferred from reading
code**. Every number comes from the running deployment. Do not propose a
knob change, or a code change, without a measurement that motivates it and a
second measurement that confirms it.

The harness is `scripts/dh-tune.sh`.

## The one thing to know first

`/metrics` is served on the **admin port (8381)**, which is deliberately
*not* published to the host. `curl 127.0.0.1:8382/metrics` returns 404 and
looks like the endpoint is disabled. It isn't — `HARRBOR_METRICS_ENABLED`
defaults to `true`. Every scrape has to go through the container:

```
docker compose -f .darkharrbor/compose.yml exec -T darkharrbor \
  wget -qO- http://127.0.0.1:8381/metrics
```

`dh-tune.sh` does this for you. Port **8382** is the stream port and *is*
published, which is what makes client-side timing possible with plain `curl`.

## Measurement protocol

The counters are cumulative since process start, so a before/after scrape
brackets exactly one experiment. `run` does this automatically and reports
the **delta**, which is the only figure that reflects the knobs currently in
force — a lifetime average stops moving once the process is old.

```
scripts/dh-tune.sh run baseline-defaults 24    # establish a baseline
# edit .darkharrbor/compose.yml, change ONE knob, restart the service
scripts/dh-tune.sh run warmconns-8 24          # same sweep, one variable
scripts/dh-tune.sh compare                     # side by side with knobs
```

Rules that keep results honest:

- **One knob per run.** Two changes give you one uninterpretable number.
- **Same `n` and the same `DARKHARRBOR_RANGE` across runs you intend to
  compare.** The sweep samples randomly, so use `n` ≥ 24 before believing a
  difference, and re-run a baseline occasionally to gauge natural variance.
- **A restart resets every counter.** That is a feature — it is how you get
  a clean cold-start measurement — but it means a `run` straddling a restart
  reports nonsense deltas.
- `run` only issues ordinary range requests. It is exactly what a media
  player does, so it is safe against a live deployment, but it *does* cause
  real acquisition work and real upstream traffic.

## Reading the output

| metric | what it actually tells you |
|---|---|
| `first_byte_<lane>_<cold\|warm>_*` | **The headline number.** Time from request to first byte, split by lane and cache state. `count`/`total_ms`/`max_ms`, so mean and worst case. |
| `source_first_byte_milliseconds` | Time the **upstream origin** took. Subtract this from the above and the remainder is DarkHarrbor's own resolve path. |
| `nntp_cold_acquires_total` vs `nntp_warm_acquires_total` | Fresh TCP+TLS+AUTHINFO dials vs reused connections. Directly measures whether `HARRBOR_NNTP_WARM_CONNS` is set high enough. |
| `disk_hits` / `disk_misses` | Range cache effectiveness. |
| `governor_denials_total` | Work refused by admission control. Non-zero means the governor is the constraint, not the network. |
| `items_total{state=}` | `removed` items still have `.strm` files on disk pointing at them. |

The decisive comparison is **lane first-byte vs source first-byte**. If a
lane takes seconds while its sources answer in milliseconds, the time is
being spent inside DarkHarrbor, and no provider-side or network tuning will
help.

## Knobs worth reaching for

All 78 environment-only settings are documented in `CONFIGURATION.md` under
"Environment-only settings". None of them are on the `darkharrbor.conf`
allowlist — putting one there yields a key-only warning and the default
silently in force. They must be set as `environment:` entries in
`.darkharrbor/compose.yml`, which survives `install.sh --upgrade` because
the upgrade rewrites only the two `image:` lines.

Start here, one at a time:

- `HARRBOR_NNTP_WARM_CONNS` (default `1`) — warm connections held per
  provider. Raise only if a `run` shows cold acquires dominating warm ones.
- `HARRBOR_NNTP_TOTAL_CONNECTIONS` / `_DEMAND_CONNECTIONS` /
  `_READAHEAD_CONNECTIONS` (16/8/8) — the pool split. Demand serves the
  player; readahead fills ahead of it.
- `HARRBOR_READAHEAD_MAX_SEGMENTS` (16) and
  `HARRBOR_READAHEAD_SEEK_WIDEN_MS` (0) — seek responsiveness.
- `HARRBOR_PREWARM_HOTHEAD_MB` (32) and `HARRBOR_NNTP_PREWARM_BYTES`
  (16 MiB) — how much leading data is fetched before playback starts.
  Bigger means a slower first frame but fewer early stalls.
- `HARRBOR_PREWARM_TIMEOUT_SEC` (45) — bounds one prewarm call.

## Interpreting failures, not just latency

A sweep reports `503` and `404` alongside `206`, and those matter as much as
timing:

- **`404`, instant** — the token or item is unknown. A stale `.strm` pointing
  at a removed item. `darkharrbor readopt` audits these.
- **`503` after a long uniform delay** — resolution was attempted and
  exhausted. If many items fail at *the same* duration, that is a fixed
  deadline being hit, not variable network conditions; find the deadline
  rather than tuning throughput.
- **`206` but slow** — the interesting case. Compare against
  `source_first_byte_milliseconds` to see whether the origin or the resolve
  path owns the time.

A `503` on genuinely dead content is *correct behaviour*. Taking twenty
seconds to say so is still a user-experience problem, and it is a different
problem from making a working play faster. Keep the two separate.

## Before proposing code changes

Exhaust configuration first. The perf surface largely *is* the environment
surface, and a compose edit is reversible in a way a code change is not. Only
after a measurement shows a cost that no knob can move should new
instrumentation or a code change be on the table — and then say precisely
which number you expect to improve, and by how much.
