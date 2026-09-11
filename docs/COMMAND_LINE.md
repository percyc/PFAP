# Advanced command-line and RPC reference

For normal experiment reproduction, start with [PFAP Lab](../lab/README.md)
and the [Lab-first project guide](../README.md). These commands are for builds,
local smoke tests, RPC debugging and maintenance. Run shell commands from the
repository root unless a block explicitly changes directory. Do not operate an
account manually while Lab owns an active transaction on it.

## Circuits and transaction types

| Transaction     | Public inputs                                      | Description                                       |
| --------------- | -------------------------------------------------- | ------------------------------------------------- |
| `CreateAccount` | `cmt_A`                                            | Create a ZK account (initial balance = 0)         |
| `Mint`          | `cmt_A_new, value_s, sn_A_old, rt_cmt`             | Plaintext balance → ZK balance (proves `cmt_A_old` in state tree) |
| `Redeem`        | `cmt_A_new, value_s, sn_A_old, rt_cmt`             | ZK balance → plaintext balance (proves `cmt_A_old` in state tree) |
| `Transfer`      | `cmt_S, cmt_X_new, sn_X_old, rt_cmt, type`         | Direct ZK → ZK transfer |

`type = 0` is used for the payer circuit (enforces `value_old ≥ value_s`); `type = 1` is used for the receiver circuit. Each account keeps an encryption secret `sk_A`, and all serial numbers are chained via `sn_new = SHA256(sk_A, sn_old)`. For Mint / Redeem / Transfer, `rt_cmt` is the root of the global Poseidon state Merkle tree, and the circuit proves membership of `cmt_old` (payer's `cmt_A_old`, and for `type = 1` also the receiver's `cmt_B_old`) via a Poseidon path against that root.

## 1. Prerequisites

The build is prepared for current Linux toolchains. The historic geth fork
still uses its vendored GOPATH layout internally; `build.sh` creates that link
automatically and selects the portable Go BN256 implementation. The intended
baseline is Go 1.20+, CMake 3.10+, and a C++11-capable GCC or Clang.

```bash
sudo apt-get install build-essential cmake git \
    libgmp-dev libproc2-dev libboost-all-dev libssl-dev pkg-config
```

