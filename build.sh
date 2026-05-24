#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
GETH_SRC="$REPO_ROOT/go-ethereum"
LIBSNARK_SRC="$REPO_ROOT/libsnark-vnt"
LIBSNARK_BUILD="$LIBSNARK_SRC/build"
PRFKEY_DIR="$REPO_ROOT/prfKey"
GOPATH_BIN="$(go env GOPATH)/bin"

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
        error "Go is not installed. Install Go >= 1.10 and add to PATH."
    fi
    info "Go: $(go version)"
}

check_deps() {
    local missing=()
    for cmd in cmake make g++; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        error "Missing build dependencies: ${missing[*]}\n  sudo apt-get install build-essential cmake git libgmp3-dev libboost-all-dev libssl-dev pkg-config"
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

    if [ ! -f "$LIBSNARK_BUILD/Makefile" ]; then
        info "Running cmake..."
        cmake ..
    fi

    info "Compiling..."
    make -j$(nproc)

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
        "send:send"
        "deposit:deposit"
        "redeem:redeem"
        "transfer:transfer"
    )

    for pair in "${key_generators[@]}"; do
        local IFS=":"
        read -r name dir <<< "$pair"
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
    mv -f createaccountpk.txt createaccountvk.txt \
          mintpk.txt mintvk.txt \
          sendpk.txt sendvk.txt \
          depositpk.txt depositvk.txt \
          redeempk.txt redeemvk.txt \
          transferpk.txt transfervk.txt \
          "$PRFKEY_DIR/" 2>/dev/null || true

    info "Keys generated: $(ls "$PRFKEY_DIR"/*.txt | wc -l) files"
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
        "$LIBSNARK_BUILD/src/libzk_createaccount.so"
        "$LIBSNARK_BUILD/src/libzk_mint.so"
        "$LIBSNARK_BUILD/src/libzk_send.so"
        "$LIBSNARK_BUILD/src/libzk_deposit.so"
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

    sudo cp -i "${so_files[@]}" /usr/local/lib/
    sudo ldconfig
    info "Shared libraries installed to /usr/local/lib/"

    if ! echo "$LD_LIBRARY_PATH" | grep -q "/usr/local/lib"; then
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
    info "Keys installed to /usr/local/prfKey/ ($(ls /usr/local/prfKey/*.txt | wc -l) files)"
}

# ============================================================
# Step 4: Build geth (go install -> $GOPATH/bin)
# ============================================================
build_geth() {
    step "Building geth..."
    check_go

    if command -v go-bindata &>/dev/null || [ -x "$GOPATH_BIN/go-bindata" ]; then
        info "Regenerating JS bindings (bindata)..."
        cd "$GETH_SRC/internal/jsre/deps"
        go-bindata -nometadata -pkg deps -o bindata.go bignumber.js web3.js
    else
        warn "go-bindata not found, skipping JS binding regeneration."
        warn "If web3.js was modified, install go-bindata: go get -u github.com/kevinburke/go-bindata/go-bindata"
    fi

    cd "$GETH_SRC"
    info "Running: go install -v ./cmd/geth"
    go install -v ./cmd/geth

    if [ ! -f "$GOPATH_BIN/geth" ]; then
        error "geth not found at $GOPATH_BIN/geth after build."
    fi
    info "geth installed to $GOPATH_BIN/geth ($(du -h "$GOPATH_BIN/geth" | cut -f1))"
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

    cd "$REPO_ROOT/test/new300nodes"
    uv sync
    chmod +x "$REPO_ROOT/test/new300nodes/watch_nodes.sh" 2>/dev/null || true
    info "Python test environment ready."
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
    echo "  geth:          $GOPATH_BIN/geth"
    echo "  Shared libs:   /usr/local/lib/libzk_*.so libsnark.so libff.so"
    echo "  Keys:          /usr/local/prfKey/"
    echo "  LD_LIBRARY:    /usr/local/lib"
    echo ""
    echo "  Quick test:    cd test/new300nodes && uv run quick_test.py"
    echo "  Node monitor:  ./test/new300nodes/watch_nodes.sh"
    echo ""
    echo "  Add to PATH:   export PATH=\"$GOPATH_BIN:\$PATH\""
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
    info "geth updated: $GOPATH_BIN/geth"
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

    cd "$REPO_ROOT/test/new300nodes"
    rm -rf .venv __pycache__ 2>/dev/null || true

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
    if [ -f "$GOPATH_BIN/geth" ]; then
        info "geth:         $GOPATH_BIN/geth ($(du -h "$GOPATH_BIN/geth" | cut -f1))"
        "$GOPATH_BIN/geth" version 2>/dev/null | head -3 | sed 's/^/              /'
    else
        warn "geth:         not built"
    fi

    # libsnark .so
    echo ""
    echo "--- Shared Libraries (/usr/local/lib) ---"
    local all_found=true
    for name in libzk_createaccount libzk_mint libzk_send libzk_deposit libzk_redeem libzk_transfer libsnark libff; do
        if [ -f "/usr/local/lib/${name}.so" ]; then
            info "  ${name}.so  ($(du -h "/usr/local/lib/${name}.so" | cut -f1))"
        else
            warn "  ${name}.so  MISSING"
            all_found=false
        fi
    done

    # prfKey
    echo ""
    echo "--- Proving/Verification Keys (/usr/local/prfKey) ---"
    if [ -d "/usr/local/prfKey" ]; then
        local key_names="createaccount mint send deposit redeem transfer"
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
    echo "  GOPATH:  $(go env GOPATH 2>/dev/null || echo '?')"
    echo "  LD_LIBRARY_PATH: ${LD_LIBRARY_PATH:-<not set>}"

    # Python
    echo ""
    echo "--- Python Test Environment ---"
    if [ -d "$REPO_ROOT/test/new300nodes/.venv" ]; then
        info "venv: test/new300nodes/.venv"
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
  geth           Build geth only (go install -> \$GOPATH/bin)

Install Commands:
  install-libs   Install .so files to /usr/local/lib
  install-keys   Install prfKey to /usr/local/prfKey
  tests          Setup Python test environment (uv sync)

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
    install-libs)  install_libs ;;
    install-keys)  install_keys ;;
    tests)         install_tests ;;
    clean)         clean ;;
    status)        show_status ;;
    help|-h|--help) usage ;;
    *)             error "Unknown command: $CMD\nRun './build.sh help' for usage." ;;
esac
