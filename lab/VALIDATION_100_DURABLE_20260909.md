# Fresh durable-account 100-node experiment — in progress

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
