---
title: External Blackbox Scenario Suite
date: 2026-07-29
status: completed
owner: SecondStack
provenance: SecondBox test-coverage gap analysis, 2026-07-29
---

# Plan: External Blackbox Scenario Suite

## Outcome

Introduce `just test-scenario`: a suite that drives a real deployment from outside the process boundary, over HTTP, with a real runner attached to real KVM, and proves that Sandboxes actually boot, execute, persist, snapshot, restore, and tear down.

SecondBox has three qualification gates today and none of them joins the control plane to real compute. `just test-compose` is genuinely external — it builds `secondboxd`, starts Compose, and exercises the Go and TypeScript SDKs over loopback HTTP — but `scripts/compose-test.yml` defines only `postgres` and `control-plane`, and `scripts/compose-test-init.sql` hand-inserts the `compose-live-pool` row so scheduling admits work that nothing ever performs. `just test-firecracker` boots a real microVM but calls `Manager.createAndStart` directly, bypassing the API, scheduler, reconciler, and runner protocol. `just test-multirunner` proves home-runner pinning and reflink isolation against fake runners and bare `WorkspaceStore` roots. No test proves that a `POST /v1/sandboxes` results in a guest that runs a command.

The suite closes that gap and nothing else. It reuses `sdk/go/secondboxclient` so every scenario doubles as SDK validation, and it reaches the system only through the 57 published operations — never through `psql`, never through an in-process handler, never through a runner-internal API.

## Fixed architecture

- The suite is external. It communicates with the deployment exclusively over the published HTTP API using an application credential, and over the administrative API using `SECONDBOX_PLATFORM_TOKEN`. Reading or writing PostgreSQL, calling a runner-internal entry point, or importing a control-plane package from a scenario test is prohibited.
- Bootstrap uses the public API. `createRunnerPool` and `createProfile` replace the hand-inserted fixture row in `scripts/compose-test-init.sql`. The runner enrolls itself by sending `RunnerRegistration` over the single `RunnerControl.Connect` stream, and the suite observes enrollment through `listRunners`. A scenario run that requires a pre-seeded database row has failed.
- The deployment under test is Compose. `scripts/scenario-compose.yml` composes `postgres`, `object-store`, `control-plane`, and one `secondbox-runner` derived from the `same-host-runner` service in `deploy/compose.yml`. The control plane keeps the runner-facing configuration `scripts/compose-test.yml` already carries: `SECONDBOX_RUNNER_LISTEN_ADDR`, the runner CA, the server certificate and key, and `SECONDBOX_RUNNER_CREDENTIAL`.
- Runner identity comes from `deploy/bin/bootstrap-runner-trust.sh`. The harness does not hand-roll the client certificate; the SPIFFE subject alternative name `spiffe://secondbox/runner/$SECONDBOX_RUNNER_ID` is minted by the same script operators use.
- The suite fails rather than skips. Following `scripts/test-multirunner.sh`, `SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1` makes every missing prerequisite a hard error naming the exact variable or device. Absent that variable the suite exits non-zero with the prerequisite list; it never reports success on a host that could not run it.
- Prerequisites are explicit and verified before Compose starts: `/dev/kvm` and `/dev/net/tun` present, the signed microVM artifact bundle materialized and verified per `docs/operations/microvm-image-pipeline.md`, and `SECONDBOX_RUNNER_WORKSPACE_ROOT` on an XFS or Btrfs mount confirmed by `findmnt`. The harness prints the `findmnt` line, the source commit, the Go version, and the resolved artifact manifest digest as run evidence.
- Waiting is the harness's job. `waitForSandbox` caps `deadlineMilliseconds` at 60000 and `SandboxHandle.Wait` rejects anything longer at `sdk/go/secondboxclient/sdk.go:168`. Every scenario uses one shared helper that issues repeated bounded waits against an outer deadline and fails with the last observed state, not a bare timeout.
- Scenarios are independent and self-cleaning. Each creates its own Sandbox, tears it down in `t.Cleanup`, and asserts on states and payloads rather than on wall-clock timing. A scenario must not depend on another scenario's residue.
- Snapshot semantics follow the runner-local copy-on-write model: restore is in-place, stopped-only, and confined to the Snapshot's own Sandbox. It advances the generation and invalidates stale authority. Restoring into a different Sandbox is not a scenario because it is not a supported operation.
- Home-runner authority is load-bearing. When the runner is lost, the correct observable outcome is the typed unavailable state and recovery when the same runner returns — never automatic relocation, never an empty replacement workspace. Explicit relocation requires an intact connected source.

