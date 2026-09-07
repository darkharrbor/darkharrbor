#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
output=${1:-darkharrbor-evidence-v1.0.0}
case "$output" in
	/*) ;;
	*) output=$PWD/$output ;;
esac
if [ -e "$output" ]; then
	echo 'evidence bundle: output already exists' >&2
	exit 1
fi

umask 077
tmp=$(mktemp -d /tmp/darkharrbor-evidence.XXXXXX)
bundle=$tmp/bundle
mkdir -- "$bundle"
cleanup() {
	case ${tmp:-} in
		/tmp/darkharrbor-evidence.*) rm -rf -- "$tmp" ;;
	esac
}
trap cleanup EXIT HUP INT TERM

cd "$root"
ia_evidence=curated_demo
if ! timeout 420 docker compose exec -T darkharrbor darkharrbor demo --timeout 300 --yes >"$tmp/demo" 2>&1; then
	ia_evidence=existing_ready_stream
	docker run --rm --network arr-net --volumes-from darkharrbor alpine:3.21 sh -eu -c '
		apk add --no-cache curl sqlite >/dev/null
		path=$(sqlite3 /config/darkharrbor.db "select strm_path from items where source_type='"'"'http'"'"' and state='"'"'ready'"'"' and json_extract(resolve_key,'"'"'$.handler'"'"')='"'"'ia'"'"' and length(strm_path)>0 order by updated_at desc limit 1")
		case "$path" in /data/*) ;; *) exit 1 ;; esac
		test -f "$path"
		url=$(sed -n '"'"'1p'"'"' "$path")
		case "$url" in http://darkharrbor:8381/stream/*) ;; *) exit 1 ;; esac
		headers=$(mktemp)
		trap '"'"'unlink "$headers"'"'"' EXIT HUP INT TERM
		curl -fsSI --max-time 30 "$url" >"$headers"
		length=$(awk '"'"'tolower($1) == "content-length:" { gsub("\\r", "", $2); print $2; exit }'"'"' "$headers")
		case "$length" in '"'"''"'"'|*[!0-9]*) exit 1 ;; esac
		test "$length" -gt 1
		code=$(curl -sS --max-time 30 -o /dev/null -w '"'"'%{http_code}'"'"' -H '"'"'Range: bytes=0-1023'"'"' "$url")
		test "$code" = 206
		mid=$((length / 2))
		end=$((mid + 1023))
		code=$(curl -sS --max-time 30 -o /dev/null -w '"'"'%{http_code}'"'"' -H "Range: bytes=$mid-$end" "$url")
		test "$code" = 206
	' >"$tmp/existing-ia" 2>&1
fi
docker run --rm \
	-v "$root/src:/src:ro" \
	-w /src \
	-e GOFLAGS=-buildvcs=false \
	golang:1.26.7 \
	go test ./internal/contentproof -run '^TestRecoveryCoordinatorUsesNativeCandidateDigestAndRejectsCorruption$' -count=1 >"$tmp/proof" 2>&1
docker compose exec -T darkharrbor wget -qO- http://127.0.0.1:8381/healthz >"$tmp/health"
grep -q '"ok":true' "$tmp/health"

source_commit=$(git rev-parse HEAD)
image=$(docker inspect darkharrbor --format '{{.Image}}')
generated=$(date -u +%Y-%m-%dT%H:%M:%SZ)
cat >"$bundle/evidence.json" <<EOF
{
  "schema": 1,
  "release": "v1.0.0",
  "generated_at": "$generated",
  "source_commit": "$source_commit",
  "image": "$image",
  "public_domain_ia_evidence": "$ia_evidence",
  "corrupt_cross_lane_block": "rejected",
  "service_health": "pass",
  "raw_private_output_retained": false
}
EOF
(cd "$bundle" && sha256sum evidence.json >SHA256SUMS)
mv -- "$bundle" "$output"
echo "evidence bundle written: $output"
