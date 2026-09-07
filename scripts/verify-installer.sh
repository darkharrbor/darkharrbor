#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf -- "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/media" "$test_root/install"
chmod 0700 "$test_root/install"

cat >"$test_root/bin/docker" <<'FAKE'
#!/usr/bin/env bash
set -eu
if [[ -n ${FAKE_DOCKER_LOG:-} ]]; then
    printf '%q ' "$@" >>"$FAKE_DOCKER_LOG"
    printf '\n' >>"$FAKE_DOCKER_LOG"
fi
if [[ -n ${FAKE_PRIVATE_CATALOG_PATH:-} && " $* " != *" run --rm --no-deps -T darkharrbor catalog import "* ]]; then
    for inherited_fd in /proc/$$/fd/*; do
        [[ $(readlink "$inherited_fd" 2>/dev/null || true) != "$FAKE_PRIVATE_CATALOG_PATH" ]] || {
            printf 'private catalog descriptor leaked to unrelated Docker command\n' >&2
            exit 1
        }
    done
fi
project_name=${COMPOSE_PROJECT_NAME:-darkharrbor}
case "${1:-} ${2:-}" in
    "info "|"compose version") exit 0 ;;
    "ps -aq")
        [[ ${FAKE_PROJECT_EXISTS:-0} == 1 ]] && printf 'existing-container\n'
        exit 0
        ;;
    "volume inspect")
        if [[ ${FAKE_INSTALLED_VOLUMES:-0} == 1 ]]; then
            if [[ ${3:-} == --format ]]; then
                volume_name=${5:-}
                volume_key=${volume_name#"${project_name}_"}
                if [[ ${FAKE_BAD_VOLUME_LABEL:-0} == 1 ]]; then
                    printf 'someone-else:%s\n' "$volume_key"
                else
                    printf '%s:%s\n' "$project_name" "$volume_key"
                fi
            fi
            exit 0
        fi
        [[ ${FAKE_PROJECT_EXISTS:-0} == 1 ]] && exit 0
        exit 1
        ;;
    "volume ls")
        if [[ ${FAKE_INSTALLED_VOLUMES:-0} == 1 ]]; then
            printf '%s\n' "${project_name}_darkharrbor-config" "${project_name}_darkharrbor-backups" "${project_name}_darkharrbor-keys"
        fi
        exit 0
        ;;
    "volume rm") exit 0 ;;
    "network inspect")
        [[ ${3:-} == "${project_name}_control" ]] && {
            [[ ${FAKE_PROJECT_EXISTS:-0} == 1 ]] && exit 0
            exit 1
        }
        exit 0
        ;;
    pull\ *) exit 0 ;;
    run\ *)
        if [[ " $* " == *" --user 1000:1000 "* && ${FAKE_MEDIA_PERMISSION_FAIL:-0} == 1 ]]; then
            exit 1
        fi
        exit 0
        ;;
    "image inspect")
        format=${4:-}
        ref=${5:-}
        if [[ $format == *RepoDigests* ]]; then
            if [[ $ref == *-dockerproxy:* ]]; then
                digest=2
                [[ $ref == *:v1.1.0 ]] && digest=4
            else
                digest=1
                [[ $ref == *:v1.1.0 ]] && digest=3
            fi
            printf '%s@sha256:%064d\n' "${ref%:*}" "$digest"
        elif [[ $format == *'{{.Id}}'* ]]; then
            image_id=5
            [[ $ref == *-dockerproxy:* ]] && image_id=6
            printf 'sha256:%064d\n' "$image_id"
        elif [[ $format == *dockerproxy-policy* || $format == *install-schema* ]]; then
            [[ $ref == sha256:* || $ref == *@sha256:* ]] || {
                printf 'mutable image label inspection: %s\n' "$ref" >&2
                exit 1
            }
            printf 'v1\n'
        else
            printf 'v1\n'
        fi
        ;;
    "ps --filter") printf 'sonarr\tlscr.io/linuxserver/sonarr:latest\n' ;;
    "inspect --format") printf 'arr-net\n' ;;
    "network ls") printf '  arr-net\n' ;;
    "compose -f")
		if [[ " $* " == *" --profile strm down -v --remove-orphans "* && ${FAKE_DOWN_FAIL:-0} == 1 ]]; then
			exit 1
		fi
		if [[ " $* " == *" --profile strm down --remove-orphans "* && ${FAKE_UNINSTALL_DOWN_FAIL:-0} == 1 ]]; then
			exit 1
		fi
		if [[ " $* " == *" up -d --wait --wait-timeout 120 --pull never "* && ${FAKE_START_FAIL:-0} == 1 ]]; then
			exit 1
		fi
		if [[ " $* " == *" up -d --wait --wait-timeout 120 darkharrbor "* && ${FAKE_INITIAL_HEALTH_FAIL:-0} == 1 ]]; then
			exit 1
		fi
		if [[ " $* " == *" exec -T darkharrbor darkharrbor backup snapshot "* && ${FAKE_SNAPSHOT_FAIL:-0} == 1 ]]; then
			exit 1
		fi
		if [[ " $* " == *" exec -T darkharrbor darkharrbor backup snapshot "* ]]; then
			printf 'Backup snapshot complete: darkharrbor-20260831T120000.000000000Z\n'
		fi
		if [[ " $* " == *" run --rm --no-deps -T darkharrbor backup verify --from "* && ${FAKE_VERIFY_FAIL:-0} == 1 ]]; then
			exit 1
		fi
		if [[ " $* " == *" run --rm --no-deps -T darkharrbor backup verify --from "* ]]; then
			printf 'Backup restore preflight: OK\n'
		fi
		if [[ " $* " == *" run --rm --no-deps -T darkharrbor restore --from "* && ${FAKE_RESTORE_FAIL:-0} == 1 ]]; then
			exit 1
		fi
		if [[ " $* " == *" run --rm --no-deps -T darkharrbor backup list --from /backup "* ]]; then
			printf '%s\n' darkharrbor-20260829T120000.000000000Z darkharrbor-20260830T120000.000000000Z
		fi
		if [[ " $* " == *" run --rm -it darkharrbor setup "* && ${FAKE_SETUP_FAIL:-0} == 1 ]]; then
			exit 1
		fi
		if [[ " $* " == *" run --rm -T --no-deps -v "* && " $* " == *"darkharrbor setup "* && ${FAKE_SETUP_FAIL:-0} == 1 ]]; then
			exit 1
		fi
		if [[ " $* " == *" run --rm --no-deps -T darkharrbor catalog import "* && ${FAKE_CATALOG_IMPORT_FAIL:-0} == 1 ]]; then
			exit 1
		fi
        if [[ " $* " == *" cp "* && ${FAKE_CP_FAIL:-0} == 1 ]]; then
            exit 1
        fi
        if [[ " $* " == *" exec -T darkharrbor test -f /run/darkharrbor-key/setup-handoff/"* ]]; then
            [[ " $* " == *"/recovery.key "* && ${FAKE_RECOVERY_MISSING:-0} != 1 ]] && exit 0
            [[ " $* " == *"/mediaflow.env "* && ${FAKE_MEDIAFLOW_HANDOFF:-0} == 1 ]] && exit 0
            [[ " $* " == *"/stremio-install.url "* && ${FAKE_STREMIO_HANDOFF:-0} == 1 ]] && exit 0
            exit 1
        fi
        if [[ " $* " == *" ps --status running -q docker-proxy "* && ${FAKE_PROXY_RUNNING:-0} == 1 ]]; then
            printf 'proxy-container-id\n'
        fi
        if [[ " $* " == *" ps --status running -q darkharrbor "* && ${FAKE_MAIN_RUNNING:-0} == 1 ]]; then
            printf 'main-container-id\n'
        fi
        if [[ " $* " == *" cp "* ]]; then
            destination=${!#}
            mkdir -p "$(dirname "$destination")"
            if [[ ${FAKE_CP_SYMLINK:-0} == 1 ]]; then
                ln -s "${FAKE_SYMLINK_TARGET:?}" "$destination"
            elif [[ ${FAKE_CP_OVERSIZE:-0} == 1 ]]; then
                printf '%4097s' x >"$destination"
            elif [[ $destination == */mediaflow.env ]]; then
                printf 'test-mediaflow\n' >"$destination"
            elif [[ $destination == */stremio-install.url ]]; then
                printf 'test-stremio\n' >"$destination"
            else
                printf 'test-recovery\n' >"$destination"
            fi
        fi
        if [[ " $* " == *" --entrypoint /bin/cat darkharrbor "* ]]; then
            printf '%s\n' "${FAKE_RUNTIME_TARGETS:-sonarr}"
        fi
        exit 0
        ;;
    *) printf 'unexpected fake docker call: %q ' "$@" >&2; exit 1 ;;
