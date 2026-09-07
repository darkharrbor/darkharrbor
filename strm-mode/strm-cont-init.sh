#!/bin/bash
# HARRBOR_STRM_CONT_INIT v1
# strm-cont-init.sh — DarkHarrbor .strm-mode boot launcher.
#
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Dark Harrbor
#
# Deployed into a LinuxServer.io arr container's /custom-cont-init.d so it runs
# as root at every container start. It invokes the idempotent installer staged
# at /opt/harrbor-strm (the repo strm-mode/ dir, bind-mounted read-only), which
# provisions a network-capable ffprobe-full and installs the ffprobe wrapper.
# Never blocks startup (always exits 0).
#
# Requires the strm-mode/ bind mount:   <repo>/strm-mode:/opt/harrbor-strm:ro
# On LSIO images /custom-cont-init.d runs as root only when the container starts
# as root (PUID/PGID drop the app afterwards). A service pinned to user:1000
# would run this as uid 1000 and the root-level install would be skipped.
S=/opt/harrbor-strm/strm-init.sh
if [ -f "$S" ]; then
  /bin/bash "$S"
else
  echo "$(date -u +%FT%TZ 2>/dev/null) harbor-strm: /opt/harrbor-strm not mounted; skipping .strm-mode install"
fi
exit 0
