#!/usr/bin/env bash
set -euo pipefail

LAB_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$LAB_DIR/.." && pwd)"
PORT="${PFAP_LAB_PORT:-8090}"
DATA_FILE="${PFAP_LAB_DATA:-$LAB_DIR/data/lab.json}"
PASSWORD_FILE="${PFAP_LAB_PASSWORD_FILE:-$LAB_DIR/data/password}"
BIN="$REPO_ROOT/bin/pfap-lab"

if [ ! -x "$BIN" ]; then
    "$REPO_ROOT/build.sh" lab
fi

mkdir -p "$(dirname "$DATA_FILE")"
if [ ! -s "$PASSWORD_FILE" ]; then
    umask 077
    mkdir -p "$(dirname "$PASSWORD_FILE")"
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -base64 18 >"$PASSWORD_FILE"
    else
        od -An -N18 -tx1 /dev/urandom | tr -d ' \n' >"$PASSWORD_FILE"
        printf '\n' >>"$PASSWORD_FILE"
    fi
    printf 'Generated Web password: %s\n' "$(<"$PASSWORD_FILE")"
fi

addresses="$(hostname -I 2>/dev/null || true)"
printf 'PFAP Lab is starting on all network interfaces.\n'
printf 'Local: http://127.0.0.1:%s\n' "$PORT"
for address in $addresses; do
    case "$address" in
        *:*) ;; # Skip IPv6 in the compact startup summary.
        *) printf 'LAN:   http://%s:%s\n' "$address" "$PORT" ;;
    esac
done
printf 'State: %s\n' "$DATA_FILE"
printf 'Press Ctrl-C to stop.\n\n'

exec "$BIN" -listen "0.0.0.0:$PORT" -data "$DATA_FILE" -password-file "$PASSWORD_FILE"
