#!/usr/bin/env bash
# HARRBOR_STRM_WRAPPER v2
# harrbor-ffprobe-wrapper.sh — DarkHarrbor .strm-mode ffprobe shim.
#
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Dark Harrbor
#
# Drop-in replacement for an arr container's ffprobe. When the probe input is
# a DarkHarrbor .strm sidecar (a text file whose first http(s) line is a DH
# /stream URL), this POSTs the URL + the arr's original ffprobe args (minus
# the input token) to DH's relay endpoint (REDESIGN S10 Gate 5,
# POST /api/v1/probe). DH runs a real network-capable ffprobe-full against
# the URL server-side and relays back real stdout + the real exit code,
# which this shim reproduces byte-for-byte and propagates. For every other
# input it passes straight through to the arr's original, file-only ffprobe.
#
# v2 (Gate 6) replaces v1's local ffprobe-full exec with this relay POST:
# ffprobe-full now lives ONLY in the DH image, not copied into every arr
# container. Relay-unreachable/error is a FAIL-CLOSED condition (distinct
# non-zero exit, never a silent local fallback) so the arr sees a real error
# instead of hanging or getting a stale local binary's answer.
#
# This is a clean-room implementation written from a behavioral specification.
# It contains no source code from any third-party project.
#
# Install layout (created by harrbor-strm-init.sh / DH's shiminstall):
#   /app/<app>/bin/ffprobe        -> this wrapper (what the arr now invokes)
#   /app/<app>/bin/ffprobe.real   -> the arr's original file-only ffprobe
#   curl (preferred) or wget on $PATH -- this is the ONLY external dependency
#   beyond a POSIX-ish awk + sh/bash, both already required by v1.
#
# NEVER use `set -e`: a probe that exits non-zero must surface as ffprobe's own
# exit status, not a shell abort, or the arr import pipeline misreads the result.