## Non-goals

- Do not replace or fold in `just test-compose`, `just test-firecracker`, or `just test-multirunner`. Each keeps its distinct cost and prerequisite profile; this suite is the gate that requires a full KVM host and the 11 GB signed bundle.
- Do not run this suite on GitHub-hosted runners. They provide neither KVM nor the artifact bundle. It is a self-hosted or manually invoked gate, exactly as `just test-firecracker` is gated on `SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1` today.
- Do not add a second runner, cross-runner placement, or explicit relocation scenarios to this suite. Multi-runner behavior belongs to `just test-multirunner`.
- Do not introduce fakes, stubs, recorded fixtures, or a degraded mode that runs without KVM. A host that cannot boot a microVM cannot run this suite.
- Do not assert on host paths, storage references, runner identity, or backend vocabulary through public schemas. Public-schema leak assertions belong to the contract suite.
- Do not build the microVM image bundle as part of the suite. The bundle is an input, materialized and verified separately.

## Dependencies

This plan assumes the runner-local copy-on-write workspace migration in `docs/plans/2026-07-29-runner-local-cow-workspaces.md` has landed: checkpoints removed, Snapshot create/delete/restore as asynchronous Operations, and each Sandbox assigned to one authoritative home runner at a time. Tasks 6 and 8 assert that model directly. Tasks 1 through 5 do not depend on it and can proceed in parallel.

## Validation Commands

Run the focused commands listed in each task while implementing it. Before handoff, run all repository-wide gates below from the repository root.

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- `just test-deployment`
- `just preship`
- `git diff --check`
- `just test-scenario` on a qualified KVM host with a verified microVM bundle and an XFS or Btrfs workspace root

### Task 1: Establish the scenario harness and prerequisite gate

Build the deployment and the fail-loudly prerequisite check before writing any scenario. The harness is the deliverable of this task; a single trivial scenario proves it works end to end.

- [x] Add `scripts/scenario-compose.yml` composing `postgres`, `object-store`, `control-plane`, and `secondbox-runner`. Derive the runner service from `same-host-runner` in `deploy/compose.yml`, preserving `privileged`, `network_mode: host`, `cgroup: host`, the `/dev/kvm` and `/dev/net/tun` devices, the `rshared` workspace and spool mounts, and the `secondbox-runner -healthcheck` healthcheck.
- [x] Add the `object-store` and `object-store-init` services from `deploy/compose.yml`. The control plane is already configured against `http://127.0.0.1:9000` in `scripts/compose-test.yml` with no such service present, so Artifact and Snapshot paths would otherwise fail at the first request.
- [x] Carry the runner-facing control-plane configuration from `scripts/compose-test.yml` unchanged: `SECONDBOX_RUNNER_LISTEN_ADDR`, `SECONDBOX_RUNNER_CA_CERTIFICATE`, `SECONDBOX_RUNNER_SERVER_CERTIFICATE`, `SECONDBOX_RUNNER_SERVER_PRIVATE_KEY`, `SECONDBOX_RUNNER_CREDENTIAL`, and the heartbeat and protocol bounds.
- [x] Add `scripts/test-scenario.sh` modeled on `scripts/test-compose.sh`: verify prerequisite commands, build `secondboxd` and `secondbox-runner`, mint the CA and server certificate, `compose up --detach --wait`, export `SECONDBOX_LIVE_BASE_URL`, run the suite, and `trap cleanup EXIT` to dump `control-plane`, `secondbox-runner`, and `postgres` logs on failure before `compose down --volumes --remove-orphans`.
- [x] Mint the runner client identity by invoking `deploy/bin/bootstrap-runner-trust.sh` with `SECONDBOX_RUNNER_ID` and the generated CA. Do not duplicate its `openssl` sequence.
- [x] Implement the prerequisite gate. Require `/dev/kvm` and `/dev/net/tun`; require `SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR`, `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY`, and `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256`; require `SECONDBOX_RUNNER_WORKSPACE_ROOT` on an XFS or Btrfs mount verified by `findmnt`. Under `SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1` every missing prerequisite is a hard error naming the variable. Print the `findmnt` line, source commit, Go version, and artifact manifest digest as evidence.
- [x] Add `tests/scenario/` behind `//go:build scenario_live`, following the `sdk_live` tag convention, with a fixture that constructs a `secondboxclient.Client` from `SECONDBOX_LIVE_BASE_URL` and the platform token.
- [x] Add the bounded-wait helper that issues repeated `waitForSandbox` calls under an outer deadline and reports the last observed state on failure, because `SandboxHandle.Wait` rejects deadlines above 60 seconds.
- [x] Add `just test-scenario` invoking `scripts/test-scenario.sh`.
- [x] Prove the harness starts and stops cleanly with one scenario asserting `/healthz` and `/readyz`, and prove the failure path dumps runner logs by temporarily breaking a required runner variable.

