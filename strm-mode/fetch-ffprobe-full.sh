#!/usr/bin/env bash
# fetch-ffprobe-full.sh — stage a static, network-capable ffprobe for .strm mode.
#
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Dark Harrbor
#
# Produces strm-mode/ffprobe-full: a fully-static (static-pie) ffprobe that runs
# on Alpine/musl arr containers WITHOUT apk and resolves DNS (musl's static
# resolver works; fully-static glibc does not, and glibc-dynamic builds like
# BtbN fail on musl entirely). Sourced from mwader/static-ffmpeg (Alpine-built,
# statically linked), pinned by digest for reproducibility.
#
# The binary is gitignored (large, regenerable). Run this on any host with
# docker + internet to (re)produce it. DarkHarrbor mounts strm-mode/ into arr
# containers as /opt/harrbor-strm, where strm-init.sh prefers this staged binary
# over the apk fallback.
#
# Verified 2026-06-28: ffprobe 8.1.2, static-pie, http/https/tcp/tls; runs in
# musl Radarr; resolves the `darkharrbor` hostname and probes DH /stream to
# exact metadata.
set -euo pipefail

# Pinned digest of the verified image. Bump deliberately (re-verify after).
IMAGE="mwader/static-ffmpeg@sha256:df8a363ed7089ab0779c4f019b935a0e428c0b705478b6ff371b52b4bbe818f8"
DEST="$(cd "$(dirname "$0")" && pwd)/ffprobe-full"

echo "pulling $IMAGE"
docker pull "$IMAGE" >/dev/null

cid="$(docker create "$IMAGE")"
trap 'docker rm "$cid" >/dev/null 2>&1 || true' EXIT
docker cp "$cid:/ffprobe" "$DEST"
chmod 0755 "$DEST"

echo "staged: $DEST ($(du -h "$DEST" | cut -f1))"
file "$DEST" | grep -q static || { echo "ERROR: not statically linked"; exit 1; }
"$DEST" -hide_banner -protocols 2>/dev/null | tr 'A-Z' 'a-z' | grep -qw https || { echo "ERROR: no https support"; exit 1; }
echo "verified: static + https. ffprobe-full ready."