esac
FAKE
chmod 0755 "$test_root/bin/docker"

mkdir -p "$test_root/media-permission-fail" "$test_root/install-permission-fail"
chmod 0700 "$test_root/install-permission-fail"
: >"$test_root/docker.log"
if printf '\n\n\n%s\ny\n' "$test_root/media-permission-fail" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_MEDIA_PERMISSION_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-permission-fail" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" >/dev/null 2>&1; then
    echo "installer ignored an unwritable media directory" >&2
    exit 1
fi
grep -q -- 'run --rm --read-only --network none --user 1000:1000 --cap-drop ALL' "$test_root/docker.log" || { echo "media permission preflight was not hardened" >&2; exit 1; }
grep -q -- '--mount' "$test_root/docker.log" && grep -q -- 'media-permission-fail' "$test_root/docker.log" || { echo "media permission preflight omitted the selected bind" >&2; exit 1; }
grep -q -- 'ghcr.io/darkharrbor/darkharrbor@sha256:' "$test_root/docker.log" || { echo "media permission preflight did not use the pinned image" >&2; exit 1; }
! grep -q -- 'compose -f' "$test_root/docker.log" || { echo "media permission failure published a deployment" >&2; exit 1; }
[[ ! -e "$test_root/install-permission-fail/compose.yml" ]] || { echo "media permission failure retained generated Compose" >&2; exit 1; }

: >"$test_root/docker.log"
printf '\n\n\n%s\ny\n' "$test_root/media" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" >"$test_root/install.out"

[[ $(grep -cFx -- '  approved targets: sonarr' "$test_root/install.out") == 1 ]] || { echo "installer preview did not show exactly one approved-target line" >&2; exit 1; }

compose_file="$test_root/install/compose.yml"
[[ -f $compose_file ]] || { echo "installer did not create compose file" >&2; exit 1; }
[[ $(stat -c '%a' "$compose_file") == 600 ]] || { echo "compose mode is not 0600" >&2; exit 1; }
[[ $(stat -c '%a' "$test_root/install/handoff") == 700 ]] || { echo "handoff directory mode is not 0700" >&2; exit 1; }
[[ $(stat -c '%a' "$test_root/install/handoff/recovery.key") == 600 ]] || { echo "recovery handoff mode is not 0600" >&2; exit 1; }
[[ $(grep -c -- ' cp darkharrbor:/run/darkharrbor-key/setup-handoff/' "$test_root/docker.log") == 1 ]] || { echo "installer copied more than the exact required handoff" >&2; exit 1; }
grep -q -- 'setup-handoff/recovery.key' "$test_root/docker.log" || { echo "installer omitted the required recovery handoff" >&2; exit 1; }
! grep -q -- 'setup-handoff/\. ' "$test_root/docker.log" || { echo "installer copied the complete handoff directory" >&2; exit 1; }
[[ ! -e "$test_root/install/handoff/connector-targets" ]] || { echo "internal connector handoff was retained" >&2; exit 1; }
grep -q 'ghcr.io/darkharrbor/darkharrbor@sha256:' "$compose_file"
grep -q 'HARRBOR_DOCKERPROXY_TARGETS: "sonarr"' "$compose_file"
grep -q 'name: "arr-net"' "$compose_file"
grep -q '127.0.0.1:8382:8382' "$compose_file"
grep -q 'HARRBOR_SECRETS_KEY_FILE: "/run/darkharrbor-key/secrets.key"' "$compose_file"
grep -q 'darkharrbor-keys:/run/darkharrbor-key' "$compose_file"
head -n 1 "$compose_file" | grep -qx '# DarkHarrbor installer schema: v1'
! grep -Eqi 'api.?key|password|credential|token' "$compose_file"
docker compose -f "$compose_file" config --quiet
grep -q -- 'setup --defer-arr-registration --handoff-dir /run/darkharrbor-key/setup-handoff' "$test_root/docker.log" || { echo "installer did not defer fresh Arr registration" >&2; exit 1; }
grep -q -- 'exec -T darkharrbor darkharrbor reconcile-arr' "$test_root/docker.log" || { echo "installer did not reconcile Arrs after startup" >&2; exit 1; }
main_start_line=$(grep -n -- 'up -d --wait --wait-timeout 120 darkharrbor' "$test_root/docker.log" | head -n 1 | cut -d: -f1)
arr_reconcile_line=$(grep -n -- 'exec -T darkharrbor darkharrbor reconcile-arr' "$test_root/docker.log" | head -n 1 | cut -d: -f1)
[[ -n $main_start_line && -n $arr_reconcile_line && $main_start_line -lt $arr_reconcile_line ]] || { echo "installer reconciled Arrs before DarkHarrbor startup" >&2; exit 1; }

