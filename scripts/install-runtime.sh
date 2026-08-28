#!/usr/bin/env bash
set -euo pipefail

PACKAGE_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PREFIX="${PREFIX:-/usr/local}"
DESTDIR="${DESTDIR:-}"

install -d "$DESTDIR$PREFIX/bin" "$DESTDIR$PREFIX/lib" "$DESTDIR$PREFIX/prfKey" "$DESTDIR$PREFIX/share/pfap/pow"
install -m 0755 "$PACKAGE_DIR/bin/geth" "$DESTDIR$PREFIX/bin/geth"
install -m 0644 "$PACKAGE_DIR/lib/"*.so "$DESTDIR$PREFIX/lib/"
install -m 0644 "$PACKAGE_DIR/prfKey/"*.txt "$DESTDIR$PREFIX/prfKey/"
install -m 0755 "$PACKAGE_DIR/pow/network.sh" "$DESTDIR$PREFIX/share/pfap/pow/network.sh"
install -m 0644 "$PACKAGE_DIR/pow/pow.json" "$PACKAGE_DIR/pow/network.env.example" "$DESTDIR$PREFIX/share/pfap/pow/"

if [ -z "$DESTDIR" ] && command -v ldconfig >/dev/null 2>&1; then
    ldconfig
fi

printf 'PFAP runtime installed under %s%s\n' "$DESTDIR" "$PREFIX"
printf 'Network tool: %s%s/share/pfap/pow/network.sh\n' "$DESTDIR" "$PREFIX"
