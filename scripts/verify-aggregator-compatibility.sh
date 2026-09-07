#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
lock="$root/examples/aggregator-tier/compatibility.lock"
compose="$root/examples/aggregator-tier/docker-compose.yml"
. "$lock"

require_digest() {
	printf '%s\n' "$1" | grep -Eq '^[^[:space:]]+@sha256:[0-9a-f]{64}$' || {
		echo "compatibility gate: image must be an immutable digest: $1" >&2
		exit 2
	}
}

static_checks() {
	require_digest "$AIOSTREAMS_IMAGE"
	require_digest "$STREMTHRU_IMAGE"
	require_digest "$SELENIUM_CHROMIUM_IMAGE"
	! grep -Eq 'image:.*:latest([}"[:space:]]|$)' "$compose"
	expected=$(printf 'image: "${AIOSTREAMS_IMAGE:-%s}"' "$AIOSTREAMS_IMAGE")
	grep -Fq "$expected" "$compose"
	expected=$(printf 'image: "${STREMTHRU_IMAGE:-%s}"' "$STREMTHRU_IMAGE")
	grep -Fq "$expected" "$compose"
	python3 "$root/scripts/verify-aiostreams-ui.py" 		--contract "$COMPATIBILITY_CONTRACT" --self-test
}

wait_http() {
	url=$1
	for _attempt in $(seq 1 90); do
		if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	echo "compatibility gate: timed out waiting for $url" >&2
	return 1
}

container_port() {
	docker port "$1" "$2/tcp" | sed -n '1s/.*://p'
}

assert_absent() {
	for object in "$aio" "$st" "$browser"; do
		! docker container inspect "$object" >/dev/null 2>&1 || {
			echo "compatibility gate: current-run container already exists: $object" >&2
			exit 1
		}
	done
	! docker network inspect "$network" >/dev/null 2>&1 || {
		echo "compatibility gate: current-run network already exists: $network" >&2
		exit 1
	}
	for object in "$aio_volume" "$st_volume"; do
		! docker volume inspect "$object" >/dev/null 2>&1 || {
			echo "compatibility gate: current-run volume already exists: $object" >&2
			exit 1
		}
	done
}

cleanup() {
	docker rm -f "$browser" "$aio" "$st" >/dev/null 2>&1 || true
	docker volume rm "$aio_volume" "$st_volume" >/dev/null 2>&1 || true
	docker network rm "$network" >/dev/null 2>&1 || true
	case ${tmp:-} in /tmp/*) find "$tmp" -depth -delete 2>/dev/null || true ;; esac
}

start_aio() {
	image=$1
	docker run -d --name "$aio" --network "$network" --network-alias aiostreams 		--env-file "$tmp/aiostreams.env" -v "$aio_volume:/app/data" 		-p 127.0.0.1::3000 "$image" >/dev/null
	aio_port=$(container_port "$aio" 3000)
	test -n "$aio_port"
	wait_http "http://127.0.0.1:$aio_port/"
}

wait_seadex_idle() {
	# AIOStreams holds seadex/trs.lock for the duration of its dataset sync and
	# does not release it on SIGTERM. Replacing the container while that lock is
	# held strands it, along with a partial trs.json.tmp, on the shared volume,
	# and the next container blocks on the stale lock instead of ever serving
	# HTTP -- which reads as an upgrade incompatibility that is not real.
	for _attempt in $(seq 1 180); do
		state=$(docker exec "$aio" sh -c '[ -e /app/data/seadex/trs.lock ] && echo held || echo free' 2>/dev/null || echo unknown)
		case $state in
		free) return 0 ;;
		held) ;;
		*)
			echo "compatibility gate: cannot read seadex lock state" >&2
			return 1
			;;
		esac
		sleep 1
	done
	echo "compatibility gate: seadex dataset sync did not settle" >&2
	return 1
}

probe_ui() {
	docker run -d --name "$browser" --network "$network" --shm-size 2g 		-p 127.0.0.1::4444 "$SELENIUM_CHROMIUM_IMAGE" >/dev/null
	browser_port=$(container_port "$browser" 4444)
	test -n "$browser_port"
	wait_http "http://127.0.0.1:$browser_port/status"
	python3 "$root/scripts/verify-aiostreams-ui.py" 		--webdriver "http://127.0.0.1:$browser_port" 		--app http://aiostreams:3000 --contract "$COMPATIBILITY_CONTRACT"
	docker rm -f "$browser" >/dev/null
}

run_gate() {
	candidate=$1
	upgrade=$2
	require_digest "$candidate"
	command -v curl >/dev/null
	command -v docker >/dev/null
	command -v python3 >/dev/null

	run_id="dh-aggregator-compat-$(date -u +%Y%m%dT%H%M%SZ)-$$"
	network="$run_id"
	aio="$run_id-aiostreams"
	st="$run_id-stremthru"
	browser="$run_id-chromium"
	aio_volume="$run_id-aiostreams-data"
	st_volume="$run_id-stremthru-data"
	tmp=$(mktemp -d "/tmp/$run_id.XXXXXX")
	trap cleanup EXIT HUP INT TERM
	assert_absent
	umask 077
	secret=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
	cat >"$tmp/aiostreams.env" <<EOF
BASE_URL=http://aiostreams:3000
SECRET_KEY=$secret
BUILTIN_STREMTHRU_URL=http://stremthru:8080
STREMTHRU_TORZ_URL=http://stremthru:8080/v0/torznab
STREMTHRU_STORE_URL=http://stremthru:8080/stremio/store
DISABLE_RATE_LIMITS=true
NODE_OPTIONS=--dns-result-order=ipv4first
MAX_ADDONS=25
EOF
	cat >"$tmp/stremthru.env" <<EOF
STREMTHRU_BASE_URL=http://stremthru:8080
STREMTHRU_PORT=8080
STREMTHRU_AUTH=compat:$secret
STREMTHRU_VAULT_SECRET=$secret
STREMTHRU_DATA_DIR=/app/data
EOF
	docker network create "$network" >/dev/null
	docker volume create "$aio_volume" >/dev/null
	docker volume create "$st_volume" >/dev/null
	docker run -d --name "$st" --network "$network" --network-alias stremthru 		--env-file "$tmp/stremthru.env" -v "$st_volume:/app/data" 		-p 127.0.0.1::8080 "$STREMTHRU_IMAGE" >/dev/null
	st_port=$(container_port "$st" 8080)
	test -n "$st_port"
	wait_http "http://127.0.0.1:$st_port/v0/torznab/api?t=caps"

	if test "$upgrade" = true; then
		start_aio "$AIOSTREAMS_IMAGE"
		probe_ui
		wait_seadex_idle
		docker rm -f "$aio" >/dev/null
	fi
	start_aio "$candidate"
	probe_ui
	echo "aggregator compatibility gate: PASS ($candidate)"
}

static_checks
case ${1:-static} in
static)
	echo "aggregator compatibility static checks: PASS"
	;;
clean)
	run_gate "${2:-$AIOSTREAMS_IMAGE}" false
	;;
upgrade)
	test "$#" -eq 2 || {
		echo "usage: $0 upgrade <candidate-image@sha256:digest>" >&2
		exit 2
	}
	run_gate "$2" true
	;;
*)
	echo "usage: $0 [static|clean [candidate-image@sha256:digest]|upgrade <candidate-image@sha256:digest>]" >&2
	exit 2
	;;
esac
