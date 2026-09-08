#!/usr/bin/env bash
set -euo pipefail
task_root="$(cd "$(dirname "$0")/.." && pwd)"
task_native="$task_root/.compat-build/ubuntu22/libsnark"
export GOPATH="$task_root/.gopath"
export GO111MODULE=off
export CGO_LDFLAGS="-L$task_native/src -L$task_native/depends/libsnark/libsnark -L$task_native/depends/libsnark/depends/libff/libff"
export LD_LIBRARY_PATH="$task_native/src:$task_native/depends/libsnark/libsnark:$task_native/depends/libsnark/depends/libff/libff${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export PFAP_PRFKEY_DIR="$task_root/prfKey"
go test -tags generic github.com/ethereum/go-ethereum/core github.com/ethereum/go-ethereum/zktx "$@"
