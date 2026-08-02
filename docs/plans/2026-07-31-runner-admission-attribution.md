---
title: Runner Admission Attribution and Dispatch Batching
date: 2026-07-31
status: implemented-with-open-gate
owner: SecondStack
provenance: Qualified lifecycle evidence through the 2026-08-02 ready-deadline candidate and repeated-burst race evidence at 05bbd8e
---

# Plan: Runner Admission Attribution and Dispatch Batching

## Outcome

Make the assignment-to-runner boundary truthful, remove avoidable scheduler
round trips where they do not compromise placement concurrency, and isolate
the remaining admission latency. The retained work does all three, but it does
not pass the snapshot plan's unsaturated `runner_admission` p95 target of
25 ms. The earlier quiet baseline measured 17/31 ms p50/p95. A later eager
dispatch candidate measured 11/34 ms, but it was rejected after repeated
bursts proved that locking the runner's single connection row inside every
placement could exhaust serialization retries and shut down the control plane.
The race-safe owner-claim design measured 39/66 ms on the same 30-arrival
unsaturated workload. The retained ready-deadline follow-up measures 34/58 ms;
the 25 ms admission p95 gate remains open.

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
- Queue every Assignment command as `pending`. The connection owner's
  `ClaimCommands` call binds it to the active connection and allocates its
  connection-local control sequence. Placement never locks or updates
  `runner_connections`.
- Keep runner reconnect recovery unchanged: it requeues every unacknowledged
  `delivering` command, and the current connection owner assigns a fresh local
  sequence through the same claim path.
- Classify exhausted serializable retries as expected contention, defer both
  reconcilers rather than terminating the process, and use full-jitter retry
  backoff so a colliding cohort does not retry in lockstep.
- Persist provider-neutral lifecycle pickup, start-plan, scheduler lock,
  selection, retry, and ordered-write milestones in the Assignment command's
  existing batch. Attribution adds no database round trip to placement.
- Schedule a healthy ready Sandbox's next lifecycle reconciliation at its
  earliest idle or maximum-duration deadline instead of reclaiming it every
  poll interval. Transitional states and ready Sandboxes with active sessions
  retain bounded polling; desired-state, runner-terminal, and guest-terminal
  evidence explicitly wake sleeping Sandboxes.
- Log claim and stream-send duration separately from queue and total delivery
  duration. Every qualified assignment stream send rounded to 0 ms; the
  remaining serial work is notification/queueing plus the database claim.

## Correctness boundaries

- The batch changes PostgreSQL transport, not transaction scope or authority.
  No Instance, Assignment, command, capacity reservation, Sandbox projection,
  or Workspace mutation can become visible independently.
- The Workspace mutation update must affect exactly one locked row. Batch
  execution and close errors remain explicit, domain-prefixed failures.
- Exactly one durable transaction binds a command to the active runner
  connection and allocates its control sequence: the connection owner's
  `ClaimCommands` transaction.
- A runner's single connection row must not enter the concurrent placement
  transaction. The owner-side claim has one writer by construction; placement
  has as many writers as concurrent Sandbox starts.
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

The retained relay follow-up collapses outbound claim validation, frame
selection, connection fencing, and frame mutation into one atomic PostgreSQL
statement. An empty poll no longer opens a transaction or takes a shared lock
on the active connection row; a real claim still locks that row before it
updates the selected frame. On the live development stack, the prior empty
poll changed the connection tuple's `xmax` 22 times in two idle seconds. The
retained query changed neither `xmin`, `xmax`, nor `last_control_sequence` in
the same interval. A PostgreSQL-backed regression test preserves this
invariant.

Five exact `secondbox run coding-environment -- python3 -c 'print("hello")'`
checks against the deployed candidate all succeeded in 1.264–1.535 seconds,
with a 1.276-second median. This loaded-host smoke test proves the consolidated
claim still carries real data-plane traffic; it is not a qualified replacement
for the admission baseline.

The qualified KVM/Btrfs scenario passed relay and direct Port transport,
buffered and streaming exec, cancellation, concurrent execution, ordinary
lifecycle, control-plane restart, runner-loss recovery, generation fencing,
network policy, expiry, admission, real compute boot, and Snapshot retention.
Its only failure is the existing Snapshot-restore assertion that expects
`409 state_conflict` for a stale revision even though the API correctly returns
`412 precondition_failed`; the relay candidate does not touch that contract.

The next candidate folded Assignment connection binding and control-sequence
allocation into the scheduler's existing placement commit when the runner was
idle. Its immediately preceding loaded-host control and its qualified result
used the same 30-arrival fixed-rate workload. This is historical evidence for
a rejected candidate, not the retained design:

| Loaded-host 30 arrivals | `runner_admission` p50/p95 | queue p50/p95 | claim query p50/p95 | `workspace_provision` p50/p95 |
|---|---:|---:|---:|---:|
| Empty-poll optimizations only | 51/90 ms | 15/31 ms | 33/65 ms | 299/364 ms |
| Eager Assignment dispatch | **11/34 ms** | **10/33 ms** | **0/1 ms** | 262/324 ms |

All 30 eager-dispatch arrivals completed without refusal or failure. The
directly attributable claim reduction was real, but the implementation was not
safe to retain; the 34 ms admission p95 is also not rounded down to a gate
pass. Five exact live CLI runs completed in
0.996–1.319 seconds with a 0.999-second median, and their Assignment claim
queries were 0–1 ms. The complete qualified KVM/Btrfs scenario then passed,
including control-plane restart, runner-loss recovery, real relay and direct
Port traffic, buffered and streaming exec, fencing, network policy, real
compute boot, Snapshot restore, and retention.

