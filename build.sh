#!/usr/bin/env bash
set -euo pipefail

CMAKE_EXTRA_FLAGS="${CMAKE_EXTRA_FLAGS:-}"
BUILD_JOBS="${BUILD_JOBS:-$(nproc)}"

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
GETH_SRC="$REPO_ROOT/go-ethereum"
LIBSNARK_SRC="$REPO_ROOT/libsnark-vnt"
LIBSNARK_BUILD="$LIBSNARK_SRC/build"
PRFKEY_DIR="$REPO_ROOT/prfKey"
PFAP_GOPATH="${PFAP_GOPATH:-$REPO_ROOT/.gopath}"
PFAP_GOCACHE="${PFAP_GOCACHE:-$REPO_ROOT/.gocache}"
GOPATH_BIN="$PFAP_GOPATH/bin"
GETH_OUTPUT="${GETH_OUTPUT:-$REPO_ROOT/bin/geth}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m'

info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
step()  { echo -e "${BLUE}[STEP]${NC} $*"; }

check_go() {
    if ! command -v go &>/dev/null; then
        error "Go is not installed. Install a current supported Go release and add it to PATH."
    fi
    info "Go: $(go version)"
}

# Ensure $GOPATH/src/github.com/ethereum/go-ethereum points at this repo's
# go-ethereum directory. Required because all Go source files import
# "github.com/ethereum/go-ethereum/..." but the repo lives under PFAP/.
ensure_gopath_link() {
    local gopath
    gopath="$PFAP_GOPATH"

    local target_dir="$gopath/src/github.com/ethereum"
    local target_link="$target_dir/go-ethereum"

    mkdir -p "$target_dir"

    if [ -L "$target_link" ]; then
        local current
        current="$(readlink -f "$target_link")"
        local expected
        expected="$(readlink -f "$GETH_SRC")"
        if [ "$current" = "$expected" ]; then
            info "GOPATH symlink OK: $target_link -> $expected"
            return 0
        fi
        warn "Symlink $target_link points to $current, relinking to $expected"
        rm -f "$target_link"
        ln -s "$GETH_SRC" "$target_link"
        info "Relinked: $target_link -> $GETH_SRC"
        return 0
    fi

    if [ -e "$target_link" ]; then
        error "$target_link already exists and is not a symlink.
  Please back it up and remove it, then re-run this script:
    mv \"$target_link\" \"${target_link}.bak\""
    fi

    ln -s "$GETH_SRC" "$target_link"
    info "Created symlink: $target_link -> $GETH_SRC"
}

check_deps() {
    local missing=()
    for cmd in cmake make g++; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        error "Missing build dependencies: ${missing[*]}\n  sudo apt-get install build-essential cmake git libgmp-dev libboost-all-dev libssl-dev libproc2-dev pkg-config"
    fi
}

# ============================================================
# Step 1: Build libsnark-vnt (zero-knowledge proof library)
# ============================================================
build_libsnark() {
    step "Building libsnark-vnt (ZK proof library)..."
    check_deps

    mkdir -p "$LIBSNARK_BUILD"
    cd "$LIBSNARK_BUILD"

    # Always re-run cmake so CMakeLists.txt changes (added/removed targets)
    # are picked up. A stale cache can silently drop targets like transfer_key.
    info "Running cmake..."
    # shellcheck disable=SC2086
    cmake ${CMAKE_EXTRA_FLAGS} ..

    info "Compiling..."
    make -j"$BUILD_JOBS"

    info "libsnark-vnt build complete."
}

# ============================================================
# Step 2: Generate proving/verification keys
# ============================================================
generate_keys() {
    step "Generating proving/verification keys..."

    if [ ! -f "$LIBSNARK_BUILD/src/mint_key" ]; then
        error "libsnark not built yet. Run './build.sh libsnark' first."
    fi

    mkdir -p "$PRFKEY_DIR"

    local key_generators=(
        "createaccount:createaccount"
        "mint:mint"
        "redeem:redeem"
        "transfer:transfer"
    )

    for pair in "${key_generators[@]}"; do
        local IFS=":"
        read -r name _dir <<< "$pair"
        local key_bin="$LIBSNARK_BUILD/src/${name}_key"
        if [ ! -x "$key_bin" ]; then
            error "Key generator not found: $key_bin"
        fi
        info "Generating $name keys..."
        cd "$LIBSNARK_BUILD"
        "$key_bin"
    done

    info "Moving keys to $PRFKEY_DIR..."
    cd "$LIBSNARK_BUILD"
    for f in createaccountpk.txt createaccountvk.txt \
             mintpk.txt mintvk.txt \
             redeempk.txt redeemvk.txt \
             transferpk.txt transfervk.txt; do
        [ -f "$f" ] && mv -f "$f" "$PRFKEY_DIR/"
    done

    info "Keys generated: $(find "$PRFKEY_DIR" -maxdepth 1 -type f -name '*.txt' | wc -l) files"
}

