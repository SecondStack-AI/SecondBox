# Start-path control-plane contention, 2026-10-01

Measured on branch `perf/start-path-control-plane` from `2c4b5eb`, one KVM host
(32 threads), Btrfs on LUKS, a single Firecracker Runner, the signed v0.17.0
bundle, and the lifecycle benchmark (`start_to_ready`, steady 0.25/s and 0.5/s
for 30 arrivals each, and bursts of 32). Every run had zero refusals and zero
failures.

PostgreSQL commits on this host cost about 25 ms of fsync, so absolute
control-plane spans are inflated. The transferable figures are the lock and
commit counts per start, taken from `pg_stat_statements` deltas across the two
steady windows: equal arrivals over different durations separate per-start
cost from background polling.

## Finding

Every start took the Tenant and Subject quota ledgers `FOR UPDATE` in about
twenty transactions: admission, the lifecycle claim (for lock order only),
every 250 ms `wait` pass while the Instance booted, placement, start
completion, and each Runner Assignment event. Only admission changes quota
usage. The ledger row therefore serialized every start in a Tenant, commit
fsync included. In a 4.5 s burst-32 window, sessions spent 22.7 s cumulatively
waiting on that one row. A diagnostic run with `synchronous_commit=off` cut the
burst's pre-Assignment p50 from 2,594 ms to 354 ms without changing the lock
count, which confirms the model.

Placement also ran at SERIALIZABLE and locked every Runner in the pool. A
direct 64-placement burst against PostgreSQL aborted about nine attempts per
placement regardless of Runner count (concurrent updates of the Runner row and
predicate-lock pivots on the Workspace update). READ COMMITTED with only the
home Runner locked removed the aborts and cut that burst from 2.2–2.7 s to
about 0.8 s.

## Changes

1. A starting Sandbox with an Instance waits for Runner evidence, which already
   wakes it on readiness and on every failure, and is scheduled at its
   Assignment operation deadline instead of every poll interval.
2. Migration `0035` adds a partial index of live Sandboxes for quota usage. At
   103,300 retained rows the Tenant usage query fell from 18.6 ms to 0.76 ms.
3. Migration `0036` takes the quota ledgers in the Sandbox trigger only for a
   write that increases counted usage, and `rowlock.SandboxWorkspaceForTransition`
   takes them in Go only when the caller's transition can increase usage. A
   Sandbox locked without its ledgers is recorded for the transaction, and the
   trigger rejects any later increase to it. The lifecycle claim, lifecycle
   actions, placement of an admitted start, start completion, and Assignment
   events no longer take the ledgers; placement still takes them to restart a
   failed Sandbox.
4. The lifecycle audit event is written in the admission transaction, and
   placement releases the lifecycle claim in its own transaction.
5. The lifecycle worker runs a claimed cohort's effects concurrently, bounded by
   `SECONDBOX_LIFECYCLE_RECONCILE_BATCH_SIZE`.

## Results

| Run | Burst-32 start total p50/p95 | Burst pre-Assignment p50 | Ledger `FOR UPDATE` statements per start | Commits per start |
|---|---:|---:|---:|---:|
| Before (home-Runner lock and READ COMMITTED only) | 3,153/4,379 ms | 2,594 ms | 64 | 22 |
| Steps 1–3 | 1,845/2,658 ms | 1,019 ms | 7.9 | 13 |
| Steps 1–5, three bursts | 1,795–1,971 / 2,645–2,682 ms | 844–885 ms | 7.9 | — |
| Steps 1–4, sequential cohorts, three bursts | 1,842–2,288 / 2,810–3,216 ms | 879–1,076 ms | — | — |

Steady-state starts are bounded by guest boot (about 400–550 ms on this host)
and did not change measurably; snapshot resume is the lever there.

## What remains

- With one Runner, every placement still serializes on that Runner's row,
  because the reservation update holds its lock through the commit. A single
  Runner therefore admits roughly one placement per commit latency. Concurrent
  cohorts help across Runners, which this single-Runner benchmark cannot show.
- About nine durable Runner messages per start, mostly Assignment progress
  stages, each committed separately unless the 2 ms batch window coalesces
  them.
- The benchmark attributes admission-to-first-claim time to no span: the
  `lifecycle_pickup_*` stage sits between `durable_admission` and
  `placement_reconcile_started`, so `pre_assignment` exceeds the sum of the
  placement spans.
- An idle control plane runs about 75 commits per second of pollers, and
  `POST :wait` polls PostgreSQL every 10 ms per waiter.