mkdir -p "$test_root/media-catalog" "$test_root/install-catalog"
chmod 0700 "$test_root/install-catalog"
printf '[{"type":"url","title":"private-test","url":"https://private.invalid/media"}]\n' >"$test_root/private-catalog.json"
chmod 0600 "$test_root/private-catalog.json"
: >"$test_root/docker.log"
printf '\n\n\n%s\ny\n' "$test_root/media-catalog" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_PRIVATE_CATALOG_PATH="$test_root/private-catalog.json" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-catalog" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" --catalog "$test_root/private-catalog.json" >"$test_root/catalog-install.out"
grep -qFx '  generic catalog: selected (private; contents hidden)' "$test_root/catalog-install.out" || { echo "installer exposed or omitted the private-catalog preview" >&2; exit 1; }
! grep -Fq -- "$test_root/private-catalog.json" "$test_root/catalog-install.out" || { echo "installer displayed the private catalog path" >&2; exit 1; }
grep -q -- 'run --rm --no-deps -T darkharrbor catalog import' "$test_root/docker.log" || { echo "installer did not import the private catalog over stdin" >&2; exit 1; }
catalog_import_line=$(grep -n -- 'run --rm --no-deps -T darkharrbor catalog import' "$test_root/docker.log" | head -n 1 | cut -d: -f1)
setup_line=$(grep -n -- 'run --rm -it darkharrbor setup' "$test_root/docker.log" | head -n 1 | cut -d: -f1)
[[ -n $catalog_import_line && -n $setup_line && $catalog_import_line -lt $setup_line ]] || { echo "installer did not import the catalog before setup" >&2; exit 1; }
! grep -Fq -- 'private.invalid' "$test_root/docker.log" || { echo "installer leaked private catalog content into Docker arguments" >&2; exit 1; }

printf '[]\n' >"$test_root/insecure-catalog.json"
chmod 0644 "$test_root/insecure-catalog.json"
if PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-insecure-catalog" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" --catalog "$test_root/insecure-catalog.json" </dev/null >/dev/null 2>&1; then
    echo "installer accepted a non-private generic catalog" >&2
    exit 1
fi
[[ ! -e "$test_root/install-insecure-catalog/compose.yml" ]] || { echo "insecure catalog validation published a deployment" >&2; exit 1; }

mkdir -p "$test_root/media-bad-catalog" "$test_root/install-bad-catalog"
chmod 0700 "$test_root/install-bad-catalog"
: >"$test_root/docker.log"
if printf '\n\n\n%s\ny\n' "$test_root/media-bad-catalog" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_CATALOG_IMPORT_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-bad-catalog" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" --catalog "$test_root/private-catalog.json" >/dev/null 2>&1; then
    echo "installer continued after private catalog import failure" >&2
    exit 1
fi
grep -q -- '--profile strm down -v --remove-orphans' "$test_root/docker.log" || { echo "catalog import failure did not remove the incomplete project" >&2; exit 1; }
[[ ! -e "$test_root/install-bad-catalog/compose.yml" ]] || { echo "catalog import failure retained generated Compose" >&2; exit 1; }

mkdir -p "$test_root/media-answers" "$test_root/install-answers"
chmod 0700 "$test_root/install-answers"
printf '{"responses":["a","b","c"]}\n' >"$test_root/answers.json"
chmod 0600 "$test_root/answers.json"
: >"$test_root/docker.log"
printf '\n\n\n%s\ny\n' "$test_root/media-answers" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-answers" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" --answers-file "$test_root/answers.json" >"$test_root/answers-install.out"
grep -q -- 'run --rm -T --no-deps -v .*/answers.json:/run/darkharrbor-key/answers.json:ro darkharrbor setup --defer-arr-registration --handoff-dir /run/darkharrbor-key/setup-handoff -answers /run/darkharrbor-key/answers.json' "$test_root/docker.log" || {
    echo "answers-file install did not invoke setup headlessly with -T and -answers" >&2
    exit 1
}
! grep -q -- 'run --rm -it darkharrbor setup' "$test_root/docker.log" || { echo "answers-file install still allocated a TTY for setup" >&2; exit 1; }
[[ ! -e "$test_root/answers.json" ]] || { echo "answers file was not deleted after a successful run" >&2; exit 1; }
grep -q -- 'DarkHarrbor installation completed' "$test_root/answers-install.out" || { echo "answers-file install did not complete" >&2; exit 1; }

mkdir -p "$test_root/media-insecure-answers" "$test_root/install-insecure-answers"
chmod 0700 "$test_root/install-insecure-answers"
printf '{"responses":["a"]}\n' >"$test_root/insecure-answers.json"
chmod 0644 "$test_root/insecure-answers.json"
: >"$test_root/docker.log"
if PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-insecure-answers" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" --answers-file "$test_root/insecure-answers.json" </dev/null >/dev/null 2>&1; then
    echo "installer accepted a non-private answers file" >&2
    exit 1
fi
! grep -q '^pull ' "$test_root/docker.log" || { echo "insecure answers-file validation pulled an image before failing" >&2; exit 1; }
[[ -e "$test_root/insecure-answers.json" ]] || { echo "insecure answers file was deleted despite being rejected" >&2; exit 1; }
[[ ! -e "$test_root/install-insecure-answers/compose.yml" ]] || { echo "insecure answers-file validation published a deployment" >&2; exit 1; }