### Task 2: Prove the runner enrolls itself through the control channel

Nothing today proves a runner can join a control plane it has never met. This is the first genuinely new coverage and it must not depend on any seeded database state.

- [x] Create the runner pool with `createRunnerPool` using the platform token, asserting the returned state, architectures, capabilities, and capacity policy.
- [x] Poll `listRunners` until the runner appears, then assert through `getRunner` that its reported capabilities include KVM, jailer, cgroup, network policy, storage, and cleanup readiness, and that `readinessFailures` is empty.
- [x] Assert the pool reports the enrolled runner as ready through `getRunnerPool`.
- [x] Prove the negative path: with the runner container stopped, a pool exists with no ready runner and `createSandbox` surfaces the typed no-capacity outcome rather than hanging until the assignment deadline.
- [x] Delete `scripts/compose-test-init.sql` from the scenario path and confirm no scenario issues SQL. Leave the existing `just test-compose` fixture untouched.
- [x] Run `just test-scenario`.

### Task 3: Prove a Sandbox boots to ready on real compute

This is the assertion the repository cannot currently make: that the full path from HTTP request to booted guest works.

- [x] Create a profile with `createProfile` pinned to the resources the qualified host provides, and assert the Sandbox pins the immutable profile revision resolved at creation.
- [x] Create a Sandbox, wait for `ready` through the bounded-wait helper, and assert the terminal state is `ready` rather than `failed`.
- [x] Assert `inspectSandbox` reports a live Instance and that `pingSandbox` succeeds against the running guest.
- [x] Assert the observable progression enters `creating` before `ready` and never reports `ready` before the guest answers.
- [x] Prove a profile whose requirements exceed the runner's advertised capacity is rejected with the typed error instead of being admitted and failing at launch.
- [x] Run `just test-scenario`.

### Task 4: Prove synchronous and streaming execution

Cover both execution paths, including the flow-control and cancellation semantics that only appear against a real guest.

- [x] Prove `executeSandboxCommand` returns stdout, stderr, and a zero exit code for a successful command, and a non-zero exit code with preserved output for a failing one.
- [x] Prove output exceeding the profile's maximum is truncated or rejected according to the published contract rather than silently dropped.
- [x] Prove `createSandboxExecStream` with the SDK's `ConnectExecStream` delivers interleaved stdout and stderr frames in order and terminates with the exit frame.
- [x] Prove stdin delivery through `SendInputFrame` and `CloseInput` reaches the guest process.
- [x] Prove `GrantOutput` flow control actually throttles: a guest producing more than the granted window blocks until further credit is issued.
- [x] Prove `cancelSandboxExecStream` terminates a long-running command and that the Sandbox remains `ready` and accepts a subsequent command.
- [x] Prove concurrent execution respects `SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX`.
- [x] Run `just test-scenario`.

### Task 5: Prove terminal sessions and filesystem and artifact data paths

Cover the remaining guest-facing data paths. Artifact coverage depends on the object store added in Task 1.

- [x] Prove `createSandboxTerminal` allocates a PTY, echoes typed input, and reports terminal dimensions to the guest.
- [x] Prove `reconnectSandboxTerminal` resumes an existing session and `cancelSandboxTerminal` ends it.
- [x] Prove the filesystem round trip: `writeSandboxFile` then `readSandboxFile` returns identical bytes, including a binary payload and a file at the `SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES` boundary.
- [x] Prove `sandboxFileExists`, `createSandboxDirectory`, `listSandboxDirectory`, and `removeSandboxPath` agree with what an executed command observes inside the guest.
- [x] Prove `uploadSandboxArtifact`, `getArtifact`, `downloadArtifactContent`, and `deleteArtifact` round trip through the object store with a matching digest.
- [x] Prove `listSandboxArtifacts` reflects upload and deletion.
- [x] Run `just test-scenario`.

### Task 6: Prove snapshot creation and in-place restore on reflink storage

The highest-value scenario. Workspace durability is the product claim with the least real-compute coverage, and the copy-on-write migration changes its implementation without changing its public contract.

