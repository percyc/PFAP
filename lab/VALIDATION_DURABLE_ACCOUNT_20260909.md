# Durable private-account recovery — 2026-09-09

## Result: isolated restart regression passed (14:21 +08:00)

All nine transactions confirmed with no failure/unknown: 2 Public, 2 CreateAccount,
2 Mint, 2 Transfer and 1 Redeem. Both traders survived two SIGKILL/recovery cycles
each (four abrupt terminations total). After the Transfer recovery checkpoint,
the reverse Transfer also confirmed (`tx-259610e0b714`), followed by Redeem
`tx-a34e2d1695f6`. Final balances exactly match expectations: node 3 = 999999,
node 4 = 1000000. No reinitialization, state rollback or transaction replay was
used. This validates tested restart paths, not every possible power-loss timing.

All eight pk/vk files compare identical to the previous deployed runtime.
Evidence directory: `lab/data/durable-account.sCZj85/`; saved live log, executed
runner and compatible-build log. Reusable runner is
`lab/scripts/validate-private-restart.cjs` (parameterized from the executed runner,
with additional CLI/directory/final-balance guards; not re-executed against used
accounts). Experiment HTML/JSON/CSV ZIP export and stop checks are recorded below.

Final checks: `experiment.json`, `experiment.html`, `experiment.zip` exported
after stop; ZIP integrity passed `python3 -m zipfile -t`, JSON schemaVersion=2.
All four validation nodes and their experiment are confirmed stopped. The Lab
controller remains up. Original experiment `exp-3b7637ce1061` still has 100
running nodes on its original runtime; it was not restarted, overwritten or
silently included in this validation's metrics.

This is a four-node functional recovery test, **not** the requested 100-node
one-hour performance result. The original 100-node experiment remains untouched,
including its uncertain accounts; new performance testing must use sound fresh
accounts and must not count old failed warmup or preparation as a formal window.

## Implementation and safety contract

- Each process has one private account. A mutex covers private transaction
  admission, proof generation and persistence; different nodes remain concurrent.
- `private-account-v1.json` is an owner-only (0600) authoritative record containing
  the random AccountSK, complete next sequence state, transition kind, and signed
  transaction/hash. It is secret material: do not publish, log or include it in
  ordinary experiment report exports. Filesystem permissions are not encryption;
  use encrypted storage/backups where required.
- Before broadcasting any CreateAccount/Mint/Redeem/receiver Transfer, the record
  is written to a same-directory temporary file, fsynced, atomically renamed and
  its parent directory fsynced. Payer preparation persists its frozen next state
  before returning the proof. Save failures disable further writes; uncertain
  submission never restores an earlier state or silently retries.
- Startup restores the versioned record. Legacy SN remains untouched for
  inspection; an initialized legacy account without its original random secret
  is read-only. The old public-address-derived secret fallback is refused.
- A subsequent mutation requires the current private commitment to exist in a
  canonical block-boundary snapshot, plus existing spent-SN checks. Pending
  transitions cannot be overwritten with another transaction or CreateAccount.
- `eth.getPrivateRecovery()` exposes only kind/hash/error, never keys, sequence
  secrets or the signed payload. Recovery metadata is not proof of inclusion.
  Lab still requires receipt and both-account confirmation/readiness checks.
- Unconditional `revertTransferState` is disabled: an issued payer proof may be
  in flight elsewhere. No automatic replay/rollback was added. A crash before
  submission can intentionally leave a durable reservation requiring explicit
  investigation; this change does not guarantee automatic liveness in every
  ambiguous failure or finality across arbitrary chain reorganizations.
- Do not downgrade a populated new datadir to an old binary: old binaries do not
  understand the authoritative journal. Never delete the journal to reinitialize
  an account. Previously lost random secrets cannot be reconstructed by this fix.

## Verification so far

Targeted Go race tests cover atomic replacement, owner-only permissions, checksum
and signed-hash validation, secret/state restoration, persistence failure, legacy
missing-secret rejection, durable-before-broadcast ordering, ambiguous submission,
and concurrent private mutation rejection. Historical-root regression tests pass.
Legacy core tests have a pre-existing printf-format vet failure in tx_pool_test.go;
the targeted geth suites were run with `-vet=off`, not claimed as a clean full vet.
Full Lab race tests pass, including warning clearance only after a durable,
on-chain-ready account sample. Ubuntu 22 compatible runtime built without key
regeneration; real restart validation is still pending at this checkpoint.

The old 100-node experiment and its uncertain accounts remain preserved. A fresh
small independent network will first verify CreateAccount/Mint/Transfer across
process restarts. No completed one-hour performance result is claimed here.

## Live validation in progress (14:00 +08:00)

- Source `e149d03`, runtime SHA256
  `85e1e473abd3fe3d6c56e429e56d4e0e44ece170fb12761f609e4e8cac7b6f71`.
- Fresh experiment `exp-627c5ea52f82`, network ID 55671, P2P/RPC bases 32000/42000.
  Four servers: controller-local observer, db2-03 miner, db1-03/pv4-03 traders.
  These are independent datadirs; the old 100-node network remains untouched.
- Controller PID 1267169; controller-only update activated durable-state sampling
  and warning handling. Existing uncertain transaction states were not reset.
- One-shot validation script `/tmp/pfap-durable-live.cjs`, output
  `/tmp/pfap-durable-live.log`. Do not rerun after accepted transactions. Planned
  test: fund/create both accounts; validated PID/datadir SIGKILL and recovery of
  both traders; Mint; Transfer; SIGKILL/recovery again; reverse Transfer; Redeem.
  Every transaction must confirm before the next checkpoint, with six-block depth.
- At this checkpoint the network is connected and producing blocks; first
  CreateAccount confirmed. No successful restart result is claimed yet.

### Restart evidence at 14:14 +08:00

Both traders were SIGKILLed after their CreateAccount confirmations and recovered
from the same datadirs, then each successfully minted 1000000. Transfer
`tx-8ce06d54f579` subsequently confirmed. Both were SIGKILLed and recovered again;
post-restart balances were exactly node 3 = 999999, node 4 = 1000001, with no
private-state error or legacy-key warning. This directly exercises the previously
observed stale receiver balance failure. No replacement CreateAccount was sent.
Reverse Transfer `tx-259610e0b714` is now admitted; its result and final Redeem
remain pending. Four real abrupt process terminations have been tested so far.
