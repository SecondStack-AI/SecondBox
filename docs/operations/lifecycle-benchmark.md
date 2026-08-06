# Sandbox lifecycle benchmark

`just test-lifecycle` measures one Sandbox lifecycle transition at a time under
realistic arrival patterns. It deliberately does not measure workload executed
inside a Sandbox; that belongs to `just test-stress`.

The benchmark answers one question: **at what offered arrival rate does the
deployment stop keeping up, and where does the time go?**

## Why it is open loop

The benchmark computes its arrival schedule before the measurement window opens
and offers every arrival on that schedule regardless of how the deployment is
behaving. It never slows down because earlier cycles are still running.

This matters. A closed-loop driver — N workers each looping "request, wait,
repeat" — automatically offers *less* load when the system slows down. It is
self-limiting, so a queue can never be observed growing and saturation shows up
only as a latency number with no backlog behind it. Open-loop arrivals make the
backlog visible directly.

## Measurements

Configuration version 3 accepts four measurements. Every prerequisite is
established before the arrival window and every cleanup runs after it, so a
startup result never includes teardown and a teardown result never includes
startup.

| Measurement | Setup outside the window | Timed transition |
|---|---|---|
| `start_to_ready` | Create and stop one Sandbox per arrival | Start a stopped Sandbox that retains its Workspace. |
| `create_to_ready` | None | Create a new Sandbox and wait until it is ready. |
| `stop_to_stopped` | Create one ready Sandbox per arrival | Stop the ready Sandbox. |
| `delete_to_deleted` | Create one ready Sandbox per arrival | Delete the ready Sandbox and its Workspace. |

**`start_to_ready` is the primary measurement.** It is the ephemeral hot path:
an existing Sandbox resumed on demand. Every arrival owns a distinct Sandbox;
the driver never races transitions on one Sandbox and never recycles a Sandbox
inside the measurement window.

## Arrival patterns

Every pattern is explicitly configured; there are no defaults.

| Kind | Required settings | What it shows |
|---|---|---|
| `burst` | `count` | Absorption of a standing burst and the time to drain it. |
| `steady` | `arrivalsPerSecond`, `durationSeconds`, `distribution` | Sustained load — the slow-crawl case. |
| `ramp` | `startArrivalsPerSecond`, `endArrivalsPerSecond`, `durationSeconds` | Finds the knee empirically instead of guessing concurrency levels. |
| `sawtooth` | `count`, `quietSeconds`, `repeats` | Whether capacity actually returns between bursts. |

`steady` accepts `fixed` or `poisson`. Poisson reproduces bursty real-world
arrival that a fixed interval cannot, and requires an explicit non-zero
`poissonSeed` so a reported run can be replayed exactly.

## Resident population

`residentPopulations` lists standing populations of long-lived Sandboxes held
ready for the whole cell. Arrival latency is then measured against that
backdrop, which answers whether holding M Sandboxes changes start-to-ready for
the next one. A cell runs once per resident value, so `[0, 4]` doubles the run.
The runner block also requires `maxConcurrentStarts`; this transient start-work
limit is measured separately from `maxConcurrentGlobal`, the resident Instance
capacity. Qualify it with a representative burst: lowering it can reduce
per-Instance boot contention while still increasing end-to-end drain latency.
`maxConcurrentWorkspaceCreates` independently bounds generation-one Workspace
formatting; the create-to-ready benchmark exposes whether this pool is too
small or creates storage contention.

## Reading the output

The human-readable table leads with `start_to_ready`. Three columns matter most:

- `off/s` is present only for `steady` and `ramp`, whose configuration defines a
  real rate window. It is `null` in JSON and `-` in the table for `burst` and
  `sawtooth`; drain time must not be repackaged as an offered rate.
- `drain/s` is completed arrivals divided by the time from the first offer until
  the last completion. For a burst it describes burst absorption, not sustainable
  offered throughput.
- `outstanding` is the peak number of accepted arrivals whose measured
  transition has not completed.

The machine-readable JSON additionally carries an `occupancy` time series per
cell (`offsetMilliseconds`, `ready`, `outstandingArrivals`,
`offeredRatePerSecond`). The offered rate is exact for steady and ramp patterns
and `null` for burst and sawtooth. Plot it with outstanding arrivals to locate
the rate where a ramp begins accumulating work and to see whether the system
drains between bursts.