# Must match darkharrbor setup -answers's own 1 MiB maxAnswersFileBytes
# exactly -- a looser install.sh-side cap would accept a file the wizard
# only rejects later, mid-run, inside the container.
mkdir -p "$test_root/media-oversize-answers" "$test_root/install-oversize-answers"
chmod 0700 "$test_root/install-oversize-answers"
head -c 1048577 /dev/zero | tr '\0' 'a' >"$test_root/oversize-answers.json"
chmod 0600 "$test_root/oversize-answers.json"
: >"$test_root/docker.log"
if PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-oversize-answers" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" --answers-file "$test_root/oversize-answers.json" </dev/null >/dev/null 2>&1; then
    echo "installer accepted an answers file over the wizard's own 1 MiB cap" >&2
    exit 1
fi
! grep -q '^pull ' "$test_root/docker.log" || { echo "oversize answers-file validation pulled an image before failing" >&2; exit 1; }
[[ -e "$test_root/oversize-answers.json" ]] || { echo "oversize answers file was deleted despite being rejected" >&2; exit 1; }

mkdir -p "$test_root/media-answers-fail" "$test_root/install-answers-fail"
chmod 0700 "$test_root/install-answers-fail"
printf '{"responses":["a"]}\n' >"$test_root/answers-fail.json"
chmod 0600 "$test_root/answers-fail.json"
: >"$test_root/docker.log"
if printf '\n\n\n%s\ny\n' "$test_root/media-answers-fail" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_SETUP_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-answers-fail" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" --answers-file "$test_root/answers-fail.json" >/dev/null 2>&1; then
    echo "answers-file install ignored setup failure" >&2
    exit 1
fi
grep -q -- '--profile strm down -v --remove-orphans' "$test_root/docker.log" || { echo "answers-file setup failure did not remove the incomplete project" >&2; exit 1; }
[[ ! -e "$test_root/install-answers-fail/compose.yml" ]] || { echo "answers-file setup failure retained generated Compose" >&2; exit 1; }
[[ ! -e "$test_root/answers-fail.json" ]] || { echo "answers file was not deleted after a failed run" >&2; exit 1; }

mkdir -p "$test_root/media-preloaded" "$test_root/install-preloaded"
chmod 0700 "$test_root/install-preloaded"
: >"$test_root/docker.log"
printf '\n\n%s\ny\n' "$test_root/media-preloaded" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-preloaded" \
    DARKHARRBOR_VERSION=v1.0.0 \
    DARKHARRBOR_HOST_PORT=18382 \
    DARKHARRBOR_PRELOADED_IMAGES=1 \
    "$repo_root/install.sh" >/dev/null
! grep -q '^pull ' "$test_root/docker.log" || { echo "preloaded install pulled an image" >&2; exit 1; }
grep -q 'image inspect --format .*\\{\\{.Id\\}\\}' "$test_root/docker.log" || { echo "preloaded install did not resolve local image IDs" >&2; exit 1; }
grep -q 'image: "sha256:0000000000000000000000000000000000000000000000000000000000000005"' "$test_root/install-preloaded/compose.yml" || { echo "preloaded main image was not ID-pinned" >&2; exit 1; }
grep -q 'image: "sha256:0000000000000000000000000000000000000000000000000000000000000006"' "$test_root/install-preloaded/compose.yml" || { echo "preloaded connector image was not ID-pinned" >&2; exit 1; }
grep -q '127.0.0.1:18382:8382' "$test_root/install-preloaded/compose.yml" || { echo "installer host-port override was not applied" >&2; exit 1; }

if printf '\n' | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-invalid-port" \
    DARKHARRBOR_HOST_PORT=65536 \
    "$repo_root/install.sh" >/dev/null 2>&1; then
    echo "installer accepted an invalid host port" >&2
    exit 1
fi

before_non_image=$(sed '/^    image: /d' "$compose_file" | sha256sum)
: >"$test_root/docker.log"
printf 'y\ny\n' | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install" \
    DARKHARRBOR_VERSION=v1.1.0 \
    "$repo_root/install.sh" --upgrade >/dev/null
after_non_image=$(sed '/^    image: /d' "$compose_file" | sha256sum)
[[ $before_non_image == "$after_non_image" ]] || { echo "upgrade changed non-image deployment state" >&2; exit 1; }
grep -q 'ghcr.io/darkharrbor/darkharrbor@sha256:0000000000000000000000000000000000000000000000000000000000000003' "$compose_file"
grep -q 'ghcr.io/darkharrbor/darkharrbor-dockerproxy@sha256:0000000000000000000000000000000000000000000000000000000000000004' "$compose_file"
[[ $(stat -c '%a' "$compose_file") == 600 ]] || { echo "upgrade loosened compose mode" >&2; exit 1; }
! grep -q -- '--profile strm up -d --no-deps docker-proxy' "$test_root/docker.log" || { echo "upgrade enabled a stopped connector" >&2; exit 1; }
snapshot_line=$(grep -n -- 'exec -T darkharrbor darkharrbor backup snapshot' "$test_root/docker.log" | head -n 1 | cut -d: -f1)
upgrade_line=$(grep -n -- 'up -d --wait --wait-timeout 120 --no-deps darkharrbor' "$test_root/docker.log" | head -n 1 | cut -d: -f1)
[[ -n $snapshot_line && -n $upgrade_line && $snapshot_line -lt $upgrade_line ]] || { echo "upgrade did not snapshot before deployment mutation" >&2; exit 1; }

: >"$test_root/docker.log"
printf 'y\ny\n' | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_PROXY_RUNNING=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install" \
    DARKHARRBOR_VERSION=v1.1.0 \
    "$repo_root/install.sh" --upgrade >/dev/null
