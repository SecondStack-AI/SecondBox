# Plans

## Open work

| Plan | Remaining scope |
| --- | --- |
| [Snapshot-resume startup](2026-07-30-snapshot-resume-startup.md) | Cache/trust, identity-isolation, recovery, and full end-to-end qualification; recorded creation p95 exceeds the 200 ms target. |
| [Runner admission attribution](2026-07-31-runner-admission-attribution.md) | Recorded admission p95 is 50 ms against the 25 ms gate. Implemented batching and rejected experiments remain documented. |
| [Guided single-host installation](2026-08-07-guided-single-host-install.md) | Outstanding Task 9 fault-injection and filesystem qualification evidence; the installer itself has shipped. |

## Archived implementations

`completed/` holds closed implementation efforts and their recorded outcomes.
An archived checklist is historical: unchecked experiments and stated limits
remain visible and are not proof of a passing gate. Use [current documentation](../README.md)
for supported behavior and commands.

| Plan | Disposition and evidence |
| --- | --- |
| [External scenario suite](completed/2026-07-29-blackbox-scenario-suite.md) | Completed; maintained by `scripts/test-scenario.sh` and `tests/scenario`. |
| [Runner-local Workspaces](completed/2026-07-29-runner-local-cow-workspaces.md) | Completed; current storage contract is in `docs/design/workspace-durability.md`. |
| [Control-plane wakeups](completed/2026-07-30-control-plane-wakeups.md) | Implemented; retains the measured result and recovery-poll design. |
| [Friendly Sandbox interface](completed/2026-07-30-friendly-sandbox-interface.md) | Implemented; extended and scenario-tested by target Sandbox shape. |
| [Direct Port transport](completed/2026-07-31-direct-port-data-plane.md) | Implemented and latency-qualified; SSH/VS Code qualification was not completed. |
| [Relay wakeups](completed/2026-07-31-relay-data-plane-wakeups.md) | Implemented; retains measured latency and measurement limits. |
| [Transactional ready projection](completed/2026-07-31-transactional-ready-projection.md) | Implemented; retains qualified timings and fencing boundaries. |
| [Workspace receipt pipelining](completed/2026-07-31-workspace-receipt-pipelining.md) | Implemented; retains measurement evidence. |
| [Workspace templates](completed/2026-07-31-workspace-template-provisioning.md) | Implemented; current template sizing also supports requested resources. |
| [Capacity ladder](completed/2026-08-01-capacity-ladder.md) | Previously completed; retains benchmark evidence. |
| [Deployment manifest](completed/2026-08-01-configuration-surface.md) | Completed; later configuration simplification supersedes the original field inventory. |
| [Relay retention](completed/2026-08-01-relay-retention-scope.md) | Implemented; retains replay and accounting evidence. |
| [CLI presentation](completed/2026-08-07-polished-cli-ui.md) | Implemented; the original aggregate handoff checkbox lacks a separate result. |
| [Microsandbox spike](completed/2026-08-13-microsandbox-backend-spike.md) | Merged in #96; dual-platform functional evidence retained, production macOS signing unqualified. |
| [Customer-shared tenancy](completed/2026-08-25-customer-shared-tenancy.md) | Completed and merged in #100; v0.6.0 clean-install boundary retained. |
| [gVisor spike](completed/2026-08-25-gvisor-backend-spike.md) | Merged in #99; distribution followed in #117; host and pod evidence retained. |
| [Attributed execution](completed/2026-09-10-attributed-command-execution.md) | Merged in #123 and corrected in #125; original unchecked experiments are not retroactively certified. |
| [Automated qualification and release](completed/2026-09-12-fast-qualification.md) | Completed; Task 5 supersedes the original full-matrix defaults. |
| [Target Sandbox shape](completed/2026-09-12-target-sandbox-shape.md) | Merged in #126; later qualification records cover the integrated CLI path; deferred features remain deferred. |
| [Release skill and worklog](completed/2026-09-15-release-skill-and-worklog.md) | Completed and merged in #145; release recovery, note preservation, and project skills. |

## Evidence and removed material

[evidence/](evidence/) retains source-bound measurements and backend qualification
records, including the [fixed placement-shutdown incident](evidence/assignment-dispatch-stall.md).
Paths under `.tmp/` and named host worktrees in these records identify original
run artifacts; their continued availability is not guaranteed.

The 2026-08-03 coordinated-release proposal was removed because it was explicitly
superseded on 2026-08-04 and prescribed an abandoned attestation/final-index flow.
Its history remains in Git. The maintained replacement is
[release distribution](../operations/release-distribution.md).

New plans belong here while work remains open. On closeout, record the implemented
outcome, evidence, and any deferred scope, move the plan into `completed/`, and
update its links and this index. Preserve unique qualification evidence; delete
superseded instructions when current documentation already covers the useful contract.
