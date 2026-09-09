# Fresh durable-account 100-node experiment — in progress

## Live recovery verified, 18:17 +08:00

The original-directory resume completed at 18:16:17. At 18:17 all 100 nodes
report running and each has 99 peers; the minimum sampled block is 78. All five
selected miners (2, 23, 43, 63, 83), including the previously failing node 63,
report mining=true. The guarded runner passed its network/runtime checks and
started the first Public funding batch. Formal measurement has not started;
funding, private-account creation, Mint and warmup still precede that window.

## Recovery checkpoint, 18:08 +08:00

Deployment reached full-mesh configuration at 17:28, but the mining-state read
on node 63 failed with `ssh: signal: killed: false (false)`. The original
SetMining operation shared a 15-second deadline across all SSH/IPC/disk calls.
This is consistent with an exhausted control deadline, not proof of a geth
crash: node 63's retained log shows normal block imports and subsequent orderly
shutdown. Deployment cleanup stopped all 100 nodes. No transactions or workload
runs had been created, so no formal measurement exists.

The controller now gives mining startup 90 seconds (stop remains 15 seconds),
respects earlier caller deadlines, and preserves cancellation/deadline causes
with an explicit unconfirmed-remote-state message. A slow 16-second read and
short caller cancellation are covered by regression tests. Whole-network resume
now reads each identity once and submits one acknowledged peer batch per node,
instead of repeated per-pair SSH reads and bidirectional writes. Missing peer
identities fail closed; single-node recovery is unchanged. Peer admission is
still not evidence of live connectivity: the runner requires all 99 peers.

Full `go test -race ./...` passed after these changes; the log is in the evidence
directory. At a checkpoint with no active experiments/runs, controller binary
and private state were backed up, and the controller was restarted (PID 1348714).
Only the controller changed; node runtime SHA and keys are unchanged.
The original experiment was resumed via its lifecycle API, and the one-shot
runner was continued using its guarded `--resume-preparation` mode. Recovery is
in progress; do not interpret this checkpoint as a successful one-hour result.

## Switch checkpoint (2026-09-09, UTC+08:00)

The user explicitly authorized fresh accounts after being warned that stopping
the old runtime can lose its in-memory private keys. Old experiment
`exp-3b7637ce1061` reached `stopped`, with all 100 node records stopped.
Its directories and unresolved transactions were not reset, replayed or deleted.
Pre-stop JSON/HTML/CSV ZIP reports and a private controller-state backup are in
`lab/data/hour-100-durable.LVuLdv/`. The ZIP passed `python3 -m zipfile -t`.
The private state backup contains credentials and must not be published.

Read-only strict-host-key SSH checks passed on all 100 placements: no running
process named geth, and at least 5 GiB available on the root filesystem. Actual
deployment additionally applies the controller's runtime/DAG disk preflight.
Detailed checks are in `preflight.json` in the evidence directory.

New experiment: `exp-05406ae40c07`

- Runtime SHA-256: `85e1e473abd3fe3d6c56e429e56d4e0e44ece170fb12761f609e4e8cac7b6f71`.
- Network ID 55672; P2P/RPC bases 33000/43000; full mesh.
- 100 distinct server placements, one process per server.
- Five dedicated miners, one per existing physical-host group; one local
  observer; 94 traders. This is five shared physical hosts, not 100 physical hosts.
- Fresh account preparation, at least 600 seconds warmup, then a separate
  3600-second formal Transfer measurement, followed by draining and report export.
- Independent account pairs run concurrently (up to 47 pairs); each individual
  account remains reserved until confirmation and canonical-state readiness.
- Preparation Mint is still globally serial; it is excluded from measurement.

The one-shot `lab/scripts/run-live-hour.cjs` was started with the exact runtime
SHA, new experiment ID and this evidence directory. `started.json` guards against
accidental duplicate execution. Inspect `progress.log` and persisted state before
any intervention; never replay unknown transactions or restart this runner blindly.
It is configured to export run and experiment JSON/HTML/CSV ZIP, then stop the new
network after a complete valid window. On failure it halts and preserves state.

At this checkpoint deployment is still underway. There is **no completed one-hour
measurement or performance conclusion yet**. Fresh-account restart functionality
was separately validated in `VALIDATION_DURABLE_ACCOUNT_20260909.md`.
