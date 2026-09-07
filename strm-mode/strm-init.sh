#!/usr/bin/env bash
# HARRBOR_STRM_INIT v1
# harrbor-strm-init.sh — install DarkHarrbor's .strm-mode ffprobe wrapper into
# an arr container. Runs as root, idempotently, at every container start (via
# LinuxServer.io /custom-cont-init.d) or on demand via `docker exec --user root`.
#
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Dark Harrbor
#
# Always exits 0: a media-probe convenience must never block container startup.
#
# Env overrides:
#   HARRBOR_STRM_STAGE   staging dir holding ffprobe-wrapper.sh (default
#                        /opt/harrbor-strm). DarkHarrbor mounts/copies the repo
#                        strm-mode/ dir here.
#   HARRBOR_FFPROBE_FULL explicit path to a network-capable ffprobe (optional).
#
# A network-capable ffprobe ("full") is required because stock arr ffprobe is
# file-protocol only. ffprobe-full is NOT bundled with DarkHarrbor; it is
# provisioned here, adaptively, in this order:
#   1. $HARRBOR_FFPROBE_FULL  2. staged $STAGE/ffprobe-full  3. already on PATH
#   4. native package manager (apk on Alpine/musl, apt on Debian/glibc; retried)
#   5. best-effort static download (BtbN glibc; skipped on musl)
# The wrapper finds it at /usr/local/bin/ffprobe-full (PATH).

MARKER="HARRBOR_STRM_WRAPPER"
STAGE="${HARRBOR_STRM_STAGE:-/opt/harrbor-strm}"
WRAPPER_SRC="$STAGE/ffprobe-wrapper.sh"
FULL_DEST="/usr/local/bin/ffprobe-full"

log() { printf '%s harrbor-strm-init: %s\n' "$(date -u +%FT%TZ 2>/dev/null)" "$*"; }

is_musl() { ls /lib/ld-musl-* >/dev/null 2>&1; }

# busybox- and GNU-safe test for our wrapper marker (avoids grep -a on a binary).
is_wrapped() { head -c 8192 "$1" 2>/dev/null | tr -d '\0' | grep -q "$MARKER"; }

# Network-capable means it must speak http/https/tcp/tls to follow a DH URL.
ffprobe_full_ok() {
  local p="$1"
  [ -n "$p" ] || return 1
  command -v "$p" >/dev/null 2>&1 || [ -x "$p" ] || return 1
  "$p" -hide_banner -protocols 2>/dev/null | tr 'A-Z' 'a-z' | grep -qw https || return 1
  return 0
}

