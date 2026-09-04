#!/usr/bin/env bash

set -euo pipefail

root=${1:?repository root is required}
log_dir="$root/.logs"
log_file="$log_dir/rex.log"

if ! command -v jq >/dev/null 2>&1; then
    printf 'jq is required for colored Docker logs\n' >&2
    exit 1
fi

mkdir -p "$log_dir"

cd "$root"

docker compose logs --no-color --tail=0 -f |
    tee -a "$log_file" |
    while IFS= read -r line; do
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