- A current Go release
- Optional: [`uv`](https://github.com/astral-sh/uv) for the multi-node Python test scripts

After the full build, expose the local geth binary and installed libraries:

```bash
export PATH="$PWD/bin:$PATH"
export LD_LIBRARY_PATH=/usr/local/lib
```

## 2. Build

### 2.1 Native build and compatible deployment bundle

```bash
git clone https://github.com/percyc/PFAP.git
cd PFAP
./build.sh all
```

For a runtime that can be deployed to Ubuntu 22.04-or-newer amd64 workers,
prefer the reproducible compatibility builder:

```bash
./scripts/build-compatible-runtime.sh
```

This preserves existing keys. On a fresh clone without generated keys, use
`./scripts/build-compatible-runtime.sh --generate-keys` for a new network only;
never regenerate parameters during an existing experiment.

It compiles the C/C++ runtime in an Ubuntu 22.04 Docker image, mounts the
controller's Go 1.24 toolchain read-only, and creates
`dist/pfap-runtime.tar.gz`. Worker nodes run the resulting binaries natively;
they do not need Docker or a compiler. See [lab/README.md](../lab/README.md) for
controller and new-worker provisioning instructions.

`build.sh all` performs, in order:

1. Compile `libsnark-vnt`.
2. Run `createaccount_key / mint_key / redeem_key / transfer_key` to produce (pk, vk).
3. Copy the 8 generated `*.txt` keys into `/usr/local/prfKey/`.
4. Install `libzk_*.so`, `libsnark.so`, `libff.so` into `/usr/local/lib/` and run `ldconfig`.
5. Build geth from vendored dependencies → outputs to `./bin/geth`.
6. If `uv` is installed, initialize the Python test environment.

### 2.2 Sub-commands

```bash
./build.sh quick         # Rebuild geth only (use after Go code changes)
./build.sh libsnark      # Build libsnark-vnt only
./build.sh keys          # Regenerate (pk, vk) only
./build.sh install-libs  # Install .so files to /usr/local/lib only
./build.sh install-keys  # Install prfKey to /usr/local/prfKey only
./build.sh geth          # Build geth into ./bin/geth
./build.sh bundle        # Package a deployable runtime into ./dist
./build.sh status        # Show current build/install status
./build.sh clean         # Clean build artifacts (system-installed files kept)
./build.sh help          # Full help
```

Build concurrency and CMake options are configurable:

```bash
BUILD_JOBS=8 CMAKE_EXTRA_FLAGS='-DWITH_PROCPS=OFF' ./build.sh libsnark
```

> ⚠️ All geth nodes on the same network MUST share the **same** `prfKey`; otherwise proof verification will fail.

### 2.3 After modifying C++ circuits

After editing any `.tcc / .cpp` under `libsnark-vnt/src`:

```bash
./build.sh libsnark         # rebuild
./build.sh install-libs     # re-copy .so files
# If the circuit constraints (structure) actually changed, also run:
./build.sh keys
./build.sh install-keys
```

Note: any change to circuit structure invalidates (pk, vk); all nodes must be re-synced with the new keys.


## 3. Running nodes

For repeatable local multi-node tests, use the lifecycle manager instead of
creating datadirs and peers manually:

```bash
cd test/pow
cp network.env.example network.env
GETH_BIN=../../bin/geth ./network.sh start
./network.sh status
./network.sh attach 1
./network.sh stop
```

It supports configurable node counts and ports and keeps generated state under
`test/pow/.network/`. See [test/pow/README.md](../test/pow/README.md) for runtime
bundles and multi-machine deployment.

Using `test/pow` as the example. The
`signerX/` directories only ship with `passwd.txt`; you must create a fresh
account in each datadir and then unlock that exact address.

```bash
cd test/pow

# Terminal 1 -------------------------------------------------------------
# Wipe any previous run (including the keystore) and start from scratch
rm -rf signer1/data signer1.log

# Create an account in signer1/data/keystore; record the printed address
geth --datadir signer1/data account new --password signer1/passwd.txt
# => Address: {abcd...}    <-- use THIS address below as <signer1_addr>

# Initialize the genesis block
geth --datadir signer1/data init pow.json

# Start the node (replace <signer1_addr> with the address printed above)
geth --datadir signer1/data --networkid 55661 --port 2007 \
    --unlock <signer1_addr> --password signer1/passwd.txt \
    console 2>> signer1.log

# Terminal 2 -------------------------------------------------------------
rm -rf signer2/data signer2.log
geth --datadir signer2/data account new --password signer2/passwd.txt
# => Address: {efgh...}    <-- use THIS address below as <signer2_addr>
geth --datadir signer2/data init pow.json
geth --datadir signer2/data --networkid 55661 --port 2008 \
    --unlock <signer2_addr> --password signer2/passwd.txt \
    console 2>> signer2.log
```

> Note: deleting only `signer1/data/geth` (keeping `keystore/`) also works
> for repeated runs, but the addresses above must always match whatever is
> currently in `signer1/data/keystore`. The two are independent — `account new`
> writes to `keystore/`, while `init` / chain data live under `geth/`.

Connect the two nodes:

```javascript
// Terminal 2
admin.nodeInfo.enode
// Terminal 1
admin.addPeer("enode://<id>@<ip>:2008")
net.peerCount
miner.start()
```


## 4. RPC / console API

### 4.1 Balances & account state

```javascript
eth.getBalance(addr)        // plaintext balance
eth.getAccountState()       // { balance, commitment, lastTxBlockNumber }
```


### 4.2 Single-party transactions

```javascript
// Create a ZK account (must be called once before any ZK transaction)
eth.sendCreateAccountTransaction({from: eth.accounts[0]})

// Plaintext → ZK
eth.sendMintTransaction({from: eth.accounts[0], value: "0x1234"})

// ZK → plaintext
eth.sendRedeemTransaction({from: eth.accounts[0], value: "0x123"})
```


### 4.3 Transfer (cooperative ZK → ZK)

A Transfer is **submitted by the receiver (B)**, but the payer (A) must first generate a proof locally. While a Transfer is in progress, **neither side should issue other ZK transactions**.

```javascript
// Terminal 1
// 1) Payer A: generate the proof (no tx broadcast yet) and stage the new local state.
//    Membership of cmt_A_old is proven against the global Poseidon state Merkle tree.
var valueS = "0x10"
var rs     = "<fresh cryptographically random 32-byte hex value>"
var payerData = eth.getPayerNextState(rs, valueS)
// payerData includes { cmtANew, snAOld, proofA, proofRoot, proofBlock }

// Terminal 2
// 2) Ship payerData / valueS / rs to receiver B, who submits the final tx
eth.sendTransferTransaction({
    from:    eth.accounts[0],
    value:   valueS,
    rs:      rs,
    cmtANew: payerData.cmtANew,
    snAOld:  payerData.snAOld,
    proofA:  payerData.proofA,
    proofRoot: payerData.proofRoot,
    proofBlock: payerData.proofBlock
})

// 3) After inclusion and the chosen confirmation depth, verify both sides.
// A state query alone does not replace canonical receipt/readiness checks.
eth.getAccountState()
```


Pass the payer’s original `proofRoot` and `proofBlock` unchanged. The receiver
validates the proof against an accepted historical root on the canonical chain,
not an arbitrary root or the latest root substituted by the caller.
Do not retry unknown transactions or regenerate a frozen payer state. Prefer
Lab for receipt tracking, confirmation depth and safe release of both accounts.

A complete end-to-end walkthrough is in [`test/pow/TRANSFER_TEST.md`](../test/pow/TRANSFER_TEST.md).

## 5. Status & troubleshooting

```bash
./build.sh status   # check geth / .so / prfKey / venv
```

Common issues:

- **Proof verification fails** — nodes have inconsistent `prfKey`; resync `/usr/local/prfKey/` and restart.
- **`libzk_*.so` not found** — `LD_LIBRARY_PATH=/usr/local/lib` not exported, or `install-libs` was never run.

## 6. References

- BlockMaze: <https://github.com/Agzs/BlockMaze>
