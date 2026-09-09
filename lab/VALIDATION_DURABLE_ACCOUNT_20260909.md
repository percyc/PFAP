# Durable private-account recovery — 2026-09-09

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
