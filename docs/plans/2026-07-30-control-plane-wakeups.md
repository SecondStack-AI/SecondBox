---
title: Event-Driven Control-Plane Wakeups
date: 2026-07-30
status: implementing
owner: SecondStack
provenance: Qualified lifecycle evidence through 5d5f8ff
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