grep -q -- '--profile strm up -d --no-deps docker-proxy' "$test_root/docker.log" || { echo "upgrade did not refresh a running connector" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-snapshot-fail"
failed_compose="$test_root/install-snapshot-fail/compose.yml"
failed_before=$(sha256sum "$failed_compose")
: >"$test_root/docker.log"
if printf 'y\ny\n' | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_SNAPSHOT_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-snapshot-fail" \
    DARKHARRBOR_VERSION=v1.1.0 \
    "$repo_root/install.sh" --upgrade >/dev/null 2>&1; then
    echo "upgrade ignored snapshot failure" >&2
    exit 1
fi
failed_after=$(sha256sum "$failed_compose")
[[ $failed_before == "$failed_after" ]] || { echo "snapshot failure changed the deployment" >&2; exit 1; }
! grep -q -- 'up -d --wait --wait-timeout 120 --no-deps darkharrbor' "$test_root/docker.log" || { echo "snapshot failure started an upgrade" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-uninstall-retain"
: >"$test_root/docker.log"
printf 'retain\n' | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-uninstall-retain" \
    "$repo_root/install.sh" --uninstall >/dev/null
grep -q -- '--profile strm down --remove-orphans' "$test_root/docker.log" || { echo "retain uninstall did not remove runtime" >&2; exit 1; }
! grep -q -- 'volume rm' "$test_root/docker.log" || { echo "retain uninstall deleted a volume" >&2; exit 1; }
! grep -q '^pull ' "$test_root/docker.log" || { echo "retain uninstall pulled an image" >&2; exit 1; }
[[ -f "$test_root/install-uninstall-retain/compose.yml" ]] || { echo "retain uninstall deleted Compose" >&2; exit 1; }
[[ -f "$test_root/install-uninstall-retain/handoff/recovery.key" ]] || { echo "retain uninstall deleted handoff" >&2; exit 1; }

: >"$test_root/docker.log"
PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-uninstall-retain" \
    "$repo_root/install.sh" --start >/dev/null
grep -q -- '--profile strm up -d --wait --wait-timeout 120 --pull never' "$test_root/docker.log" || { echo "retained start omitted its required connector" >&2; exit 1; }
! grep -q '^pull ' "$test_root/docker.log" || { echo "retained start pulled an image" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-start-no-proxy"
sed -i 's/HARRBOR_DOCKERPROXY_TARGETS: "sonarr"/HARRBOR_DOCKERPROXY_TARGETS: ""/' "$test_root/install-start-no-proxy/compose.yml"
: >"$test_root/docker.log"
PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-start-no-proxy" \
    "$repo_root/install.sh" --start >/dev/null
grep -q -- 'up -d --wait --wait-timeout 120 --pull never darkharrbor' "$test_root/docker.log" || { echo "connector-free start omitted DarkHarrbor" >&2; exit 1; }
! grep -q -- '--profile strm up' "$test_root/docker.log" || { echo "connector-free start enabled Docker socket authority" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-start-fail"
: >"$test_root/docker.log"
if PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_START_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-start-fail" \
    "$repo_root/install.sh" --start >/dev/null 2>&1; then
    echo "retained start ignored failed health" >&2
    exit 1
fi
grep -q -- 'stop darkharrbor' "$test_root/docker.log" || { echo "failed retained start left a new main service running" >&2; exit 1; }
grep -q -- '--profile strm stop docker-proxy' "$test_root/docker.log" || { echo "failed retained start left a new connector running" >&2; exit 1; }

: >"$test_root/docker.log"
if PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_START_FAIL=1 \
    FAKE_MAIN_RUNNING=1 \
    FAKE_PROXY_RUNNING=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-start-fail" \
    "$repo_root/install.sh" --start >/dev/null 2>&1; then
    echo "retained start ignored failed health for existing services" >&2
    exit 1
fi
! grep -q -- 'stop darkharrbor' "$test_root/docker.log" || { echo "failed retained start stopped a pre-existing main service" >&2; exit 1; }
! grep -q -- 'stop docker-proxy' "$test_root/docker.log" || { echo "failed retained start stopped a pre-existing connector" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-start-invalid"
sed -i 's/HARRBOR_DOCKERPROXY_TARGETS: "sonarr"/HARRBOR_DOCKERPROXY_TARGETS: "sonarr, bad"/' "$test_root/install-start-invalid/compose.yml"
: >"$test_root/docker.log"
if PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-start-invalid" \
    "$repo_root/install.sh" --start >/dev/null 2>&1; then
    echo "retained start accepted an invalid connector allowlist" >&2
    exit 1
fi
! grep -q -- ' up -d ' "$test_root/docker.log" || { echo "invalid connector allowlist started services" >&2; exit 1; }

restore_snapshot=darkharrbor-20260830T120000.000000000Z
cp -a "$test_root/install" "$test_root/install-snapshots"
: >"$test_root/docker.log"
PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-snapshots" \
    "$repo_root/install.sh" --snapshots >"$test_root/snapshots.out"
grep -qx 'darkharrbor-20260829T120000.000000000Z' "$test_root/snapshots.out" || { echo "snapshot list omitted an older candidate" >&2; exit 1; }
grep -qx 'darkharrbor-20260830T120000.000000000Z' "$test_root/snapshots.out" || { echo "snapshot list omitted the newest candidate" >&2; exit 1; }
! grep -Eq '/backup|secrets|key|database' "$test_root/snapshots.out" || { echo "snapshot list exposed internal backup details" >&2; exit 1; }
! grep -q '^pull ' "$test_root/docker.log" || { echo "snapshot list pulled an image" >&2; exit 1; }
! grep -Eq ' stop | up -d ' "$test_root/docker.log" || { echo "snapshot list mutated runtime services" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-restore"
: >"$test_root/docker.log"
printf '%s\nRESTORE %s\n' "$restore_snapshot" "$restore_snapshot" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_MAIN_RUNNING=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-restore" \
    "$repo_root/install.sh" --restore >/dev/null
verify_line=$(grep -n -- "run --rm --no-deps -T darkharrbor backup verify --from /backup/$restore_snapshot --key-file /run/darkharrbor-key/secrets.key" "$test_root/docker.log" | head -n 1 | cut -d: -f1)
snapshot_line=$(grep -n -- 'exec -T darkharrbor darkharrbor backup snapshot' "$test_root/docker.log" | head -n 1 | cut -d: -f1)
stop_line=$(grep -n -- '--profile strm stop docker-proxy darkharrbor' "$test_root/docker.log" | head -n 1 | cut -d: -f1)
restore_line=$(grep -n -- "run --rm --no-deps -T darkharrbor restore --from /backup/$restore_snapshot" "$test_root/docker.log" | head -n 1 | cut -d: -f1)
start_line=$(grep -n -- '--profile strm up -d --wait --wait-timeout 120 --pull never' "$test_root/docker.log" | tail -n 1 | cut -d: -f1)
[[ -n $verify_line && -n $snapshot_line && -n $stop_line && -n $restore_line && -n $start_line && \
   $verify_line -lt $snapshot_line && $snapshot_line -lt $stop_line && $stop_line -lt $restore_line && $restore_line -lt $start_line ]] || {
    echo "installer restore lifecycle order is unsafe" >&2
    exit 1
}
! grep -q '^pull ' "$test_root/docker.log" || { echo "installer restore pulled an image" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-restore-verify-fail"
: >"$test_root/docker.log"
if printf '%s\nRESTORE %s\n' "$restore_snapshot" "$restore_snapshot" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_MAIN_RUNNING=1 \
    FAKE_VERIFY_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-restore-verify-fail" \
    "$repo_root/install.sh" --restore >/dev/null 2>&1; then
    echo "installer restore ignored snapshot/key preflight failure" >&2
    exit 1
fi
grep -q -- 'backup verify --from' "$test_root/docker.log" || { echo "preflight failure fixture did not attempt verification" >&2; exit 1; }
! grep -Eq -- 'backup snapshot| stop docker-proxy| restore --from| up -d ' "$test_root/docker.log" || {
    echo "preflight failure mutated runtime" >&2
    exit 1
}

cp -a "$test_root/install" "$test_root/install-restore-invalid"
: >"$test_root/docker.log"
if printf '../snapshot\n' | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-restore-invalid" \
    "$repo_root/install.sh" --restore >/dev/null 2>&1; then
    echo "installer restore accepted a path-shaped snapshot name" >&2
    exit 1
fi
! grep -q -- ' backup snapshot\| restore --from\| stop docker-proxy' "$test_root/docker.log" || { echo "invalid restore name mutated runtime" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-restore-snapshot-fail"
: >"$test_root/docker.log"
if printf '%s\nRESTORE %s\n' "$restore_snapshot" "$restore_snapshot" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_MAIN_RUNNING=1 \
    FAKE_SNAPSHOT_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-restore-snapshot-fail" \
    "$repo_root/install.sh" --restore >/dev/null 2>&1; then
    echo "installer restore ignored safety snapshot failure" >&2
    exit 1
fi
! grep -q -- 'run --rm --no-deps -T darkharrbor restore' "$test_root/docker.log" || { echo "safety snapshot failure attempted restore" >&2; exit 1; }
! grep -q -- 'stop docker-proxy darkharrbor' "$test_root/docker.log" || { echo "safety snapshot failure stopped runtime" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-restore-fail"
: >"$test_root/docker.log"
if printf '%s\nRESTORE %s\n' "$restore_snapshot" "$restore_snapshot" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_RESTORE_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-restore-fail" \
    "$repo_root/install.sh" --restore >/dev/null 2>&1; then
    echo "installer restore ignored restore failure" >&2
    exit 1
fi
grep -q -- 'run --rm --no-deps -T darkharrbor restore' "$test_root/docker.log" || { echo "restore failure fixture did not attempt restore" >&2; exit 1; }
! grep -q -- ' up -d ' "$test_root/docker.log" || { echo "failed restore restarted services" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-uninstall-delete"
: >"$test_root/docker.log"
printf 'delete\nDELETE DARKHARRBOR\n' | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_INSTALLED_VOLUMES=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-uninstall-delete" \
    "$repo_root/install.sh" --uninstall >/dev/null
grep -q -- '--profile strm down --remove-orphans' "$test_root/docker.log" || { echo "delete uninstall did not remove runtime" >&2; exit 1; }
[[ $(grep -c '^volume rm darkharrbor_darkharrbor-' "$test_root/docker.log") == 3 ]] || { echo "delete uninstall did not target exactly three volumes" >&2; exit 1; }
! grep -q '^pull ' "$test_root/docker.log" || { echo "delete uninstall pulled an image" >&2; exit 1; }
[[ ! -e "$test_root/install-uninstall-delete/compose.yml" ]] || { echo "delete uninstall retained generated Compose" >&2; exit 1; }
[[ -f "$test_root/install-uninstall-delete/handoff/recovery.key" ]] || { echo "delete uninstall deleted handoff" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-uninstall-custom-project"
: >"$test_root/docker.log"
printf 'delete\nDELETE DARKHARRBOR\n' | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_INSTALLED_VOLUMES=1 \
    COMPOSE_PROJECT_NAME=rel06fixture \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-uninstall-custom-project" \
    "$repo_root/install.sh" --uninstall >/dev/null
[[ $(grep -c '^volume rm rel06fixture_darkharrbor-' "$test_root/docker.log") == 3 ]] || { echo "custom-project uninstall did not target exactly its three volumes" >&2; exit 1; }
! grep -q '^volume rm darkharrbor_darkharrbor-' "$test_root/docker.log" || { echo "custom-project uninstall targeted default-project volumes" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-uninstall-bad-label"
: >"$test_root/docker.log"
if printf 'delete\n' | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_INSTALLED_VOLUMES=1 \
    FAKE_BAD_VOLUME_LABEL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-uninstall-bad-label" \
    "$repo_root/install.sh" --uninstall >/dev/null 2>&1; then
    echo "delete uninstall accepted a foreign-owned volume" >&2
    exit 1
fi
! grep -q -- '--profile strm down --remove-orphans' "$test_root/docker.log" || { echo "foreign volume rejection mutated runtime" >&2; exit 1; }
! grep -q -- 'volume rm' "$test_root/docker.log" || { echo "foreign volume rejection deleted a volume" >&2; exit 1; }

cp -a "$test_root/install" "$test_root/install-uninstall-cancel"
: >"$test_root/docker.log"
if printf 'delete\nnot-the-confirmation\n' | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_INSTALLED_VOLUMES=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-uninstall-cancel" \
    "$repo_root/install.sh" --uninstall >/dev/null 2>&1; then
    echo "delete uninstall accepted an incorrect confirmation" >&2
    exit 1
fi
! grep -q -- '--profile strm down --remove-orphans' "$test_root/docker.log" || { echo "cancelled uninstall mutated runtime" >&2; exit 1; }
! grep -q -- 'volume rm' "$test_root/docker.log" || { echo "cancelled uninstall deleted a volume" >&2; exit 1; }
[[ -f "$test_root/install-uninstall-cancel/compose.yml" ]] || { echo "cancelled uninstall deleted Compose" >&2; exit 1; }

cp "$compose_file" "$test_root/compose.yml"
sed -i '1d' "$test_root/compose.yml"
if PATH="$test_root/bin:$PATH" DARKHARRBOR_INSTALL_DIR="$test_root" "$repo_root/install.sh" --upgrade </dev/null >/dev/null 2>&1; then
    echo "upgrade accepted an unmarked Compose file" >&2
    exit 1
fi

mkdir -p "$test_root/media-adversarial" "$test_root/install-adversarial"
chmod 0700 "$test_root/install-adversarial"
: >"$test_root/docker.log"
if printf '\n\n\n%s\ny\n' "$test_root/media-adversarial" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_RUNTIME_TARGETS=attacker \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-adversarial" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" >/dev/null 2>&1; then
    echo "installer accepted an unapproved runtime connector target" >&2
    exit 1
fi
grep -q -- '--profile strm stop docker-proxy' "$test_root/docker.log" || { echo "broad discovery connector remained running after target rejection" >&2; exit 1; }
grep -q -- '--profile strm down -v --remove-orphans' "$test_root/docker.log" || { echo "target rejection did not remove the incomplete project" >&2; exit 1; }
[[ ! -e "$test_root/install-adversarial/compose.yml" ]] || { echo "target rejection retained generated Compose" >&2; exit 1; }

mkdir -p "$test_root/media-setup-fail" "$test_root/install-setup-fail"
chmod 0700 "$test_root/install-setup-fail"
: >"$test_root/docker.log"
if printf '\n\n\n%s\ny\n' "$test_root/media-setup-fail" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_SETUP_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-setup-fail" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" >/dev/null 2>&1; then
    echo "installer ignored setup failure" >&2
    exit 1
fi
grep -q -- '--profile strm stop docker-proxy' "$test_root/docker.log" || { echo "discovery connector remained running after setup failure" >&2; exit 1; }
grep -q -- '--profile strm down -v --remove-orphans' "$test_root/docker.log" || { echo "setup failure did not remove the incomplete project" >&2; exit 1; }
[[ ! -e "$test_root/install-setup-fail/compose.yml" ]] || { echo "setup failure retained generated Compose" >&2; exit 1; }

mkdir -p "$test_root/media-health-fail" "$test_root/install-health-fail"
chmod 0700 "$test_root/install-health-fail"
: >"$test_root/docker.log"
if printf '\n\n\n%s\ny\n' "$test_root/media-health-fail" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_INITIAL_HEALTH_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-health-fail" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" >"$test_root/health-fail.out" 2>&1; then
    echo "installer ignored unhealthy first boot" >&2
    exit 1
fi
grep -q -- 'up -d --wait --wait-timeout 120 darkharrbor' "$test_root/docker.log" || { echo "first install did not use the health gate" >&2; exit 1; }
grep -q -- '--profile strm down -v --remove-orphans' "$test_root/docker.log" || { echo "unhealthy first boot did not clean up" >&2; exit 1; }
! grep -q -- ' cp ' "$test_root/docker.log" || { echo "unhealthy first boot published credential handoff" >&2; exit 1; }
! grep -q -- 'DarkHarrbor installation completed' "$test_root/health-fail.out" || { echo "unhealthy first boot reported success" >&2; exit 1; }
[[ ! -e "$test_root/install-health-fail/compose.yml" ]] || { echo "unhealthy first boot retained generated Compose" >&2; exit 1; }
[[ ! -e "$test_root/install-health-fail/handoff/recovery.key" ]] || { echo "unhealthy first boot retained a recovery credential" >&2; exit 1; }

mkdir -p "$test_root/media-copy-fail" "$test_root/install-copy-fail"
chmod 0700 "$test_root/install-copy-fail"
: >"$test_root/docker.log"
if printf '\n\n\n%s\ny\n' "$test_root/media-copy-fail" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_CP_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-copy-fail" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" >/dev/null 2>&1; then
    echo "installer ignored credential-handoff copy failure" >&2
    exit 1
fi
grep -q -- '--profile strm down -v --remove-orphans' "$test_root/docker.log" || { echo "handoff failure did not remove the incomplete project" >&2; exit 1; }
[[ ! -e "$test_root/install-copy-fail/compose.yml" ]] || { echo "handoff failure retained generated Compose" >&2; exit 1; }
[[ ! -e "$test_root/install-copy-fail/handoff/recovery.key" ]] || { echo "handoff failure retained a recovery credential" >&2; exit 1; }

mkdir -p "$test_root/media-symlink-fail" "$test_root/install-symlink-fail"
chmod 0700 "$test_root/install-symlink-fail"
printf 'do-not-change\n' >"$test_root/symlink-target"
chmod 0600 "$test_root/symlink-target"
symlink_target_before=$(sha256sum "$test_root/symlink-target")
: >"$test_root/docker.log"
if printf '\n\n\n%s\ny\n' "$test_root/media-symlink-fail" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_CP_SYMLINK=1 \
    FAKE_SYMLINK_TARGET="$test_root/symlink-target" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-symlink-fail" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" >/dev/null 2>&1; then
    echo "installer accepted a symlinked credential handoff" >&2
    exit 1
fi
[[ $(sha256sum "$test_root/symlink-target") == "$symlink_target_before" ]] || { echo "symlink rejection modified its target" >&2; exit 1; }
grep -q -- '--profile strm down -v --remove-orphans' "$test_root/docker.log" || { echo "symlinked handoff did not remove the incomplete project" >&2; exit 1; }
[[ ! -e "$test_root/install-symlink-fail/compose.yml" ]] || { echo "symlinked handoff retained generated Compose" >&2; exit 1; }
[[ ! -e "$test_root/install-symlink-fail/handoff" ]] || { echo "symlinked handoff was published" >&2; exit 1; }

for handoff_case in missing oversize; do
    handoff_flag=FAKE_RECOVERY_MISSING
    [[ $handoff_case == missing ]] || handoff_flag=FAKE_CP_OVERSIZE
    mkdir -p "$test_root/media-handoff-$handoff_case" "$test_root/install-handoff-$handoff_case"
    chmod 0700 "$test_root/install-handoff-$handoff_case"
    : >"$test_root/docker.log"
    if printf '\n\n\n%s\ny\n' "$test_root/media-handoff-$handoff_case" | \
        env PATH="$test_root/bin:$PATH" \
        FAKE_DOCKER_LOG="$test_root/docker.log" \
        "$handoff_flag=1" \
        DARKHARRBOR_INSTALL_DIR="$test_root/install-handoff-$handoff_case" \
        DARKHARRBOR_VERSION=v1.0.0 \
        "$repo_root/install.sh" >/dev/null 2>&1; then
        echo "installer accepted $handoff_case recovery material" >&2
        exit 1
    fi
    grep -q -- '--profile strm down -v --remove-orphans' "$test_root/docker.log" || { echo "$handoff_case recovery material did not remove the incomplete project" >&2; exit 1; }
    [[ ! -e "$test_root/install-handoff-$handoff_case/compose.yml" ]] || { echo "$handoff_case recovery material retained generated Compose" >&2; exit 1; }
    [[ ! -e "$test_root/install-handoff-$handoff_case/handoff" ]] || { echo "$handoff_case recovery material was published" >&2; exit 1; }
done

mkdir -p "$test_root/media-cleanup-fail" "$test_root/install-cleanup-fail"
chmod 0700 "$test_root/install-cleanup-fail"
: >"$test_root/docker.log"
if printf '\n\n\n%s\ny\n' "$test_root/media-cleanup-fail" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_SETUP_FAIL=1 \
    FAKE_DOWN_FAIL=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-cleanup-fail" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" >/dev/null 2>&1; then
    echo "installer ignored setup and cleanup failure" >&2
    exit 1
fi
[[ -f "$test_root/install-cleanup-fail/compose.yml" ]] || { echo "cleanup failure hid the retained deployment" >&2; exit 1; }

mkdir -p "$test_root/install-existing"
chmod 0700 "$test_root/install-existing"
: >"$test_root/docker.log"
if PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    FAKE_PROJECT_EXISTS=1 \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-existing" \
    DARKHARRBOR_VERSION=v1.0.0 \
    "$repo_root/install.sh" </dev/null >/dev/null 2>&1; then
    echo "installer accepted an existing fixed-name Compose project" >&2
    exit 1
fi
! grep -q '^pull ' "$test_root/docker.log" || { echo "project collision pulled images before failing" >&2; exit 1; }

# Optional `darkharrbor` command. It is the only thing the installer writes
# outside its install root, so each branch is pinned: opted in, opted out, and
# the non-interactive default. DARKHARRBOR_CLI_DIR keeps every case inside the
# test root -- a regression here would otherwise write into a real $HOME.
mkdir -p "$test_root/media-cli" "$test_root/install-cli" "$test_root/cli-bin"
chmod 0700 "$test_root/install-cli"
: >"$test_root/docker.log"
printf '\n\n%s\ny\n' "$test_root/media-cli" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-cli" \
    DARKHARRBOR_VERSION=v1.0.0 \
    DARKHARRBOR_HOST_PORT=18383 \
    DARKHARRBOR_PRELOADED_IMAGES=1 \
    DARKHARRBOR_INSTALL_CLI=1 \
    DARKHARRBOR_CLI_DIR="$test_root/cli-bin" \
    "$repo_root/install.sh" >/dev/null
[[ -f "$test_root/cli-bin/darkharrbor" ]] || { echo "opted-in CLI command was not installed" >&2; exit 1; }
[[ $(stat -c '%a' "$test_root/cli-bin/darkharrbor") == 755 ]] || { echo "CLI command is not mode 0755" >&2; exit 1; }
sh -n "$test_root/cli-bin/darkharrbor" || { echo "generated CLI command is not valid sh" >&2; exit 1; }
grep -qF -- "$test_root/install-cli/compose.yml" "$test_root/cli-bin/darkharrbor" || \
    { echo "CLI command does not target its own deployment" >&2; exit 1; }
grep -qF -- 'DARKHARRBOR_COMPOSE' "$test_root/cli-bin/darkharrbor" || \
    { echo "CLI command lost its deployment override" >&2; exit 1; }
# A stopped deployment must still be reachable, otherwise the command is
# useless in exactly the situation that needs diagnostics.
grep -qF -- 'run --rm --no-deps' "$test_root/cli-bin/darkharrbor" || \
    { echo "CLI command cannot reach a stopped deployment" >&2; exit 1; }
# Piped invocations must suppress the TTY; the wizard hard-fails under a
# pseudo-TTY on non-interactive stdin.
grep -qF -- 'exec -T darkharrbor' "$test_root/cli-bin/darkharrbor" || \
    { echo "CLI command does not disable the TTY when piped" >&2; exit 1; }

mkdir -p "$test_root/media-cli-off" "$test_root/install-cli-off" "$test_root/cli-bin-off"
chmod 0700 "$test_root/install-cli-off"
printf '\n\n%s\ny\n' "$test_root/media-cli-off" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-cli-off" \
    DARKHARRBOR_VERSION=v1.0.0 \
    DARKHARRBOR_HOST_PORT=18384 \
    DARKHARRBOR_PRELOADED_IMAGES=1 \
    DARKHARRBOR_INSTALL_CLI=0 \
    DARKHARRBOR_CLI_DIR="$test_root/cli-bin-off" \
    "$repo_root/install.sh" >/dev/null
[[ ! -e "$test_root/cli-bin-off/darkharrbor" ]] || { echo "opted-out CLI command was installed anyway" >&2; exit 1; }

# No TTY and no explicit choice must never write to the user's $HOME.
mkdir -p "$test_root/media-cli-default" "$test_root/install-cli-default" "$test_root/cli-bin-default"
chmod 0700 "$test_root/install-cli-default"
printf '\n\n%s\ny\n' "$test_root/media-cli-default" | \
    PATH="$test_root/bin:$PATH" \
    FAKE_DOCKER_LOG="$test_root/docker.log" \
    DARKHARRBOR_INSTALL_DIR="$test_root/install-cli-default" \
    DARKHARRBOR_VERSION=v1.0.0 \
    DARKHARRBOR_HOST_PORT=18385 \
    DARKHARRBOR_PRELOADED_IMAGES=1 \
    DARKHARRBOR_CLI_DIR="$test_root/cli-bin-default" \
    "$repo_root/install.sh" >/dev/null
[[ ! -e "$test_root/cli-bin-default/darkharrbor" ]] || \
    { echo "non-interactive install wrote a CLI command without being asked" >&2; exit 1; }

echo "installer verification: OK"
