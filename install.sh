#!/usr/bin/env bash
set -Eeuo pipefail
set -f
umask 077

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

mode=install
catalog_path=
answers_file=
case ${1:-} in
    "") ;;
    --catalog)
        (($# == 2)) || die "usage: ./install.sh [--catalog /absolute/private/catalog.json|--answers-file /absolute/private/answers.json|--start|--snapshots|--restore|--upgrade|--uninstall]"
        catalog_path=$2
        ;;
    --answers-file)
        (($# == 2)) || die "usage: ./install.sh [--catalog /absolute/private/catalog.json|--answers-file /absolute/private/answers.json|--start|--snapshots|--restore|--upgrade|--uninstall]"
        answers_file=$2
        ;;
    --upgrade) mode=upgrade ;;
    --start) mode=start ;;
    --snapshots) mode=snapshots ;;
    --restore) mode=restore ;;
    --uninstall) mode=uninstall ;;
    -h|--help)
        say "usage: ./install.sh [--catalog /absolute/private/catalog.json|--answers-file /absolute/private/answers.json|--start|--snapshots|--restore|--upgrade|--uninstall]"
        exit 0
        ;;
    *) die "usage: ./install.sh [--catalog /absolute/private/catalog.json|--answers-file /absolute/private/answers.json|--start|--snapshots|--restore|--upgrade|--uninstall]" ;;
esac

for command_name in docker sed; do
    command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"
done
docker info >/dev/null 2>&1 || die "Docker is not reachable by this user"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required"

validate_catalog_file() {
    local catalog_ref=$1 catalog_stat catalog_owner catalog_mode catalog_size
    [[ -f $catalog_ref ]] || die "private catalog must resolve to a regular file"
    catalog_stat=$(stat -Lc '%u:%a:%s' "$catalog_ref") || die "could not inspect the private catalog"
    IFS=: read -r catalog_owner catalog_mode catalog_size <<<"$catalog_stat"
    [[ $catalog_owner == "$(id -u)" ]] || die "private catalog must be owned by the current user"
    [[ $catalog_mode == 600 ]] || die "private catalog must be mode 0600"
    ((catalog_size > 0 && catalog_size <= 2097152)) || die "private catalog must be between 1 byte and 2 MiB"
}

