# Changelog

## Unreleased

- Release staging now refuses to run without qualification evidence produced by a full scenario-suite pass at the exact staged commit on a clean checkout; the evidence document ships in the release manifest and `SHA256SUMS`, and hosted publishing remains publish-only with no qualification step in CI ([#33](https://github.com/SecondStack-AI/SecondBox/pull/33)).
- Removed the dead candidate, qualification-attestation, release-index, and publication release surface left behind by the local-publish refactor, and rewrote the deployment and downstream-integration runbooks around the artifact-manifest verification flow the release actually produces ([#28](https://github.com/SecondStack-AI/SecondBox/pull/28)).
- Hardened microVM isolation: every jailed Firecracker instance now runs under a unique UID allocated from an explicitly declared, validated manifest range with crash-safe lease reconciliation, and host-side cgroups now cap cpu and pids alongside memory; the assignment protocol now carries the exact cpu millis and guest process limit, which production previously delivered as zero ([#36](https://github.com/SecondStack-AI/SecondBox/pull/36)).
- Added `secondbox-deploy runner-template`, which emits a complete, commented `[[runners]]` declaration whose placeholder values cannot accidentally validate, plus a documented walkthrough kept byte-identical to the command output by test; the README same-host path now starts from the scaffold instead of asking the operator to supply every value from memory ([#32](https://github.com/SecondStack-AI/SecondBox/pull/32)).
- Brought the TypeScript SDK to exact parity with Go: `readFile` now requires and enforces an explicit maximum-bytes bound during streaming, `exec` and `run` accept the full argv/shell command union with standard input, and the Flue adapter configuration gains a required `maximumFileBytes`. The Go `ExecStream` now rejects non-canonical base64 output like the Terminal already did ([#31](https://github.com/SecondStack-AI/SecondBox/pull/31)).
- Removed the per-keystroke PostgreSQL write from terminal sessions: durable accounting now checkpoints on detach, close, terminal outcome, and a poll-interval cadence, and terminal reattachment adopts the Runner's authoritative input sequence through a new optional protocol and API field, with both SDKs updated. Reattaching a running session through a Runner that predates the field is refused with the typed replay-evicted result ([#35](https://github.com/SecondStack-AI/SecondBox/pull/35)).
- Expired idempotency records are no longer replayed on any path and are swept periodically together with aged activity touches, with a validated retention override; per-stream Runner-connection dedupe state is released at stream termination and the message-ID dedupe set is capped with a typed error ([#35](https://github.com/SecondStack-AI/SecondBox/pull/35)).
- Hardened the in-memory data-plane broker: frames addressed to a closed or unknown session route are dropped and counted instead of tearing down the Runner control connection, route delivery no longer blocks the control-stream reader behind a slow consumer, per-session credit violations fail only the offending session, and File reads grant stream-window credit incrementally instead of the full transfer limit up front ([#29](https://github.com/SecondStack-AI/SecondBox/pull/29)).
- Removed the fleet-wide Runner lock from Sandbox creation, Snapshot-clone, and relocation placement: candidates are now ranked unlocked, locked one at a time with `SKIP LOCKED` and a blocking fallback, and admission durably reserves homed Workspace and inbound-relocation disk so concurrent placements cannot oversubscribe a Runner's storage ([#30](https://github.com/SecondStack-AI/SecondBox/pull/30)).
- Indexed the lifecycle reconciler claim scan and the active-Lease expiry sweep, moved the global Lease sweep out of the claim transaction as a bounded `SKIP LOCKED` statement, and scoped in-transaction Lease fencing to the claimed Sandboxes so lifecycle workers no longer block each other ([#30](https://github.com/SecondStack-AI/SecondBox/pull/30)).
- Fixed audit events to record the resolved tenant and subject attribution in the service layer for operator and subject actions, instead of depending on store-side normalization of an empty tenant reference ([#27](https://github.com/SecondStack-AI/SecondBox/pull/27)).
- Simplified releases to two operator commands: build every artifact locally with `just release-stage`, then publish the supplied artifacts directly to GitHub, GHCR, and npm with `just release-upload` ([#25](https://github.com/SecondStack-AI/SecondBox/pull/25)).

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