# ============================================================
# Step 3: Install shared libraries + keys to system
# ============================================================
install_libs() {
    step "Installing shared libraries to /usr/local/lib..."

    if [ ! -d "$LIBSNARK_BUILD/src" ]; then
        error "libsnark not built yet. Run './build.sh libsnark' first."
    fi

    local so_files=(
        "$LIBSNARK_BUILD/src/libzk_smt.so"
        "$LIBSNARK_BUILD/src/libzk_createaccount.so"
        "$LIBSNARK_BUILD/src/libzk_mint.so"
        "$LIBSNARK_BUILD/src/libzk_redeem.so"
        "$LIBSNARK_BUILD/src/libzk_transfer.so"
        "$LIBSNARK_BUILD/depends/libsnark/libsnark/libsnark.so"
        "$LIBSNARK_BUILD/depends/libsnark/depends/libff/libff/libff.so"
    )

    for f in "${so_files[@]}"; do
        if [ ! -f "$f" ]; then
            error "Missing: $f"
        fi
    done

    # Remove obsolete libraries from earlier builds (send/deposit removed).
    sudo rm -f /usr/local/lib/libzk_send.so /usr/local/lib/libzk_deposit.so

    sudo cp -f "${so_files[@]}" /usr/local/lib/
    sudo ldconfig
    info "Shared libraries installed to /usr/local/lib/"

    if ! echo "${LD_LIBRARY_PATH:-}" | grep -q "/usr/local/lib"; then
        warn "LD_LIBRARY_PATH does not include /usr/local/lib"
        warn "Add this to ~/.bashrc:"
        warn "  export LD_LIBRARY_PATH=/usr/local/lib"
    fi
}

install_keys() {
    step "Installing prfKey to /usr/local/prfKey..."

    if [ ! -d "$PRFKEY_DIR" ] || [ -z "$(ls -A "$PRFKEY_DIR" 2>/dev/null)" ]; then
        error "prfKey directory is empty. Run './build.sh keys' first."
    fi

    sudo rm -rf /usr/local/prfKey
    sudo cp -r "$PRFKEY_DIR" /usr/local/prfKey
    info "Keys installed to /usr/local/prfKey/ ($(find /usr/local/prfKey -maxdepth 1 -type f -name '*.txt' | wc -l) files)"
}

# ============================================================
# Step 4: Build geth (go install -> $GOPATH/bin)
# ============================================================
build_geth() {
    step "Building geth..."
    check_go
    ensure_gopath_link

    # IMPORTANT: build from the canonical import path
    # ($GOPATH/src/github.com/ethereum/go-ethereum) instead of $GETH_SRC.
    # Otherwise cmd/geth is compiled as "github.com/PFAP/go-ethereum/cmd/geth"
    # and its vendor tree becomes a different identity than the vendor tree
    # of "github.com/ethereum/go-ethereum/cmd/utils", causing duplicate-type
    # errors (e.g. two cli.Context types).
    local geth_build_dir
    geth_build_dir="$PFAP_GOPATH/src/github.com/ethereum/go-ethereum"

    if command -v go-bindata &>/dev/null || [ -x "$GOPATH_BIN/go-bindata" ]; then
        info "Regenerating JS bindings (bindata)..."
        cd "$geth_build_dir/internal/jsre/deps"
        GOPATH="$PFAP_GOPATH" GOCACHE="$PFAP_GOCACHE" GO111MODULE=off go-bindata -nometadata -pkg deps -o bindata.go bignumber.js web3.js
    else
        warn "go-bindata not found, skipping JS binding regeneration."
        warn "If web3.js was modified, install go-bindata: go get -u github.com/kevinburke/go-bindata/go-bindata"
    fi

    cd "$geth_build_dir"
    mkdir -p "$(dirname "$GETH_OUTPUT")"
    info "Running: GO111MODULE=off go build -tags generic -o $GETH_OUTPUT ./cmd/geth"
    local zk_lib_dir="$LIBSNARK_BUILD/src"
    local snark_lib_dir="$LIBSNARK_BUILD/depends/libsnark/libsnark"
    local ff_lib_dir="$LIBSNARK_BUILD/depends/libsnark/depends/libff/libff"
    [ -d "$zk_lib_dir" ] || error "Native ZK libraries are not built. Run './build.sh libsnark' first."
    local native_ldflags="-L$zk_lib_dir -L$snark_lib_dir -L$ff_lib_dir -Wl,-rpath,$zk_lib_dir -Wl,-rpath,$snark_lib_dir -Wl,-rpath,$ff_lib_dir"
    GOPATH="$PFAP_GOPATH" GOCACHE="$PFAP_GOCACHE" CGO_LDFLAGS="$native_ldflags ${CGO_LDFLAGS:-}" \
        GO111MODULE=off go build -tags generic -o "$GETH_OUTPUT" ./cmd/geth

    if [ ! -x "$GETH_OUTPUT" ]; then
        error "geth not found at $GETH_OUTPUT after build."
    fi
    info "geth built at $GETH_OUTPUT ($(du -h "$GETH_OUTPUT" | cut -f1))"
}