# Resolve this script's directory so we can find ffprobe.real.
self="$0"
case "$self" in */*) : ;; *) self="$(command -v -- "$self" 2>/dev/null || echo "$self")" ;; esac
dir="$(cd -- "$(dirname -- "$self")" 2>/dev/null && pwd)"

real="$dir/ffprobe.real"

# Tunables (env-overridable).
RELAY_URL="${HARRBOR_STRM_RELAY_URL:-http://darkharrbor:8381/api/v1/probe}"
# BUGFIX 2026-07-01: was 25s -- found live to be SHORTER than the
# relay server's own exec timeout (bumped to 45s the same session, see
# config.go), meaning this client-side curl timeout always fired first
# and aborted the connection before the server's own timeout logic ever
# got a chance to run cleanly. Set comfortably longer than the server
# default so the server's 502-on-timeout path is what actually fires.
RELAY_TIMEOUT="${HARRBOR_STRM_RELAY_TIMEOUT:-50}"
RELAY_SOURCE="${HARRBOR_STRM_RELAY_SOURCE:-$(hostname 2>/dev/null || echo unknown)}"
LOG="${HARRBOR_STRM_LOG:-}"

logline() {
  [ -n "$LOG" ] || return 0
  printf '%s harrbor-ffprobe: %s\n' "$(date -u +%FT%TZ 2>/dev/null)" "$*" >>"$LOG" 2>/dev/null
  return 0
}

passthrough() {
  if [ -x "$real" ]; then
    logline "passthrough -> ffprobe.real"
    exec "$real" "$@"
  fi
  logline "ERROR ffprobe.real missing at $real; cannot passthrough"
  exit 127
}

# Options whose following token is a VALUE (not the input). A superset is
# harmless; the only risk of an omission is mis-detecting the input, which at
# worst falls through to passthrough (stock behavior), never corruption.
is_value_opt() {
  case "$1" in
    -f|-loglevel|-v|-print_format|-of|-show_entries|-select_streams|-read_intervals|\
    -probesize|-analyzeduration|-rw_timeout|-timeout|-protocol_whitelist|-user_agent|\
    -headers|-fpsprobesize|-max_probe_packets|-o|-sources|-format) return 0 ;;
  esac
  return 1
}

# --- JSON string encode: writes a JSON-quoted string (with outer quotes) to
# stdout for the given argument. Handles backslash/quote/newline/tab/CR --
# the only characters realistically present in a ffprobe argv token or a DH
# /stream URL. (No control chars are expected in either; this is not a
# general-purpose JSON encoder.)
json_str() {
  printf '%s' "$1" | awk '
    BEGIN { printf "\"" }
    NR > 1 { printf "\\n" }
    {
      line = $0
      n = length(line)
      i = 1
      runstart = 1
      while (i <= n) {
        c = substr(line, i, 1)
        if (c == "\\" || c == "\"" || c == "\t" || c == "\r") {
          if (i > runstart) printf "%s", substr(line, runstart, i - runstart)
          if (c == "\\") printf "\\\\"
          else if (c == "\"") printf "\\\""
          else if (c == "\t") printf "\\t"
          else if (c == "\r") printf "\\r"
          i++
          runstart = i
        } else {
          i++
        }
      }
      if (i > runstart) printf "%s", substr(line, runstart, i - runstart)
    }
    END { printf "\"" }
  '
}

# --- JSON string decode of the response body's "stdout" field, streamed
# straight to $2 (a file). Single pass, run-batched (only stops char-by-char
# at escape/quote boundaries) so large ffprobe JSON output doesn't pay a
# per-character printf cost. Handles the full escape set Go's
# encoding/json can emit for this field: \" \\ \/ \n \t \r \b \f and \uXXXX
# (BMP only -- Go never emits surrogate pairs; astral-plane chars pass
# through as raw UTF-8, unescaped). Exit 3 if the "stdout" key isn't found
# (malformed/unexpected response shape).
decode_stdout_field() {
  awk -v outfile="$2" '
    function hexd(c) { return index("0123456789abcdef", tolower(c)) - 1 }
    function u2utf8(n,    b1, b2, b3) {
      if (n < 128) return sprintf("%c", n)
      if (n < 2048) { b1 = 192 + int(n / 64); b2 = 128 + (n % 64); return sprintf("%c%c", b1, b2) }
      b1 = 224 + int(n / 4096); b2 = 128 + int((n % 4096) / 64); b3 = 128 + (n % 64)
      return sprintf("%c%c%c", b1, b2, b3)
    }
    {
      line = $0
      key = "\"stdout\":\""
      p = index(line, key)
      if (p == 0) { found = 0; exit }
      found = 1
      i = p + length(key)
      n = length(line)
      runstart = i
      while (i <= n) {
        c = substr(line, i, 1)
        if (c == "\\" || c == "\"") {
          if (i > runstart) printf "%s", substr(line, runstart, i - runstart) >> outfile
          if (c == "\"") { closed = 1; break }
          i++
          e = substr(line, i, 1)
          if (e == "n") printf "\n" >> outfile
          else if (e == "t") printf "\t" >> outfile
          else if (e == "r") printf "\r" >> outfile
          else if (e == "b") printf "%c", 8 >> outfile
          else if (e == "f") printf "%c", 12 >> outfile
          else if (e == "\"") printf "\"" >> outfile
          else if (e == "\\") printf "\\" >> outfile
          else if (e == "/") printf "/" >> outfile
          else if (e == "u") {
            cp = hexd(substr(line, i + 1, 1)) * 4096 + hexd(substr(line, i + 2, 1)) * 256 + hexd(substr(line, i + 3, 1)) * 16 + hexd(substr(line, i + 4, 1))
            printf "%s", u2utf8(cp) >> outfile
            i += 4
          }
          else printf "%s", e >> outfile
          i++
          runstart = i
        } else {
          i++
        }
      }
      if (!closed && i > runstart) printf "%s", substr(line, runstart, i - runstart) >> outfile
    }
    END { if (!found) exit 3; if (!closed) exit 3 }
  ' "$1"
}

# --- POST url+args to the relay; on success writes decoded stdout to
# $stdoutfile and prints the numeric exit code on its own stdout line (the
# caller captures it). Any failure path logs and returns non-zero with
# nothing printed -- the caller treats that as fail-closed.
relay_probe() {
  url="$1"; shift
  workdir="$(mktemp -d 2>/dev/null)" || { logline "ERROR mktemp -d failed"; return 90; }
  reqfile="$workdir/req.json"
  bodyfile="$workdir/resp.json"
  stdoutfile="$workdir/stdout.bin"
  : >"$stdoutfile"

  {
    printf '{"url":'
    json_str "$url"
    printf ',"args":['
    first=1
    for a in "$@"; do
      [ "$first" -eq 1 ] || printf ','
      json_str "$a"
      first=0
    done
    printf '],"source":'
    json_str "$RELAY_SOURCE"
    printf '}'
  } >"$reqfile"

  http_code=""
  if command -v curl >/dev/null 2>&1; then
    http_code="$(curl -sS -m "$RELAY_TIMEOUT" -o "$bodyfile" -w '%{http_code}' \
      -H 'Content-Type: application/json' --data-binary @"$reqfile" "$RELAY_URL" 2>>"${LOG:-/dev/null}")"
    curl_rc=$?
    if [ "$curl_rc" -ne 0 ]; then
      logline "ERROR curl failed rc=$curl_rc url=$RELAY_URL"
      rm -rf "$workdir"
      return 90
    fi
  elif command -v wget >/dev/null 2>&1; then
    if ! wget -q -O "$bodyfile" --header='Content-Type: application/json' \
      --post-file="$reqfile" -T "$RELAY_TIMEOUT" "$RELAY_URL"; then
      logline "ERROR wget failed url=$RELAY_URL"
      rm -rf "$workdir"
      return 90
    fi
    http_code="200"  # wget already treats non-2xx as a hard failure above
  else
    logline "ERROR neither curl nor wget available; cannot reach relay"
    rm -rf "$workdir"
    return 90
  fi

  if [ "$http_code" != "200" ]; then
    errmsg="$(sed -n 's/.*"error":"\([^"]*\)".*/\1/p' "$bodyfile" 2>/dev/null | head -n1)"
    logline "ERROR relay http_code=$http_code error=${errmsg:-unknown} url=$RELAY_URL"
    rm -rf "$workdir"
    return 91
  fi

  if ! decode_stdout_field "$bodyfile" "$stdoutfile"; then
    logline "ERROR relay response missing/malformed stdout field"
    rm -rf "$workdir"
    return 92
  fi

  exit_code="$(sed -n 's/.*"exit_code":\(-\{0,1\}[0-9][0-9]*\).*/\1/p' "$bodyfile" | head -n1)"
  if [ -z "$exit_code" ]; then
    logline "ERROR relay response missing exit_code field"
    rm -rf "$workdir"
    return 93
  fi

  cat "$stdoutfile"
  rm -rf "$workdir"
  return "$exit_code"
}

