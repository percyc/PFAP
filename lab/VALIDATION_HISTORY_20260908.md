# Historical-root live validation, 2026-09-08

## Result: not passed; network safely stopped

- Source baseline: `7fec093`; controller amount fix: `3adf07b`.
- Experiment: `exp-c0debc45c804`, `history-root-7nodes-20260908`.
- Runtime used by all seven nodes: `8829b73987fd514d5e3877a92e7d4c9d49eba08d42f0ea92e8a571880948d9a2`.
- One process per server; node 1 observer; nodes 2/5 dedicated miners;
  nodes 3/4/6/7 traders. Five host groups, full mesh, six actual peers per node.
- Run: `load-a7030fd51874`, 60-second minimum warmup and planned 300-second
  measurement, six-block confirmation depth. **Measurement never started.**
- All seven nodes confirmed stopped at 2026-09-08 22:04:35 +08:00.
- Controller updated/restarted; previous binary and data backup are in
  `lab/data/history-release.BGz0oJ`. PID is recorded in `lab/data/server.pid`.
- No keys regenerated, no old experiment data overwritten, and no frozen payer
  state reverted or resubmitted. The 100-node one-hour experiment was not started.

## Preparation and parameter-format fix

All four traders eventually completed Public funding, CreateAccount and Mint,
with private balance 1,000,000 each. Preparations were serial and waited for all
nodes to observe six-block depth before proceeding.

`tx-399cf869baf4` (Mint) was rejected before handler invocation because a decimal
string was sent to `hexutil.Big`. Lab now normalizes decimal/hex amounts to RPC hex
quantities, preserving 256-bit Public amounts and zero-value Public transactions;
anonymous amounts must be positive uint64 values.

The original record was reconciled through a narrow, tested API rule matching
the exact generated expression, decoder rejection, execution stage and absence
of broadcast/receipt evidence. Its full error remains, with an appended
non-execution conclusion. This rule excludes Transfer, timeouts and ambiguous
errors. The corrected Mint is a separate transaction, `tx-74544233eadb`.

## Concurrent Transfer outcome

| Transaction | Pair | Payer proving start (+08:00) | Outcome |
| --- | --- | --- | --- |
| `tx-e609041afff6` | 3 → 4 | 21:58:58.247 | Receiver rejected payer proof; no transaction hash, payer frozen, remains unknown |
| `tx-17cedc367c86` | 6 → 7 | 21:59:00.661 | Confirmed 22:03:04.486, both accounts ready 22:03:33.847 |

Successful Transfer hash:
`0x9682d9555bdf3c6e12227a5eacda4f3821bec5b54e5b409825d8d272d9c08c7f`.

The scheduler admitted disjoint pairs concurrently (2.414 seconds apart), then
stopped new submissions on the first uncertainty. The other in-flight call was
allowed to finish before stopping the network. Total records: 13 confirmed,
1 proven-not-executed failure, 1 unknown. No formal performance claims are valid.

The explicit stop action replaced the run's invalidReason with the generic early
stop message; the original failure is retained in the transaction error and
validation logs. This must not be interpreted as a clean user-canceled run.

## Native commitment serialization defect reproduced

The active Transfer native library's `genCMTStransfer` and `genCMT` copied a
64-character digest into a 67-byte buffer but terminated at offset 66. Residual
hex characters could be decoded as a 33rd byte; `BytesToHash` then dropped the
first byte. Thus identical inputs could yield different public commitments in
different processes or allocation histories, causing proof verification failure.

Before the fix, `TestTransferCMTSDeterministic` failed at iteration 113:

```
expected a6f9b57526651965a80c9b5e18034e6155b3b1113aca75c38d660581cc1ce37b
actual   f9b57526651965a80c9b5e18034e6155b3b1113aca75c38d660581cc1ce37bf6
```

The native returns now terminate at the actual digest length. Go frees the owned
native return buffers. No circuit constraints, public inputs, or keys changed.
The regression test checks both shared account commitments and Transfer amount
commitments; 10 runs of 1000 repetitions passed, together with relevant historical
root/snapshot tests under `-race`. Go race checks do not replace native sanitizers.

The post-fix real historical-witness proof test passed in 136.05 seconds using
the unchanged keys; wrong-root and mismatched-witness cases were rejected.
Lab `go test -race ./...` and the compatible runtime rebuild also passed.

Logs:

- `lab/data/history-release.BGz0oJ/validation.log`
- `lab/data/history-release.BGz0oJ/validation-resumed.log`
- `/tmp/pfap-cmts-regression-before.log`
- `/tmp/pfap-cmts-regression-after.log`
- `/tmp/pfap-cmts-fixed-real-proof.log`

This defect explains a concrete path to cross-node parameter mismatch, but a
fresh-chain multi-node re-test is still required after the fix. Do not reuse the
frozen account or claim this failed run as a successful steady-state experiment.
