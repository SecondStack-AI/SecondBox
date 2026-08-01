# Capacity Saturation Ladder for the Lifecycle Scenario Driver

## Overview

Add capacity discovery to the existing lifecycle scenario driver: run rising batches of
Sandbox arrivals against a real deployment, find the batch size where the deployment stops
absorbing the load, and name the resource that bound it.

The ladder itself needs **no new pattern kind**. `lifecycleConfig.Patterns` is already a list and
each pattern becomes its own cell, so a config of `burst-8, burst-16, ... burst-128` is the ladder.
What is missing is report preservation on abort, host safety rails, per-cell shed accounting,
cross-cell knee identification, and an opt-in gate.

### Two runs, two configurations

Review established that discovery and gating want **opposite** configurations, and one config
cannot serve both:

- **Discovery (host-bound).** Every configured limit above the ladder max so the machine binds.
  The expected signal is a **distress knee** — `startup_failed`, `deadline_exceeded` — because the
  clean refusal paths are the configured limits that have been raised out of range. Assertions
  observe only.
- **Gate (config-bound).** One configured limit deliberately *inside* the ladder range so the
  refusal path is genuinely exercised. This is the run that can assert clean shedding. Assertions
  enforce.

### Problem this solves

`configuredBinding` arithmetic over the current stress config yields a ceiling of **8 Sandboxes**
(runner memory budget 4096 MiB / 512 MiB). Nothing in the repository answers "what can this host
actually run, and what breaks first?" The lifecycle driver already measures saturation but never
pushes toward it and never asserts anything.

### Integration

Extends `tests/scenario/lifecycle` in place. Configuration version moves 2 → 3 with the new blocks
**required and explicit**, per the repository rule against implicit defaults (`AGENTS.md:15`).

## Context (from discovery)

**Files/components involved:**

- `tests/scenario/lifecycle/run.go` — `runCell` (arrival loop, occupancy sampler, shedding),
  `prepareMeasurementPool`, `releaseCellResources`, `releaseResident`, `buildCellResult`
- `tests/scenario/lifecycle/main.go` — mode dispatch, cell loop, report writing
- `tests/scenario/lifecycle/config.go` — `lifecycleConfig`, `validateLifecycleConfig`
- `tests/scenario/lifecycle/report.go` — `cellResult`, `lifecycleReport`
- `tests/scenario/lifecycle/driver.go` — `admissionCode`, `classifyLifecycleError`, `createSandbox`
- `tests/scenario/lifecycle/patterns.go` — arrival schedules
- `tests/scenario/stress/config.go` — `configuredBinding` (to be ported)
- `scripts/lifecycle-config.example.json`, `scripts/test-lifecycle.sh`
- `docs/operations/lifecycle-benchmark.md`

**Patterns found:**

- Each scenario driver is self-contained; only `tests/scenario/harness/client.go` is shared.
  `stress` and `lifecycle` each carry their own percentile math. Duplication over abstraction is
  the established convention.
- Config structs use `DisallowUnknownFields` and reject every absent value — no implicit defaults
  (`config.go:140-142`, `AGENTS.md:15`).
- Cleanup is `defer`-based in `runCell` via `context.WithoutCancel`.

### Verified constraints

All confirmed against source during plan review.

**The driver is deliberately open loop** (`patterns.go:10-14`). Rails must be a safety trip that
aborts, never feedback that modulates offered load.

**⚠️ Rails can only act between cells.** `buildArrivalSchedule` sets every burst offset to zero
(`patterns.go:53-56`), so `wait > 0` at `run.go:167` is false for every arrival and the
`ctx.Done()` select at `run.go:169-177` is never reached. A burst-128 dispatches in microseconds
against a 500 ms occupancy tick. An in-cell abort check is still worth adding at the top of the
loop body, but the plan does not pretend rails can shrink the cell they fire in.

**⚠️ Never cancel `ctx` to stop arrivals.** The same `ctx` parents every in-flight operation
(`run.go:199-201`, `driver.go:42-46`). `classifyLifecycleError` (`driver.go:447-452`) special-cases
only `DeadlineExceeded`; `Canceled` falls through to `("client_error", failure)`. Cancelling would
manufacture genuine failures and corrupt the distress knee. Use a separate `atomic.Bool`.

