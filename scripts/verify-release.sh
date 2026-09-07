#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

grep -Fq 'defaultServerAddr   = "127.0.0.1:8381"' src/internal/config/config.go
grep -Fq 'platforms: linux/amd64,linux/arm64' .github/workflows/release.yml
grep -Fq 'target: darkharrbor' .github/workflows/release.yml
grep -Fq 'target: dockerproxy' .github/workflows/release.yml
grep -Fq 'needs: verify' .github/workflows/release.yml
grep -Fq 'image_repo=${DARKHARRBOR_IMAGE_REPO:-ghcr.io/darkharrbor/darkharrbor}' install.sh
grep -Fq 'ghcr.io/${{ github.repository }}${{ matrix.suffix }}:${{ github.ref_name }}' .github/workflows/release.yml
grep -Fq 'python3 scripts/verify-sbom.py --version "${GITHUB_REF_NAME#v}"' .github/workflows/release.yml
test "$(grep -Fc 'packages: write' .github/workflows/release.yml)" -eq 1
test "$(grep -Fc 'persist-credentials: false' .github/workflows/release.yml)" -eq 2
grep -Fq "go-version: '1.26.7'" .github/workflows/release.yml
grep -Fq 'go test -race -count=1 ./...' .github/workflows/release.yml
grep -Fq "go-version: '1.26.7'" .github/workflows/ci.yml
grep -Fq "version: '2026.2.1'" .github/workflows/ci.yml
grep -Fq 'permissions:' .github/workflows/ci.yml
test "$(grep -c '^  pull_request:$' .github/workflows/ci.yml)" -eq 1
test "$(grep -n '^  pull_request:$' .github/workflows/ci.yml | cut -d: -f1)" -lt "$(grep -n '^permissions:$' .github/workflows/ci.yml | cut -d: -f1)"
grep -Fq 'permissions:' .github/workflows/aggregator-compatibility.yml
grep -Fq 'distant admiralty' README.md
grep -Fq '## Reverse Proxy and Bind Safety' CONFIGURATION.md
grep -Fq '### Unauthenticated Compatibility Surface' SECURITY.md
grep -Fxq '.git' .dockerignore
grep -Fxq '.env' .dockerignore
grep -Fxq '.env.*' .dockerignore
if grep -Eq '^[[:space:]]*COPY[[:space:]]+\.[[:space:]]' Dockerfile; then
	echo 'release verification: Dockerfile broadly copies the build context' >&2
	exit 1
fi
if go version >/dev/null 2>&1; then
	python3 scripts/verify-sbom.py
else
	go_toolchain=$(sed -n 's/^toolchain go//p' src/go.mod)
	test -n "$go_toolchain" || { echo 'verify-release: go.mod has no toolchain directive' >&2; exit 1; }
	(
		modlist=$(mktemp)
		trap 'rm -f -- "$modlist"' EXIT
		docker run --rm -v "$root/src:/src:ro" -w /src "golang:${go_toolchain}-alpine" \
			go list -mod=readonly -m -f '{{if not .Main}}{{.Path}} {{.Version}}{{end}}' all >"$modlist"
		python3 scripts/verify-sbom.py --modules-from "$modlist"
	)
fi
grep -Fq 'HARRBOR_REPORTED_PATH_PREFIX` stays `/mnt/darkharrbor`' CONFIGURATION.md
grep -Fq 'reactive-only direct ManualImport does not require the optional ffprobe relay' src/internal/wizard/wizard.go
sh -n scripts/evidence-bundle.sh
bash -n install.sh
./scripts/verify-installer.sh
sh -n scripts/verify-aggregator-compatibility.sh
./scripts/verify-aggregator-compatibility.sh static

if grep -Eq '^(name:|[[:space:]]+container_name:)' examples/aggregator-tier/docker-compose.yml; then
	echo 'release verification: aggregator example has fixed Compose ownership' >&2
	exit 1
fi
grep -Fq 'name: "${AGGREGATOR_NETWORK:-arr-net}"' examples/aggregator-tier/docker-compose.yml

if grep -R -E '"(go\.opentelemetry|github\.com/getsentry|github\.com/posthog)' src --include='*.go' >/dev/null; then
	echo 'release verification: outbound telemetry import found' >&2
	exit 1
fi

rendered=$(mktemp)
trap 'case ${rendered:-} in /tmp/*) rm -f -- "$rendered" ;; esac' EXIT HUP INT TERM
docker compose -f docker-compose.yml config >"$rendered"
if grep -Eq '^[[:space:]]+target:[[:space:]]+8381$' "$rendered"; then
	echo 'release verification: canonical Compose publishes the private API' >&2
	exit 1
fi
test "$(grep -Ec '^[[:space:]]+target:[[:space:]]+8382$' "$rendered")" -eq 1
test "$(grep -Ec '^[[:space:]]+published:[[:space:]]+"8382"$' "$rendered")" -eq 1
test "$(grep -Ec '^[[:space:]]+protocol:[[:space:]]+tcp$' "$rendered")" -eq 1

echo 'release invariants: PASS'