Refusals and failures are counted separately from latency and are never folded
into a percentile. A typed admission refusal (`home_runner_unavailable`,
`quota_exceeded`, `backpressure`, `limit_exceeded`, `execution_node_unavailable`)
means the deployment declined the arrival, which is a capacity observation, not a
slow request. `shedArrivals` counts arrivals the driver itself did not offer
because in-flight work was already at `maximumInFlight`; that is also saturation,
reported separately so it can never be mistaken for throughput.

No latency percentile is reported for a cell with zero successful samples.

## Startup attribution

Each successful arrival reads the Operation timing and accumulates the public
stage breakdown: `runner_admission`, `artifact_verify`, `workspace_attach`,
`network_setup`, `compute_launch`, `guest_negotiation`, and `ready`. The report
names the dominant stage. Comparing the dominant stage at low and high offered
rate shows whether the bottleneck moves under load.

Each startup cell also emits its own `startupSpans`; setup boots are excluded.
These spans overlap and must not be added:

| Span | Boundary |
|---|---|
| `operation_total` | Durable Operation creation to completion. |
| `workspace_provision` | Durable admission to the runner's committed Workspace-ready result. |
| `placement` | Workspace ready to the committed Instance placement and Assignment command. |
| `placement_pickup` | Workspace ready to the lifecycle worker beginning the placement reconciliation. |
| `placement_reconcile` | Lifecycle reconciliation start to effect execution. |
| `placement_plan` | Effect execution to the complete provider-neutral start plan. |
| `placement_handoff` | Complete start plan to scheduler entry. |
| `placement_retry` | Scheduler entry to the successful serializable attempt; includes failed attempts and retry backoff. |
| `placement_sandbox_lock` | Successful attempt start through the ordered Sandbox and Workspace lock. |
| `placement_assignment_check` | Sandbox and Workspace lock through the existing-Assignment and mutation checks. |
| `placement_candidate_lock` | Assignment checks through locking the eligible runner candidates. |
| `placement_candidate_select` | Locked candidates through provider-neutral runner selection. |
| `placement_prepare` | Selected runner through the committed ordered placement writes. |
| `startup_dispatch` | Placement ready to successful Assignment stream delivery. |
| `pre_assignment` | Durable admission to placement ready, derived directly from the persisted orchestration milestones. |
| `runner_boot` | Assignment creation through the runner's final startup observation. |
| `runner_event_ingest` | Runner observation to control-plane receipt for every startup stage. |
| `ready_event_ingest` | Runner `ready` observation to control-plane receipt. |
| `ready_projection` | Receipt of the runner's `ready` observation to the committed ready projection. |
| `client_visibility` | Durable Operation total to the benchmark observing the target state. |

The Operation timing contract supplies ordered provider-neutral milestones from
`durable_admission` through Workspace provisioning, lifecycle pickup, start-plan
construction, placement locks and writes, dispatch, and ready projection. The
placement milestones are persisted with the Assignment command's existing
ordered write batch, so measuring them adds no database round trip to startup.
PostgreSQL notifications only wake workers; these durable rows remain the timing
and work authority. Runner logs additionally record command queue-to-delivery
latency, per-event persistence latency, local Workspace command execution, and
Workspace format/fsync/publish time. Runner-private details do not enter public
schemas.

The lifecycle worker claims an explicitly configured bounded cohort of
oldest-due Sandboxes atomically, then processes its effects sequentially. This
amortizes claim cleanup and selection without multiplying concurrent
serializable scheduler transactions. `placement_pickup` includes time an item
waits behind earlier effects in its claimed cohort; `placement_reconcile`
starts when that specific item begins processing. Claim acquisition changes
only the private owner and expiry fence, so it does not create a public Sandbox
revision or activity update.

## Teardown attribution

Drain, stop, and delete Operations carry their own ordered milestones, written
by the transactions that establish each fact and read through the same
Operation timing contract:

| Milestone | Boundary |
|---|---|
| `durable_admission` | The lifecycle intent and its Operation row committed. |
| `lifecycle_pickup_notify` | The lifecycle worker first claimed the Sandbox after a PostgreSQL commit notification. |
| `lifecycle_pickup_deadline` | The lifecycle worker first claimed the Sandbox only after its bounded recovery poll interval elapsed. |
| `lifecycle_pickup_immediate` | The lifecycle worker claimed the Sandbox without waiting, draining work it was already processing. |
| `teardown_drain_committed` | The drain transition committed. |
| `teardown_fence_dispatched` | The stop effect committed and queued the Fence command. |
| `teardown_fence_acknowledged` | The runner's Fence result was persisted and the generation advance queued. |
| `teardown_generation_advanced` | The runner's Workspace generation-advance receipt was persisted. |
| `teardown_stop_committed` | The finish-stop transition committed and advanced the durable generation. |
| `teardown_workspace_delete_dispatched` | The delete effect committed and queued the Workspace delete command. |
| `teardown_finalized` | The Workspace delete receipt was persisted and the Sandbox became `deleted`. |