**⚠️ `maximumInFlight` is the driver's own shedding cap** (`run.go:179-184`), not a safety limit,
and the example sets it to **8**. Worse, `shedArrivals` is a single run-global atomic
(`driver.go:26`, surfaced once at `report.go:64`), so a ladder report cannot attribute a shed to a
step. Self-inflicted saturation is currently indistinguishable from real saturation.

**⚠️ Clean refusal and host binding are mutually exclusive.** `home_runner_unavailable`
(`internal/store/postgres_store.go:1295-1310`) and `quota_exceeded` (`:1486-1494`) are raised by
the configured runner and subject limits. Raise them above the ladder max and the host binds
instead, producing `startup_failed` (`internal/reconcile/postgres.go:486`) — a **failure**, not a
refusal. Hence two configurations.

**⚠️ `deadline_exceeded` is a failure** (`driver.go:448-450`). With a 128-burst against
`maxConcurrentStarts: 32`, deep queueing is the intent, so `operationTimeoutSeconds` must be raised
for capacity runs or queue timeouts land in `Failures` and trip the distress knee first.

**⚠️ Cleanup is serial and dominates wall clock.** `releaseCellResources` (`run.go:391-413`) and
`releaseResident` (`run.go:331-350`) delete one Sandbox at a time with a full `WaitOperation`. For
`create_to_ready` every created Sandbox lands in `resources` (`run.go:252`), so the ladder ends
with **344 sequential deletes**. Budget 60-90 minutes, not 30. `maximumWallClockSeconds` must
therefore be evaluated outside the occupancy window, which only covers the arrival phase.

**⚠️ Quota counts non-deleted Sandboxes** (`postgres_store.go:1468`, `state<>'deleted'`). Leaked
Sandboxes accumulate across the ladder against `subjectMaxSandboxes` and can manufacture a **false
refusal knee**.

**⚠️ Pool provisioning restricts the ladder to `create_to_ready`.** `prepareMeasurementPool`
(`run.go:355-389`) returns `nil` for `create_to_ready` but serially pre-creates `arrivals`
Sandboxes for every other measurement. Concurrent provisioning is out of scope.

**⚠️ Hard ceiling of 253 concurrent Sandboxes.** `scripts/test-scenario.sh:312` allocates a /24
from `198.18.0.0/15`; capacity is `2^(32-prefix)-3`. Widening the prefix is out of scope.

**⚠️ Host prerequisite.** The target host currently has 80 GB of 125 GB in use, swap exhausted,
44 GB available. At 512 MiB per Sandbox that is a ~85-Sandbox ceiling reached via the OOM killer
rather than any clean signal. Must be resolved before a meaningful run.

## Development Approach

- **testing approach**: Regular (code first, then tests) — chosen during planning
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - write unit tests for new and modified functions
  - add new test cases for new code paths, update existing cases when behaviour changes
  - cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- every task lists the existing test files it breaks; those are part of the task
- run tests after each change

## Testing Strategy

- **unit tests**: required for every task. `parseMeminfo`, rail evaluation, knee identification,
  and the assertion checks are pure functions over structs — table-driven, matching
  `patterns_test.go` style.
- **integration tests**: config round-trip through `readLifecycleConfig` with
  `DisallowUnknownFields`; rejection of unknown fields, of version 2, and of a `maximumInFlight`
  below the largest pattern.
- **e2e tests**: a short ladder (`burst-2, burst-4, burst-8`) against the real deployment via
  `just test-lifecycle`. No mocks, no fake runner — real APIs only, per `AGENTS.md`.
- unit/integration command: `just test`
- e2e command: `SECONDBOX_LIFECYCLE_CONFIG=<abs path> just test-lifecycle`

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- keep plan in sync with actual work done

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, documentation in this repository
- **Post-Completion** (no checkboxes): running the real ladder, freeing host memory, interpreting results

## Implementation Steps

### Task 1: Preserve the report when a run ends early

Foundational — every later task depends on a partial run still producing output. Today
`main.go:114-120` returns on the first `runCell` error and `writeLifecycleReport` (`main.go:132`)
is never reached, so one failed cleanup delete (`run.go:405-408`) discards the entire run.

