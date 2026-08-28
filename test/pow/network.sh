#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -f "$SCRIPT_DIR/network.env" ]; then
    # shellcheck disable=SC1091
    source "$SCRIPT_DIR/network.env"
fi

NODE_COUNT="${NODE_COUNT:-3}"
NETWORK_ID="${NETWORK_ID:-55661}"
P2P_PORT_BASE="${P2P_PORT_BASE:-20000}"
HTTP_PORT_BASE="${HTTP_PORT_BASE:-21000}"
RUNTIME_DIR="${RUNTIME_DIR:-.network}"
GETH_BIN="${GETH_BIN:-geth}"
ENABLE_HTTP="${ENABLE_HTTP:-false}"
MINE="${MINE:-true}"
OFFLINE="${OFFLINE:-false}"
if [ -z "${PFAP_PRFKEY_DIR:-}" ]; then
    if [ -d "$SCRIPT_DIR/../../prfKey" ]; then
        PFAP_PRFKEY_DIR="$(cd "$SCRIPT_DIR/../../prfKey" && pwd)"
    else
        PFAP_PRFKEY_DIR="/usr/local/prfKey"
    fi
fi
export PFAP_PRFKEY_DIR

case "$RUNTIME_DIR" in
    /*) NETWORK_ROOT="$RUNTIME_DIR" ;;
    *) NETWORK_ROOT="$SCRIPT_DIR/$RUNTIME_DIR" ;;
esac

GENESIS="$SCRIPT_DIR/pow.json"
PASSWORD_FILE="$NETWORK_ROOT/password.txt"

die() { printf '[ERROR] %s\n' "$*" >&2; exit 1; }
info() { printf '[INFO] %s\n' "$*"; }

validate_config() {
    [[ "$NODE_COUNT" =~ ^[1-9][0-9]*$ ]] || die "NODE_COUNT must be a positive integer"
    [[ "$P2P_PORT_BASE" =~ ^[0-9]+$ ]] || die "P2P_PORT_BASE must be an integer"
    command -v "$GETH_BIN" >/dev/null 2>&1 || die "geth not found: $GETH_BIN"
    [ -f "$GENESIS" ] || die "genesis not found: $GENESIS"
}

node_dir() { printf '%s/node%s' "$NETWORK_ROOT" "$1"; }
ipc_path() { printf '%s/geth.ipc' "$(node_dir "$1")"; }
pid_file() { printf '%s/geth.pid' "$(node_dir "$1")"; }

is_running() {
    local node="$1" file pid cmdline expected_dir
    file="$(pid_file "$node")"
    [ -f "$file" ] || return 1
    pid="$(<"$file")"
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    [ "$(awk '{print $3}' "/proc/$pid/stat" 2>/dev/null || true)" != "Z" ] || return 1
    cmdline="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)"
    expected_dir="$(node_dir "$node")"
    [[ "$cmdline" == *"--datadir $expected_dir"* ]]
}

datadir_locked() {
    local dir
    dir="$(node_dir "$1")"
    if command -v fuser >/dev/null 2>&1; then
        fuser "$dir/geth/LOCK" >/dev/null 2>&1
    elif command -v flock >/dev/null 2>&1; then
        if flock -n "$dir/geth/LOCK" true 2>/dev/null; then
            return 1
        fi
        return 0
    else
        die "Cannot safely check the datadir lock: install fuser (psmisc) or flock (util-linux)"
    fi
}

attach() {
    local node="$1" expression="$2"
    "$GETH_BIN" attach "$(ipc_path "$node")" --exec "$expression"
}

init_network() {
    validate_config
    mkdir -p "$NETWORK_ROOT"
    if [ ! -f "$PASSWORD_FILE" ]; then
        printf 'pfap-dev-password\n' >"$PASSWORD_FILE"
        chmod 600 "$PASSWORD_FILE"
    fi

    local node dir address
    for ((node=1; node<=NODE_COUNT; node++)); do
        dir="$(node_dir "$node")"
        mkdir -p "$dir"
        if ! compgen -G "$dir/keystore/*" >/dev/null; then
            info "Creating account for node $node"
            if ! "$GETH_BIN" --datadir "$dir" account new --password "$PASSWORD_FILE" >"$dir/account-create.log" 2>&1; then
                printf '[ERROR] geth account creation failed for node %s:\n' "$node" >&2
                sed -n '1,80p' "$dir/account-create.log" >&2
                die "Cannot create account for node $node"
            fi
        fi
        address="$(find "$dir/keystore" -maxdepth 1 -type f -printf '%f\n' | sed -n 's/.*--\([0-9a-fA-F]\{40\}\)$/\1/p' | head -1)"
        [ -n "$address" ] || die "Cannot determine account for node $node"
        printf '%s\n' "$address" >"$dir/address"
        if [ ! -d "$dir/geth/chaindata" ]; then
            info "Initializing node $node"
            if ! "$GETH_BIN" --datadir "$dir" init "$GENESIS" >"$dir/init.log" 2>&1; then
                printf '[ERROR] geth initialization failed for node %s:\n' "$node" >&2
                sed -n '1,80p' "$dir/init.log" >&2
                die "Cannot initialize node $node"
            fi
        fi
    done
    info "Initialized $NODE_COUNT nodes in $NETWORK_ROOT"
}

start_network() {
    init_network
    local node dir port http_port address pid http_args=()
    for ((node=1; node<=NODE_COUNT; node++)); do
        if is_running "$node"; then
            info "Node $node is already running"
            continue
        fi
        dir="$(node_dir "$node")"
        if datadir_locked "$node"; then
            die "Node $node datadir is locked by a process not owned by this launcher; stop that process or restore its PID file before starting"
        fi
        rm -f "$(ipc_path "$node")"
        port=$((P2P_PORT_BASE + node - 1))
        if [ "$OFFLINE" = true ]; then
            [ "$NODE_COUNT" -eq 1 ] || die "OFFLINE=true requires NODE_COUNT=1"
            port=-1
        fi
        http_port=$((HTTP_PORT_BASE + node - 1))
        address="$(<"$dir/address")"
        http_args=()
        if [ "$ENABLE_HTTP" = true ]; then
            http_args=(--rpc --rpcaddr 127.0.0.1 --rpcport "$http_port" --rpcapi "admin,debug,eth,miner,net,personal,txpool,web3")
        fi
        info "Starting node $node (p2p=$port, account=0x$address)"
        mkdir -p "$NETWORK_ROOT/ethash"
        nohup setsid "$GETH_BIN" --datadir "$dir" --networkid "$NETWORK_ID" --port "$port" \
            --ipcpath "$(ipc_path "$node")" --unlock "$address" --password "$PASSWORD_FILE" \
            --ethash.dagdir "$NETWORK_ROOT/ethash" --nodiscover --nousb \
            "${http_args[@]}" \
            </dev/null >>"$dir/geth.log" 2>&1 &
        pid=$!
        printf '%s\n' "$pid" >"$(pid_file "$node")"
    done

    wait_for_ipc
    if [ "$OFFLINE" = true ]; then
        info "Offline mode: P2P TCP listener disabled"
    else
        connect_network
    fi
    if [ "$MINE" = true ]; then
        attach 1 'miner.setEtherbase(eth.accounts[0]); miner.start(1)' >/dev/null
        info "Started mining on node 1 after peer setup"
    fi
    status_network
}

wait_for_ipc() {
    local node attempt
    for ((node=1; node<=NODE_COUNT; node++)); do
        for ((attempt=1; attempt<=60; attempt++)); do
            if [ -S "$(ipc_path "$node")" ] && attach "$node" 'web3.clientVersion' >/dev/null 2>&1; then
                break
            fi
            is_running "$node" || die "Node $node exited; see $(node_dir "$node")/geth.log"
            sleep 0.25
        done
        if [ ! -S "$(ipc_path "$node")" ] || ! attach "$node" 'web3.clientVersion' >/dev/null 2>&1; then
            die "Timed out waiting for node $node IPC"
        fi
    done
}

connect_network() {
    local node left right enode
    local -a enodes
    for ((node=1; node<=NODE_COUNT; node++)); do
        is_running "$node" || die "Node $node is not running"
        enode="$(attach "$node" 'admin.nodeInfo.enode' | tr -d '"\r\n')"
        enode="${enode/@\[::\]:/@127.0.0.1:}"
        enode="${enode/@0.0.0.0:/@127.0.0.1:}"
        enodes[node]="$enode"
    done
    for ((left=1; left<=NODE_COUNT; left++)); do
        for ((right=left+1; right<=NODE_COUNT; right++)); do
            attach "$left" "admin.addPeer('${enodes[$right]}')" >/dev/null
            attach "$right" "admin.addPeer('${enodes[$left]}')" >/dev/null
        done
    done
    info "Connected all nodes in a full-mesh topology"
}

status_network() {
    local node address peers block pid state
    printf '%-6s %-8s %-8s %-10s %s\n' NODE PID PEERS BLOCK ACCOUNT
    for ((node=1; node<=NODE_COUNT; node++)); do
        address="-"; [ -f "$(node_dir "$node")/address" ] && address="0x$(<"$(node_dir "$node")/address")"
        if is_running "$node" && [ -S "$(ipc_path "$node")" ]; then
            pid="$(<"$(pid_file "$node")")"
            peers="$(attach "$node" 'net.peerCount' | tr -d '\r\n')"
            block="$(attach "$node" 'eth.blockNumber' | tr -d '\r\n')"
            state="$pid"
        else
            state="stopped"; peers="-"; block="-"
        fi
        printf '%-6s %-8s %-8s %-10s %s\n' "$node" "$state" "$peers" "$block" "$address"
    done
}

smoke_network() {
    [ "$NODE_COUNT" -ge 2 ] || die "Smoke test requires at least 2 nodes"
    if ! is_running 1 || ! is_running 2; then
        die "Start the network before running the smoke test"
    fi
    local recipient tx_hash receipt node attempt
    recipient="0x$(<"$(node_dir 2)/address")"
    tx_hash="$(attach 1 "eth.sendPublicTransaction({from:eth.accounts[0],to:'$recipient',value:'0x1'})" | tr -d '"\r\n')"
    [[ "$tx_hash" == 0x* ]] || die "Failed to submit public transaction: $tx_hash"
    info "Submitted public transaction $tx_hash"

    receipt="null"
    for ((attempt=1; attempt<=120; attempt++)); do
        receipt="$(attach 1 "eth.getTransactionReceipt('$tx_hash')" | tr -d '\r\n')"
        [ "$receipt" != "null" ] && break
        sleep 0.25
    done
    [ "$receipt" != "null" ] || die "Transaction was not mined within 30 seconds"

    for ((node=1; node<=NODE_COUNT; node++)); do
        receipt="$(attach "$node" "eth.getTransactionReceipt('$tx_hash')" | tr -d '\r\n')"
        [ "$receipt" != "null" ] || die "Node $node does not have the transaction receipt"
    done
    info "Smoke test passed on all $NODE_COUNT nodes"
}

stop_network() {
    local node pid
    for ((node=1; node<=NODE_COUNT; node++)); do
        if is_running "$node"; then
            pid="$(<"$(pid_file "$node")")"
            info "Stopping node $node (pid=$pid)"
            kill "$pid"
        fi
    done
    for ((node=1; node<=NODE_COUNT; node++)); do
        if is_running "$node"; then
            pid="$(<"$(pid_file "$node")")"
            for _ in {1..40}; do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
        fi
        rm -f "$(pid_file "$node")"
    done
}

clean_network() {
    stop_network
    case "$NETWORK_ROOT" in
        "$SCRIPT_DIR"/*) ;;
        *) die "Refusing to remove runtime directory outside $SCRIPT_DIR: $NETWORK_ROOT" ;;
    esac
    [ "$NETWORK_ROOT" != "$SCRIPT_DIR" ] || die "Refusing to remove test directory"
    rm -rf -- "$NETWORK_ROOT"
    info "Removed $NETWORK_ROOT"
}

usage() {
    printf 'Usage: %s {init|start|connect|status|smoke|stop|clean|attach NODE [JS]}\n' "$(basename "$0")"
}

command="${1:-status}"
case "$command" in
    init) init_network ;;
    start) start_network ;;
    connect) validate_config; connect_network ;;
    status) validate_config; status_network ;;
    smoke) validate_config; smoke_network ;;
    stop) stop_network ;;
    clean) clean_network ;;
    attach)
        node="${2:-1}"; expression="${3:-}"
        if [ -n "$expression" ]; then attach "$node" "$expression"; else "$GETH_BIN" attach "$(ipc_path "$node")"; fi
        ;;
    *) usage; exit 2 ;;
esac