Exactly one pickup milestone exists per Operation, so its name is the wake
trigger of that Operation's first claim. Later hops are read from the intervals
between the teardown milestones.

`sandboxes_notify_due` only emits a lifecycle wakeup when a committed
`next_reconcile_at` is at or before the commit's own clock. Two teardown
transitions leave their successor decision immediately available and still
schedule it one recovery poll interval away, so no notification is emitted and
the successor waits:

- `teardown_drain_committed` to `teardown_fence_dispatched`
- `teardown_stop_committed` to `teardown_workspace_delete_dispatched`

At the default 250 ms interval a delete of a running Sandbox therefore pays
500 ms that no external work occupies. The runner-evidence transitions do move
the Sandbox due — `teardown_generation_advanced` is followed by a notified
pickup — so the gap is specific to the transitions the control plane commits
for itself. `tests/integration/teardown_attribution_test.go` asserts both the
complete milestone coverage and this wake behaviour.

## Running it

The benchmark reuses the qualified artifact bundle, trust anchor, and Workspace
root prepared by `just prepare-stress`, and fails loudly naming the exact missing
variable rather than skipping.

```sh
just prepare-stress   # once, builds and signs the local bundle

export SECONDBOX_LIFECYCLE_CONFIG=/absolute/path/to/lifecycle-config.json
export SECONDBOX_LIFECYCLE_OUTPUT=/absolute/path/to/result.json
export SECONDBOX_RUNNER_WORKSPACE_ROOT=/absolute/mode-0700/dir/on/xfs/or/btrfs
just test-lifecycle
```

The deployment also requires
`SECONDBOX_LIFECYCLE_RECONCILE_BATCH_SIZE`. The qualified scenario uses `8`;
it is a bounded claim size, not a worker-concurrency setting. Treat changes as
performance candidates and requalify both unsaturated and repeated-burst runs.

Omitting `SECONDBOX_LIFECYCLE_CONFIG` uses `scripts/lifecycle-config.example.json`.
Omitting the artifact, trust, and Workspace variables falls back to the prepared
`.secondbox/stress` state.

Every lifecycle run also writes a sibling diagnostics directory containing
`runner.jsonl` and timestamped control-plane/PostgreSQL/object-store Compose
logs. Set `SECONDBOX_SCENARIO_DIAGNOSTICS_DIR` to an explicit absent absolute
path to place it elsewhere. The runner log carries command delivery, event
persistence, local Workspace execution, and format/fsync/publish timing needed
for before/after optimization comparisons.

The host requires `/dev/kvm`, `/dev/net/tun`, and a Workspace root on XFS or
Btrfs. These are the same prerequisites as `just test-scenario`.

## Sizing a run

Total cells are `measurements × patterns × residentPopulations`.
`start_to_ready`, `stop_to_stopped`, and `delete_to_deleted` provision one
Sandbox per offered arrival before their window opens. This can be substantial,
but it ensures the measured transition is isolated and no Sandbox is reused.

Start small. Two measurements, two patterns, and one resident value is four
cells and validates the whole path quickly. Add measurements, patterns, and
resident values once the numbers look sane. Choose rate-bearing patterns that
straddle the knee: far below it every cell looks healthy, and far above it every
cell is shed.

## Capacity ladders

A ladder answers a different question from the rest of the benchmark: **how many
Sandboxes can this deployment absorb at once, and what stops it?**

It needs no new arrival pattern. `patterns` is a list and each entry becomes its
own cell with its own pool, report row and cleanup, so rising `burst` counts are
the ladder:

```sh
SECONDBOX_LIFECYCLE_CONFIG=/abs/path/capacity.json just test-lifecycle
```

Two configurations ship, because discovery and gating want opposite ones.

| | `capacity-config.example.json` | `capacity-gate-config.example.json` |
|---|---|---|
| Binding | The host | One configured limit |
| Every configured limit | Above the ladder | `subjectMaxActiveInstances` inside it |
| Expected signal | Distress knee | Refusal knee |
| `gate.mode` | `observe` | `enforce` |

The reason they cannot be one file: the clean refusal paths *are* the configured
limits. `quota_exceeded` and `home_runner_unavailable` are raised by the subject
and runner settings, so raising those above the ladder means the host binds
first and the deployment produces `startup_failed`, which is a failure rather
than a refusal. A host-bound run therefore cannot assert clean shedding, and a
run that can assert it is not measuring the host.