**Files:**
- Modify: `tests/scenario/lifecycle/main.go`
- Modify: `tests/scenario/lifecycle/report.go`
- Create: `tests/scenario/lifecycle/main_partial_test.go`

- [ ] restructure the cell loop to accumulate `cellResult`s and record a per-cell error rather
      than returning immediately
- [ ] write the report before returning any run error, so completed cells always survive
- [ ] add `IncompleteReason string` to `lifecycleReport`, empty on a clean run
- [ ] return a descriptive error after the report is written, naming the cell that ended the run
- [ ] make `runCell`'s own cancel path (`run.go:169-177`) return the partial `cellResult` instead
      of `cellResult{}`
- [ ] write tests: error in cell 3 of 5 still writes cells 1-2 and populates `IncompleteReason`
- [ ] write tests: clean run leaves `IncompleteReason` empty and is otherwise unchanged
- [ ] run `just test` — must pass before Task 2

### Task 2: Per-cell shed accounting and `maximumInFlight` validation

Turns the single most dangerous trap from prose advice into a startup error and a visible number.

**Files:**
- Modify: `tests/scenario/lifecycle/run.go`
- Modify: `tests/scenario/lifecycle/report.go`
- Modify: `tests/scenario/lifecycle/config.go`
- Modify: `tests/scenario/lifecycle/run_test.go`
- Modify: `tests/scenario/lifecycle/config_test.go`

- [ ] add a per-cell shed counter alongside the run-global `driver.shedArrivals`
- [ ] add `ShedArrivals int64` to `cellResult` and populate it in `buildCellResult`
- [ ] convert `buildCellResult`'s parameter list (already 10 positional args at `run.go:415-426`)
      to a struct before adding more fields
- [ ] extend `validateLifecycleConfig` to reject a config whose `maximumInFlight` is below the
      largest pattern's `offeredCount()`; `buildArrivalSchedule` already runs in validate mode
      (`main.go:52-61`) so the schedule is available
- [ ] update `run_test.go:28-38` and `run_test.go:64-73` for the `buildCellResult` signature change
- [ ] write tests for per-cell shed attribution across two cells
- [ ] write tests for the validation (accepts equal, rejects below, unaffected by non-burst patterns)
- [ ] run `just test` — must pass before Task 3

### Task 3: Port `configuredBinding` so the binding resource can be named

`quota_exceeded` is a single sentinel (`internal/ports/ports.go:23`) mapped to one code with no
dimension (`internal/api/http.go:1228-1229`). The refusal code names the refusal *class*, not the
resource. Naming the resource needs the configured-limit arithmetic the stress driver already has.

**Files:**
- Create: `tests/scenario/lifecycle/binding.go`
- Create: `tests/scenario/lifecycle/binding_test.go`

- [ ] port `configuredBinding` and `minimumConfiguredLimit` from
      `tests/scenario/stress/config.go:318-363`, adapted to `lifecycleConfig` field names
- [ ] include the guest-IP capacity term, reading the CIDR from the existing runtime inputs
- [ ] duplicate rather than abstract, per the convention already set by the two drivers
- [ ] write table-driven tests covering each limit binding in turn
- [ ] write a test for the guest-IP term at /24 (expect 253)
- [ ] run `just test` — must pass before Task 4

### Task 4: Host rail configuration and threshold evaluation

**Files:**
- Create: `tests/scenario/lifecycle/rails.go`
- Create: `tests/scenario/lifecycle/rails_test.go`
- Modify: `tests/scenario/lifecycle/config.go`
- Modify: `tests/scenario/lifecycle/config_test.go`
- Modify: `scripts/lifecycle-config.example.json`

- [ ] add a **required** `hostRails` block with an explicit `enabled` boolean, plus
      `availableMemoryFloorMiB`, `stepFailureCeiling`, `maximumWallClockSeconds` — required and
      explicit rather than absent-means-off, per `AGENTS.md:15`
- [ ] drop `swapGrowthCeilingMiB`: on a host with swap already exhausted it is redundant with the
      memory floor, and it is two rails, two tests, and two docs for one signal
- [ ] add the required `latencyKneeRatio` here, not in the assertions block — the knee must be
      computable in observe-only mode
- [ ] split parsing from I/O: `parseMeminfo(io.Reader)` plus a thin file reader, so tests never
      touch `/proc`