- [x] Prove the full durability cycle: write a file, stop the Sandbox, create a Snapshot, start it, mutate the file, stop it, restore, start it, and assert the file holds its snapshotted contents.
- [x] Assert restore advances the Sandbox generation and that authority captured before the restore is rejected as stale.
- [x] Assert Snapshot create, delete, and restore each return an Operation that reaches a terminal state through `getOperation`, and that replaying each with the same `Idempotency-Key` converges rather than duplicating work.
- [x] Prove restore is stopped-only and same-Sandbox-only: a restore against a running Sandbox and a restore naming another Sandbox's Snapshot both fail with their typed errors.
- [x] Prove ordinary stop and start preserve workspace contents without an explicit Snapshot, which is the behavior that replaced checkpoints.
- [x] Prove `listSandboxSnapshots`, `getSnapshot`, and `deleteSnapshot` behave correctly, and that deleting a Snapshot does not disturb the active workspace.
- [x] Prove the profile's `snapshotLimit` and `snapshotRetentionSeconds` are enforced.
- [x] Run `just test-scenario`.

### Task 7: Prove port sessions, leases, and network policy

Cover the remaining public surfaces that require a live guest and a configured host network.

- [x] Prove `createSandboxPortSession` against a server started inside the guest carries a real HTTP request and response, and that `closeSandboxPortSession` stops it.
- [x] Prove `getSandboxPortSession` reports the session and that the profile's port-session quota is enforced.
- [x] Prove `acquireSandboxLease`, `renewSandboxLease`, `getSandboxLease`, and `releaseSandboxLease` behave correctly, and that a second acquisition is refused while a lease is held.
- [x] Prove generation headers from `GenerationHeaders` fence a request issued against a superseded generation.
- [x] Prove the profile network policy is enforced inside the guest: a denied destination fails and an allowed destination succeeds, exercising the `deny-all` and `allow-list` modes.
- [x] Prove `touchSandbox` extends idle expiry and that a Sandbox left idle past its profile's limit reaches the expected state.
- [x] Run `just test-scenario`.

### Task 8: Prove lifecycle transitions and home-runner unavailability

Prove the reconciler converges against real compute, including the failure mode that pinning makes correct.

- [x] Prove every ordinary transition through the public API: create, drain, stop, start, and delete, waiting on the published state at each step and asserting terminal states.
- [x] Prove `deleteSandbox` releases the runner's workspace, network, and capacity, verified through `getRunner` capacity returning to its pre-Sandbox value.
- [x] Prove control-plane restart mid-lifecycle converges: restart the container while a Sandbox is starting and assert it reaches a correct terminal state without operator action.
- [x] Prove runner loss produces the typed unavailable state rather than automatic relocation or an empty replacement workspace, by stopping the runner container while a Sandbox is `ready`.
- [x] Prove the same runner returning restores service and that the Sandbox's workspace contents survived, which is the observable guarantee home-runner pinning exists to provide.
- [x] Prove a runner killed mid-execution does not leave the Sandbox permanently wedged: the in-flight operation reaches a terminal state and the Sandbox is recoverable or explicitly failed.
- [x] Run `just test-scenario`.

### Task 9: Document the gate and wire it into CI

Make the suite reproducible by an operator who has never run it, and give it a place in CI that does not weaken the portable gate.

- [x] Add `docs/operations/scenario-qualification.md` following `docs/operations/multirunner-qualification.md`: every required variable, how to materialize and verify the microVM bundle, the KVM and reflink host requirements, the evidence a qualified run prints, and the explicit statement that a passing `just test-compose` is not evidence for this gate.
- [x] Add a CI job gated on a self-hosted KVM label that runs `just test-scenario` with `SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1`. Leave `just test-non-kvm` unchanged as the portable gate so pull requests from hosted runners keep passing.
- [x] Extend `tests/deployment/deployment_policy_test.go` to assert the scenario job exists and that it does not run on hosted runners.
- [x] Record expected wall-clock duration and the guest boot budget in the operations document so a regression in boot time is visible as a timing change rather than an opaque timeout.
- [x] Update `AGENTS.md` to list `just test-scenario` among the suites to run before handoff when a change touches the runner protocol, lifecycle reconciliation, or workspace durability.
- [x] Update `README.md` alongside the existing `just test-firecracker` guidance to describe the scenario gate and its host requirements.
- [x] Run `just verify-generated`, `just test`, `just test-contract`, `just test-compose`, `just test-deployment`, `just preship`, and `git diff --check` from a clean checkout, then `just test-scenario` on a qualified host.
