#!/usr/bin/env bash
# dh-tune.sh -- repeatable knob-tuning measurement harness for a live
# DarkHarrbor deployment.
#
# Everything here is read-only against the deployment except `run`, which
# only issues ordinary range requests -- the same thing a media player does.
# No knob is ever written by this script; you change knobs yourself in
# .darkharrbor/compose.yml and re-run, so every experiment is one variable
# you chose.
#
# The measurement idea: DarkHarrbor's /metrics counters are cumulative since
# process start, so a before/after scrape brackets exactly one experiment.
# Dividing the *delta* of first_byte_<lane>_<state>_total_ms by the delta of
# its _count gives the mean first-byte latency for that run alone, rather
# than a lifetime average that never moves once the process is old.

set -uo pipefail

COMPOSE="${DARKHARRBOR_COMPOSE:-./docker-compose.yml}"
SERVICE="${DARKHARRBOR_SERVICE:-darkharrbor}"
ADMIN_URL="${DARKHARRBOR_ADMIN_URL:-http://127.0.0.1:8381}"
STREAM_BASE="${DARKHARRBOR_STREAM_BASE:-http://127.0.0.1:8382}"
LIBRARY="${DARKHARRBOR_LIBRARY:-/mnt/darkharrbor}"
# Deliberately outside the repo: runs accumulate raw metrics dumps and
# release titles, neither of which belongs in version control.
RESULTS="${DARKHARRBOR_RESULTS:-${XDG_STATE_HOME:-$HOME/.local/state}/darkharrbor-tuning}"
TIMEOUT="${DARKHARRBOR_TIMEOUT:-45}"
PARALLEL="${DARKHARRBOR_PARALLEL:-6}"
RANGE="${DARKHARRBOR_RANGE:-bytes=0-65535}"

die() { echo "dh-tune: $*" >&2; exit 1; }

dc() { docker compose -f "$COMPOSE" "$@"; }

# --- metrics -----------------------------------------------------------

# The Prometheus endpoint is bound to the ADMIN port, which is deliberately
# not published to the host, so every scrape goes through the container.
scrape() {
  dc exec -T "$SERVICE" wget -qO- --timeout=10 "$ADMIN_URL/metrics" 2>/dev/null \
    || die "scrape failed -- is the deployment up? (docker compose -f $COMPOSE ps)"
}

# rc <file> <field>  -> value of darkharrbor_rangecache{field="..."}
rc() {
  awk -v f="$2" '
    $0 ~ "^darkharrbor_rangecache\\{field=\"" f "\"\\}" { print $2; exit }
  ' "$1"
}

# metric <file> <name>  -> summed value of a metric across all label sets
metric() {
  awk -v m="$2" '
    index($0, "darkharrbor_" m) == 1 { s += $NF } END { printf "%.0f", s+0 }
  ' "$1"
}

# --- knob provenance ---------------------------------------------------

# Records which HARRBOR_* variables are actually set on the running
# container. Anything absent is at its compiled-in default, which is the
# state every fresh install starts in.
knobs() {
  dc exec -T "$SERVICE" env 2>/dev/null \
    | grep '^HARRBOR_' \
    | grep -Ev 'SECRET|TOKEN|PASSWORD|KEY|UNLOCK' \
    | sort
}

knobs_perf() {
  knobs | grep -E 'NNTP|PREWARM|READAHEAD|CACHE|STREAM|TORRENT|GOVERNOR' || true
}

# --- probing -----------------------------------------------------------

# The two lanes are served on DIFFERENT ports, and getting this wrong is the
# easiest way to generate a library-wide false alarm:
#
#   /stream  tokenized, published on 8382  -- the Stremio client port
#   /dav     UNauthenticated, 8381 only    -- the admin port, deliberately
#                                             not published, reached by
#                                             Jellyfin over the docker network
#
# Probing /dav on 8382 returns an instant 404 for every usenet item in the
# library, which looks exactly like mass corruption and is not. DAV_BASE is
# resolved to the container's own address, which the host can route to.
dav_base() {
  if [ -z "${DAV_BASE:-}" ]; then
    local ip
    ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' \
           "$(dc ps -q "$SERVICE" 2>/dev/null | head -1)" 2>/dev/null | awk '{print $1}')
    [ -n "$ip" ] || die "cannot resolve container address for the /dav lane"
    DAV_BASE="http://$ip:8381"
  fi
  echo "$DAV_BASE"
}