args=("$@")
n=${#args[@]}
input=""
input_idx=-1
have_i=0

i=0
while [ "$i" -lt "$n" ]; do
  a="${args[$i]}"
  if [ "$a" = "-i" ]; then
    j=$((i + 1))
    if [ "$j" -lt "$n" ]; then
      input="${args[$j]}"; input_idx="$j"; have_i=1
    fi
    i=$((j + 1)); continue
  fi
  if is_value_opt "$a"; then
    i=$((i + 2)); continue
  fi
  case "$a" in
    -*) i=$((i + 1)); continue ;;
    *)
      if [ "$have_i" -eq 0 ]; then input="$a"; input_idx="$i"; fi
      i=$((i + 1)); continue ;;
  esac
done

# Reroute only for a real .strm file that actually exists and carries a URL.
if [ "$input_idx" -ge 0 ] && [ -n "$input" ]; then
  case "$input" in
    *.strm)
      if [ -f "$input" ]; then
        url="$(grep -oE 'https?://[^[:space:]]+' "$input" 2>/dev/null | head -n1)"
        if [ -n "$url" ]; then
          # Build relay args = original argv minus the input token (and its
          # "-i" flag, if that's how the input was supplied) -- the relay
          # endpoint supplies -i <url> itself and rejects a client-supplied
          # -i (see relayArgDenylist server-side).
          relargs=()
          k=0
          while [ "$k" -lt "$n" ]; do
            if [ "$have_i" -eq 1 ] && [ "$k" -eq $((input_idx - 1)) ]; then
              k=$((k + 2)); continue   # skip "-i" and its value together
            fi
            if [ "$have_i" -eq 0 ] && [ "$k" -eq "$input_idx" ]; then
              k=$((k + 1)); continue   # skip the bare positional input
            fi
            relargs+=("${args[$k]}")
            k=$((k + 1))
          done
          logline "reroute .strm '$input' -> relay ($RELAY_URL) url='$url'"
          relay_probe "$url" "${relargs[@]}"
          rc=$?
          logline "relay result exit_code=$rc"
          exit "$rc"
        fi
        logline "no URL inside '$input'; passthrough"
      fi
      ;;
  esac
fi

passthrough "$@"
