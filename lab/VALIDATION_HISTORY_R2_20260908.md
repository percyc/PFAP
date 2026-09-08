# Historical-root live validation R2, 2026-09-08

## Deployment and preparation

- Source: `ce0022d` (includes `3adf07b` and `7fec093`).
- Fresh experiment: `exp-4936a62d9930`, `history-root-7nodes-20260908-r2`.
- Runtime SHA256: `c3933f1bd7be12d3b9b1cb96ae92547e9c5a338a66186599ef05d8e72e509070`.
- Seven servers, one process per server; full mesh with six actual peers each.
- Node 1 is the independent observer; nodes 2 and 5 are dedicated miners;
  nodes 3, 4, 6 and 7 are traders. Workers span five physical host groups.
- All four traders received Public funding, completed CreateAccount, and Minted
  private balance 1,000,000. All 12 preparation transactions succeeded.
  Mint was deliberately serial because its singleton witness is outside the
  Transfer historical-snapshot fix. Preparation waited for six-block depth on
  every node. No keys were regenerated and no old experiment was overwritten.

## Five-minute concurrent run: passed

- Run: `load-eb7bc768ee4e`, saturation, two disjoint pairs, confirmation depth 6.
- Measurement: 22:38:27.737–22:43:27.737 +08:00, exactly 300 seconds.
- Completed after draining: six Transfers confirmed and both accounts ready;
  three warmup submissions and three formal submissions, zero failed/unknown.
- Export reports a complete window, no block error, 183 contiguous sampled
  blocks and 183 execution/state-validation samples (zero missing).

| Metric | Observed value | Scope |
| --- | --- | --- |
| Window confirmations | 4 | May include warmup submissions confirmed inside window |
| Observed confirmation throughput | 0.01333 TPS | 4 / 300 seconds; controller-observed receipts |
| Mean end-to-end latency | 129.114 s | Three formal successful submissions, includes proving/waiting |
| Mean chain confirmation latency | 19.404 s | Same batch, measured from broadcast |
| Mean block interval | 1.676 s | Header timestamps of continuous sampled blocks |
| Mean encoded block size | 622.678 B | All 183 blocks, including empty blocks |
| Mean execution/state validation | 1.356 ms | Observer Process + ValidateState, not full consensus validation |
| Mean payer proof generation | 31.455 s | Three formal transactions |
| Mean receiver proof generation | 30.490 s | Three formal transactions |

This is functional validation, not a capacity benchmark. The small sample count,
shared LXC hosts, short window and receipt polling do not establish one-hour
statistical stationarity or 100-node performance. Startup proving-key loading
made the first pair substantially slower. Additional historical snapshot cost
cannot be isolated from this run without a controlled comparison.

## Raw on-chain anchor audit

The observer fetched `eth_getRawTransactionByHash` for each confirmed Transfer.
Decoded RLP retained 28 fields, two 512-character proofs, a 32-byte root and
exactly one historical anchor height. No privileged state edits were used.

| Historical Transfer | Anchor | Intervening Transfer block | Own block |
| --- | --- | --- | --- |
| `tx-487a3db5c43e` | 601 | 739 (`tx-93c8c4c04b1f`) | 746 |
| `tx-0158a0d5fc85` | 808 | 848 (`tx-76295390ae17`) | 851 |
| `tx-8cc55480846e` | 906 | 945 (`tx-3089e7efffeb`) | 948 |

These establish acceptance at block execution after another Transfer changed the
tree, but do not alone establish a tree change before the receiver RPC began.
An additional staggered run targets that stronger timing condition.

## Staggered receiver validation

Run `load-53780e3a0157` uses the same four ready traders, rate mode 0.02/s
(50-second admission spacing), minimum warmup 60 seconds, a 100-second formal
window and six-block depth.

`tx-8c2fbd5c240f` was observed confirmed at 22:47:04.260 +08:00, before
`tx-cfbd418d35fd` entered its receiver RPC at 22:47:04.728. Both transactions
subsequently confirmed and completed account readiness checks. The raw anchor
audit establishes that the second transaction used block **1074**, the first
transaction changed the tree in block **1082**, and the second was accepted in
block **1113**. This covers a real tree change before receiver verification,
not merely a later tree change during mining.

The staggered run completed its 100-second window and drained successfully:
five Transfers (three warmup, two formal), all confirmed and ready. All 53 sampled
blocks have execution/state-validation timings; neither run has a block error.
Across preparation and both runs: **23 confirmed, zero failed, zero unknown**.
After audit and export, all seven nodes were confirmed `stopped`; the controller
remains available. The result ZIP integrity check and Lab `go test ./...` passed.

## Evidence and safety

Evidence directory: `lab/data/history-r2.Or43Xe/` (ignored runtime data).
It contains `validation.log`, `audit.json`, `audit-final.json`, and separate
`run`, `staggered`, and `experiment` exports in JSON, HTML and CSV ZIP formats.
Exported reports include metric definitions and distinguish preparation, warmup
and measurement. The CSV archive includes the HTML/JSON reports and raw samples.
Old failed runs, including the frozen payer in `tx-e609041afff6`, remain intact;
none was reset, replayed or relabeled as successful.

The 100-node one-hour experiment has not started. Before scaling, review physical
host CPU/memory contention and miner placement, the old 100-node experiment's
unresolved node-7 stop record, and initialization cost. These short runs establish
the historical-root functionality, not automatic recovery from lost responses,
reorg finality, Mint/Redeem witness isolation, or a statistically steady workload.
