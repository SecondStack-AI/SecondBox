# Assignment dispatch stalls behind a Workspace stuck in `creating`

Found by the first capacity ladder run against a local qualified deployment
(`.secondbox/stress/results/lifecycle-20260801T172946Z-683951`). This describes
the defect and the evidence; it does not propose a fix.

## Symptom

A burst of 32 concurrent `create_to_ready` arrivals stalls. The deployment stops
making progress entirely — no control-plane request completes, no runner control
event is persisted, no runner event is logged — until the clients abandon their
waits. Bursts of 8 and 16 against the same deployment complete normally.

## The gate

Two independent paths refuse to dispatch an `assignment` command while any
Workspace on the target runner is in state `creating`.

`internal/runnercontrol/commands.go:97-106`, in the claim query:

```sql
WHERE command.runner_id=$1
  AND command.state='pending'
  ...
  AND (
    command.kind<>'assignment'
    OR NOT EXISTS (
      SELECT 1 FROM secondbox.workspaces AS workspace
      WHERE workspace.home_runner_id=$1 AND workspace.state='creating'
    )
  )
```

`internal/scheduler/postgres.go:409-414`, in `lockEagerAssignmentDispatch`, has
the same condition.

The predicate is per **runner**, not per Sandbox. One Workspace row left in
`creating` therefore blocks every assignment for that runner. There is no
timeout, no claim expiry, and no other escape: the condition is re-evaluated on
every claim attempt and simply keeps failing.

## Evidence that the gate never reopened

From the run's diagnostics, all timestamps UTC:

| Observation | Value |
|---|---|
| Workspace reflinks published by the runner | **56 of 56** (8 + 16 + 32) |
| Last reflink published | 17:31:14.115 |
| Last Workspace mutation committed | 17:31:14.227 |
| Last microVM started | 17:31:15.071 — the **39th** of 56 |
| Log lines from any service, 17:31:15.3 → 17:32:12 | **0** |
| Sandbox wait expiries | 17:32:12, 17 × HTTP 408 at 60 000 ms |

The 408s are the API's per-request wait maximum (`control_plane_service.go:1006`
bounds one wait at 60 000 ms), not the driver giving up: `WaitSandbox` reissues
a wait after each expiry. The run ended because the refresh that follows an
expiry hit a connection reset, which was fatal at the time.

The runner published every Workspace it was asked for and then went idle. 17
assignment commands were never dispatched. The runner command poll interval in
this deployment is 100 ms (`scripts/scenario-compose.yml:120`), so the claim
query ran roughly 570 times during the silence and selected nothing each time.

This rules out a missed wakeup: the relay was scanning throughout. It also rules
out saturation, which degrades rather than stops — every boot stage of the
microVMs that did start was normal at both rung sizes:

| Stage | ≤16 concurrent, median/max ms | burst-32, median/max ms |
|---|---|---|
| `guest_protocol_negotiated` | 490 / 566 | 558 / 618 |
| `network_ready` | 12 / 29 | 22 / 35 |
| all others | ~0 | ~0 |

Runner-side admission phases were ~0 ms and local Workspace commands completed
in a median of 273 ms. Host memory never fell below 45 GB available.

The remaining explanation is that at least one `secondbox.workspaces` row stayed
in `creating` after the runner had published it, leaving the gate permanently
shut for that runner.

## Why concurrency changes the outcome

At 8 and 16 arrivals, Workspace creation and command insertion interleave, so the
gate is open at moments when commands are claimable. At 32 the runner is
configured to create up to 32 Workspaces at once
(`maxConcurrentWorkspaceCreates`), so the whole batch is in `creating`
simultaneously and every assignment inserted during that window is blocked. The
stall then persists past the batch because the durable state never caught up with
the runner.

## What to check next

- Whether any `secondbox.workspaces` row remains in `creating` after the runner
  reports the Workspace published — the transition from `creating` is the thing
  to trace.
- The three recent commits that changed this area: `1beb82a` (folds assignment
  claims into placement and adds the eager-dispatch path), `91e3d09`, `e947eb7`.
  `1beb82a` introduced the eager path that inserts a command directly in state
  `delivering`; a command left in `delivering` is also never re-notified, since
  `migrations/postgres/0006_eager_assignment_dispatch.sql` only notifies on
  transitions **to** `pending`.
- Whether the per-runner predicate should be per-Sandbox or per-Workspace
  instead, which would confine the blast radius of one slow Workspace.

## Reproducing

```sh
SECONDBOX_LIFECYCLE_CONFIG=$PWD/scripts/capacity-config.example.json \
  just test-lifecycle
```

The ladder's `burst-32` rung reproduces it. Reports are written even when a run
ends early, so the partial record and the diagnostics directory both survive.
