# Transfer historical commitment roots

Follow-up: the first seven-node live run exposed a native commitment-string
termination defect, now corrected and regression-tested. The live run did not
pass; see [live validation record](VALIDATION_HISTORY_20260908.md). The subsequent
[fresh-chain R2 validation](VALIDATION_HISTORY_R2_20260908.md) passed two live runs,
including a tree change before receiver verification. Eleven Transfers completed
without frozen accounts; the 100-node one-hour performance test is still pending.

## Protocol and deployment

This change is a **consensus upgrade for fresh experiments**, not a transparent
upgrade of an existing chain. Uniformly deploy the new native libraries/geth and
updated Lab controller to a fresh experiment. Do not replay old experiment
databases under the new rules or mix old/new binaries. Existing results and
frozen accounts must not be reset or automatically resubmitted.

The proof circuit and its public inputs are unchanged; no pk/vk regeneration is
required. Transaction RLP structure is unchanged. `CMTBlock` now contains exactly
one historical block height. Legacy unanchored or payer-only Transfer transactions
are rejected. Transfer proof serialization is exactly 512 ASCII hex characters
(two G1 points and one G2 point); the old native return buffer incorrectly left
uninitialized bytes after the actual proof.

## Flow and safety boundaries

1. Reserve **both** initialized, funded accounts. Each account remains serial;
   disjoint pairs may operate concurrently. The receiver's current commitment
   must already exist at the payer's anchor block.
2. `eth.getPayerNextState(rs, value)` builds an isolated snapshot from the locally
   verified chain and returns `proofA`, `cmtANew`, `snAOld`, `proofRoot`, and
   `proofBlock`. The proof generator consumes a copied historical witness, never
   rereads the mutable global SMT. Before freezing the payer, ancestry is checked
   again to detect a reorg during proving.
3. Lab passes both anchor fields unchanged to `eth.sendTransferTransaction`.
   The receiver verifies that the root belongs to that ancestor, verifies proof A,
   and generates proof B from the same historical snapshot.
4. The transaction pool and block execution independently validate the anchor.
   Block execution checks ancestry of the **candidate block's parent**, not an
   unrelated current canonical head. A root from a discarded fork is not accepted
   merely because it was once cached. Missing ancestry fails closed.
5. Both payer and receiver serial numbers must be unspent; both are consumed
   when executing the combined transaction. An old root does not permit replay
   of either account's already-used state.

Historical roots are reconstructed from block commitments, not supplied by the
peer or registered from the speculative mining/execution singleton. This matches
the existing commitment insertion convention, which inserts before ApplyMessage
and does not filter by receipt status.

This does **not** solve arbitrary reorgs after payer freezing, lost RPC responses,
or recovery of already-frozen accounts. Keep confirmation-depth and reconciliation
rules. Mint/Redeem still use the existing singleton witness implementation; this
change does not claim to fix their speculative-tree isolation.

## Performance scope

No additional ZK proofs or circuit constraints are introduced. Each Transfer
still generates and verifies one payer and one receiver proof. Additional work
is ancestor lookup, cached root comparison, and isolated witness construction.

- Root metadata cache: at most 4096 entries; cache keys are immutable block hashes.
- Native snapshot cache: at most four snapshots, serialized access. Proof generation
  runs outside its lock, using a copied 16,705-character witness.
- Empty blocks reuse the existing tree without copying it.
- A changed-tree snapshot clones the native map and inserts new commitments.
  Copy cost and memory grow with tree size; limiting snapshot count does not
  impose a fixed byte limit.
- A cold/missed historical snapshot may replay blocks back to a cached ancestor
  or genesis. This cost can be significant on long chains. A persistent/versioned
  SMT and durable root index are the next scalability improvement if measurements
  show frequent misses. Do not infer 100-node/one-hour throughput from microbenchmarks.

Local microbenchmarks on Xeon E5-2640 v4 (shared host, not an isolated capacity test):

| Operation | Result |
| --- | --- |
| Cached root, anchor is head, 1000 iterations | 87 ns/op |
| Cached root, walk 120 in-memory ancestors, 1000 iterations | 6.7 us/op |
| Clone and free snapshot with 256 commitments, 10 iterations | 26.7 ms/op |

The ancestor benchmark does not measure database cache misses. The snapshot
benchmark does not measure genesis replay, and Go allocation counters do not
account for native snapshot memory.

## Reproduction

Build without regenerating keys:

```sh
./scripts/build-compatible-runtime.sh
bash scripts/test-transfer-history.sh -race -vet=off \
  -run 'TestTransferHistory|TestTransferRequires|TestSMTSnapshot|TestReplayCanonicalCommitments|TestTransactionCommitments' -count=1
PFAP_TEST_REAL_PROOFS=1 bash scripts/test-transfer-history.sh -vet=off \
  -run '^TestTransferHistoricalWitnessRealProof$' -count=1 -v
bash scripts/test-transfer-history.sh -vet=off -run '^$' \
  -bench 'BenchmarkTransferHistory|BenchmarkSMTSnapshotClone256' -benchtime=10x
```

`-vet=off` works around an existing formatting diagnostic in
`core/tx_pool_test.go:129`; it is not a suppression of a new production-code error.
The real-proof test uses existing keys and no live account or node. It changes the
tree between payer/receiver proving, checks both proofs at the old root, and
rejects the different root and mismatched witness. It is not a live multi-node run.

## Validation result (2026-09-08)

- Ubuntu 22 compatible runtime build succeeded without regenerating keys.
- Historical-root, fork rejection, missing ancestry, future commitment rejection,
  concurrent snapshot access, and both-party spent-serial tests passed with `-race`.
- Lab `go test -race ./...` passed; Web JavaScript syntax and `git diff --check` passed.
- Real proof test passed in 135.80 seconds, including cold proving-key load.
  Both proofs verified at the original root after a later tree update; their
  combined verification took 17.8 ms. The different-root and mismatched-witness
  negative cases were rejected as expected.
- Real-proof output: `/tmp/pfap-history-real-proof-v2.log`. Other outputs:
  `/tmp/pfap-history-race.log`, `/tmp/pfap-history-lab-race.log`,
  `/tmp/pfap-history-root-bench.log`, `/tmp/pfap-history-bench.log`.
- No live controller restart, node deployment, key replacement, or 100-node
  experiment was performed for these tests. Complete a fresh-chain multi-node
  integration run before treating this as validated for the one-hour experiment.
