#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
image="${PFAP_BUILDER_IMAGE:-pfap-runtime-builder:ubuntu22}"
build_rel="${PFAP_COMPAT_BUILD_REL:-.compat-build/ubuntu22}"
case "$build_rel" in
    /*|*..*) printf 'PFAP_COMPAT_BUILD_REL must be a relative path without ..\n' >&2; exit 2 ;;
esac
build_root="$repo_root/$build_rel"
go_root="$(go env GOROOT)"
go_shared_root="$(readlink -f "$go_root/src")"
go_shared_root="${go_shared_root%/src}"
go_shared_name="$(basename "$go_shared_root")"
generate_keys=0
if [ "${1:-}" = "--generate-keys" ]; then
    generate_keys=1
elif [ "$#" -gt 0 ]; then
    printf 'Usage: %s [--generate-keys]\n' "$0" >&2
    exit 2
fi
if [ "$generate_keys" = "1" ]; then
    build_command='./build.sh runtime-bundle'
else
    build_command='./build.sh libsnark && ./build.sh geth && ./build.sh bundle'
fi

docker build -t "$image" "$repo_root/containers/runtime-builder-ubuntu22"
mkdir -p "$build_root/libsnark" "$build_root/gopath" "$build_root/gocache"

docker run --rm \
    --user "$(id -u):$(id -g)" \
    -e HOME=/tmp \
    -e LIBSNARK_BUILD="/workspace/$build_rel/libsnark" \
    -e CMAKE_EXTRA_FLAGS=-DWITH_PROCPS=OFF \
    -e PFAP_GOPATH="/workspace/$build_rel/gopath" \
    -e PFAP_GOCACHE="/workspace/$build_rel/gocache" \
    -v "$go_root:/opt/go:ro" \
    -v "$go_shared_root:/share/$go_shared_name:ro" \
    -v "$repo_root:/workspace" \
    "$image" \
    bash -lc "$build_command"

printf 'Compatible runtime created: %s\n' "$repo_root/dist/pfap-runtime.tar.gz"
