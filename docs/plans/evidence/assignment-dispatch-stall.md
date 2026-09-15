# A burst of placements could shut the control plane down

Found by the capacity ladder, reproduced deliberately, and fixed in `05bbd8e`.
This records the defect, the evidence, and what changed.

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

## The fix

Three changes, each independently enough to avoid the outcome.

**Contention is named.** `Schedule` now wraps an exhausted retry budget in
`ports.ErrSerializationContention` instead of returning the raw driver error. A
caller handed a `*pgconn.PgError` cannot tell "try again" from "this is broken",
and the reconciler assumed the latter.

**Both reconcilers defer on it** rather than failing, exactly as they already
defer on Workspace contention (`internal/lifecycle/worker.go`,
`internal/reconcile/worker.go`, and the loops in `cmd/secondboxd/main.go`).
Losing a race must not end a reconciler, because ending a reconciler stops the
server and takes every attached runner with it.

**Retries back off with full jitter** (`serializationBackoff`). The previous loop
retried immediately, so every loser of a race woke at the same instant and
collided again. Full jitter draws each retry independently from `[0, window]`,
spreading a colliding cohort instead of letting it resonate.

**And the contention itself is gone from placement.** Placement no longer assigns
the control-stream sequence and no longer touches `runner_connections`. The
command is queued `pending`; the connection owner assigns the sequence when it
claims, which it already did. This reverses the round trip `1beb82a` saved — a
round trip is cheaper than a single-row lock inside every placement.

The owner-side lock in `ClaimCommands` stays. `pumpOutboundFrames`
(`internal/runnercontrol/server.go:423`) is its only caller and runs one
goroutine per connection, so that lock has exactly one writer by construction: it
is uncontended, and it keeps the single-owner invariant enforced rather than
assumed. Moving that counter into process memory would trade a free lock for
state that must be invalidated exactly when a connection is superseded.

### Before and after, same ten-rung reproduction

| | Before | After |
|---|---|---|
| Rungs completed | 1 of 10 | **10 of 10** |
| Arrivals admitted | 32 of 320 | **320 of 320** |
| Rung 2 arrivals admitted | 0 of 32 | 32 of 32 |
| Control-plane shutdowns | 1 | **0** |
| SQLSTATE 40001 occurrences | escalated to fatal | **24, all absorbed** |
| Run outcome | rail-aborted | `SecondBox lifecycle qualification passed` |

Per-rung after the fix, `create_to_ready`, 32 concurrent each:

| Rung | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 |
|---|---|---|---|---|---|---|---|---|---|---|
| p50 ms | 6064 | 5556 | 3695 | 3670 | 3764 | 3578 | 3785 | 4302 | 3560 | 3914 |
| p95 ms | 8058 | 7243 | 4713 | 4761 | 4911 | 4701 | 4871 | 5632 | 4770 | 5016 |

Latency settles after the first two rungs rather than degrading, which is what a
deployment absorbing repeated bursts should look like. Nothing was refused,
nothing failed, nothing was shed, and the gate reported no violations.

### Confirmed by a second independent run

The defect was intermittent, so one clean run is weaker evidence than it looks.
A second run of the same ten rungs also completed: 320 of 320 arrivals admitted,
zero shutdowns, 26 serialization failures absorbed, `qualification passed`.

| Rung | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 |
|---|---|---|---|---|---|---|---|---|---|---|
| p50 ms | 3859 | 3531 | 3644 | 3632 | 3743 | 3491 | 3867 | 4057 | 3551 | 4094 |
| p95 ms | 4871 | 4621 | 4794 | 4549 | 4558 | 4702 | 4949 | 5126 | 4555 | 5284 |

Twenty consecutive 32-arrival bursts across two runs, 640 arrivals, no stall.
Latency is also flatter than the first run, which began at 6064/8058 ms before
settling: with the connection-row lock gone from placement, the first rung no
longer pays for contention that the rest of the run had to absorb.