# Point FULL_DEST at a validated network-capable ffprobe (symlink or copy).
link_full() {
  local p="$1"
  [ -n "$p" ] || return 1
  case "$p" in */*) : ;; *) p="$(command -v "$p" 2>/dev/null)" ;; esac
  [ -n "$p" ] || return 1
  ln -sf "$p" "$FULL_DEST" 2>/dev/null || { cp -f "$p" "$FULL_DEST" && chmod 0755 "$FULL_DEST"; } || return 1
  ffprobe_full_ok "$FULL_DEST"
}

install_full() {
  if ffprobe_full_ok "$FULL_DEST"; then log "ffprobe-full present ($FULL_DEST)"; return 0; fi
  if [ -n "$HARRBOR_FFPROBE_FULL" ] && ffprobe_full_ok "$HARRBOR_FFPROBE_FULL"; then
    link_full "$HARRBOR_FFPROBE_FULL" && { log "ffprobe-full <- \$HARRBOR_FFPROBE_FULL"; return 0; }
  fi
  if [ -f "$STAGE/ffprobe-full" ] && ffprobe_full_ok "$STAGE/ffprobe-full"; then
    link_full "$STAGE/ffprobe-full" && { log "ffprobe-full <- staging"; return 0; }
  fi
  if command -v ffprobe-full >/dev/null 2>&1 && ffprobe_full_ok ffprobe-full; then
    log "ffprobe-full already on PATH"; return 0
  fi

  # Native package manager — gives a libc-matched, network-capable ffprobe.
  # ffmpeg is a large dependency tree; a single flaky package download aborts
  # the install, so retry a few times before giving up.
  local n
  if command -v apk >/dev/null 2>&1; then
    log "installing ffmpeg via apk (Alpine/musl)"
    n=0
    while [ "$n" -lt 3 ]; do
      apk add --no-cache ffmpeg >/dev/null 2>&1
      if [ -x /usr/bin/ffprobe ] && ffprobe_full_ok /usr/bin/ffprobe; then
        link_full /usr/bin/ffprobe && { log "ffprobe-full <- apk ffmpeg"; return 0; }
      fi
      n=$((n + 1)); log "apk ffmpeg attempt $n incomplete; retrying"; sleep 2
    done
    log "apk ffmpeg failed after retries"
  elif command -v apt-get >/dev/null 2>&1; then
    log "installing ffmpeg via apt (Debian/glibc)"
    n=0
    while [ "$n" -lt 3 ]; do
      apt-get update >/dev/null 2>&1 && apt-get install -y --no-install-recommends ffmpeg >/dev/null 2>&1
      if [ -x /usr/bin/ffprobe ] && ffprobe_full_ok /usr/bin/ffprobe; then
        link_full /usr/bin/ffprobe && { log "ffprobe-full <- apt ffmpeg"; return 0; }
      fi
      n=$((n + 1)); log "apt ffmpeg attempt $n incomplete; retrying"; sleep 2
    done
    log "apt ffmpeg failed after retries"
  fi

  # Last resort: static download. BtbN builds are glibc-dynamic and cannot run
  # on musl, so skip the futile download there (use apk or a staged binary).
  if is_musl; then
    log "musl libc: no compatible static download; need apk ffmpeg or a staged musl-static ffprobe-full"
    return 1
  fi
  local arch tag url tmp fp
  arch="$(uname -m 2>/dev/null)"
  case "$arch" in
    x86_64|amd64)  tag="linux64" ;;
    aarch64|arm64) tag="linuxarm64" ;;
    *) log "no static-build mapping for arch '$arch'"; return 1 ;;
  esac
  url="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-${tag}-gpl.tar.xz"
  tmp="$(mktemp -d 2>/dev/null)" || return 1
  log "attempting static ffprobe-full download: $url"
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$url" -o "$tmp/f.tar.xz" 2>/dev/null
  elif command -v wget >/dev/null 2>&1; then wget -q "$url" -O "$tmp/f.tar.xz" 2>/dev/null; fi
  if [ -s "$tmp/f.tar.xz" ] && tar -xJf "$tmp/f.tar.xz" -C "$tmp" 2>/dev/null; then
    fp="$(find "$tmp" -type f -name ffprobe 2>/dev/null | head -n1)"
    [ -n "$fp" ] && { cp -f "$fp" "$FULL_DEST" && chmod 0755 "$FULL_DEST"; }
  fi
  rm -rf "$tmp"
  if ffprobe_full_ok "$FULL_DEST"; then log "ffprobe-full <- static download"; return 0; fi
  rm -f "$FULL_DEST"   # don't leave a broken binary behind
  log "ffprobe-full could not be provisioned (.strm probes will fail until staged)"
  return 1
}

# Replace each arr ffprobe with the wrapper; back up the original once.
install_wrapper() {
  if [ ! -f "$WRAPPER_SRC" ]; then log "wrapper source missing at $WRAPPER_SRC"; return 1; fi
  local found=0 t
  for t in /app/*/bin/ffprobe; do
    [ -e "$t" ] || continue
    found=1
    if is_wrapped "$t"; then log "already wrapped: $t"; continue; fi
    # Lacks our marker => stock (file-only) binary: first install, or restored
    # by an image update. Back it up, then install the wrapper.
    if cp -a "$t" "$t.real"; then
      if cp -f "$WRAPPER_SRC" "$t" && chmod 0755 "$t"; then
        log "wrapped $t (original -> $t.real)"
      else
        log "ERROR install failed at $t; restoring original"; cp -a "$t.real" "$t" 2>/dev/null
      fi
    else
      log "ERROR backup failed for $t; leaving stock in place"
    fi
  done
  [ "$found" -eq 1 ] || log "no /app/*/bin/ffprobe found (arr not detected here)"
  return 0
}

log "starting (stage=$STAGE)"
install_full || true
install_wrapper || true
log "done"
exit 0
