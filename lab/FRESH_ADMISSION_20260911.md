# Fresh-only admission live checkpoint, 2026-09-11

Commit `824f1bf` changes running ready-pool admission: stale idle observations
are excluded from payer/receiver selection, but do not invalidate the entire run.
Fresh observations automatically restore eligibility. Initial run preflight is
still strict. Privacy errors, unresolved transaction faults, role changes and
block-sampling errors retain their existing protections. No reservation is reset.

Tests cover strict initial preflight, healthy-pair progress with a stale peer,
readmission after refresh, all-stale waiting, preservation of busy reservations,
availability audit and continued privacy-fault stop behavior. Full Lab race suite
passed; log `lab/data/high-concurrency-100.VLnVI0/stale-admission-tests.log`.

Controller-only restart at an idle transaction checkpoint activated the change
(PID 1817589). Existing node processes, runtime and accounts were not restarted.
Run `load-a581a51d23ed` in experiment `exp-c17eebe55019` is active in warmup:
94 traders, five dedicated miners, one observer; minimum 600-second warmup and
3600-second measurement. Evidence: `lab/data/fresh-only-100.hhlv6f/`.

At 00:53:23 +08:00, an availability sample recorded 34 occupied accounts,
48 fresh eligible accounts and 12 stale idle accounts. The run continued and
reached 18 submissions, rather than stopping at the first stale observation.
This verifies live quarantine behavior, not completion of a valid measurement.

Configuration/export `admissionSamples` records observed counts and stale node
IDs at roughly ten-second opportunities. These are discrete scheduler samples,
not exact pause-duration or maximum-concurrency measurements; under load they
can be delayed. `eligibleAccounts` describes freshness/role/error eligibility,
not a guarantee every account can be paired with its current balance. Historical
run semantics are not rewritten. Reports must distinguish selected node count
from actual participating accounts and observed occupancy.