Serialization failures still occur after the fix, and should: they are inherent
to serializable isolation under concurrency. The change is that they are now
retried and reported as contention rather than ending the process.

## Latency consequence and next target

Removing the connection-row lock restores the owner-side Assignment claim, so
the eager candidate's 0–1 ms read-only claim no longer describes the retained
system. A fresh qualified run on clean source commit `89afa63` used 30 fixed
arrivals at 0.25/s and maximum in-flight 1. All 30 completed without refusal or
failure:

| Span | p50 | p95 | p99 |
|---|---:|---:|---:|
| `create_to_ready` | 1,144 ms | 1,695 ms | 1,698 ms |
| `pre_assignment` | 662 ms | 1,152 ms | 1,178 ms |
| `placement` | 415 ms | 843 ms | 859 ms |
| `workspace_provision` | 235 ms | 307 ms | 391 ms |
| `runner_admission` | 39 ms | 66 ms | 74 ms |
| Assignment claim query | 25 ms | 48 ms | 52 ms |
| `runner_boot` | 433 ms | 479 ms | 488 ms |

Pool acquisition, command decoding, and stream send remained 0 ms. Five live
`secondbox run durable-coding -- python3 -c 'print("hello")'` executions on
a control plane rebuilt from that commit completed in 1.234–1.391 seconds,
with a 1.363-second median.

The next safe target was not the owner-side claim. Provider-neutral milestones
partitioned placement without adding a database round trip. A 30-arrival
observer run measured `placement` at 337/859 ms p50/p95 and lifecycle worker
pickup at 327/839 ms; start-plan lookup, runner selection, and ordered scheduler
writes were 0–1 ms.

Every healthy ready Sandbox had remained due and was reclaimed every 250 ms,
so the lifecycle queue grew permanently with the completed population. The
retained correction schedules a ready Sandbox at its actual idle or
maximum-duration deadline. Transitional states and active sessions keep the
bounded poll, while desired-state changes and terminal runner or guest evidence
move the Sandbox due immediately.

The same unsaturated workload then measured:

| Span | p50 | p95 | p99 |
|---|---:|---:|---:|
| `create_to_ready` | **692 ms** | **800 ms** | 821 ms |
| `pre_assignment` | **225 ms** | **315 ms** | 346 ms |
| `placement` | **27 ms** | **54 ms** | 62 ms |
| `placement_pickup` | **12 ms** | **29 ms** | 30 ms |
| `workspace_provision` | 204 ms | 264 ms | 310 ms |
| `runner_admission` | 34 ms | 58 ms | 71 ms |
| `runner_boot` | 416 ms | 445 ms | 447 ms |

A third ten-rung burst-32 qualification admitted 320/320 arrivals with no
failure, refusal, shed, or shutdown while absorbing 21 serialization failures.
Its end-to-end p50 ranged from 3,300 to 4,355 ms and p95 from 4,262 to
5,859 ms. The remaining burst placement pickup was 1.5–2.1 seconds p50 because
one lifecycle worker still consumed each simultaneously due cohort serially.

Independent-worker experiments were rejected: two workers raised burst p50 to
4.4–5.6 seconds with 134 serialization failures, while eight raised it to
5.6–6.7 seconds with 1,434. The serializable scheduler transaction became the
contention point.

The retained configuration atomically claims at most eight oldest-due
Sandboxes with `FOR UPDATE SKIP LOCKED`, then runs their effects sequentially.
The private claim fence does not mutate the public revision or `updated_at`.
The 30-arrival workload improved to 635/710 ms end to end and 192/223 ms
pre-assignment. A ten-rung burst-32 qualification completed 320/320 with no
failure, refusal, shed, or shutdown; mean pickup improved from 1,738/1,933 ms
to 1,156/1,342 ms p50/p95 and serialization failures fell from 21 to 14.

Keep `runner_connections` out of placement and preserve the owner-side command
claim, Sandbox/Workspace lock order, generation fence, and bounded sequential
effects. The next investigation belongs in runner-local Workspace provisioning
and guest negotiation, not additional control-plane worker concurrency.
