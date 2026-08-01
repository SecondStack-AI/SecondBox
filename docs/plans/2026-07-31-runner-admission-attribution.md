---
title: Runner Admission Attribution and Dispatch Batching
date: 2026-07-31
status: implemented-with-open-gate
owner: SecondStack
provenance: Qualified lifecycle evidence on the dirty candidate based on de574bf
---

# Plan: Runner Admission Attribution and Dispatch Batching

## Outcome

Make the assignment-to-runner boundary truthful, remove avoidable scheduler
round trips, and isolate the remaining admission latency. The retained candidate
does all three, but it does not pass the snapshot plan's unsaturated
`runner_admission` p95 target of 25 ms: the final 30-arrival run measured
17/31 ms p50/p95.

The scheduler previously reused the lifecycle worker's `Now`, captured before
the Sandbox claim and start-plan load, as the Assignment creation and
`placement_ready` time. That attributed work performed before an Assignment
existed to runner admission. The scheduler now has an explicit clock and
captures `placementAt` only after runner selection and command encoding, just
before the durable assignment writes.

## Retained design

- Keep the existing serializable transaction, advisory Sandbox lock, and
  ordered Sandbox, Workspace, and Runner row locks.
- Queue the Workspace start mutation, Instance, Assignment, runner command,
  Runner capacity reservation, and Sandbox projection as one ordered
  PostgreSQL batch. They still commit or roll back together.
- Timestamp the entire placement projection from the scheduler's explicit
  clock. Lifecycle `request.Now` remains the decision time for heartbeat and
  deadline validation, not a fabricated Assignment creation time.
- Execute the authoritative `ClaimCommands` data-modifying CTE with pgx's
  one-round-trip extended execution mode. This avoids first-use statement
  preparation on each pooled connection while preserving the same connection
  lock, command lock, sequence allocation, and delivery state transition.
- Log claim and stream-send duration separately from queue and total delivery
  duration. Every qualified assignment stream send rounded to 0 ms; the
  remaining serial work is notification/queueing plus the database claim.

## Correctness boundaries

- The batch changes PostgreSQL transport, not transaction scope or authority.
  No Instance, Assignment, command, capacity reservation, Sandbox projection,
  or Workspace mutation can become visible independently.
- The Workspace mutation update must affect exactly one locked row. Batch
  execution and close errors remain explicit, domain-prefixed failures.
- The claim remains the only authority that binds a pending command to the
  active runner connection and allocates its control sequence.
- PostgreSQL commit notification and bounded polling remain the durable,
  replica-safe wake paths. A tested same-process post-commit hint was removed
  because it did not improve the tail and added a second delivery coupling.
- Public contracts and compute ports are unchanged and provider-neutral.

## Qualified result

All runs used KVM, Btrfs Workspaces, the same signed artifact bundle, one ready
runner, and zero refusals or failures. Queue, claim, and send values come from
the runner-control delivery log and are direct but separately distributed
phase percentiles; they must not be added as percentile arithmetic.

| 30 fixed arrivals, max in-flight 1 | `runner_admission` p50/p95 | queue p50/p95 | claim p50/p95 | stream send p50/p95 |
|---|---:|---:|---:|---:|
| Corrected-clock baseline | 18/31 ms | 7/15 ms | 10/15 ms | 0/0 ms |
| Batched scheduler writes | 20/37 ms | **6/9 ms** | 11/30 ms | 0/0 ms |
| Batch plus one-round-trip claim | **17/31 ms** | 6/15 ms | 10/16 ms | 0/0 ms |
| Rejected local wake hint | 20/31 ms | 6/17 ms | 11/25 ms | 0/0 ms |

The phase runs show the intended local effects, while the top-line p95 remains
host-tail sensitive. The retained candidate reduced median admission by 1 ms
against the corrected baseline but did not reduce its 31 ms p95. The 25 ms
gate therefore remains open; no result is rounded down or treated as a pass.

