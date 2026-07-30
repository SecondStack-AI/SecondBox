# Sandbox lifecycle benchmark

`just test-lifecycle` measures how quickly Sandboxes start and stop under
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

## Cycles

Two lifecycle cycles are measured, and both edges of each are timed separately.

| Cycle | Edges measured | Meaning |
|---|---|---|
| `warm` | `start_to_ready`, `stop_to_stopped` | A stopped Sandbox that retains its Workspace is started and stopped again. |
| `cold` | `create_to_ready`, `delete_to_deleted` | A Sandbox is created from nothing and deleted. |

**The warm cycle is the primary measurement.** It is the ephemeral hot path: an
existing Sandbox resumed on demand. Warm Sandboxes are provisioned and stopped
before the window opens, so provisioning cost is never counted as arrival
latency. Each warm arrival checks a Sandbox out of an exclusive pool, so two
concurrent arrivals can never race start against stop on the same Sandbox.

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

## Reading the output

The human-readable table leads with the warm cycle. Two columns matter most:

- `off/s` versus `done/s` — offered against completed rate. When completed falls
  below offered, the deployment is not keeping up.
- `backlog` — peak in-flight cycles. A backlog that grows and does not recover
  is saturation.

The machine-readable JSON additionally carries an `occupancy` time series per
cell (`offsetMilliseconds`, `ready`, `inFlight`, `backlog`), which is the series
to plot when you want to see whether the system recovers between bursts.

Refusals and failures are counted separately from latency and are never folded
into a percentile. A typed admission refusal (`home_runner_unavailable`,
`quota_exceeded`, `backpressure`, `limit_exceeded`, `execution_node_unavailable`)
means the deployment declined the arrival, which is a capacity observation, not a
slow request. `shedArrivals` counts arrivals the driver itself did not offer
because in-flight work was already at `maximumInFlight`; that is also saturation,
reported separately so it can never be mistaken for throughput.

No latency percentile is reported for a cell with zero successful samples.

## Boot stage attribution

Each successful arrival reads the Operation timing and accumulates the public
stage breakdown: `runner_admission`, `artifact_verify`, `workspace_attach`,
`network_setup`, `compute_launch`, `guest_negotiation`, and `ready`. The report
names the dominant stage. Comparing the dominant stage at low and high offered
rate shows whether the bottleneck moves under load.

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

The host requires `/dev/kvm`, `/dev/net/tun`, and a Workspace root on XFS or
Btrfs. These are the same prerequisites as `just test-scenario`.

## Sizing a run

Total cells are `cycles × patterns × residentPopulations`. Warm cells also pay
pool provisioning before their window opens, which is roughly one cold create
plus one stop per pooled Sandbox.

Start small. Two patterns, one resident value, and both cycles is four cells and
validates the whole path quickly. Add patterns and resident values once the
numbers look sane. Choose arrival rates that straddle the knee: far below it
every cell looks healthy, and far above it every cell is shed.
