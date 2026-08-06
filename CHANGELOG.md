# Changelog

## 0.2.3 - 2026-08-06

Repaired the colliding migration prefix that stopped v0.2.0 and v0.2.1 deployments from starting on v0.2.2, and restored TypeScript SDK terminal replay parity.

Upgrade directly from v0.2.0 or v0.2.1 to v0.2.3 and skip v0.2.2. Deployments already running v0.2.2 are repaired automatically on first start.

### Fixed

- Repaired the schema lineage collision that made v0.2.2 unreachable from v0.2.0 and v0.2.1: v0.2.2 shipped the lifecycle fence retag as `0010_lifecycle_fence_command_kind`, which sorts before the `0010_lifecycle_hot_path_indexes` already shipped in v0.2.0, so an upgraded ledger failed positional prefix validation and the control plane refused to start. The migration is renamed to `0013_lifecycle_fence_command_kind` with byte-identical content, and a checksum-verified ledger repair renames a row recorded under the v0.2.2 name in place, preserving `applied_at` and re-executing nothing. A row whose checksum is not the embedded fence content is refused, a ledger that recorded the fence ahead of the migrations preceding it is refused with an instruction to run the recording release to completion, and every 4-digit numeric prefix must now be unique across the embedded lineage so a colliding prefix cannot embed again ([#49](https://github.com/SecondStack-AI/SecondBox/pull/49)).
- Restored terminal replay-resume parity in the TypeScript SDK: `connectTerminalAfter` threads the replay cursor through the connector descriptor, sends the `SecondBox-Terminal-After-Sequence` header, resumes output sequencing at `afterSequence + 1`, and surfaces a rejected WebSocket handshake — including `terminal_replay_evicted` — as the same typed Problem error the Go SDK returns. The SDK parity contract test now asserts the capability in both SDKs ([#48](https://github.com/SecondStack-AI/SecondBox/pull/48)).
- Stopped the TypeScript `request` path from discarding the HTTP status when an error body is not a Problem document: the transport error carrying the status and a bounded body is surfaced instead of a `SyntaxError` ([#48](https://github.com/SecondStack-AI/SecondBox/pull/48)).
- Made `scripts/test-firecracker.sh` export `SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1` so an unqualified machine fails loudly instead of skipping every targeted test and passing vacuously, and corrected six runner configuration errors to name the environment variables that actually exist (`SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY`, `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256`, `SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH`) ([#48](https://github.com/SecondStack-AI/SecondBox/pull/48)).

## 0.2.2 - 2026-08-06

Stopped single poison records from taking down the whole control plane or locking a runner into a reconnect loop.

### Fixed

- Quarantined every known poison-record path in the reconcilers: a terminal Assignment failure that coincides with an in-flight Workspace stop, an Assignment whose durable command payload has an absent or mismatched correlation, an invalid Profile revision, a delete effect recorded as succeeded without a finalized Sandbox, and a lost generation race all now defer with backoff and a logged warning instead of ending the reconciler — previously any one such row shut down the control plane and kept it down across restarts ([#45](https://github.com/SecondStack-AI/SecondBox/pull/45)).
- Separated lifecycle stop commands from the assignment reconciler's fence commands in `runner_commands` (`kind='lifecycle_fence'`, retagged by migration): the reconciler's bulk fence expiry could collaterally expire a lifecycle-owned command, after which the stop broker's delivery guard killed the process. The exhaustion path also tolerates an already-terminal command, and it now releases the Workspace stop mutation it holds — previously the slot leaked forever and blocked the restart that recovers a failed Sandbox ([#45](https://github.com/SecondStack-AI/SecondBox/pull/45)).
- Stopped dropping the runner session over replayed messages: a result for an already-completed local-workspace effect is absorbed as a no-op even after the Workspace row has moved on, and `AssignmentProgress` stage timings are truncated to the microsecond precision PostgreSQL stores, so an exact replay no longer reads as different evidence. Genuine authority conflicts now name each divergent field, and telemetry disagreement is a logged anomaly that never costs the control connection ([#45](https://github.com/SecondStack-AI/SecondBox/pull/45)).

## 0.2.1 - 2026-08-06

Stopped the same-host runner deployment from leaking bind mounts into the host mount namespace.

### Fixed

- The same-host runner Compose service now binds its state and workspace directories with `rslave` propagation instead of `rshared`. When `workspace_root` nested inside the state mount, `rshared` let each runner container start propagate the workspace bind back into the host mount namespace, doubling a stack of identical mounts on the workspace host directory every start; the leaked mounts survived container teardown, made the directory undeletable, blocked re-bootstrap, and grew the host mount table without bound. Nothing in the runner creates mounts intended for the host, so `rslave` preserves the needed host-to-container propagation while cutting the leaking direction ([#42](https://github.com/SecondStack-AI/SecondBox/pull/42)).

## 0.2.0 - 2026-08-05

Hardened microVM and data-plane isolation, unlocked placement and lifecycle database hot paths, brought the TypeScript SDK to parity with Go, and bound release staging to scenario qualification evidence.

### Added

- Added `secondbox-deploy runner-template`, which emits a complete, commented `[[runners]]` declaration whose placeholder values cannot accidentally validate, plus a documented walkthrough kept byte-identical to the command output by test; the README same-host path now starts from the scaffold instead of asking the operator to supply every value from memory ([#32](https://github.com/SecondStack-AI/SecondBox/pull/32)).

### Changed

- Renamed the proxied Port transport wire value from `relay` to `proxied` across the OpenAPI contract, SDKs, and stored session rows, matching the in-memory architecture that replaced the deleted PostgreSQL frame relay ([#34](https://github.com/SecondStack-AI/SecondBox/pull/34)).
- Brought the TypeScript SDK to exact parity with Go: `readFile` now requires and enforces an explicit maximum-bytes bound during streaming, `exec` and `run` accept the full argv/shell command union with standard input, and the Flue adapter configuration gains a required `maximumFileBytes`. The Go `ExecStream` now rejects non-canonical base64 output like the Terminal already did ([#31](https://github.com/SecondStack-AI/SecondBox/pull/31)).
- Hardened microVM isolation: every jailed Firecracker instance now runs under a unique UID allocated from an explicitly declared, validated manifest range with crash-safe lease reconciliation, and host-side cgroups now cap cpu and pids alongside memory; the assignment protocol now carries the exact cpu millis and guest process limit, which production previously delivered as zero ([#36](https://github.com/SecondStack-AI/SecondBox/pull/36)).
- Removed the per-keystroke PostgreSQL write from terminal sessions: durable accounting now checkpoints on detach, close, terminal outcome, and a poll-interval cadence, and terminal reattachment adopts the Runner's authoritative input sequence through a new optional protocol and API field, with both SDKs updated. Reattaching a running session through a Runner that predates the field is refused with the typed replay-evicted result ([#35](https://github.com/SecondStack-AI/SecondBox/pull/35)).
- Expired idempotency records are no longer replayed on any path and are swept periodically together with aged activity touches, with a validated retention override; per-stream Runner-connection dedupe state is released at stream termination and the message-ID dedupe set is capped with a typed error ([#35](https://github.com/SecondStack-AI/SecondBox/pull/35)).
- Removed the fleet-wide Runner lock from Sandbox creation, Snapshot-clone, and relocation placement: candidates are now ranked unlocked, locked one at a time with `SKIP LOCKED` and a blocking fallback, and admission durably reserves homed Workspace and inbound-relocation disk so concurrent placements cannot oversubscribe a Runner's storage ([#30](https://github.com/SecondStack-AI/SecondBox/pull/30)).
- Indexed the lifecycle reconciler claim scan and the active-Lease expiry sweep, moved the global Lease sweep out of the claim transaction as a bounded `SKIP LOCKED` statement, and scoped in-transaction Lease fencing to the claimed Sandboxes so lifecycle workers no longer block each other ([#30](https://github.com/SecondStack-AI/SecondBox/pull/30)).
- Release staging now refuses to run without qualification evidence produced by a full scenario-suite pass at the exact staged commit on a clean checkout; the evidence document ships in the release manifest and `SHA256SUMS`, and hosted publishing remains publish-only with no qualification step in CI ([#33](https://github.com/SecondStack-AI/SecondBox/pull/33)).
- Simplified releases to two operator commands: build every artifact locally with `just release-stage`, then publish the supplied artifacts directly to GitHub, GHCR, and npm with `just release-upload` ([#25](https://github.com/SecondStack-AI/SecondBox/pull/25)).
- Removed the dead candidate, qualification-attestation, release-index, and publication release surface left behind by the local-publish refactor, and rewrote the deployment and downstream-integration runbooks around the artifact-manifest verification flow the release actually produces ([#28](https://github.com/SecondStack-AI/SecondBox/pull/28)).
- Added pinned golangci-lint (govet, staticcheck, unused, ineffassign, errcheck) to CI for both Go modules, removed the dead code the relay deletion and earlier refactors left behind, and made deployment-config migration fail on malformed legacy values instead of silently treating them as unset ([#34](https://github.com/SecondStack-AI/SecondBox/pull/34)).

### Fixed

- Stopped counting stopped Sandboxes against subject CPU and memory quotas: usage now sums only the same active states as instance counting, so accumulated stopped Sandboxes no longer wedge Sandbox creation into `quota_exceeded`; cleanly fenced assignments with stop authority are now released in the fencing transaction instead of parking unclaimably once the Sandbox generation advances ([#39](https://github.com/SecondStack-AI/SecondBox/pull/39)).
- Hardened the in-memory data-plane broker: frames addressed to a closed or unknown session route are dropped and counted instead of tearing down the Runner control connection, route delivery no longer blocks the control-stream reader behind a slow consumer, per-session credit violations fail only the offending session, and File reads grant stream-window credit incrementally instead of the full transfer limit up front ([#29](https://github.com/SecondStack-AI/SecondBox/pull/29)).
- Fixed audit events to record the resolved tenant and subject attribution in the service layer for operator and subject actions, instead of depending on store-side normalization of an empty tenant reference ([#27](https://github.com/SecondStack-AI/SecondBox/pull/27)).

## 0.1.4 - 2026-08-04

First stable public release of SecondBox.

### Added

- Added the self-hosted SecondBox control plane with PostgreSQL-backed projects, service accounts, API keys, immutable Profiles, durable Sandboxes, quotas, Runner scheduling, lifecycle reconciliation, and end-to-end request correlation.
- Added home-Runner-pinned reflink Workspaces and local Snapshots with generation fencing, crash-safe restore, exact-home scheduling, storage-pressure admission, and explicit stopped-Sandbox relocation.
- Added the outbound authenticated Runner protocol and Firecracker backend with independently versioned guest assets, cgroup isolation, network policy, termination reporting, and durable generation receipts.
- Added direct and proxied Exec, filesystem, PTY, and Profile-approved port sessions with bounded backpressure, reconnect, cancellation, idempotency, and Lease fencing.
- Added immutable Artifact storage, quota enforcement, retention garbage collection, and fail-closed PostgreSQL and Artifact backup tooling.
- Added generated Go and TypeScript transports, high-level lifecycle helpers, Node transports, the Flue 2 adapter, and the `secondbox` CLI.
- Added the strict versioned `secondbox.toml` deployment manifest, `secondbox-deploy`, explicit RunnerPool and standard Profile reconciliation, and base, development, and privileged same-host Runner Compose topologies.
- Published `secondbox` and `secondbox-deploy` binaries for Linux and macOS on amd64 and arm64, the Go and TypeScript SDKs, digest-addressed OCI images, microVM assets, OpenAPI, checksums, and the `agent-compartment` and `durable-coding` standard bundles.

### Changed

- Defined `Sandbox` as the durable public resource and `Instance` as replaceable compute fenced to one Sandbox generation.
- Removed the PostgreSQL data-plane frame relay; Exec, File, PTY, and Port payloads now travel directly to a Runner or through the bounded in-memory proxy.
- Replaced implicit built-in Profile reconciliation and ambient deployment defaults with explicit release resources, identities, paths, authorities, and validated configuration.

### Fixed

- Preserved distinct runtime and toolchain identities throughout the release manifest and standard bundles ([#23](https://github.com/SecondStack-AI/SecondBox/pull/23)).
- Included the mandatory `compute` capability in generated RunnerPools and compared pool architectures and capabilities as unordered sets, preventing missing admission and false deployment drift ([#24](https://github.com/SecondStack-AI/SecondBox/pull/24)).
- Made packaged lifecycle checks tolerate Runner enrollment races, wait for completed transitions, and resolve the exact Go SDK package ([#24](https://github.com/SecondStack-AI/SecondBox/pull/24)).
