# A burst of placements can shut the control plane down

Found by the capacity ladder, then reproduced deliberately. This describes the
defect and the evidence; it does not propose a fix.

## Symptom

A burst of concurrent `create_to_ready` arrivals stops the deployment dead. No
control-plane request completes, no runner control event is persisted, no runner
event is logged. Clients hang until their deadlines. Nothing recovers.

It looks like saturation and is not: every microVM that started booted normally,
the runner was idle, host memory never fell below 41 GB, and no limit was
reached. The deployment did not slow down — it stopped.

## Root cause

The control plane **shuts itself down**. From the reproduction run's
control-plane log:

```
SecondBox coordinated server shutdown:
  SecondBox lifecycle reconciliation failed:
    SecondBox lifecycle effect failed:
      SecondBox scheduler eager Assignment connection lock:
        ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)
```

The chain:

1. `lockEagerAssignmentDispatch` (`internal/scheduler/postgres.go:394-435`,
   added by `1beb82a`) takes `FOR UPDATE OF connection` on the runner's row in
   `secondbox.runner_connections`, inside the placement transaction.
2. A runner has **one** connection row, so every concurrent placement for that
   runner contends on the same row. Placement transactions run at
   `pgx.Serializable` (`internal/scheduler/postgres.go:127`).
3. Contention produces SQLSTATE 40001. That is expected and retryable, and
   `scheduleOnce` is wrapped in a retry loop (`postgres.go:117`) bounded by
   `SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT`, which the scenario and
   deployment compose files set to **3**.
4. A burst of 32 placements exceeds three retries. The loop gives up and returns
   the error.
5. The error is not treated as a routine, expected condition. It propagates
   through the lifecycle effect and the reconciler, and reaching the reconciler
   triggers a **coordinated shutdown of the whole server**.

So a retryable database condition, arriving more often than a fixed retry budget
allows, takes down the control plane.

## Evidence

**Live state captured during the stall**, before teardown:

| Query | Result |
|---|---|
| `runner_connections` | `state = disconnected` |
| `workspaces` | `ready=32`, `deleted=32` — **none** in `creating` |
| `runner_commands` | every row `acknowledged` — none pending or delivering |
| `sandboxes` | `stopped=19` (desired `running`), `ready=13`, `deleted=32` |
| stopped sandboxes' `next_reconcile_at` | set, and **overdue by ~85 s** |

Nineteen Sandboxes were due for reconciliation and overdue, with no assignment
and nothing claiming them, because the process that would claim them had exited.

**Timeline, reproduction run:**

| Time | Event |
|---|---|
| 22:02:09.7 | last runner control event persisted |
| ~22:02:10 | reconciliation fails; coordinated shutdown begins |
| 22:02:10 → 22:03:25 | total silence from every service |
| 22:03:25.6 | `"SecondBox stopped"` logged, after four subsystems time out their graceful shutdown with `context deadline exceeded` |

**The first occurrence had the same cause.** The original ladder run's PostgreSQL
log carries the same errors at 17:31:14.48, 17:31:14.50 and 17:31:14.53 —
`could not serialize access due to concurrent update` — one second before its
last runner event at 17:31:15.07. Its control-plane log simply ends, because
diagnostics were collected before the shutdown finished.

## Reproducing

Ten identical 32-arrival bursts reproduce it on the **second** rung:

```sh
# ten rungs of burst-32, operationTimeoutSeconds 240
SECONDBOX_LIFECYCLE_CONFIG=/abs/path/stall-repro.json just test-lifecycle
```

| Rung | Offered | Completed | p95 |
|---|---|---|---|
| burst-32-01 | 32 | 32 | 4 985 ms |
| burst-32-02 | 32 | **0** | — |

Repetition matters more than depth: a six-rung ladder rising 8 → 32 completed
cleanly, while ten identical 32-bursts failed on the second. The trigger is
concurrent contention on one row, which is a race, not a threshold.

To catch it live, watch for the runner going quiet while the driver still runs,
then query `runner_connections.state` and the `sandboxes` reconcile columns
before the harness tears the deployment down.

## What to look at

- **The retry budget is a fixed count against unbounded contention.** Three
  retries cannot absorb 32 concurrent placements contending on one row, and the
  limit is a constant regardless of burst size.
- **Whether a serialization failure should reach the reconciler at all.** It is a
  routine outcome of `Serializable`, not a fault, and today it ends the process.
- **Whether the eager dispatch path needs that lock.** `1beb82a` folded assignment
  claims into placement to save a round trip; the cost is a single-row lock in
  the hot path of every placement for a given runner.
- Related commits: `91e3d09`, `e947eb7`.

## Corrections to an earlier version of this document

An earlier draft blamed the `NOT EXISTS (workspace ... state='creating')` guard
shared by `commands.go:105` and `scheduler/postgres.go:413`, reasoning that one
Workspace stuck in `creating` would block every assignment for that runner. That
was wrong. The live capture shows no Workspace in `creating` and no undispatched
command; the guard was never the obstacle. The hypothesis was built from reading
the code, and it survived until the state was actually measured.