# ============================================================
# Step 5: Setup Python test environment
# ============================================================
install_tests() {
    step "Setting up Python test environment..."
    if ! command -v uv &>/dev/null; then
        warn "uv not found, skipping Python test setup."
        return 0
    fi
    info "uv: $(uv --version)"

    local test_dir="$REPO_ROOT/test/pow"
    if [ ! -f "$test_dir/pyproject.toml" ]; then
        warn "No pyproject.toml in $test_dir, skipping uv sync."
        return 0
    fi
    cd "$test_dir"
    uv sync
    chmod +x "$test_dir/watch_nodes.sh" 2>/dev/null || true
    info "Python test environment ready."
}

package_runtime() {
    step "Packaging deployable PFAP runtime..."
    "$REPO_ROOT/scripts/package-runtime.sh"
}

build_lab() {
    step "Building PFAP Lab Web control plane..."
    check_go
    local lab_dir="$REPO_ROOT/lab"
    [ -f "$lab_dir/go.mod" ] || error "PFAP Lab source not found: $lab_dir"
    mkdir -p "$lab_dir/.cache" "$lab_dir/.gopath" "$REPO_ROOT/bin"
    cd "$lab_dir"
    GOCACHE="$lab_dir/.cache" GOPATH="$lab_dir/.gopath" go test ./...
    GOCACHE="$lab_dir/.cache" GOPATH="$lab_dir/.gopath" go build -o "$REPO_ROOT/bin/pfap-lab" ./cmd/pfap-lab
    info "PFAP Lab built at $REPO_ROOT/bin/pfap-lab"
}

# ============================================================
# Composed commands
# ============================================================
build_all() {
    echo ""
    echo "========================================="
    echo "  BlockMaze Full Build"
    echo "========================================="
    echo ""

    check_go

    build_libsnark
    generate_keys
    install_libs
    install_keys
    build_geth
    install_tests

    echo ""
    echo "========================================="
    echo "  Build Complete!"
    echo "========================================="
    echo ""
    echo "  geth:          $GETH_OUTPUT"
    echo "  Shared libs:   /usr/local/lib/libzk_*.so libsnark.so libff.so"
    echo "  Keys:          /usr/local/prfKey/"
    echo "  LD_LIBRARY:    /usr/local/lib"
    echo ""
    echo "  Quick test:    cd test/pow && uv run quick_test.py"
    echo "  Node monitor:  ./test/pow/watch_nodes.sh"
    echo ""
    echo "  Add to PATH:   export PATH=\"$REPO_ROOT/bin:\$PATH\""
    echo ""
}

build_quick() {
    echo ""
    echo "========================================="
    echo "  Quick Build (geth only)"
    echo "========================================="
    echo ""

    check_go
    build_geth

    echo ""
    info "geth updated: $GETH_OUTPUT"
    info "Run './build.sh all' for full setup including libsnark + keys."
    echo ""
}