The later repeated burst-32 capacity ladder exposed the missing concurrency
case. Every placement for one runner took `FOR UPDATE` on that runner's single
connection row under serializable isolation. The second rung could exhaust the
three-retry budget and terminate the reconciler, which initiated coordinated
server shutdown. Commit `05bbd8e` removed that lock from placement, restored
owner-side claims, made exhausted contention explicitly retryable, and added
full-jitter backoff. Two independent ten-rung qualifications then admitted
640/640 arrivals with no shutdown; 24 and 26 serialization failures were
absorbed. See `docs/operations/assignment-dispatch-stall.md`.

The race-safe current `main` was rebaselined with 30 fixed arrivals at 0.25/s,
maximum in-flight 1, KVM, Btrfs Workspaces, and the same signed bundle. All 30
completed with no refusal or failure:

| Current race-safe dispatch | p50 | p95 | p99 |
|---|---:|---:|---:|
| `create_to_ready` | 1,144 ms | 1,695 ms | 1,698 ms |
| `pre_assignment` | 662 ms | 1,152 ms | 1,178 ms |
| `placement` | 415 ms | 843 ms | 859 ms |
| `workspace_provision` | 235 ms | 307 ms | 391 ms |
| `runner_admission` | 39 ms | 66 ms | 74 ms |
| Assignment queue | 12 ms | 32 ms | 34 ms |
| Assignment claim query | 25 ms | 48 ms | 52 ms |
| `runner_boot` | 433 ms | 479 ms | 488 ms |
| `client_visibility` | 33 ms | 57 ms | 62 ms |

Pool acquisition, decode, and stream send remained 0 ms. Five exact live
`secondbox run coding-environment -- python3 -c 'print("hello")'` checks on a
control plane rebuilt from `89afa63` completed in 1.234–1.391 seconds, with a
1.363-second median. The full current KVM/Btrfs scenario also passed every
group, including Snapshot restore and retention.

The provider-neutral split then showed that scheduler work was not the
unsaturated bottleneck. In a 30-arrival observer run, `placement` was 337/859 ms
p50/p95 and `placement_pickup` alone was 327/839 ms. Start-plan construction,
runner selection, and every ordered-write subphase rounded to 0–1 ms. The delay
grew with the number of completed Sandboxes because every healthy ready
Sandbox remained due and was reclaimed every 250 ms forever.

The retained correction schedules healthy ready Sandboxes at the earliest
durable idle or maximum-duration deadline. A useful-activity touch that moves
the idle deadline later may cause one harmless early wake at the old deadline;
the worker then reads the new durable activity time and reschedules it. A ready
Sandbox with an active session polls so session closure is still discovered,
and terminal guest liveness now moves `next_reconcile_at` to the observation
time in the same database statement. Runner terminal evidence and public
desired-state writes already perform the same wakeup.

On the same 30-arrival fixed workload, all arrivals completed with no refusal
or failure:

| Ready-deadline candidate | p50 | p95 | p99 |
|---|---:|---:|---:|
| `create_to_ready` | **692 ms** | **800 ms** | 821 ms |
| `pre_assignment` | **225 ms** | **315 ms** | 346 ms |
| `placement` | **27 ms** | **54 ms** | 62 ms |
| `placement_pickup` | **12 ms** | **29 ms** | 30 ms |
| `placement_reconcile` | 8 ms | 34 ms | 36 ms |
| `workspace_provision` | 204 ms | 264 ms | 310 ms |
| `runner_admission` | 34 ms | 58 ms | 71 ms |
| `runner_boot` | 416 ms | 445 ms | 447 ms |
| `client_visibility` | 21 ms | 44 ms | 54 ms |

Against the attribution observer run, end-to-end p50/p95 fell from
1,153/1,704 ms and placement pickup fell from 327/839 ms. A ten-rung burst-32
qualification completed 320/320 arrivals with no refusal, failure, shed, or
shutdown while absorbing 21 PostgreSQL serialization failures. The complete
qualified KVM/Btrfs suite also passed, including useful-activity deadline
extension, restart, runner loss, generation fencing, Snapshot restore, and
retention.

Do not optimize stream sending, pool acquisition, decoding, start-plan lookup,
or the scheduler's unsaturated row work: they remain 0–1 ms. Do not recover
claim-query savings by moving the connection row back into placement. The next
safe target is burst pickup: one lifecycle worker serially consumes a cohort of
32 simultaneously due Sandboxes, leaving `placement_pickup` at 1.5–2.1 seconds
p50 across the ten rungs even though the per-placement reconciliation is only
24–45 ms p50. Any worker-concurrency or batched-claim candidate must preserve
SKIP LOCKED claims, Sandbox/Workspace lock order, generation fencing, and the
race-safe owner-side Assignment claim. Re-run at least 30 unsaturated arrivals,
the repeated burst ladder, and the full scenario after each retained candidate,
and require `runner_admission` p95 at or below 25 ms before marking the admission
gate complete.

Additional machine-readable evidence:

- `.tmp/lifecycle-claim-prepared-c1-30-result.json`
- `.tmp/lifecycle-claim-attributed-exec-c1-30-result.json`
- `.tmp/lifecycle-claim-idle-read-c1-30-result.json`
- `.tmp/lifecycle-relay-claim-c1-30-result.json`
- `.tmp/lifecycle-eager-assignment-c1-30-result.json`
- `.tmp/lifecycle-post-race-fix-c1-30-result.json`
- `.tmp/lifecycle-placement-attribution-c1-30-result.json`
- `.tmp/lifecycle-ready-deadline-c1-30-result.json`
- `.tmp/lifecycle-ready-deadline-burst32x10-result.json`

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
- two independent ten-rung burst-32 race qualifications
- one ten-rung burst-32 ready-deadline qualification
- ready deadline, active-session polling, transitional polling, and terminal
  guest-liveness wakeup tests
- `git diff --check`