catalog_fd=
if [[ -n $catalog_path ]]; then
    [[ $catalog_path == /* && $catalog_path != *$'\n'* ]] || die "private catalog path must be absolute and contain no newline"
    [[ ! -L $catalog_path ]] || die "private catalog must be a regular non-symlink file"
    validate_catalog_file "$catalog_path"
fi

# `-answers` (darkharrbor setup's own headless-replay flag) already enforces
# mode-0600 and a non-group/world-writable parent directory itself; this
# validates fast, before any image pull, so a bad answers file fails closed
# here rather than mid-run.
validate_answers_file() {
    local answers_ref=$1 answers_dir answers_stat answers_owner answers_mode answers_size answers_dir_mode
    [[ -f $answers_ref ]] || die "answers file must resolve to a regular file"
    answers_stat=$(stat -Lc '%u:%a:%s' "$answers_ref") || die "could not inspect the answers file"
    IFS=: read -r answers_owner answers_mode answers_size <<<"$answers_stat"
    [[ $answers_owner == "$(id -u)" ]] || die "answers file must be owned by the current user"
    [[ $answers_mode == 600 ]] || die "answers file must be mode 0600"
    # Matches darkharrbor setup -answers's own maxAnswersFileBytes (1 MiB) --
    # fail here, before any image pull, rather than accept a file this
    # install only rejects later, mid-run, inside the container.
    ((answers_size > 0 && answers_size <= 1048576)) || die "answers file must be between 1 byte and 1 MiB"
    answers_dir=$(dirname -- "$answers_ref")
    answers_dir_mode=$(stat -Lc '%a' "$answers_dir") || die "could not inspect the answers file's directory"
    (( (8#$answers_dir_mode & 0022) == 0 )) || die "answers file's directory must not be group/world-writable"
}

if [[ -n $answers_file ]]; then
    [[ $answers_file == /* && $answers_file != *$'\n'* ]] || die "answers file path must be absolute and contain no newline"
    [[ ! -L $answers_file ]] || die "answers file must be a regular non-symlink file"
    validate_answers_file "$answers_file"
fi

install_root=${DARKHARRBOR_INSTALL_DIR:-"$(pwd)/.darkharrbor"}
compose_file="$install_root/compose.yml"
project_name=${COMPOSE_PROJECT_NAME:-darkharrbor}
[[ $project_name =~ ^[a-z0-9][a-z0-9_-]*$ ]] || die "COMPOSE_PROJECT_NAME must match ^[a-z0-9][a-z0-9_-]*$"
if [[ $mode == install ]]; then
    [[ ! -e "$compose_file" ]] || die "$compose_file already exists; rerun with --upgrade"
    if [[ -e $install_root ]]; then
        [[ -d $install_root && ! -L $install_root ]] || die "$install_root must be a real directory"
        [[ $(stat -c '%u' "$install_root") == "$(id -u)" ]] || die "$install_root is not owned by the current user"
        install_mode=$(stat -c '%a' "$install_root")
        (( (8#$install_mode & 0022) == 0 )) || die "$install_root must not be group/world writable"
    fi
    [[ ! -e "$install_root/handoff" ]] || die "$install_root/handoff already exists; move or remove it deliberately"

    [[ -z $(docker ps -aq --filter "label=com.docker.compose.project=$project_name") ]] || \
        die "a $project_name Compose project already has containers"
    for object_name in darkharrbor-config darkharrbor-backups darkharrbor-keys; do
        ! docker volume inspect "${project_name}_${object_name}" >/dev/null 2>&1 || \
            die "Docker volume already exists: ${project_name}_${object_name}"
    done
    ! docker network inspect "${project_name}_control" >/dev/null 2>&1 || \
        die "Docker network already exists: ${project_name}_control"
else
    [[ -f $compose_file && ! -L $compose_file ]] || die "$compose_file is not a regular installer deployment"
    [[ $(stat -c '%u' "$compose_file") == "$(id -u)" ]] || die "$compose_file is not owned by the current user"
    [[ $(stat -c '%a' "$compose_file") == 600 ]] || die "$compose_file must be mode 0600"
    IFS= read -r installer_marker <"$compose_file"
    [[ $installer_marker == '# DarkHarrbor installer schema: v1' ]] || die "$compose_file is not owned by installer schema v1"
    docker compose -f "$compose_file" config --quiet || die "$compose_file is not a valid Compose deployment"
fi

if [[ $mode == snapshots ]]; then
    say "Structurally complete snapshot candidates (newest last):"
    docker compose -f "$compose_file" run --rm --no-deps -T darkharrbor \
        backup list --from /backup || die "snapshot candidates are unavailable"
    say "Final permission, identity, size, and SQLite-integrity validation occurs during restore."
    exit 0
fi

if [[ $mode == restore ]]; then
    say "DarkHarrbor snapshot restore"
    say ""
    say "Enter one exact snapshot directory name from the private backup volume."
    say "The matching recovery key must already be retained; no key is read or displayed here."
    say "The current darkharrbor.conf and bind-mounted media are not restored."
    printf 'Snapshot name: '
    IFS= read -r restore_snapshot
    [[ $restore_snapshot =~ ^darkharrbor-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z$ ]] || \
        die "snapshot name must match darkharrbor-YYYYMMDDTHHMMSS.NNNNNNNNNZ"
    printf 'Type RESTORE %s to continue: ' "$restore_snapshot"
    IFS= read -r restore_confirmation
    [[ $restore_confirmation == "RESTORE $restore_snapshot" ]] || die "cancelled without changes"

    docker compose -f "$compose_file" run --rm --no-deps -T darkharrbor \
        backup verify --from "/backup/$restore_snapshot" \
        --key-file /run/darkharrbor-key/secrets.key || \
        die "snapshot or recovery-key preflight failed; runtime was not stopped"

    restore_main_running=$(docker compose -f "$compose_file" ps --status running -q darkharrbor) || \
        die "could not inspect DarkHarrbor runtime state"
    if [[ -n $restore_main_running ]]; then
        docker compose -f "$compose_file" exec -T darkharrbor darkharrbor backup snapshot || \
            die "pre-restore safety snapshot failed; runtime was not stopped"
    else
        say "DarkHarrbor is stopped; no new safety snapshot can be created."
    fi

    docker compose -f "$compose_file" --profile strm stop docker-proxy darkharrbor || \
        die "could not stop the retained deployment; restore was not attempted"
    if ! docker compose -f "$compose_file" run --rm --no-deps -T darkharrbor \
        restore --from "/backup/$restore_snapshot"; then
        die "restore failed; services remain stopped and the pre-restore state was preserved"
    fi
    "$0" --start || die "snapshot restored, but the retained deployment did not become healthy"
    say "Snapshot restore completed and the retained deployment is healthy."
    exit 0
fi

if [[ $mode == start ]]; then
    mapfile -t connector_target_lines < <(
        sed -n 's/^      HARRBOR_DOCKERPROXY_TARGETS: "\(.*\)"$/\1/p' "$compose_file"
    )
    ((${#connector_target_lines[@]} == 1)) || die "installer deployment has an unexpected connector target field"
    runtime_targets_csv=${connector_target_lines[0]}
    [[ -z $runtime_targets_csv || $runtime_targets_csv =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*(,[A-Za-z0-9][A-Za-z0-9_.-]*)*$ ]] || \
        die "installer deployment has an invalid connector target list"

    main_was_running=$(docker compose -f "$compose_file" ps --status running -q darkharrbor) || \
        die "could not inspect DarkHarrbor runtime state"
    proxy_was_running=
    if [[ -n $runtime_targets_csv ]]; then
        proxy_was_running=$(docker compose -f "$compose_file" --profile strm ps --status running -q docker-proxy) || \
            die "could not inspect connector runtime state"
        start_args=(--profile strm up -d --wait --wait-timeout 120 --pull never)
    else
        start_args=(up -d --wait --wait-timeout 120 --pull never darkharrbor)
    fi

    if ! docker compose -f "$compose_file" "${start_args[@]}"; then
        [[ -n $main_was_running ]] || docker compose -f "$compose_file" stop darkharrbor >/dev/null 2>&1 || true
        if [[ -n $runtime_targets_csv && -z $proxy_was_running ]]; then
            docker compose -f "$compose_file" --profile strm stop docker-proxy >/dev/null 2>&1 || true
        fi
        die "retained deployment did not become healthy; newly started services were stopped"
    fi
    say "Retained DarkHarrbor deployment started from pinned local images."
    [[ -z $runtime_targets_csv ]] || say "The scoped connector was started for its retained non-empty target allowlist."
    say "Run diagnostics: docker compose -f $compose_file exec darkharrbor darkharrbor doctor"
    exit 0
fi

if [[ $mode == uninstall ]]; then
    say "DarkHarrbor uninstall"
    say ""
    say "Choose exactly one action:"
    say "  retain  remove runtime containers and the internal network; keep Compose, volumes, handoff, media, and images"
    say "  delete  also delete the three installer-owned Docker volumes and generated Compose"
    say "External Arr networks, bind-mounted media, pulled images, and private handoff files are never deleted."
    printf 'Action [cancel]: '
    IFS= read -r uninstall_action
    case $uninstall_action in
        retain)
            docker compose -f "$compose_file" --profile strm down --remove-orphans || \
                die "runtime removal failed; installer state was retained"
            say "Runtime removed. Installer state retained at $install_root."
            say "Restart later with: ./install.sh --start"
            exit 0
            ;;
        delete) ;;
        ""|cancel) die "cancelled without changes" ;;
        *) die "action must be retain, delete, or cancel" ;;
    esac

    declare -a uninstall_volumes=(
        "${project_name}_darkharrbor-config"
        "${project_name}_darkharrbor-backups"
        "${project_name}_darkharrbor-keys"
    )
    volume_inventory=$(docker volume ls -q) || die "could not inventory Docker volumes"
    declare -a present_uninstall_volumes=()
    for volume_name in "${uninstall_volumes[@]}"; do
        volume_present=false
        while IFS= read -r existing_volume; do
            [[ $existing_volume == "$volume_name" ]] && { volume_present=true; break; }
        done <<<"$volume_inventory"
        [[ $volume_present == true ]] || continue
        volume_key=${volume_name#"${project_name}_"}
        volume_labels=$(docker volume inspect --format \
            '{{index .Labels "com.docker.compose.project"}}:{{index .Labels "com.docker.compose.volume"}}' \
            "$volume_name" 2>/dev/null) || die "could not inspect $volume_name"
        [[ $volume_labels == "$project_name:$volume_key" ]] || \
            die "refusing volume with unexpected ownership labels: $volume_name"
        present_uninstall_volumes+=("$volume_name")
    done

    say ""
    say "Permanent deletion preview:"
    printf '  %s\n' "${uninstall_volumes[@]}"
    say "  generated Compose: $compose_file"
    say "  retained: external networks, bind-mounted media, images, and $install_root/handoff"
    printf 'Type DELETE DARKHARRBOR to continue: '
    IFS= read -r delete_confirmation
    [[ $delete_confirmation == 'DELETE DARKHARRBOR' ]] || die "cancelled without changes"

    docker compose -f "$compose_file" --profile strm down --remove-orphans || \
        die "runtime removal failed; volumes and Compose were retained"
    for volume_name in "${present_uninstall_volumes[@]}"; do
        docker volume rm "$volume_name" >/dev/null || \
            die "could not delete $volume_name; Compose was retained for recovery"
    done
    rm -f -- "$compose_file"
    # Only remove the command if it points at THIS deployment; another
    # deployment's wrapper, or a hand-written one, is left alone.
    uninstall_cli_dir="${DARKHARRBOR_CLI_DIR:-$HOME/.local/bin}"
    if [[ -f $uninstall_cli_dir/darkharrbor ]] && grep -qF -- "$compose_file" "$uninstall_cli_dir/darkharrbor"; then
        rm -f -- "$uninstall_cli_dir/darkharrbor"
        say "Removed the darkharrbor command for this deployment."
    fi
    say "Runtime, installer Docker volumes, and generated Compose removed."
    say "Private handoff files, external networks, bind-mounted media, and images were retained."
    exit 0
fi

release_tag=${DARKHARRBOR_VERSION:-latest}
[[ $release_tag =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "invalid DARKHARRBOR_VERSION"
image_repo=${DARKHARRBOR_IMAGE_REPO:-ghcr.io/darkharrbor/darkharrbor}
[[ $image_repo =~ ^[a-z0-9][a-z0-9./_-]*$ ]] || die "invalid DARKHARRBOR_IMAGE_REPO"
preloaded_images=${DARKHARRBOR_PRELOADED_IMAGES:-0}
[[ $preloaded_images == 0 || $preloaded_images == 1 ]] || die "DARKHARRBOR_PRELOADED_IMAGES must be 0 or 1"
host_port=${DARKHARRBOR_HOST_PORT:-8382}
[[ $host_port =~ ^[0-9]{1,5}$ ]] || die "DARKHARRBOR_HOST_PORT must be a decimal port from 1 through 65535"
((10#$host_port >= 1 && 10#$host_port <= 65535)) || die "DARKHARRBOR_HOST_PORT must be a decimal port from 1 through 65535"
main_ref="$image_repo:$release_tag"
proxy_ref="$image_repo-dockerproxy:$release_tag"

say "Welcome to DarkHarrbor — the dark port for your media library."
say ""
say "Credentials are not requested or read by this installer."

resolve_digest() {
    local repository=$1 reference=$2 digest
    while IFS= read -r digest; do
        [[ $digest == "$repository"@sha256:* ]] && { printf '%s\n' "$digest"; return 0; }
    done < <(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$reference")
    return 1
}

if [[ $preloaded_images == 1 ]]; then
    say "Using explicitly selected preloaded images for release $release_tag; no registry pull will run."
    main_digest=$(docker image inspect --format '{{.Id}}' "$main_ref" 2>/dev/null) || die "preloaded image is unavailable: $main_ref"
    proxy_digest=$(docker image inspect --format '{{.Id}}' "$proxy_ref" 2>/dev/null) || die "preloaded image is unavailable: $proxy_ref"
    [[ $main_digest =~ ^sha256:[0-9a-f]{64}$ && $proxy_digest =~ ^sha256:[0-9a-f]{64}$ ]] || \
        die "preloaded images did not resolve to immutable local image IDs"
else
    say "This will download the main and scoped-connector images for release $release_tag."
    printf 'Continue with the image download? [Y/n]: '
    IFS= read -r download
    [[ ${download:-y} =~ ^[Yy]$ ]] || die "cancelled without changes"
    docker pull "$main_ref"
    docker pull "$proxy_ref"
    main_digest=$(resolve_digest "$image_repo" "$main_ref") || die "could not resolve immutable digest for $main_ref"
    proxy_digest=$(resolve_digest "$image_repo-dockerproxy" "$proxy_ref") || die "could not resolve immutable digest for $proxy_ref"
fi
[[ $(docker image inspect --format '{{index .Config.Labels "org.darkharrbor.install-schema"}}' "$main_digest") == v1 ]] || die "release does not support this installer schema"
[[ $(docker image inspect --format '{{index .Config.Labels "org.darkharrbor.dockerproxy-policy"}}' "$proxy_digest") == v1 ]] || die "connector release predates the hardened target/command policy"

if [[ $mode == upgrade ]]; then
    say ""
    say "Upgrade preview (no secrets):"
    say "  deployment:      $compose_file"
    say "  main image:      $main_digest"
    say "  connector image: $proxy_digest"
    say "  preserved:       networks, paths, targets, volumes, settings, and secrets"
    say "The current release must create a complete snapshot before deployment changes."
    say "Its unlock key remains separate and must already be retained for rollback."
    printf 'Confirm the separate recovery key is retained, then create the snapshot? [y/N]: '
    IFS= read -r confirm_upgrade
    [[ $confirm_upgrade =~ ^[Yy]$ ]] || die "cancelled without changes"
    docker compose -f "$compose_file" exec -T darkharrbor darkharrbor backup snapshot || \
        die "pre-upgrade snapshot failed; deployment was not changed"

    tmp_compose=$(mktemp "$install_root/compose.yml.tmp.XXXXXX")
    trap 'rm -f -- "$tmp_compose"' EXIT
    awk -v main_image="$main_digest" -v proxy_image="$proxy_digest" '
        /^  [A-Za-z0-9_-]+:$/ { service = substr($0, 3, length($0) - 3) }
        service == "darkharrbor" && /^    image: ".*"$/ {
            print "    image: \"" main_image "\""; main_count++; next
        }
        service == "docker-proxy" && /^    image: ".*"$/ {
            print "    image: \"" proxy_image "\""; proxy_count++; next
        }
        { print }
        END { if (main_count != 1 || proxy_count != 1) exit 1 }
    ' "$compose_file" >"$tmp_compose" || die "installer deployment has unexpected image fields"
    chmod 0600 "$tmp_compose"
    docker compose -f "$tmp_compose" config --quiet || die "updated deployment failed Compose validation"

    proxy_running=$(docker compose -f "$compose_file" ps --status running -q docker-proxy)
    mv "$tmp_compose" "$compose_file"
    trap - EXIT
    if ! docker compose -f "$compose_file" up -d --wait --wait-timeout 120 --no-deps darkharrbor; then
        docker compose -f "$compose_file" stop darkharrbor >/dev/null 2>&1 || true
        die "upgrade did not become healthy; keep the new image stopped and follow the documented snapshot rollback procedure"
    fi
    if [[ -n $proxy_running ]]; then
        docker compose -f "$compose_file" --profile strm up -d --no-deps docker-proxy
    fi
    say "Upgrade completed. The deployment remains pinned to immutable image digests."
    say "Run diagnostics: docker compose -f $compose_file exec darkharrbor darkharrbor doctor"
    exit 0
fi

declare -a candidate_names=() candidate_images=()
while IFS=$'\t' read -r container_name container_image; do
    [[ $container_name =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || continue
    case "$container_image" in
        */linuxserver/sonarr:*|*/linuxserver/sonarr@*|linuxserver/sonarr:*|linuxserver/sonarr@*|\
        */linuxserver/radarr:*|*/linuxserver/radarr@*|linuxserver/radarr:*|linuxserver/radarr@*|\
        */linuxserver/prowlarr:*|*/linuxserver/prowlarr@*|linuxserver/prowlarr:*|linuxserver/prowlarr@*|\
        */jellyfin/jellyfin:*|*/jellyfin/jellyfin@*|jellyfin/jellyfin:*|jellyfin/jellyfin@*)
            candidate_names+=("$container_name")
            candidate_images+=("$container_image")
            ;;
    esac
done < <(docker ps --filter status=running --format '{{.Names}}\t{{.Image}}')

declare -a approved_targets=()
if ((${#candidate_names[@]})); then
    say ""
    say "Detected supported running containers (names and images only):"
    for i in "${!candidate_names[@]}"; do
        printf '  %d. %s (%s)\n' "$((i + 1))" "${candidate_names[$i]}" "${candidate_images[$i]}"
    done
    printf 'Approve connector access by number, "all", or "manual" [all]: '
    IFS= read -r selection
    selection=${selection:-all}
    if [[ $selection != manual ]]; then
        if [[ $selection == all ]]; then
            approved_targets=("${candidate_names[@]}")
        else
            selection=${selection//,/ }
            for number in $selection; do
                [[ $number =~ ^[0-9]+$ ]] || die "invalid target selection"
                ((number >= 1 && number <= ${#candidate_names[@]})) || die "target selection out of range"
                approved_targets+=("${candidate_names[$((number - 1))]}")
            done
        fi
    fi
else
    say "No supported running containers were detected; the wizard will use manual Arr entry."
fi

declare -A network_seen=()
declare -a networks=()
if ((${#approved_targets[@]})); then
    for target in "${approved_targets[@]}"; do
        while IFS= read -r network_name; do
            [[ $network_name =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || continue
            [[ $network_name == bridge || $network_name == host || $network_name == none ]] && continue
            if [[ -z ${network_seen[$network_name]+x} ]]; then
                network_seen[$network_name]=1
                networks+=("$network_name")
            fi
        done < <(docker inspect --format '{{range $name, $_ := .NetworkSettings.Networks}}{{println $name}}{{end}}' "$target")
    done
fi

if ((${#networks[@]})); then
    say ""
    say "Detected user-defined networks for the approved containers: ${networks[*]}"
    printf 'Attach DarkHarrbor to these networks? [Y/n]: '
    IFS= read -r confirm_networks
    [[ ${confirm_networks:-y} =~ ^[Yy]$ ]] || networks=()
fi
if ((${#networks[@]} == 0)); then
    say ""
    say "Available user-defined Docker networks:"
    docker network ls --filter type=custom --format '  {{.Name}}'
    printf 'Enter one or more existing network names (comma-separated): '
    IFS= read -r network_input
    network_input=${network_input//,/ }
    for network_name in $network_input; do
        [[ $network_name =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die "invalid network name"
        docker network inspect "$network_name" >/dev/null 2>&1 || die "network does not exist: $network_name"
        networks+=("$network_name")
    done
fi
((${#networks[@]})) || die "at least one user-defined network is required"

printf 'Host media path for DarkHarrbor .strm files [/mnt/darkharrbor]: '
IFS= read -r data_path
data_path=${data_path:-/mnt/darkharrbor}
[[ $data_path =~ ^/[A-Za-z0-9._/-]+$ && $data_path != */../* && $data_path != */.. ]] || die "media path must be a clean absolute path without spaces"
[[ -d $data_path ]] || die "$data_path does not exist; create and permission it deliberately before installation"
docker run --rm --read-only --network none --user 1000:1000 --cap-drop ALL \
    --security-opt no-new-privileges \
    --mount "type=bind,src=$data_path,dst=/data" \
    --entrypoint /bin/sh "$main_digest" -c 'test -d /data && test -w /data' || \
    die "$data_path is not writable by DarkHarrbor's non-root UID/GID 1000:1000"

targets_csv=
if ((${#approved_targets[@]})); then
    targets_csv=$(IFS=,; printf '%s' "${approved_targets[*]}")
fi
targets_display=${targets_csv:-"none (manual entry)"}

say ""
say "Installation preview (no secrets):"
say "  main image:      $main_digest"
say "  connector image: $proxy_digest"
say "  compose file:    $compose_file (mode 0600)"
say "  media path:      $data_path"
say "  networks:        ${networks[*]}"
say "  approved targets: $targets_display"
say "  generic catalog: $([[ -n $catalog_path ]] && printf 'selected (private; contents hidden)' || printf 'none')"
say "  runtime access:  narrowed after setup to only selected managed services"
say "  published port:  127.0.0.1:$host_port only"
printf 'Create this deployment and run the first-use wizard? [y/N]: '
IFS= read -r apply
[[ $apply =~ ^[Yy]$ ]] || die "cancelled without changes"

mkdir -p "$install_root"
chmod 0700 "$install_root"
tmp_compose=$(mktemp "$install_root/compose.yml.tmp.XXXXXX")
trap 'rm -f -- "$tmp_compose"' EXIT
{
    printf '%s\n' '# DarkHarrbor installer schema: v1'
    printf '%s\n' 'name: darkharrbor'
    printf '%s\n' 'services:'
    printf '%s\n' '  darkharrbor:'
    printf '    image: "%s"\n' "$main_digest"
    printf '%s\n' '    restart: unless-stopped' '    user: "1000:1000"' '    read_only: true'
    printf '%s\n' '    tmpfs: ["/tmp"]' '    cap_drop: ["ALL"]' '    security_opt: ["no-new-privileges:true"]'
    printf '%s\n' '    environment:'
    printf '%s\n' '      HARRBOR_SERVER_ADDRESS: ":8381"' '      HARRBOR_SERVER_BASE_URL: "http://darkharrbor:8381"'
    printf '%s\n' '      HARRBOR_STREMIO_CLIENT_ADDRESS: ":8382"' '      HARRBOR_DATABASE_PATH: "/config/darkharrbor.db"'
    printf '%s\n' '      HARRBOR_DATA_ROOT: "/data"' '      HARRBOR_REPORTED_PATH_PREFIX: "/mnt/darkharrbor"'
    printf '%s\n' '      HARRBOR_SECRETS_FILE: "/config/secrets.sealed"' '      HARRBOR_SECRETS_UNLOCK: "keyfile"'
    printf '%s\n' '      HARRBOR_SECRETS_KEY_FILE: "/run/darkharrbor-key/secrets.key"' '      HARRBOR_DOCKER_PROXY_URL: "http://docker-proxy:2375"'
    printf '    ports: ["127.0.0.1:%s:8382"]\n' "$host_port"
    printf '%s\n' '    volumes:'
    printf '%s\n' '      - darkharrbor-config:/config' '      - darkharrbor-backups:/backup' '      - darkharrbor-keys:/run/darkharrbor-key'
    printf '      - "%s:/data"\n' "$data_path"
    printf '%s\n' '    networks:' '      - control'
    for i in "${!networks[@]}"; do printf '      - arr_%d\n' "$i"; done
    printf '%s\n' '    healthcheck:' '      test: ["CMD-SHELL", "wget -qO- http://localhost:8381/healthz | grep -q ok || exit 1"]'
    printf '%s\n' '      interval: 30s' '      timeout: 10s' '      retries: 3' '      start_period: 15s'
    printf '%s\n' '  docker-proxy:'
    printf '    image: "%s"\n' "$proxy_digest"
    printf '%s\n' '    restart: unless-stopped' '    profiles: ["strm"]' '    read_only: true'
    printf '%s\n' '    tmpfs: ["/tmp"]' '    cap_drop: ["ALL"]' '    security_opt: ["no-new-privileges:true"]' '    environment:'
    printf '      HARRBOR_DOCKERPROXY_TARGETS: "%s"\n' "$targets_csv"
    printf '%s\n' '    volumes: ["/var/run/docker.sock:/var/run/docker.sock:ro"]' '    networks: ["control"]'
    printf '%s\n' 'networks:' '  control:' '    internal: true'
    for i in "${!networks[@]}"; do
        printf '  arr_%d:\n    external: true\n    name: "%s"\n' "$i" "${networks[$i]}"
    done
    printf '%s\n' 'volumes:' '  darkharrbor-config:' '  darkharrbor-backups:' '  darkharrbor-keys:'
} >"$tmp_compose"
chmod 0600 "$tmp_compose"
docker compose -f "$tmp_compose" config --quiet
mv "$tmp_compose" "$compose_file"
trap - EXIT

install_complete=false
runtime_targets_file=
runtime_compose=
handoff_dir="$install_root/handoff"
handoff_tmp=
cleanup_incomplete_install() {
    local status=$?
    trap - EXIT INT TERM
    if [[ $install_complete != true ]]; then
        local project_removed=true
        docker compose -f "$compose_file" --profile strm down -v --remove-orphans >/dev/null 2>&1 || project_removed=false
        [[ -n $runtime_targets_file ]] && rm -f -- "$runtime_targets_file"
        [[ -n $runtime_compose ]] && rm -f -- "$runtime_compose"
        if [[ -n $handoff_tmp ]]; then
            rm -f -- "$handoff_tmp/recovery.key" "$handoff_tmp/mediaflow.env" "$handoff_tmp/stremio-install.url"
            rmdir -- "$handoff_tmp" >/dev/null 2>&1 || true
        fi
        rm -f -- "$handoff_dir/recovery.key" "$handoff_dir/mediaflow.env" \
            "$handoff_dir/stremio-install.url" "$handoff_dir/connector-targets"
        rmdir -- "$handoff_dir" >/dev/null 2>&1 || true
        if [[ $project_removed == true ]]; then
            rm -f -- "$compose_file"
            rmdir -- "$install_root" >/dev/null 2>&1 || true
            printf '%s\n' "Incomplete installation removed; pulled images were retained." >&2
        else
            printf 'ERROR: incomplete Docker project cleanup failed; deployment retained at %s for explicit recovery\n' "$compose_file" >&2
        fi
    fi
    exit "$status"
}
trap cleanup_incomplete_install EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ -n $catalog_path ]]; then
    [[ -f $catalog_path && ! -L $catalog_path ]] || die "private catalog changed before import"
    exec {catalog_fd}<"$catalog_path"
    validate_catalog_file "/proc/self/fd/$catalog_fd"
    if ! docker compose -f "$compose_file" run --rm --no-deps -T darkharrbor \
        catalog import <&$catalog_fd; then
        die "private generic catalog validation/import failed"
    fi
    exec {catalog_fd}<&-
    catalog_fd=
fi

if ((${#approved_targets[@]})); then
    docker compose -f "$compose_file" --profile strm up -d docker-proxy
fi
setup_status=0
if [[ -n $answers_file ]]; then
    # Headless replay: `-T` disables pseudo-TTY allocation (required — a
    # piped/non-interactive stdin cannot satisfy `-it`, and forcing it fails
    # closed with "the input device is not a TTY" rather than hanging), and
    # the answers file is bind-mounted read-only into the run for this one
    # invocation only. It is never copied into a persistent volume.
    [[ -f $answers_file && ! -L $answers_file ]] || die "answers file changed before use"
    validate_answers_file "$answers_file"
    docker compose -f "$compose_file" run --rm -T --no-deps \
        -v "$answers_file:/run/darkharrbor-key/answers.json:ro" \
        darkharrbor setup --defer-arr-registration \
        --handoff-dir /run/darkharrbor-key/setup-handoff \
        -answers /run/darkharrbor-key/answers.json || setup_status=$?
    # The answers file holds every credential entered for this run in
    # plaintext. Remove it once consumed, on success or failure alike -- it
    # has no reason to persist past this one invocation either way.
    rm -f -- "$answers_file"
else
    docker compose -f "$compose_file" run --rm -it darkharrbor setup --defer-arr-registration --handoff-dir /run/darkharrbor-key/setup-handoff || setup_status=$?
fi
if ((setup_status != 0)); then
    docker compose -f "$compose_file" --profile strm stop docker-proxy >/dev/null 2>&1 || true
    die "setup failed; the discovery connector has been stopped"
fi
docker compose -f "$compose_file" --profile strm stop docker-proxy >/dev/null 2>&1 || true

runtime_targets_file=$(mktemp "$install_root/connector-targets.tmp.XXXXXX")
runtime_compose=$(mktemp "$install_root/compose.yml.runtime.XXXXXX")
docker compose -f "$compose_file" run --rm --no-deps -T --entrypoint /bin/cat darkharrbor \
    /run/darkharrbor-key/setup-handoff/connector-targets >"$runtime_targets_file"
[[ $(stat -c '%s' "$runtime_targets_file") -le 16384 ]] || die "runtime connector target handoff is too large"
declare -A runtime_seen=()
declare -a runtime_targets=()
while IFS= read -r target || [[ -n $target ]]; do
    [[ -n $target ]] || continue
    [[ $target =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die "setup returned an invalid runtime connector target"
    approved=false
    for approved_target in "${approved_targets[@]}"; do
        if [[ $target == "$approved_target" ]]; then
            approved=true
            break
        fi
    done
    [[ $approved == true ]] || die "setup attempted to broaden connector access beyond operator approval"
    if [[ -z ${runtime_seen[$target]+x} ]]; then
        runtime_seen[$target]=1
        runtime_targets+=("$target")
    fi
    ((${#runtime_targets[@]} <= 64)) || die "setup returned too many runtime connector targets"
done <"$runtime_targets_file"
runtime_targets_csv=
if ((${#runtime_targets[@]})); then
    runtime_targets_csv=$(IFS=,; printf '%s' "${runtime_targets[*]}")
fi
awk -v targets="$runtime_targets_csv" '
    /^      HARRBOR_DOCKERPROXY_TARGETS: ".*"$/ {
        print "      HARRBOR_DOCKERPROXY_TARGETS: \"" targets "\""; count++; next
    }
    { print }
    END { if (count != 1) exit 1 }
' "$compose_file" >"$runtime_compose" || die "installer deployment has an unexpected connector target field"
chmod 0600 "$runtime_compose"
docker compose -f "$runtime_compose" config --quiet || die "narrowed deployment failed Compose validation"
mv "$runtime_compose" "$compose_file"
docker compose -f "$compose_file" run --rm --no-deps -T --entrypoint /bin/rm darkharrbor \
    -f -- /run/darkharrbor-key/setup-handoff/connector-targets
rm -f -- "$runtime_targets_file"
runtime_targets_file=
runtime_compose=
if ((${#runtime_targets[@]})); then
    docker compose -f "$compose_file" --profile strm up -d --no-deps --force-recreate docker-proxy
else
    docker compose -f "$compose_file" --profile strm stop docker-proxy >/dev/null 2>&1 || true
    docker compose -f "$compose_file" --profile strm rm -f docker-proxy >/dev/null 2>&1 || true
fi
docker compose -f "$compose_file" up -d --wait --wait-timeout 120 darkharrbor || \
    die "DarkHarrbor did not become healthy; installation was not completed"
docker compose -f "$compose_file" exec -T darkharrbor darkharrbor reconcile-arr || \
    die "DarkHarrbor could not register the selected Arr search/grab topology"

handoff_tmp=$(mktemp -d "$install_root/handoff.tmp.XXXXXX")
chmod 0700 "$handoff_tmp"
copy_handoff_file() {
    local name=$1 max_bytes=$2 required=$3 destination="$handoff_tmp/$1" handoff_size
    if ! docker compose -f "$compose_file" exec -T darkharrbor \
        test -f "/run/darkharrbor-key/setup-handoff/$name"; then
        [[ $required == true ]] && die "required private handoff file is unavailable"
        return 0
    fi
    docker compose -f "$compose_file" cp \
        "darkharrbor:/run/darkharrbor-key/setup-handoff/$name" "$destination"
    [[ -f $destination && ! -L $destination ]] || die "private handoff contained an unsafe file"
    [[ $(stat -c '%u' "$destination") == "$(id -u)" ]] || die "private handoff file has unexpected ownership"
    handoff_size=$(stat -c '%s' "$destination")
    ((handoff_size > 0 && handoff_size <= max_bytes)) || die "private handoff file has an invalid size"
    chmod 0600 "$destination"
}
copy_handoff_file recovery.key 4096 true
copy_handoff_file mediaflow.env 65536 false
copy_handoff_file stremio-install.url 65536 false
mv "$handoff_tmp" "$handoff_dir"
handoff_tmp=
docker compose -f "$compose_file" exec -T darkharrbor rm -f -- \
    /run/darkharrbor-key/setup-handoff/recovery.key \
    /run/darkharrbor-key/setup-handoff/mediaflow.env \
    /run/darkharrbor-key/setup-handoff/stremio-install.url \
    /run/darkharrbor-key/setup-handoff/connector-targets
docker compose -f "$compose_file" exec -T darkharrbor rmdir -- /run/darkharrbor-key/setup-handoff

install_complete=true

say ""
say "DarkHarrbor installation completed."
say "Deployment file: $compose_file"
say "Private recovery handoff: $handoff_dir/recovery.key"
if [[ -e "$handoff_dir/mediaflow.env" ]]; then
    say "Private AIOStreams handoff: $handoff_dir/mediaflow.env"
fi
if [[ -e "$handoff_dir/stremio-install.url" ]]; then
    say "Private Stremio handoff: $handoff_dir/stremio-install.url"
fi
say "Move retained handoff files to your secure vault, then delete the local copies."

# Optional `darkharrbor` command. This is the only thing the installer writes
# outside its own install root, so it is asked for explicitly and declining
# changes nothing. It reaches the in-container CLI only -- upgrade, restore,
# snapshots and uninstall stay with this script, which owns the deployment.
cli_dir="${DARKHARRBOR_CLI_DIR:-$HOME/.local/bin}"
install_cli=n
if [[ -n ${DARKHARRBOR_INSTALL_CLI:-} ]]; then
    # Explicit choice, so headless runs (--answers-file) can opt in or out
    # without a prompt they could never answer.
    [[ $DARKHARRBOR_INSTALL_CLI == 0 ]] || install_cli=y
elif [[ -t 0 ]]; then
    say ""
    say "Optional: a 'darkharrbor' command so diagnostics are one word from any"
    say "directory, instead of the full Compose invocation."
    printf 'Install the darkharrbor command to %s? [Y/n]: ' "$cli_dir/darkharrbor"
    IFS= read -r install_cli || install_cli=
    install_cli=${install_cli:-y}
fi
if [[ $install_cli =~ ^[Yy]$ ]]; then
    mkdir -p "$cli_dir" || die "could not create $cli_dir"
    cli_tmp=$(mktemp "$cli_dir/darkharrbor.tmp.XXXXXX")
    {
        printf '%s\n' '#!/bin/sh'
        printf '%s\n' '# Generated by DarkHarrbor install.sh -- do not edit.'
        printf '%s\n' 'set -eu'
        printf 'compose="${DARKHARRBOR_COMPOSE:-%s}"\n' "$compose_file"
        printf '%s\n' '[ -f "$compose" ] || { echo "darkharrbor: deployment not found: $compose" >&2; exit 1; }'
        printf '%s\n' 'if [ -t 0 ] && [ -t 1 ]; then interactive=1; else interactive=0; fi'
        printf '%s\n' 'if [ -n "$(docker compose -f "$compose" ps --status running -q darkharrbor 2>/dev/null)" ]; then'
        printf '%s\n' '    if [ "$interactive" = 1 ]; then'
        printf '%s\n' '        exec docker compose -f "$compose" exec darkharrbor darkharrbor "$@"'
        printf '%s\n' '    else'
        printf '%s\n' '        exec docker compose -f "$compose" exec -T darkharrbor darkharrbor "$@"'
        printf '%s\n' '    fi'
        printf '%s\n' 'else'
        printf '%s\n' '    if [ "$interactive" = 1 ]; then'
        printf '%s\n' '        exec docker compose -f "$compose" run --rm --no-deps darkharrbor "$@"'
        printf '%s\n' '    else'
        printf '%s\n' '        exec docker compose -f "$compose" run --rm --no-deps -T darkharrbor "$@"'
        printf '%s\n' '    fi'
        printf '%s\n' 'fi'
    } >"$cli_tmp"
    chmod 0755 "$cli_tmp"
    mv "$cli_tmp" "$cli_dir/darkharrbor"
    say "Installed: $cli_dir/darkharrbor"
    case ":${PATH-}:" in
        *":$cli_dir:"*) say "Run diagnostics: darkharrbor doctor" ;;
        *) say "NOTE: $cli_dir is not on your PATH. Add it, or run $cli_dir/darkharrbor doctor" ;;
    esac
else
    say "Run diagnostics: docker compose -f $compose_file exec darkharrbor darkharrbor doctor"
fi