# A .strm file holds one URL pointing at the service's own advertised base.
# Rewrite the host to whichever endpoint actually serves that route.
strm_url() {
  local line base
  line=$(head -n1 "$1" 2>/dev/null | tr -d '\r\n')
  [ -n "$line" ] || return 1
  case "$line" in
    */dav/*) base="${DAV_BASE:?dav base not resolved}" ;;
    *)       base="$STREAM_BASE" ;;
  esac
  echo "$line" | sed -E "s#^https?://[^/]+#${base}#"
}

# probe_one <strm> -> "code<TAB>ttfb<TAB>bytes<TAB>name"
probe_one() {
  local f="$1" u code
  u=$(strm_url "$f") || { printf 'ERR\t0\t0\t%s\n' "$(basename "$f")"; return; }
  code=$(curl -s -o /dev/null --max-time "$TIMEOUT" \
           -H "Range: $RANGE" \
           -w '%{http_code}\t%{time_starttransfer}\t%{size_download}' \
           "$u" 2>/dev/null) || code=$'000\t0\t0'
  printf '%s\t%s\n' "$code" "$(basename "$f")"
}

# --- subcommands -------------------------------------------------------

cmd_snapshot() {
  local out="${1:-}"
  if [ -n "$out" ]; then scrape > "$out"; echo "wrote $out"; else scrape; fi
}

cmd_report() {
  local m="${1:-}" tmp=""
  if [ -z "$m" ]; then
    tmp=$(mktemp); scrape > "$tmp"; m="$tmp"
  fi
  [ -s "$m" ] || die "empty metrics file: $m"

  echo "=== first byte by lane (cumulative since process start) ==="
  printf '%-8s %-6s %8s %12s %10s\n' lane state count mean_ms max_ms
  local lane state c t mx
  for lane in http nntp torrent; do
    for state in cold warm; do
      c=$(rc "$m" "first_byte_${lane}_${state}_count")
      t=$(rc "$m" "first_byte_${lane}_${state}_total_ms")
      mx=$(rc "$m" "first_byte_${lane}_${state}_max_ms")
      c=${c:-0}; t=${t:-0}; mx=${mx:-0}
      [ "${c%.*}" -eq 0 ] 2>/dev/null && { printf '%-8s %-6s %8s %12s %10s\n' "$lane" "$state" 0 - -; continue; }
      printf '%-8s %-6s %8s %12s %10s\n' "$lane" "$state" "$c" \
        "$(awk -v t="$t" -v c="$c" 'BEGIN{printf "%.0f", t/c}')" "$mx"
    done
  done

  echo
  echo "=== cache ==="
  local dh dm
  dh=$(rc "$m" disk_hits); dm=$(rc "$m" disk_misses)
  dh=${dh:-0}; dm=${dm:-0}
  printf 'disk_hits=%s disk_misses=%s hit_rate=%s%%\n' "$dh" "$dm" \
    "$(awk -v h="$dh" -v s="$dm" 'BEGIN{d=h+s; printf "%.1f", (d>0)?100*h/d:0}')"
  printf 'pinned_entries=%s pinned_bytes=%s inflight=%s\n' \
    "$(rc "$m" pinned_entries)" "$(rc "$m" pinned_bytes)" "$(rc "$m" inflight)"

  echo
  echo "=== nntp pool ==="
  grep -E '^darkharrbor_nntp_(warm_pool_(open|target)|cold_acquires_total|warm_acquires_total)' "$m" \
    | sed 's/darkharrbor_nntp_//' || echo "(none)"

  echo
  echo "=== upstream source first byte (origin only, excludes our own path) ==="
  grep -E '^darkharrbor_source_first_byte_milliseconds' "$m" \
    | sed -E 's/.*lane="([^"]+)".*source="([^"]+)"\} (.*)/  lane=\1 src=\2 \3ms/' \
    | head -20 || echo "(no observations yet)"

  echo
  echo "=== governor denials ==="
  grep -E '^darkharrbor_governor_denials_total\{' "$m" | sed 's/darkharrbor_//' || echo "  (none)"

  [ -n "$tmp" ] && rm -f "$tmp"
  return 0
}

# DARKHARRBOR_ROUTE restricts the sample to one lane. Lanes are visible in
# the .strm URL itself: /stream is the http+torrent lane, /dav is the usenet
# lane. Without this, a random sample is dominated by whichever lane holds
# most of the library and the smaller lane is never exercised at all.
cmd_sweep() {
  local n="${1:-24}"
  mkdir -p "$RESULTS"
  local list; list=$(mktemp)
  local all; all=$(mktemp)
  find "$LIBRARY" -name '*.strm' -type f 2>/dev/null > "$all"
  if [ -n "${DARKHARRBOR_ROUTE:-}" ]; then
    echo "restricting sample to route /${DARKHARRBOR_ROUTE}" >&2
    xargs -a "$all" -d'\n' -n50 grep -lE "^https?://[^/]+/${DARKHARRBOR_ROUTE}/" 2>/dev/null \
      | shuf -n "$n" > "$list"
  else
    shuf -n "$n" "$all" > "$list"
  fi
  rm -f "$all"
  local total; total=$(wc -l < "$list")
  [ "$total" -gt 0 ] || die "no .strm files under $LIBRARY"
  echo "probing $total items, $PARALLEL at a time, ${TIMEOUT}s timeout, Range: $RANGE" >&2

  DAV_BASE=$(dav_base)
  export -f probe_one strm_url
  export STREAM_BASE DAV_BASE TIMEOUT RANGE
  local out; out=$(mktemp)
  # shellcheck disable=SC2016
  xargs -a "$list" -I{} -P "$PARALLEL" bash -c 'probe_one "$1"' _ {} 2>/dev/null > "$out"

  sort -t$'\t' -k1,1 -k2,2n "$out"
  echo
  echo "=== outcome distribution ==="
  cut -f1 "$out" | sort | uniq -c | sort -rn
  echo
  echo "=== TTFB of successful (206) plays ==="
  awk -F'\t' '$1==206 {print $2}' "$out" | sort -n | awk '
    {v[n++]=$1}
    END{
      if(n==0){print "  none"; exit}
      printf "  n=%d  min=%.2fs  p50=%.2fs  p90=%.2fs  max=%.2fs\n",
        n, v[0], v[int(n*0.5)], v[int(n*0.9)], v[n-1]
    }'
  echo
  echo "=== slow failures (>5s to an error) ==="
  awk -F'\t' '$1!=206 && $2>5 {printf "  %s  %.1fs  %s\n", $1, $2, $4}' "$out" | head -10
  cp "$out" "$RESULTS/sweep-$(date -u +%Y%m%dT%H%M%SZ).tsv"
  rm -f "$list" "$out"
}

# run <label> [n] -- the full experiment: bracket a sweep with two scrapes
# and report the per-run deltas, which is the only number that reflects the
# knobs currently in force rather than the process's whole history.
cmd_run() {
  local label="${1:?usage: dh-tune.sh run <label> [n]}" n="${2:-24}"
  mkdir -p "$RESULTS"
  local stamp; stamp=$(date -u +%Y%m%dT%H%M%SZ)
  local dir="$RESULTS/$stamp-$label"
  mkdir -p "$dir"

  knobs_perf > "$dir/knobs.txt"
  echo "=== knobs in force (perf-relevant, explicitly set) ==="
  if [ -s "$dir/knobs.txt" ]; then cat "$dir/knobs.txt"; else echo "  (none set -- all compiled-in defaults)"; fi
  echo

  scrape > "$dir/before.txt"
  cmd_sweep "$n" | tee "$dir/sweep.txt"
  scrape > "$dir/after.txt"

  echo
  echo "=== PER-RUN first byte (delta between the two scrapes) ==="
  printf '%-8s %-6s %8s %12s\n' lane state count mean_ms
  local lane state c0 c1 t0 t1 dc dt
  for lane in http nntp torrent; do
    for state in cold warm; do
      c0=$(rc "$dir/before.txt" "first_byte_${lane}_${state}_count"); c0=${c0:-0}
      c1=$(rc "$dir/after.txt"  "first_byte_${lane}_${state}_count"); c1=${c1:-0}
      t0=$(rc "$dir/before.txt" "first_byte_${lane}_${state}_total_ms"); t0=${t0:-0}
      t1=$(rc "$dir/after.txt"  "first_byte_${lane}_${state}_total_ms"); t1=${t1:-0}
      dc=$(awk -v a="$c1" -v b="$c0" 'BEGIN{printf "%.0f", a-b}')
      dt=$(awk -v a="$t1" -v b="$t0" 'BEGIN{printf "%.0f", a-b}')
      [ "$dc" -gt 0 ] 2>/dev/null || continue
      printf '%-8s %-6s %8s %12s\n' "$lane" "$state" "$dc" \
        "$(awk -v t="$dt" -v c="$dc" 'BEGIN{printf "%.0f", t/c}')"
    done
  done

  echo
  echo "=== NNTP dials consumed by this run ==="
  local p
  for p in $(grep -oE 'provider="[^"]+"' "$dir/after.txt" | sort -u | sed 's/provider="//; s/"//'); do
    local a0 a1 w0 w1
    a0=$(awk -v p="$p" '$0 ~ "nntp_cold_acquires_total\\{provider=\""p"\"\\}" {print $2}' "$dir/before.txt"); a0=${a0:-0}
    a1=$(awk -v p="$p" '$0 ~ "nntp_cold_acquires_total\\{provider=\""p"\"\\}" {print $2}' "$dir/after.txt");  a1=${a1:-0}
    w0=$(awk -v p="$p" '$0 ~ "nntp_warm_acquires_total\\{provider=\""p"\"\\}" {print $2}' "$dir/before.txt"); w0=${w0:-0}
    w1=$(awk -v p="$p" '$0 ~ "nntp_warm_acquires_total\\{provider=\""p"\"\\}" {print $2}' "$dir/after.txt");  w1=${w1:-0}
    printf '  %-14s cold=+%s warm=+%s\n' "$p" \
      "$(awk -v a="$a1" -v b="$a0" 'BEGIN{printf "%.0f",a-b}')" \
      "$(awk -v a="$w1" -v b="$w0" 'BEGIN{printf "%.0f",a-b}')"
  done

  echo
  echo "results saved under $dir"
}

cmd_compare() {
  mkdir -p "$RESULTS"
  echo "=== runs recorded ==="
  local d
  for d in "$RESULTS"/*/; do
    [ -d "$d" ] || continue
    [ -f "$d/sweep.txt" ] || continue
    printf '%s\n' "$(basename "$d")"
    grep -A1 'TTFB of successful' "$d/sweep.txt" | tail -n1
    if [ -s "$d/knobs.txt" ]; then
      sed 's/^/    /' "$d/knobs.txt"
    else
      echo "    (all defaults)"
    fi
    echo
  done
}

