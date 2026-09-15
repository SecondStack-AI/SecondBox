---
title: Event-Driven Control-Plane Wakeups
date: 2026-07-30
status: implemented
owner: SecondStack
provenance: Qualified lifecycle evidence at d167010
---

# Plan: Event-Driven Control-Plane Wakeups

## Outcome

Remove polling phase from the unsaturated Sandbox startup path while preserving PostgreSQL as durable authority. The existing explicit poll intervals remain mandatory recovery and future-deadline bounds; transactional notifications become latency hints only.

The concurrency-1 baseline is 854/975 ms p50/p95 `create_to_ready`. Its 89/122 ms `runner_admission` and repeated 80–115 ms runner-command `queueMs` align with the configured 100 ms command poll. `pre_assignment` is 289/401 ms and crosses Workspace completion, lifecycle reconciliation, placement, and assignment delivery. `ready_projection` is 66/187 ms and crosses the assignment-result-to-lifecycle-reconciliation boundary.

Success for this pass is:

- `runner_admission` p95 at or below 25 ms without reducing the explicit recovery poll interval;
- machine-readable separation of Workspace provisioning, placement, startup dispatch, and ready projection;
- lower unsaturated `pre_assignment` and `ready_projection` with no loss of burst throughput;
- correctness across control-plane restart, runner reconnect, missed/coalesced notification, generation fencing, and Workspace durability.

## Selected design

An additive PostgreSQL migration emits one compact notification after a transaction makes due work visible:

- a pending runner command publishes `runner_command` keyed by runner ID;
- a due Sandbox publishes `lifecycle`;
- a due Assignment publishes `assignment`.

One dedicated PostgreSQL connection per control-plane process listens for those notifications and feeds a bounded, coalescing in-process hub. Lifecycle and Assignment workers subscribe before their claim loops. Each runner stream subscribes by runner ID and drains immediately once on connection, closing the race where a command committed before subscription. A notification only causes the existing fenced claim query to run; it carries no state and grants no authority.

Consumers drain until the authoritative query reports no work. Buffered wakeups coalesce safely. A command burst larger than the delivery batch continues immediately until empty rather than waiting for another notification. The existing timers still wake every configured poll interval, so notification loss, listener restart, future `next_reconcile_at` deadlines, and PostgreSQL failover do not strand work.

New Workspace-backed Sandboxes are not due until their local Workspace result
arrives, and new Assignments are first due at their operation deadline. Their
inserts therefore do not produce speculative worker work. A committed Workspace
result, Assignment failure, retry, deadline, or runner-loss transition moves the
corresponding durable deadline to now and emits the wakeup.

The notification listener is a required process component. Invalid payloads or listener failure stop the control plane explicitly; they are not logged and swallowed.

## Attribution

Add provider-neutral orchestration milestones to Operation timing:

- `durable_admission`
- `workspace_ready`
- `placement_ready`
- `startup_dispatched`
- `ready_projected`

Milestones are persisted in the transaction that establishes each fact. Timing responses expose ordered milestones with elapsed and cumulative milliseconds. The lifecycle benchmark derives these spans:

- `workspace_provision`
- `placement`
- `startup_dispatch`
- `ready_projection`

Backend references, runner credentials, host paths, PostgreSQL channel names, and command IDs remain absent from public timing contracts.

## Alternatives rejected

- Process-local callbacks fail when the writer and runner stream are owned by different control-plane replicas.
- Short polling intervals create constant database load, preserve phase-dependent tails, and turn a recovery setting into the normal delivery path.
- Notifications carrying commands or lifecycle state would create a second, lossy authority and violate durable reconciliation.

## Validation

- Migration lineage, notification transaction visibility, payload validation, coalescing, subscription cancellation, immediate drain, burst drain, and fallback-poll tests.
- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- focused runner-control race tests
- qualified concurrency-1 lifecycle gate
- qualified burst-32 lifecycle comparison
- `just test-scenario` with real KVM, TUN/TAP, Btrfs Workspaces, and required qualification settings

## Result

The final code candidate is `d167010`. PostgreSQL remains authoritative and
the configured polling intervals remain recovery bounds. Normal startup work
now wakes after commit, command claims and durable delivery updates are
batched, and milestone writes share the SQL statements that establish their
facts. Assignment delivery also preserves the runner's existing global
Workspace-create barrier without blocking later Workspace commands on the
stream.

Qualified concurrency-1 results:

| metric | baseline p50/p95 | final p50/p95 |
|---|---:|---:|
| `create_to_ready` | 854/975 ms | 701/1,027 ms |
| `pre_assignment` | 289/401 ms | 185/383 ms |
| `runner_admission` | 89/122 ms | 28/55 ms |
| `ready_projection` | 66/187 ms | 25/161 ms |

The final ten-sample run contained one control-plane tail outlier. The
immediately preceding candidate, before the burst-only Workspace barrier
change, measured 640/692 ms `create_to_ready` and 28/49 ms
`runner_admission`. Across the final candidate family, the uncontended median
improved materially while the small-sample p95 remained variable.

Qualified burst-32 results:

| metric | baseline | final |
|---|---:|---:|
| `create_to_ready` p50 | 3,130 ms | 3,020 ms |
| `create_to_ready` p95 | 3,366 ms | 3,463 ms |
| completion rate | 9.48/s | 9.07/s |
| refusals / failures | 0 / 0 | 0 / 0 |

The burst median improved; p95 and throughput moved by 2.9% and 4.3%
respectively, within the observed run-to-run host variance. Delivering
Assignments while unrelated Workspace creates were active was explicitly
tested and rejected: it regressed the burst to 4,848/5,679 ms and 5.56/s, so
that change was reverted.

The 25 ms `runner_admission` p95 gate was not met. The remaining 28/55 ms
final result is no longer poll-quantized: it is the commit notification,
authoritative command claim, and stream handoff. Snapshot-resume alone cannot
reach the 100–200 ms Sandbox target while the measured non-guest orchestration
remains at this level.

Validation passed:

- `just verify-generated`
- `SECONDBOX_TEST_DATABASE_URL=... just test`
- `just test-contract`
- `just test-compose`
- qualified concurrency-1 and burst-32 lifecycle runs
- unfiltered `just test-scenario` on KVM and Btrfs in 122.3 seconds

Machine-readable final evidence:

- `.tmp/lifecycle-control-wakeup-c1-v8-result.json`
- `.tmp/lifecycle-control-wakeup-burst32-v8-result.json`
