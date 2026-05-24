# PFAP

PFAP refactors the ZK transaction flow of BlockMaze. The key change is that a **single Transfer circuit (combining the original send + deposit) replaces the two-step ZK-to-ZK transfer**, and a dedicated **CreateAccount** circuit is introduced for account initialization. The Mint and Redeem circuits are also rewritten under the new constraints (now using a Merkle path `path` and root `rt_cmt`).

## Repository layout

```
PFAP/
├── go-ethereum/    geth fork (based on 040dd5bd) with ZK tx types and RPCs
├── libsnark-vnt/   libsnark gadgets: createAccount / mint / redeem / transfer / send / deposit
├── prfKey/         generated (pk, vk) files (produced after build)
├── test/
│   ├── pow/        PoW test environment (includes Transfer walkthrough)
│   └── clique/     PoA test environment
└── build.sh        one-shot build/install script
```

## Circuits and transaction types

| Transaction     | Public inputs                                      | Description                                       |
| --------------- | -------------------------------------------------- | ------------------------------------------------- |
| `CreateAccount` | `cmt_A`                                            | Create a ZK account (initial balance = 0)         |
| `Mint`          | `cmt_A_new, value_s, sn_A_old, rt_cmt`             | Plaintext balance → ZK balance                    |
| `Redeem`        | `cmt_A_new, value_s, sn_A_old, rt_cmt`             | ZK balance → plaintext balance                    |
| `Transfer`      | `cmt_S, cmt_X_new, sn_X_old, rt_cmt, type`         | Direct ZK → ZK transfer (replaces send + deposit) |

`type = 0` is used for the payer circuit (enforces `value_old ≥ value_s`); `type = 1` is used for the receiver circuit. Each account keeps an encryption secret `sk_A`, and all serial numbers are chained via `sn_new = SHA256(sk_A, sn_old)`.

## 1. Prerequisites

> ⚠️ **Tested environment**: Ubuntu **18.04.1 LTS** (x86_64) + Go **1.10.8**.
> Other Ubuntu versions or Go versions are **not** verified and may fail to build (libsnark in particular is sensitive to compiler / boost / OpenSSL versions). Use a matching environment if possible.

```bash
sudo apt-get install build-essential cmake git \
    libgmp3-dev libprocps-dev libboost-all-dev libssl-dev pkg-config
```

- Go **1.10.x** (tested with 1.10.8)
- Optional: [`uv`](https://github.com/astral-sh/uv) for the multi-node Python test scripts

Make sure `GOPATH` and `LD_LIBRARY_PATH` are exported:

```bash
export PATH=$(go env GOPATH)/bin:$PATH
export LD_LIBRARY_PATH=/usr/local/lib
```

## 2. Build

### 2.1 One-shot build (recommended)

```bash
git clone <this-repo> $GOPATH/src/github.com/PFAP
cd $GOPATH/src/github.com/PFAP
./build.sh all
```

`build.sh all` performs, in order:

1. Compile `libsnark-vnt`.
2. Run `createaccount_key / mint_key / redeem_key / transfer_key / send_key / deposit_key` to produce (pk, vk).
3. Copy the 12 generated `*.txt` keys into `/usr/local/prfKey/`.
4. Install `libzk_*.so`, `libsnark.so`, `libff.so` into `/usr/local/lib/` and run `ldconfig`.
5. `go install ./cmd/geth` → outputs to `$GOPATH/bin/geth`.
6. If `uv` is installed, initialize the Python test environment.

> The Merkle tree height in `libsnark` is fixed at `8` (`2^8 = 256` leaves). In `go-ethereum/zktx/zktx.go`, `ZKCMTNODES = 1` for development; raise it (e.g. 20) for production deployments.

### 2.2 Sub-commands

```bash
./build.sh quick         # Rebuild geth only (use after Go code changes)
./build.sh libsnark      # Build libsnark-vnt only
./build.sh keys          # Regenerate (pk, vk) only
./build.sh install-libs  # Install .so files to /usr/local/lib only
./build.sh install-keys  # Install prfKey to /usr/local/prfKey only
./build.sh geth          # go install geth only
./build.sh status        # Show current build/install status
./build.sh clean         # Clean build artifacts (system-installed files kept)
./build.sh help          # Full help
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

Using `test/pow` as the example (`test/clique` for PoA, same flow):

```bash
cd test/pow

# Terminal 1
rm -rf signer1/data/geth signer1/data/SN signer1.log
geth --datadir signer1/data init pow.json
geth --datadir signer1/data --networkid 55661 --port 2007 \
    --unlock <signer1_addr> --password signer1/passwd.txt \
    console 2>> signer1.log

# Terminal 2
rm -rf signer2/data/geth signer2/data/SN signer2.log
geth --datadir signer2/data init pow.json
geth --datadir signer2/data --networkid 55661 --port 2008 \
    --unlock <signer2_addr> --password signer2/passwd.txt \
    console 2>> signer2.log
```

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
eth.getBalance2(addr)       // ZK balance (locally decrypted, node-local)
eth.getAccountState()       // { balance, commitment, lastTxBlockNumber }
eth.getPubKeyRLP(addr, "")  // node's public key RLP (kept for legacy send flow)
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
// 1) Both sides query their current state
// Terminal A
var b1 = eth.getAccountState()
// Terminal B
var b2 = eth.getAccountState()

// 2) Payer A: generate the proof (no tx broadcast yet) and stage the new local state.
//    To abort, call eth.revertTransferState()
var bn1 = parseInt(b1.lastTxBlockNumber, 16)
var bn2 = parseInt(b2.lastTxBlockNumber, 16)
var seq = [bn1-2, bn1-1, bn1, bn2-2, bn2-1, bn2]  // block sequence agreed by both parties
var valueS = "0x10"
var rs     = "0x01"
var payerData = eth.getPayerNextState(seq, rs, valueS)
// payerData = { cmtANew, snAOld, proofA }

// 3) Ship payerData / seq / valueS / rs to receiver B, who submits the final tx
eth.sendTransferTransaction({
    from:    eth.accounts[0],
    value:   valueS,
    rs:      rs,
    seq:     seq,
    cmtANew: payerData.cmtANew,
    snAOld:  payerData.snAOld,
    proofA:  payerData.proofA
})

// 4) Both sides verify
eth.getAccountState()
```

A complete end-to-end walkthrough is in [`test/pow/TRANSFER_TEST.md`](./test/pow/TRANSFER_TEST.md).

## 5. Status & troubleshooting

```bash
./build.sh status   # check geth / .so / prfKey / venv
```

Common issues:

- **Proof verification fails** — nodes have inconsistent `prfKey`; resync `/usr/local/prfKey/` and restart.
- **`libzk_*.so` not found** — `LD_LIBRARY_PATH=/usr/local/lib` not exported, or `install-libs` was never run.
- **`sn has been used`** — the account SN has already been consumed; reset the account data (`rm -rf signerX/data/SN`) or create a new account.
- **Other ZK txs issued during a Transfer** — local cached `cmt/sn/r` will drift from the chain. Use `eth.revertTransferState()` or reset state and retry.

## 6. References

- Original BlockMaze: <https://github.com/Agzs/BlockMaze>
- Large-scale test reference: <https://github.com/Agzs/BlockMaze-Test>
