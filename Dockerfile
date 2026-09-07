# syntax=docker/dockerfile:1

# ── Stage 0: ffprobe-full (network-capable static ffprobe for D-RELAY) ──────
# Pinned by digest for reproducibility, matching strm-mode/fetch-ffprobe-full.sh
# (same source, same verification: static-pie, http/https/tcp/tls support).
# This is what internal/api's D-RELAY endpoint (Gate 5) execs against a
# validated DH /stream URL on the arr-side shim's behalf -- ffprobe-full only
# needs to exist in THIS image now, not staged into every arr container.
FROM mwader/static-ffmpeg@sha256:df8a363ed7089ab0779c4f019b935a0e428c0b705478b6ff371b52b4bbe818f8 AS ffprobefull

# ── Stage 1: build ──────────────────────────────────────────────────────────
FROM golang:1.26.7-alpine AS builder

# modernc.org/sqlite is pure-Go — static binary, no CGO needed

WORKDIR /build
COPY src/go.mod src/go.sum ./
RUN go mod download

COPY src/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o darkharrbor ./cmd/darkharrbor
RUN CGO_ENABLED=0 GOOS=linux go build -o harrbor-dockerproxy ./cmd/harrbor-dockerproxy

# ── Stage 2: runtime ────────────────────────────────────────────────────────
FROM alpine:3.21 AS darkharrbor
LABEL org.darkharrbor.install-schema="v1"

# ffprobe for one-shot probe at import
RUN apk add --no-cache ffmpeg ca-certificates tzdata

# Non-root user matching host PUID/PGID 1000:1000
RUN addgroup -g 1000 darkharrbor && adduser -D -u 1000 -G darkharrbor darkharrbor

COPY --from=builder /build/darkharrbor /usr/local/bin/darkharrbor
COPY --from=ffprobefull /ffprobe /usr/local/bin/ffprobe-full

# /config  — SQLite DB (persistent volume)
# /data    — strm + sidecar output tree (bind-mounted to HDD path)
RUN mkdir -p /config /data /backup /run/darkharrbor-key && \
    chown -R darkharrbor:darkharrbor /config /data /backup /run/darkharrbor-key && \
    chmod 0700 /config /backup /run/darkharrbor-key

USER darkharrbor

EXPOSE 8381

ENTRYPOINT ["/usr/local/bin/darkharrbor"]

# ── Stage 3: dockerproxy runtime (separate image; .strm-mode opt-in only) ───
# Purpose-built allowlist proxy in front of the Docker socket -- see
# cmd/harrbor-dockerproxy/main.go for the full rationale (default-deny,
# explicit method+path allowlist, no third-party proxy image trusted for
# this boundary). Must run as root: reading /var/run/docker.sock requires
# it (same posture any docker-socket-proxy image needs), but the allowlist
# is enforced entirely in-process before any request reaches the socket.
FROM alpine:3.21 AS dockerproxy
LABEL org.darkharrbor.install-schema="v1" \
      org.darkharrbor.dockerproxy-policy="v1"
RUN apk add --no-cache ca-certificates
COPY --from=builder /build/harrbor-dockerproxy /usr/local/bin/harrbor-dockerproxy
EXPOSE 2375
ENTRYPOINT ["/usr/local/bin/harrbor-dockerproxy"]