- [ ] implement `railState.evaluate(...)` returning the tripped rail name or empty, first rail in
      a fixed order wins so results are deterministic
- [ ] bump the config version literal at `config.go:144-145` from 2 to 3 and update both messages
- [ ] update `scripts/lifecycle-config.example.json` to version 3 with `hostRails.enabled: false`
- [ ] update `config_test.go:16` (`validLifecycleConfig()` sets `Version: 2`) and any other fixture
- [ ] write tests for `parseMeminfo` (success, missing key, malformed value)
- [ ] write tests for `evaluate` (each rail alone, none, deterministic order when two trip)
- [ ] write tests for config validation (version 3 required, rails required, disabled rails valid)
- [ ] run `just test` and `just test-lifecycle` — both must pass before Task 5

### Task 5: Wire rails as a between-cell abort

**Files:**
- Modify: `tests/scenario/lifecycle/run.go`
- Modify: `tests/scenario/lifecycle/main.go`
- Modify: `tests/scenario/lifecycle/report.go`
- Create: `tests/scenario/lifecycle/rails_run_test.go`

- [ ] sample host memory on the existing occupancy sampler tick (`run.go:131-157`), recording the
      cell's `MemAvailable` low-water
- [ ] evaluate `maximumWallClockSeconds` in the cell loop in `main.go`, **not** on the occupancy
      tick — the sampler stops at `run.go:210`, before the serial cleanup that dominates the run
- [ ] use a dedicated `atomic.Bool` abort flag checked at the top of the arrival loop body; never
      cancel `ctx`, which would classify in-flight arrivals as `client_error` failures
- [ ] document plainly in the code that for zero-offset burst patterns the in-cell check cannot
      shrink the current cell — rails act between cells
- [ ] on trip, skip remaining cells and set `AbortedAtRail` on `cellResult` and `lifecycleReport`
- [ ] add `MemAvailableLowWaterMiB int64` to `cellResult`
- [ ] verify the existing defer chain (`run.go:104-121`) still releases residents and cell
      resources on the abort path; add a regression test asserting cleanup ran
- [ ] write tests: rail trip preserves prior cells (depends on Task 1), populates `AbortedAtRail`,
      skips later cells, and runs cleanup
- [ ] write tests: `hostRails.enabled: false` never aborts
- [ ] run `just test` — must pass before Task 6

### Task 6: Identify the knee across a ladder of cells

**Files:**
- Create: `tests/scenario/lifecycle/knee.go`
- Create: `tests/scenario/lifecycle/knee_test.go`
- Modify: `tests/scenario/lifecycle/report.go`
- Modify: `tests/scenario/lifecycle/main.go`

- [ ] `identifyLadder(results []cellResult)` selecting cells sharing measurement, resident
      population **and `PatternKind`** with strictly increasing `OfferedArrivals` (the offered
      count is the right ladder key; admitted count is an outcome)
- [ ] **refusal knee**: first cell with any `Refusals` entry, plus the dominant code and the
      binding name from Task 3's `configuredBinding`
- [ ] **latency knee**: first cell whose p95 is at or above the first cell's p95 times
      `latencyKneeRatio`; handle `Latency == nil` (zero successes, `run.go:464-467`) explicitly
- [ ] **distress knee**: first cell with any `Failures` entry or a non-empty `AbortedAtRail`
- [ ] add `capacitySummary` to `lifecycleReport`; retain all cells past the knee
- [ ] call `identifyLadder` from `main.go` before `writeLifecycleReport`
- [ ] extend the human-readable report with a capacity section
- [ ] write table-driven tests per knee (none, first step, last step, several at different steps)
- [ ] write tests for `Latency == nil` in the baseline cell and in a later cell
- [ ] write tests for ladder selection (non-monotonic excluded, mixed kinds and measurements separated)
- [ ] run `just test` — must pass before Task 7

### Task 7: Fix the orphan source in `createSandbox`

Pre-existing bug, in scope because the ladder amplifies it and leaked Sandboxes manufacture a false
refusal knee via `subjectMaxSandboxes`.

**Files:**
- Modify: `tests/scenario/lifecycle/driver.go`
- Modify: `tests/scenario/lifecycle/driver_test.go` (create if absent)