### Reading the result

The report names three knees, because they do not fire in a fixed order:

- **refusal knee** — the first rung refused, and the dominant refusal code
- **latency knee** — the first rung whose p95 reached `latencyKneeRatio` times
  the first rung's p95
- **distress knee** — the first rung that failed or tripped a rail

`configuredBinding` accompanies them. A refusal code names the *class* of
refusal, not the resource: `ErrQuotaExceeded` is one sentinel covering
Sandboxes, active instances, memory, CPU, Snapshots and concurrent Operations
alike, so naming the resource takes arithmetic over the configuration.

### `maximumInFlight` is not a safety limit

It is the driver's own shedding cap. An arrival offered past it is discarded
before the deployment ever sees it and counted as a saturation observation, so a
ladder whose rungs exceed it reports the driver's limit as though it were the
deployment's. Configuration now rejects that outright, comparing against peak
*simultaneous* arrivals — arrivals spread over time never coincide, so a steady
pattern offering more than the cap over a long window is legitimate.

Every cell reports its own `shedArrivals`; a capacity run should show zero.

### Host rails

Rails are a safety trip, never feedback. Tripping one ends the run; it never
reduces the load being offered, because modulating offered load in response to
strain is exactly what makes a benchmark closed loop.

| Rail | Trips when |
|---|---|
| `availableMemoryFloorMiB` | `MemAvailable` falls below the floor |
| `stepFailureCeiling` | genuine failures exceed the ceiling; refusals are excluded |
| `maximumWallClockSeconds` | the run exceeds its budget |

`abortedAtRail` names the rail in both the cell and the report, and
`incompleteReason` explains a run that ended early. **A rail never discards the
cells already measured** — a safety abort that destroyed its own record would
defeat its purpose.

Two limits shape what a ladder can ask for:

- **Rails act between cells.** A burst schedule sets every arrival offset to
  zero, so a 128-rung dispatches in microseconds against a sampler ticking in
  hundreds of milliseconds. A rail cannot shrink the cell it fires in.
- **Guest addresses cap a run at 253.** The scenario harness allocates one `/24`
  per run and three addresses are unavailable, so no ladder can exceed 253
  concurrent Sandboxes however much memory the host has.
- **Only `create_to_ready` ladders.** Every other measurement pre-creates one
  Sandbox per arrival *serially*, so a deep rung would spend the run
  provisioning rather than measuring.

Cleanup is serial too, one delete per Sandbox with a full operation wait, so a
six-rung ladder to 128 spends far longer tearing down than offering. Budget for
it, and note that `maximumWallClockSeconds` is evaluated between cells precisely
because the occupancy sampler covers only the arrival window.

### A measured baseline

One clean six-rung ladder, `create_to_ready`, no resident population, on a
32-thread AMD Ryzen AI MAX+ 395 with 125 GB RAM and a Btrfs Workspace root.
Every rung was fully admitted: no refusal, no failure, nothing shed, no rail
tripped.

| Concurrent creates | p50 ms | p95 ms | Host MiB available at low water |
|---|---|---|---|
| 8 | 1 514 | 1 988 | 45 330 |
| 16 | 3 016 | 3 959 | 44 761 |
| 20 | 3 821 | 4 701 | 44 114 |
| 24 | 4 131 | 5 224 | 43 828 |
| 28 | 4 634 | 5 772 | 43 889 |
| 32 | 5 827 | 7 490 | 42 196 |

Latency rises smoothly and roughly linearly — about 150 ms of p50 per additional
concurrent create beyond the first eight — with no knee in the usual sense. The
run reports a latency knee at 16 only because p95 doubles from the 8 baseline,
which the 1.5 ratio flags; that is the cost of concurrency, not a ceiling.

Nothing here bounded the deployment. The configured ceiling was 160 and memory
never dropped below 41 GB free, so this ladder measures how a healthy deployment
scales rather than where it stops. Finding the actual ceiling needs deeper rungs,
which the /24 guest network caps at 253.

Note that wall-clock is dominated by teardown, not measurement: each rung deletes
its Sandboxes one at a time with a full Operation wait, so the six rungs above
took roughly 25 minutes of which the measured windows were a small fraction.

### Reproducing the placement shutdown

`scripts/stall-repro-config.example.json` runs ten identical 32-arrival bursts
rather than a rising ladder, because the defect it targets is a race on one
database row and repetition finds it faster than depth. It reproduced on the
second rung where a six-rung ladder to 32 had passed cleanly.

See [assignment-dispatch-stall.md](assignment-dispatch-stall.md).
