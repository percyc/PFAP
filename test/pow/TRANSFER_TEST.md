# Transfer Two-Node End-to-End Test

> Test the Transfer flow on a PoW network with two nodes: signer1 (payer) and signer2 (receiver).

---

## Preparation

### 1. Clean old data (including keystore)

The `signerX/` directories only ship with `passwd.txt`. For a fresh end-to-end
run we wipe the whole `data/` directory (keystore included) so the addresses
created below are guaranteed to match what we unlock later.

```bash
cd ~/go/src/github.com/PFAP/test/pow
rm -rf signer1/data signer2/data *.log
```

### 2. Create an account in each datadir

```bash
geth --datadir signer1/data account new --password signer1/passwd.txt
# => Address: {aaaa...}   <-- this is <signer1_addr>

geth --datadir signer2/data account new --password signer2/passwd.txt
# => Address: {bbbb...}   <-- this is <signer2_addr>
```

Write both addresses down — you will paste them into the `--unlock` flags
below. They will be different from the placeholders shown in this document.

### 3. Initialize both nodes

```bash
geth --datadir signer1/data init pow.json
geth --datadir signer2/data init pow.json
```

---

## Step 1: Start both nodes

> Replace `<signer1_addr>` / `<signer2_addr>` with the addresses printed in
> the previous step. Do **not** copy the hex literals shown here verbatim.

### Terminal 1 (signer1 - payer)

```bash
cd ~/go/src/github.com/PFAP/test/pow
geth --datadir signer1/data --networkid 55661 --port 2007 \
  --unlock <signer1_addr> \
  --password signer1/passwd.txt console 2>> signer1.log
```

### Terminal 2 (signer2 - receiver)

```bash
cd ~/go/src/github.com/PFAP/test/pow
geth --datadir signer2/data --networkid 55661 --port 2008 \
  --unlock <signer2_addr> \
  --password signer2/passwd.txt console 2>> signer2.log
```

---

## Step 2: Connect the nodes

In **Terminal 1 (signer1)**, get the local enode info:

```javascript
admin.nodeInfo.enode
// e.g. "enode://xxx@127.0.0.1:2007"
```

In **Terminal 2 (signer2)**, get the local enode info:

```javascript
admin.nodeInfo.enode
// e.g. "enode://yyy@127.0.0.1:2008"
```

In **Terminal 1**, add Terminal 2 as a peer:

```javascript
admin.addPeer("enode://<signer2-enode-id>@<signer2-ip>:2008")
net.peerCount  // should return 1
```

---

## Step 3: Mine to get ETH

In **Terminal 1**, start mining:

```javascript
miner.start()
// Wait for a few blocks, then check the balance
eth.getBalance(eth.accounts[0])
```

---

## Step 4: Create a ZK account and Mint

Run on both terminals:

```javascript
// 1. Create a ZK account
eth.sendCreateAccountTransaction({from: eth.accounts[0]})

// 2. Wait until the tx is mined
eth.blockNumber

// 3. Mint a ZK balance
eth.sendMintTransaction({from: eth.accounts[0], value: "0x100"})

// 4. Wait for a block, then check the balance
eth.getAccountState()
```

---

## Step 5: Run a Transfer (new flow)

### 5a. Both sides query their current state

**Terminal 1 (signer1 - payer):**

```javascript
var b1 = eth.getAccountState()
console.log("payer state:", b1)
// b1 = {balance: ..., commitment: "...", lastTxBlockNumber: ...}
```

**Terminal 2 (signer2 - receiver):**

```javascript
var b2 = eth.getAccountState()
console.log("receiver state:", b2)
// b2 = {balance: ..., commitment: "...", lastTxBlockNumber: ...}
```

### 5b. Payer generates the proof

Continue in **Terminal 1 (signer1 - payer)**:

```javascript
// 1. Build the seq array (contains the blocks of both CMTs plus a few surrounding blocks)
var bn1 = parseInt(b1.lastTxBlockNumber, 16)
var bn2 = parseInt(b2.lastTxBlockNumber, 16)
var seq = [bn1-2, bn1-1, bn1, bn2-2, bn2-1, bn2]

// 2. Transfer parameters
var valueS = "0x10"     // transfer amount (16)
var rs = "0x01"         // random value agreed by both parties

// 3. Generate the payer-side proof (no tx broadcast yet)
var payerData = eth.getPayerNextState(seq, rs, valueS)
console.log("payer data:", payerData)

/* payerData contains:
   {
     cmtANew: "0x...",   // payer's new commitment
     snAOld: "0x...",    // payer's old serial number
     proofA: "0x..."     // payer's proof
   }
*/
```

> If you want to abort the transfer, call: `eth.revertTransferState()`

### 5c. Receiver submits the final transaction

Copy `payerData` from **Terminal 1** to **Terminal 2**. In **Terminal 2 (signer2 - receiver)**:

```javascript
// Transfer parameters
var valueS = "0x10"     // transfer amount (16)
var rs = "0x01"         // random value agreed by both parties
var seq = [<bn1-2>, <bn1-1>, <bn1>, <bn2-2>, <bn2-1>, <bn2>]  // copy manually from Terminal 1

// Submit the final tx using the data received from the payer
eth.sendTransferTransaction({
    from: eth.accounts[0],
    value: valueS,
    rs: rs,
    seq: seq,
    cmtANew: payerData.cmtANew,
    snAOld: payerData.snAOld,
    proofA: payerData.proofA
})
```

---

## Step 6: Verify the result

Run on both terminals:

```javascript
// Check the new ZK balance
eth.getBalance2(eth.accounts[0])

// payer's balance should decrease by valueS
// receiver's balance should increase by valueS

// Inspect the txpool and latest block
txpool.status
eth.getBlock("latest")
```

---

## Step 7: Redeem

```javascript
eth.sendRedeemTransaction({from: eth.accounts[0], value: "0x1"})
```