- [ ] `createSandbox` returns a nil handle when the follow-up `getSandbox` fails
      (`driver.go:178-185`) even though the Sandbox exists server-side, so `resources.add(nil)` at
      `run.go:252` is a no-op and it is never cleaned up — return the handle alongside the error
- [ ] ensure `resources.add` tolerates and skips nil defensively
- [ ] write tests covering create-succeeded-get-failed
- [ ] run `just test` — must pass before Task 8

### Task 8: Opt-in gate

**Files:**
- Create: `tests/scenario/lifecycle/assertions.go`
- Create: `tests/scenario/lifecycle/assertions_test.go`
- Modify: `tests/scenario/lifecycle/config.go`
- Modify: `tests/scenario/lifecycle/main.go`

- [ ] add a **required** `gate` block with explicit `mode: "observe" | "enforce"` and
      `declaredCeiling`; `observe` preserves instrument-only behaviour without an implicit default
- [ ] check (a): every cell at or below `declaredCeiling` has zero refusals and zero failures
- [ ] check (b): every cell above `declaredCeiling` has refusals present and typed, and zero
      genuine failures — **only meaningful in the config-bound gate configuration**; document that
      it is expected to be unmet in a host-bound discovery run, which is why that run uses `observe`
- [ ] check (c): **re-specified** — assert `ShedArrivals == 0` for every cell (proving the ladder
      measured the deployment, not `maximumInFlight`) and that peak outstanding arrivals never
      exceeded the offered count. The original "occupancy returns to resident baseline" is
      unobservable: the sampler is cancelled at `run.go:210` before cleanup, and `driver.readyCount`
      is run-global and drifts permanently on failed deletes (`driver.go:309-311`)
- [ ] check (d): **re-specified** — after cleanup, list Sandboxes via `GET /v1/sandboxes`
      (`sdk/go/secondboxclient/transport.go:77`, used by neither driver today) filtered by the
      qualification metadata and assert **zero** remain; `releaseResident` deletes residents too,
      so the expected count is zero, not the declared population
- [ ] wire into `main.go` after the report is written; in `enforce` mode return a descriptive error
      naming the failing check and cell
- [ ] write tests for each check passing and failing independently
- [ ] write a test proving `mode: "observe"` evaluates and reports but never errors
- [ ] run `just test` — must pass before Task 9

### Task 9: Discovery and gate example configurations

**Files:**
- Create: `scripts/capacity-config.example.json`
- Create: `scripts/capacity-gate-config.example.json`
- Create: `tests/scenario/lifecycle/config_examples_test.go`

- [ ] both files must be **full copies** of the example with overrides — `scripts/test-lifecycle.sh`
      `jq -er`-reads ~20 fields and fails before the driver runs if any is missing
- [ ] no change to `scripts/test-lifecycle.sh`: it defaults `SECONDBOX_LIFECYCLE_CONFIG`
      (`:24-26`) and is otherwise path-agnostic; the ladder runs as
      `SECONDBOX_LIFECYCLE_CONFIG=/abs/path just test-lifecycle`
- [ ] **discovery config**: `create_to_ready` only, patterns `burst-8` … `burst-128`,
      `residentPopulations: [0]`, `maximumInFlight: 160`, `gate.mode: "observe"`,
      `hostRails.enabled: true`
- [ ] discovery runner limits above 128 so the host binds: `memoryBudgetMiB 81920`,
      `maxConcurrentGlobal 160`, `maxConcurrentStarts 32`, `maxConcurrentWorkspaceCreates 32`,
      `maxConcurrentOperationsGlobal 640`
- [ ] discovery subject limits: `subjectMaxActiveInstances 160`, `subjectMaxSandboxes 400`
      (headroom against leaked non-deleted Sandboxes), `subjectMaxMemoryBytes 85899345920`,
      `subjectMaxCpuMillis 160000`
- [ ] raise `operationTimeoutSeconds` well above 180 for both configs so deep queueing does not
      surface as `deadline_exceeded` failures
- [ ] **gate config**: identical ladder, but `subjectMaxActiveInstances 48` — inside the ladder
      range — so the refusal path is exercised; `gate.mode: "enforce"`, `declaredCeiling: 48`
