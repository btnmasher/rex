#!/usr/bin/env bash

set -euo pipefail

root=${1:?repository root is required}
log_dir="$root/.logs"
log_file="$log_dir/rex.log"
max_log_bytes=$((10 * 1024 * 1024))
max_log_files=5

if ! command -v jq >/dev/null 2>&1; then
    printf 'jq is required for colored Docker logs\n' >&2
    exit 1
fi

mkdir -p "$log_dir"

rotate_log_if_needed() {
    if [[ ! -f "$log_file" ]] || (( $(wc -c <"$log_file") < max_log_bytes )); then
        return
    fi

    rm -f -- "$log_file.$max_log_files"
    for ((index = max_log_files - 1; index >= 1; index--)); do
        if [[ -f "$log_file.$index" ]]; then
            mv -- "$log_file.$index" "$log_file.$((index + 1))"
        fi
    done
    mv -- "$log_file" "$log_file.1"
}

cd "$root"

docker compose logs --no-color --tail=0 -f |
    while IFS= read -r line; do
        rotate_log_if_needed
        printf '%s\n' "$line" >>"$log_file"

        prefix=${line%%|*}
        payload=${line#*|}
        if [[ "$line" != *'|'* ]]; then
            prefix=''
            payload=$line
        fi

        payload="${payload#"${payload%%[![:space:]]*}"}"
        if colored_payload=$(jq -C -c . <<<"$payload" 2>/dev/null); then
            printf '\033[1;90m%s\033[0m \033[2m|\033[0m %s\n' "$prefix" "$colored_payload"
            continue
        fi

        printf '\033[1;90m%s\033[0m \033[2m|\033[0m %s\n' "$prefix" "$payload"
    done