# ============================================================
# Clean
# ============================================================
clean() {
    step "Cleaning build artifacts..."

    cd "$GETH_SRC"
    if [ -f Makefile ]; then
        make clean 2>/dev/null || true
    fi

    cd "$LIBSNARK_SRC"
    rm -rf "$LIBSNARK_BUILD"

    if [ -d "$REPO_ROOT/test/pow" ]; then
        cd "$REPO_ROOT/test/pow"
        rm -rf .venv __pycache__ 2>/dev/null || true
    fi

    info "Clean done."
    warn "System-installed files NOT removed: /usr/local/lib/libzk*.so, /usr/local/prfKey/"
    warn "Remove manually if needed: sudo rm /usr/local/lib/libzk*.so /usr/local/lib/libsnark.so /usr/local/lib/libff.so; sudo rm -rf /usr/local/prfKey"
}

# ============================================================
# Status
# ============================================================
show_status() {
    echo "=== Build Status ==="
    echo ""

    # geth
    if [ -f "$GETH_OUTPUT" ]; then
        info "geth:         $GETH_OUTPUT ($(du -h "$GETH_OUTPUT" | cut -f1))"
        "$GETH_OUTPUT" version 2>/dev/null | head -3 | sed 's/^/              /'
    else
        warn "geth:         not built"
    fi

    # libsnark .so
    echo ""
    echo "--- Shared Libraries (/usr/local/lib) ---"
    for name in libzk_smt libzk_createaccount libzk_mint libzk_redeem libzk_transfer libsnark libff; do
        if [ -f "/usr/local/lib/${name}.so" ]; then
            info "  ${name}.so  ($(du -h "/usr/local/lib/${name}.so" | cut -f1))"
        else
            warn "  ${name}.so  MISSING"
        fi
    done

    # prfKey
    echo ""
    echo "--- Proving/Verification Keys (/usr/local/prfKey) ---"
    if [ -d "/usr/local/prfKey" ]; then
        local key_names="createaccount mint redeem transfer"
        for name in $key_names; do
            if [ -f "/usr/local/prfKey/${name}pk.txt" ] && [ -f "/usr/local/prfKey/${name}vk.txt" ]; then
                info "  ${name}: pk+vk OK"
            else
                warn "  ${name}: MISSING"
            fi
        done
    else
        warn "  /usr/local/prfKey/ not found"
    fi

    # Environment
    echo ""
    echo "--- Environment ---"
    echo "  Go:      $(go version 2>/dev/null || echo 'not found')"
    echo "  Build GOPATH: $PFAP_GOPATH"
    echo "  Build cache:  $PFAP_GOCACHE"
    echo "  LD_LIBRARY_PATH: ${LD_LIBRARY_PATH:-<not set>}"

    # Python
    echo ""
    echo "--- Python Test Environment ---"
    if [ -d "$REPO_ROOT/test/pow/.venv" ]; then
        info "venv: test/pow/.venv"
    else
        warn "venv not set up. Run './build.sh tests'"
    fi
}

# ============================================================
# Main
# ============================================================
usage() {
    cat <<EOF
Usage: $(basename "$0") <command>

Build Commands:
  all            Full build: libsnark -> keys -> install libs -> install keys -> geth -> tests
  quick          Quick build: geth only (assumes libsnark+keys already installed)
  libsnark       Build libsnark-vnt only
  keys           Generate proving/verification keys only
  geth           Build geth only (output: ./bin/geth)
  lab            Build and test the PFAP Lab Web control plane

Install Commands:
  install-libs   Install .so files to /usr/local/lib
  install-keys   Install prfKey to /usr/local/prfKey
  tests          Setup Python test environment (uv sync)
  bundle         Package geth, libraries, keys and network tooling into dist/

Other:
  clean          Remove build artifacts (libsnark build, geth build)
  status         Show current build/install status
  help           Show this help

Typical usage:
  ./build.sh all        # First time: build everything
  ./build.sh quick      # After code changes: rebuild geth only
  ./build.sh status     # Check what's installed
EOF
}

CMD="${1:-all}"
case "$CMD" in
    all)           build_all ;;
    quick)         build_quick ;;
    libsnark)      check_deps; build_libsnark ;;
    keys)          generate_keys ;;
    geth)          check_go; build_geth ;;
    lab)           build_lab ;;
    install-libs)  install_libs ;;
    install-keys)  install_keys ;;
    tests)         install_tests ;;
    bundle)        package_runtime ;;
    clean)         clean ;;
    status)        show_status ;;
    help|-h|--help) usage ;;
    *)             error "Unknown command: $CMD\nRun './build.sh help' for usage." ;;
esac