| Burst 32 | Ready-projection candidate | Retained candidate |
|---|---:|---:|
| `create_to_ready` p50/p95 | 2,070/2,639 ms | 2,191/2,733 ms |
| `runner_admission` p50/p95 | 63/960 ms | 62/1,046 ms |
| completion rate | 11.51/s | 11.45/s |
| completed / failed | 32/0 | 32/0 |

Burst admission includes the intentional global Workspace-create barrier and
is not the unsaturated 25 ms gate. The retained candidate preserves burst
throughput within observed host variance but does not claim a burst speedup.

Machine-readable evidence:

- `.tmp/lifecycle-runner-admission-final-c1-30-result.json`
- `.tmp/lifecycle-scheduler-batch-c1-30-result.json`
- `.tmp/lifecycle-claim-exec-c1-30-result.json`
- `.tmp/lifecycle-local-wakeup-exec-c1-30-result.json`
- `.tmp/lifecycle-scheduler-batch-claim-exec-burst32-result.json`

The reports identify source commit `de574bf`; they qualified dirty candidates
from this optimization pass.

## Next gate

`ClaimCommands` now attributes pool acquisition, PostgreSQL execution, and row
decoding independently. Two consecutive qualified 30-arrival runs compared a
pre-prepared claim statement with the retained one-round-trip extended
execution mode on the same host:

| Claim mode | pool acquire p50/p95 | query p50/p95 | decode p50/p95 | `runner_admission` p50/p95 |
|---|---:|---:|---:|---:|
| Pre-prepared statement | 0/0 ms | 28/59 ms | 0/0 ms | 45/91 ms |
| One-round-trip execution | 0/0 ms | **25/48 ms** | 0/0 ms | **41/83 ms** |

The host was slower throughout both runs than in the retained qualification:
the one-round-trip control measured `workspace_provision` at 311/377 ms versus
126/153 ms previously. The A/B result therefore rejects statement preparation
but does not replace the earlier absolute baseline. It does establish that pool
waiting and protobuf decoding are not optimization targets: PostgreSQL query
execution accounts for the entire measured claim duration at millisecond
resolution.

The retained follow-up removes row locking and tuple updates from empty command
polls while preserving the transactionally sequenced claim when work is
present. On the live development stack, the prior query changed the active
connection tuple 11 times in two idle seconds; the retained query changed it 0
times in the same interval, with `last_control_sequence` unchanged. A
PostgreSQL-backed regression test verifies the same tuple-version invariant.

The follow-up qualification passed all 30 arrivals but ran while the host was
under heavier system-wide PostgreSQL and filesystem load. It measured query at
33/62 ms, queue at 15/32 ms, `runner_admission` at 58/93 ms, and
`workspace_provision` at 348/407 ms p50/p95. Those numbers neither demonstrate
an admission improvement nor invalidate the earlier quiet-host baseline. The
change is retained for the exact write-amplification reduction, not as closure
of the latency gate.

The next candidate should attribute and remove the empty relay transaction that
currently follows every empty command claim and can delay a newly committed
command wakeup. Do not optimize stream sending: it remains 0 ms. Re-run at
least 30 unsaturated arrivals on a quiet qualified host, and require measured
p95 at or below 25 ms before marking runner admission complete in the
snapshot-resume plan.

Additional machine-readable evidence:

- `.tmp/lifecycle-claim-prepared-c1-30-result.json`
- `.tmp/lifecycle-claim-attributed-exec-c1-30-result.json`
- `.tmp/lifecycle-claim-idle-read-c1-30-result.json`

## Validation

- scheduler clock requirement unit test
- replica-safe scheduling, runner protocol conformance, and two-runner
  home-pinning integration tests
- qualified concurrency-1 and burst-32 lifecycle runs
- `just verify-generated`
- `just test` against a disposable PostgreSQL database
- `just test-contract`
- `just test-compose`
- focused scheduler, runner-control, and integration race tests
- `just test-scenario` on the qualified KVM/Btrfs host
- `git diff --check`
