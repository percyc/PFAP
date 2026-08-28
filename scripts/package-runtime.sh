#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LIB_BUILD="$REPO_ROOT/libsnark-vnt/build"
OUTPUT_DIR="${OUTPUT_DIR:-$REPO_ROOT/dist}"
GETH_BIN="${GETH_BIN:-$REPO_ROOT/bin/geth}"

[ -x "$GETH_BIN" ] || { printf 'geth binary not found; set GETH_BIN\n' >&2; exit 1; }

libraries=(
    "$LIB_BUILD/src/libzk_smt.so"
    "$LIB_BUILD/src/libzk_createaccount.so"
    "$LIB_BUILD/src/libzk_mint.so"
    "$LIB_BUILD/src/libzk_redeem.so"
    "$LIB_BUILD/src/libzk_transfer.so"
    "$LIB_BUILD/depends/libsnark/libsnark/libsnark.so"
    "$LIB_BUILD/depends/libsnark/depends/libff/libff/libff.so"
)
for file in "${libraries[@]}"; do [ -f "$file" ] || { printf 'missing %s\n' "$file" >&2; exit 1; }; done
compgen -G "$REPO_ROOT/prfKey/*.txt" >/dev/null || { printf 'proof keys not found in prfKey/\n' >&2; exit 1; }

stage="$(mktemp -d)"
trap 'rm -rf -- "$stage"' EXIT
package="$stage/pfap-runtime"
mkdir -p "$package/bin" "$package/lib" "$package/prfKey" "$package/pow" "$package/scripts" "$OUTPUT_DIR"
cp "$GETH_BIN" "$package/bin/geth"
cp "${libraries[@]}" "$package/lib/"
cp "$REPO_ROOT"/prfKey/*.txt "$package/prfKey/"
cp "$REPO_ROOT/test/pow/network.sh" "$REPO_ROOT/test/pow/pow.json" "$REPO_ROOT/test/pow/network.env.example" "$package/pow/"
cp "$REPO_ROOT/scripts/install-runtime.sh" "$package/scripts/"

archive="$OUTPUT_DIR/pfap-runtime.tar.gz"
tar -C "$stage" -czf "$archive" pfap-runtime
sha256sum "$archive" >"$archive.sha256"
printf 'Created %s\n' "$archive"
