---
title: Transactional Sandbox Ready Projection
date: 2026-07-31
status: implemented
owner: SecondStack
provenance: Qualified lifecycle evidence on the dirty candidate based on c1cfcaf
---

# Plan: Transactional Sandbox Ready Projection

## Outcome

Remove the lifecycle-worker claim and action transactions from the successful
Sandbox startup critical path without weakening desired-state reconciliation.
The runner's fenced READY result now projects the Sandbox and its create or
start Operation ready in the same PostgreSQL transaction that records the
Assignment, Instance, Workspace-release, and command-acknowledgement facts.

The previous path committed READY on the Assignment and Instance, moved the
Sandbox deadline to now, emitted a PostgreSQL notification, and then waited for
the global lifecycle worker to claim the Sandbox and commit `mark_ready` in a
second transaction. That durable two-transaction path measured 100/300 ms
p50/p95 when unsaturated and 495/621 ms in a burst of 32.

## Selected design

Runner evidence processing already holds the invariant Sandbox, Workspace, and
Assignment locks and validates runner identity, generation, Instance,
Assignment, fencing token, and operation correlation. A successful READY result
continues to update the Assignment and Instance and release the exact durable
Workspace start mutation. It then takes one guarded provider-neutral step:

- only a Sandbox whose locked state is `starting`, desired state is `running`,
  generation and current Instance match the fence may be projected `ready`;
- that update establishes `lifecycle_action=mark_ready`, initial useful
  activity, a cleared stale reconcile claim, a new revision, and an immediate
  next reconciliation deadline;
- the same transaction records `ready_projected` and completes every pending
  create or start Operation for the Sandbox;
- if the guard does not match, the result records Instance readiness but only
  wakes normal lifecycle reconciliation.

The operation projection is one shared PostgreSQL primitive used by both the
runner fast path and the existing lifecycle `mark_ready` action. The latter
remains the restart-safe recovery path. The immediate deadline emits the normal
after-commit notification so lifecycle policy, idle timeout, maximum duration,
stop, and delete work still run through the authoritative reconciler.

## Correctness boundaries

- READY projection grants no new runner authority. It is downstream of the
  existing assignment fence, correlation, backend-evidence, and durable
  Workspace-start-mutation checks.
- Assignment, Instance, Workspace, Sandbox, operation milestone, Operation,
  and command acknowledgement either commit together or all roll back.
- A concurrent stop or delete intent cannot be reported ready by the fast path.
  Its desired-state change conflicts on the same Sandbox row lock; after either
  ordering, the guard fails or the later intent schedules lifecycle drain.
- An in-flight lifecycle claim is invalidated by clearing its owner and
  incrementing the Sandbox revision. Its later compare-and-swap action fails
  with the existing revision conflict.
- Public contracts remain provider-neutral. Runner identity, fencing tokens,
  backend references, PostgreSQL details, and host resources remain private.

## Alternatives rejected

- Unconditionally projecting every READY result would incorrectly complete a
  create or start Operation after a stop or delete intent won the row lock.
- A database trigger would hide lifecycle policy inside a side effect of the
  Instance update and make the guard harder to review and test.
- A process-local callback would not coordinate multiple control-plane replicas
  and would create a second, non-durable wakeup path.
- Shorter polling or notification tuning cannot remove the claim and action
  transactions or their burst serialization.

## Qualified result

Both comparisons used KVM, Btrfs Workspaces, the signed qualified artifact
bundle, zero refusals or failures, and the same workload configuration. Host
variance moved placement and guest negotiation independently, so the
`ready_projection` span is the direct attribution for this change.

| Workload | Metric | Receipt-pipelining candidate | Ready-projection candidate |
|---|---|---:|---:|
| 30 fixed arrivals, max in-flight 1 | `ready_projection` p50/p95 | 100/300 ms | **0/5 ms** |
| 30 fixed arrivals, max in-flight 1 | `ready_event_ingest` p50/p95 | 8/16 ms | 0/13 ms |
| 30 fixed arrivals, max in-flight 1 | `create_to_ready` p50/p95 | 810/1,190 ms | **752/954 ms** |
| Burst 32 | `ready_projection` p50/p95 | 495/621 ms | **0/0 ms** |
| Burst 32 | `create_to_ready` p50/p95 | 2,489/2,878 ms | **2,070/2,639 ms** |
| Burst 32 | completion rate | 11.07/s | **11.51/s** |

Machine-readable evidence:

- `.tmp/lifecycle-ready-fastpath-c1-30-result.json`
- `.tmp/lifecycle-ready-fastpath-burst32-result.json`

The reports identify source commit `c1cfcaf`; they qualified the dirty candidate
containing this implementation.

## Validation

- focused PostgreSQL tests for atomic ready projection and stop-intent deferral
- runner-control, lifecycle, and PostgreSQL store packages
- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- qualified concurrency-1 and burst-32 lifecycle runs
- `just test-scenario` on the qualified KVM/Btrfs host
- `git diff --check`