- [ ] write a config round-trip test loading all three example files through `readLifecycleConfig`
- [ ] write a test asserting unknown fields and version 2 are rejected
- [ ] run `just test` — must pass before Task 10

### Task 10: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented
- [ ] verify `hostRails.enabled: false` plus `gate.mode: "observe"` is **semantically** unchanged
      from today (not byte-for-byte — new report fields make that impossible)
- [ ] decide and apply report schema versioning: `lifecycleReport.SchemaVersion` is hardcoded 2 at
      `main.go:126`; either `omitempty` every new field and keep 2, or bump to 3
- [ ] verify edge cases: single-step ladder, ladder aborted at step 1, ladder with no knee, cell
      with zero successes
- [ ] run full test suite: `just test`; run `go vet ./...` and `go build ./...`
- [ ] run a short e2e ladder (`burst-2, burst-4, burst-8`) via `just test-lifecycle`
- [ ] confirm zero orphans after an intentionally rail-aborted run, via the Task 8 check (d) listing

### Task 11: [Final] Update documentation

- [ ] update `docs/operations/lifecycle-benchmark.md`: the ladder recipe, the two configurations and
      why they differ, the `maximumInFlight` trap, the `create_to_ready`-only constraint, the 253
      guest-IP ceiling, and how to read `abortedAtRail`
- [ ] update `docs/operations/lifecycle-benchmark.md:24` — it states "Configuration version 2"
- [ ] update `AGENTS.md` if new patterns discovered (note: this repository has no `CLAUDE.md`)
- [ ] move this plan to `docs/plans/completed/`

## Technical Details

### Ladder as configuration

No new pattern kind. The ladder is a list of `burst` patterns with increasing `count`. Each becomes
its own cell with its own pool, report row, and cleanup, so every step starts from the resident
baseline and measures absorption of a cold burst of N.

### Rail semantics

Rails abort between cells. Evaluation order is fixed and deterministic:
`availableMemoryFloorMiB`, `stepFailureCeiling`, `maximumWallClockSeconds`. The wall-clock rail is
evaluated in the `main.go` cell loop because the occupancy sampler covers only the arrival phase,
while serial cleanup dominates elapsed time.

### Sizing for the reference host

AMD Ryzen AI MAX+ 395, 32 threads, 125 GB RAM, btrfs with 597 GB free.

| Resource | Sandboxes at 512 MiB / 1 GiB workspace |
|---|---|
| Guest IPs (/24) | 253 — hard cap |
| RAM (leaving ~50 GB for host, containers, page cache) | ~140 — binds first |
| Workspace | ~590 |
| CPU (2 vCPU each) | 8× oversubscribed at 128; the boot storm is the point |

Ladder `8/16/32/64/96/128` tops out at ~64 GiB of guest RAM. **Budget 60-90 minutes**, dominated by
344 sequential deletes, not the 30 minutes originally estimated. `maxConcurrentStarts: 32` is
deliberate — at 8 the run measures a queue rather than a boot storm.

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Host preparation:**

- Free the 80 GB currently in use and reclaim swap before a real run. Until then the ladder finds
  the OOM killer at ~85 Sandboxes rather than any clean signal.

**Manual verification:**

- Run the discovery ladder and confirm the reported binding name from `configuredBinding` matches
  the limit expected from the config arithmetic.
- Run the gate ladder and confirm `quota_exceeded` appears at `subjectMaxActiveInstances 48`.
- Re-run with one runner limit deliberately lowered and confirm the binding name changes.

**Follow-on work (not in scope):**

- Concurrent pool provisioning in `prepareMeasurementPool`, which would let `start_to_ready`,
  `stop_to_stopped`, and `delete_to_deleted` ladder as deep as `create_to_ready`.
- Concurrent cleanup in `releaseCellResources`, which currently dominates wall clock.
- Wider guest CIDR allocation in `scripts/test-scenario.sh` to lift the 253 ceiling.
- A `ladder` pattern kind, if sustained cross-step pressure with in-cell recovery probes proves
  necessary beyond what a list of bursts gives.
- Adding a dimension to `ErrQuotaExceeded` (`internal/ports/ports.go:23`) so a refusal names the
  quota it hit without needing client-side arithmetic.
