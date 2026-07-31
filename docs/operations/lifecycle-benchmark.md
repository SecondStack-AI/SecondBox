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

Configuration version 2 accepts four measurements. Every prerequisite is
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
| `startup_dispatch` | Placement ready to successful Assignment stream delivery. |
| `pre_assignment` | Durable admission to placement ready, derived directly from the persisted orchestration milestones. |
| `runner_boot` | Assignment creation through the runner's final startup observation. |
| `runner_event_ingest` | Runner observation to control-plane receipt for every startup stage. |
| `ready_event_ingest` | Runner `ready` observation to control-plane receipt. |
| `ready_projection` | Receipt of the runner's `ready` observation to the committed ready projection. |
| `client_visibility` | Durable Operation total to the benchmark observing the target state. |

The Operation timing contract supplies the ordered provider-neutral milestones
`durable_admission`, `workspace_ready`, `placement_ready`,
`startup_dispatched`, and `ready_projected`. PostgreSQL notifications only wake
workers; these durable rows remain the timing and work authority. Runner logs
additionally record command queue-to-delivery latency, per-event persistence
latency, local Workspace command execution, and Workspace format/fsync/publish
time. Runner-private details do not enter public schemas.

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