usage() {
  cat <<'EOF'
dh-tune.sh -- DarkHarrbor knob-tuning measurement harness

  run <label> [n]     full experiment: scrape, sweep n items, scrape, report deltas
  sweep [n]           probe n random library items, report outcome + TTFB spread
  report [file]       decode a metrics snapshot (or scrape live) into a readable table
  snapshot [file]     dump raw /metrics
  knobs               show HARRBOR_* actually set on the container
  compare             list past runs with their knobs and TTFB, to diff settings

Environment overrides:
  DARKHARRBOR_COMPOSE      compose file (default the installer-managed one)
  DARKHARRBOR_STREAM_BASE  host-reachable stream base (default http://127.0.0.1:8382)
  DARKHARRBOR_LIBRARY      .strm root (default /mnt/darkharrbor)
  DARKHARRBOR_RESULTS      results dir (default ~/.local/state/darkharrbor-tuning)
  DARKHARRBOR_TIMEOUT      per-probe seconds (default 45)
  DARKHARRBOR_PARALLEL     concurrent probes (default 6)
  DARKHARRBOR_RANGE        Range header (default bytes=0-65535)
  DARKHARRBOR_ROUTE        restrict sample to one lane: "stream" (http+torrent) or "dav" (usenet)
EOF
}

case "${1:-}" in
  run)      shift; cmd_run "$@" ;;
  sweep)    shift; cmd_sweep "$@" ;;
  report)   shift; cmd_report "$@" ;;
  snapshot) shift; cmd_snapshot "$@" ;;
  knobs)    knobs ;;
  compare)  cmd_compare ;;
  ""|-h|--help|help) usage ;;
  *) usage; exit 2 ;;
esac
