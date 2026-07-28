---
title: SecondBox Standalone Service
date: 2026-07-28
status: in_progress
owner: SecondStack
provenance: SecondBox architecture dialogue and Sandbox Service extraction review, 2026-07-28
---

# Plan: SecondBox Standalone Service

## Outcome

Create `../SecondBox` as an independently versioned, self-hostable network service for durable isolated sandboxes. SecondBox provides an unprivileged Go control plane, one or more separately deployed Go Firecracker runners, explicit server-owned profiles, persistent workspaces, command and file operations, and client SDKs. Its first release supports Firecracker only and qualifies both a same-host installation and a control plane connected to multiple Linux runner hosts.

SecondBox is a product boundary, not a renamed copy of SecondStack's internal API. Preserve the implementation history and the security-sensitive Firecracker, workspace, generation-fencing, artifact, and recovery work from `apps/sandbox-service`, but replace its public contract and remove all Agent Service, Chat, harness-launcher, and SecondStack deployment assumptions before the first release.

The standalone plan is staged in the SecondStack repository because SecondBox does not exist yet. Task 1 moves this plan to `../SecondBox/docs/plans/2026-07-28-secondbox-standalone-service.md`; the moved copy becomes authoritative and this bootstrap copy is removed from SecondStack when the extraction commit is recorded.

## Fixed architecture

### Product and deployment boundary

- The public durable resource is a `Sandbox`. A running `Instance` is replaceable compute for one fenced Sandbox generation; stopping or losing an Instance does not delete its Sandbox or workspace.
- The control plane is an unprivileged Go service backed by PostgreSQL and S3-compatible object storage. It can run in Compose, Kubernetes, or another container platform without KVM, TUN, host cgroups, host paths, or container-engine access.
- A Go runner is the only privileged execution component. Runners dial the control plane over an authenticated, versioned protocol, advertise pools and capacity, and run Firecracker on Linux hosts with usable KVM.
- The supported v1 deployment is a Compose control plane plus one or more Linux Firecracker runners. The runner may be on the same machine or remote. Kubernetes manifests, Kubernetes-native sandbox backends, and runner DaemonSets are not v1 release requirements.
- PostgreSQL owns desired state, assignments, generations, leases, profile revisions, audit, and reconciliation state. S3-compatible storage owns portable workspace checkpoints, snapshots, artifacts, and immutable execution assets. Runner-local storage is an active cache, not the sole durable copy.

### Profiles and tenancy

- Operators explicitly create every profile through the administrative API or CLI. There is no implicit or built-in default profile.
- A client creates a Sandbox by selecting an authorized profile name and supplying client-owned idempotency and metadata. It cannot override backend, image, resources, lifecycle, storage, networking, timeout, or runner placement fields.
- A profile resolves to an immutable revision at Sandbox creation. Editing a profile creates a new revision for future Sandboxes; existing Sandboxes stay pinned until an explicit migration operation is introduced.
- Firecracker is the only implemented v1 backend. Keep one provider-neutral backend port and one conformance suite, but do not add placeholder adapters, fake capabilities, or fallback execution.
- Applications authenticate as service accounts scoped to a project. Project scope, profile grants, and API-key scopes establish isolation between applications; clients do not assert arbitrary tenant identities.

### API and client model

- OpenAPI 3.1 is the canonical public HTTP contract. Administrative profile/project/key operations and application Sandbox operations are separate scopes within the same versioned API.
- The lifecycle surface covers create/idempotent resolve, get/list, start, ping, touch, drain, stop, wait, delete, checkpoint, inspect, and lease-aware activity. Ping reports guest health without renewing activity; touch explicitly renews activity; drain rejects new work while bounded in-flight work completes.
- Execution supports shell-string and explicit argv forms, cwd/env, buffered and streaming output, PTY input/resize/reconnect, deadlines, cancellation, exit status, and bounded output. Non-zero guest exits are results. Spawn failure, deadline, cancellation, output exhaustion, transport, admission, fencing, guest-agent, runner, and infrastructure outcomes remain typed and distinct rather than being encoded as synthetic guest exit codes.
- Filesystem operations cover binary and UTF-8 reads/writes, stat, directory listing, exists, mkdir, remove, and bounded streaming transfer beneath the Sandbox workspace.
- Flue is a thin TypeScript adapter over an already initialized SecondBox Sandbox client. Flue does not create, reuse, stop, or delete provider infrastructure; application code owns lifecycle as required by Flue's `SandboxFactory` contract.
- Gondolin is an ergonomics reference for explicit lifecycle, shell versus argv execution, streaming, PTY behavior, cancellation, filesystem methods, and ingress. Its local-process API and disposable-VM assumptions do not become SecondBox service semantics.

### Lifecycle and portability

- An active Sandbox has exactly one current generation and one fenced writer assignment. Stale control-plane replicas, runners, Instances, leases, or streams cannot mutate a newer generation.
- Agent-turn profiles may destroy compute at lease release or idle timeout while retaining the workspace. Coding-session profiles may keep compute running until explicit stop or a longer idle policy.
- API connections, operation streams, leases, Instances, and durable Sandboxes have separate lifetimes. Disconnecting a client may cancel its stream and allow its lease to expire, but never implicitly deletes a Sandbox; the pinned profile determines whether lease expiry or idleness drains and stops its Instance.
- Idle accounting comes from explicit touch and guest-reported useful activity. Active exec, filesystem transfer, PTY, and port sessions prevent idle reclamation; read-only API polling and health pings do not. Guest liveness failure and idleness are separate decisions.
- An active coding Sandbox is pinned to one runner. A stopped and successfully checkpointed Sandbox may resume on any compatible runner. V1 does not promise live migration, active-active workspace access, or transparent continuation of in-memory processes after runner loss.
- Runner loss fences the old assignment. Recovery either proves the old Instance stopped or advances the generation before materializing the last durable checkpoint elsewhere.

## Non-goals

- Do not implement Smolmachines, Kata, KubeVirt, container, QEMU, Kubernetes, or hosted-provider backends in v1.
- Do not ship a web administration UI, billing system, end-user identity directory, LLM runtime, Agent framework, Git hosting layer, or IDE.
- Do not preserve SecondStack's current `/v1/environments:*` API, `tenantRef`/`subjectRef` model, lifecycle-policy IDs, resource-class IDs, Agent emergency-revocation callbacks, or shared launcher socket.
- Do not move SecondStack's host-harness namespace launcher, egress proxy ownership, Agent artifacts, or harness spool protocol into SecondBox.
- Do not claim live migration, active-active control of mutable workspace images, generic OCI compatibility, or arbitrary nested-virtualization support.
- Do not make SecondBox a generic application-secret authority or inject model/provider credentials in v1. Applications retain credential ownership; a future egress-broker integration must be designed as a separate security boundary.

## Validation Commands

Task 1 establishes the commands below in the new repository. Run focused package and contract tests during implementation, then run the complete gates before every plan checkpoint and release candidate.

- `cd ../SecondBox && just verify-generated`
- `cd ../SecondBox && just test`
- `cd ../SecondBox && just test-contract`
- `cd ../SecondBox && just test-compose`
- `cd ../SecondBox && just preship`
- `cd ../SecondBox && git diff --check`
- `cd ../SecondBox && just test-firecracker` on a qualified KVM runner host
- `cd ../SecondBox && just test-multirunner` against two qualified runner hosts

### Task 1: Extract the repository and establish a reproducible baseline

Create `../SecondBox` from a temporary history-preserving filtered clone of the current monorepo's `apps/sandbox-service` subtree. Leave the SecondStack worktree and its embedded service unchanged so integration can proceed only after a standalone release is qualified.

- [x] Create the filtered repository at `../SecondBox`, preserve the relevant commit history, and confirm the original monorepo remote and worktree were not rewritten.
- [x] Move this plan into SecondBox's `docs/plans` and remove the bootstrap copy from SecondStack in the same extraction commit.
- [x] Adopt the `github.com/SecondStack-AI/SecondBox` Go module identity, repository-wide package naming, binary names, image labels, and documentation links without retaining `secondstack/sandbox-service` or `sandbox-host` module paths.
- [x] Carry forward the repository's MIT license and required third-party notices; audit copied guest assets, kernels, root filesystems, scripts, and vendored binaries for their governing licenses before publication.
- [x] Establish a root layout for control-plane commands, runner commands, guest agent, internal packages, contracts, migrations, SDKs, deployment assets, and documentation without retaining the monorepo-only `apps/` nesting.
- [x] Add root `just` validation targets and CI jobs for generated files, Go tests, vetting, contract tests, Compose tests, image policy checks, and build artifacts.
- [x] Repair only the paths, fixtures, and build inputs broken by extraction; keep existing behavior characterized until later tasks replace it.
- [x] Record the non-KVM baseline test result and the exact qualified-host prerequisites for Firecracker, network namespace, cgroup, signed-image, reboot, and disk-pressure tests.
- [x] Tag the extracted baseline only as an internal migration checkpoint; do not publish it as a SecondBox release.

### Task 2: Define the clean SecondBox domain and public contract

Write the product contract before changing the implementation behind it. The contract must serve ordinary API clients, SecondStack, and a Flue adapter without exposing Firecracker, host paths, runner identities, database rows, or SecondStack authority concepts.

- [x] Write concise design documents for service boundaries, domain/lifecycle, profiles, runner protocol, guest-agent protocol, workspace durability, networking, security, recovery, and API conventions.
- [x] Define Project, ServiceAccount, APIKey, Profile, ProfileRevision, Sandbox, Instance, Assignment, Lease, Workspace, Snapshot, Artifact, Runner, and RunnerPool records with explicit ownership and lifecycle.
- [x] Define the OpenAPI 3.1 resource surface, pagination, idempotency, optimistic concurrency, generation fencing, timestamps, request correlation, and stable typed errors.
- [x] Make `POST /v1/sandboxes` accept a profile name plus client idempotency and bounded metadata only; reject resource, backend, image, network, lifecycle, or placement overrides.
- [x] Define explicit create, inspect, start, ping, touch, drain, stop, wait, checkpoint, and delete semantics, including which operations are idempotent and how clients observe asynchronous transitions.
- [x] Define execution, streaming/PTY, cancellation, file, artifact, and exposed-port contracts using provider-neutral terms; distinguish guest exit, spawn failure, deadline, cancellation, output exhaustion, and infrastructure outcomes without synthetic exit codes.
- [x] Compare lifecycle, execution, filesystem, PTY, and adapter shapes against Gondolin, Microsandbox, Kilntainers, and the current Flue `SandboxApi`; document every deliberate semantic difference rather than silently approximating it.
- [x] Define public API, runner protocol, and guest-agent protocol compatibility independently from database, profile revision, checkpoint, and artifact compatibility.
- [x] Define guest-agent negotiation around one connection generation, frozen per-generation schema fixtures, feature gating before send, and a bounded supported-version window so an old guest from a durable checkpoint remains operable or fails before mutation.
- [x] Generate initial Go, TypeScript, and Python transport types/clients and add checks that generated outputs match the canonical contract.
- [x] Add contract fixtures and negative tests proving Firecracker, KVM, runner credentials, host paths, and SecondStack-specific fields cannot enter public request or response schemas.

### Task 3: Implement projects, authentication, profiles, and durable metadata

Replace the current internal authority and policy inputs with small standalone administrative resources. Keep identity at the service-account/project level; SecondBox is not an end-user directory.

- [x] Replace the extracted database baseline with a clean SecondBox-owned PostgreSQL schema and migration lineage covering the fixed domain records without physical cross-table foreign keys or CHECK constraints.
- [x] Implement bootstrap administration, project-scoped service accounts, hashed API credentials, explicit scopes, rotation, revocation, last-use evidence, and audit without returning stored credential material.
- [x] Implement administrative profile create/list/get/revise/disable operations and matching CLI commands.
- [x] Require profiles to declare the Firecracker backend, runner-pool selector, signed image/toolchain references, CPU/memory/disk quotas, drain grace, idle and maximum-duration rules, workspace checkpoint policy, network policy, exec bounds, and exposed-port policy.
- [x] Persist the resolved immutable ProfileRevision on each Sandbox and reject creation when the named profile is absent, disabled, unauthorized, or incompatible with available runner pools.
- [x] Implement project and profile quotas transactionally, including Sandbox counts, active compute, CPU, memory, retained bytes, and concurrent operation limits.
- [x] Add fixed-cardinality audit, health, readiness, and metrics projections without project names, Sandbox IDs, API keys, backend references, or workspace paths as metric labels.
- [x] Add PostgreSQL-backed store conformance and API integration tests for concurrent idempotent creation, profile revision pinning, authorization isolation, key revocation, quota races, and restart recovery.
- [x] Delete the extracted Agent Service revocation client, `tenantRef`/`subjectRef` authority, ResourceClass/LifecyclePolicy public resources, and all now-unreachable compatibility code.

### Task 4: Establish the remote runner protocol and scheduler

Turn the current one-host HTTP adapter into a genuine control-plane/runner boundary. Control-plane replicas coordinate only through durable state; runners own host execution and establish outbound authenticated connections.

- [x] Define a versioned protobuf/gRPC runner protocol for registration, heartbeat, capacity, assignments, operation progress, fencing, logs/evidence, and bidirectional exec/file/port streams.
- [x] Bootstrap runner identity deliberately, then use mutually authenticated TLS with revocable runner credentials and certificate rotation; never reuse application API keys.
- [x] Have each runner register stable identity, pool membership, architecture, KVM/Firecracker/image capabilities, local capacity, cache evidence, and software protocol versions.
- [x] Validate runner prerequisites and immutable profile compatibility before advertising schedulable capacity; a runner or compute driver must not accept an assignment until it can either return a ready Instance or a typed startup failure.
- [x] Implement scheduler selection by immutable profile requirements, compatible runner pool, free capacity, workspace locality, health, and deterministic tie-breaking.
- [x] Persist every assignment with runner ID, backend kind, backend reference, profile revision, generation, fencing token, and capability snapshot.
- [x] Serialize assignment and generation changes transactionally so multiple control-plane replicas cannot attach two runners to one mutable workspace.
- [x] Implement runner draining, loss detection, operation deadlines, bounded retry classification, and reconciliation without assigning work again until fencing is proven.
- [x] Add a fake runner and reusable runner conformance suite covering registration, capacity, stale heartbeats, duplicate messages, reordered results, reconnect, draining, assignment loss, and protocol-version rejection.
- [x] Add multi-control-plane tests proving API replicas and reconcilers remain safe without process-local ownership.

### Task 5: Convert Sandbox Host into the standalone Firecracker runner

Retain the validated Firecracker, jailer, guest-agent, networking, cgroup, image-verification, and cleanup implementation while removing every responsibility that belongs to SecondStack rather than sandbox compute.

- [x] Replace the host launcher's local control socket/API with the runner protocol and a runner composition root that receives only profile-resolved assignments.
- [x] Preserve the narrow Firecracker adapter behind the provider-neutral compute port and make its conformance suite the only backend suite implemented in v1.
- [x] Remove Agent Service peer-UID authorization, harness namespace requests, harness spools, Agent artifact conventions, egress-proxy assumptions, and SecondStack absolute paths from runner code and images.
- [x] Preserve generation-bound workspace attachment, jailer isolation, cgroup limits, network namespaces, TUN/TAP handling, vsock guest communication, process reaping, and bounded teardown.
- [x] Preserve signed kernel/rootfs/toolchain manifests and fail readiness when required artifacts, trust anchors, KVM, networking, cgroup, storage, or cleanup capabilities are missing.
- [x] Build an operator-controlled image ingestion pipeline that converts an approved OCI image or SecondBox image definition into a signed, checksummed Firecracker rootfs/kernel/toolchain bundle with architecture and guest-protocol compatibility metadata.
- [x] Make images and toolchains profile-selected immutable SecondBox artifact references; runners cache and verify released bundles but never resolve mutable tags, pull arbitrary runtime images, or silently substitute another version while satisfying an assignment.
- [x] Version the host↔guest control protocol independently from the runner protocol, negotiate a supported generation before ready, retain frozen schema fixtures, and prove current hosts interoperate with every supported released guest generation.
- [x] Enforce profile network policy at the runner boundary. The default denies loopback, private, link-local, unspecified, cloud-metadata, and runner-host destinations; controlled DNS enforces policy-aware private-answer rejection, DNS rebinding protection, and per-Sandbox DNS-to-destination pinning.
- [x] Expose inbound ports only through authenticated, profile-approved tunnels or proxies that do not wildcard-bind runner interfaces or reveal runner addresses.
- [x] Emit structured operation evidence correlated by Sandbox, Instance, generation, assignment, lease, request, and runner IDs while redacting secrets and workspace content.
- [x] Port the existing Firecracker, guest-agent, image-pipeline, network, restart, disk-pressure, and cleanup tests; add metadata SSRF, private-range, DNS-rebinding, DNS pinning, and published-port isolation coverage; remove tests for deleted SecondStack host-launcher behavior.

### Task 6: Implement durable Sandbox lifecycle and portable workspaces

Make the logical Sandbox survive compute disposal, control-plane restart, runner loss, and deliberate reassignment. Treat durable workspace state and active local state as different layers.

- [x] Implement the Sandbox desired-state reconciler for create, materialize, start, ready, draining, idle stop, explicit stop, checkpoint, failed, deleting, and deleted transitions.
- [x] Implement leases, ping, explicit touch, and guest activity independently from API process lifetime; stale leases and operation streams must not keep compute alive or authorize a newer generation, while health checks and read-only polling never renew activity.
- [x] Track active exec, filesystem stream, PTY, and port sessions as useful activity so they prevent idle reclamation; evaluate guest-agent heartbeat failure separately from idle policy.
- [x] Persist an Instance termination reason covering requested drain/stop, idle timeout, maximum duration, guest shutdown, OOM/resource exhaustion, guest-agent loss, runner loss, startup failure, fencing, and internal failure.
- [x] Materialize the active workspace on runner-local storage under an exclusive assignment and record its source checkpoint and generation.
- [x] Upload immutable stopped-state workspace checkpoints and artifacts to S3-compatible storage with content hashes, size evidence, atomic metadata publication, retention, and garbage collection; snapshots capture durable disk state but never claim to preserve RAM, processes, leases, or network sessions.
- [x] Restore a stopped Sandbox on another compatible runner from its last committed checkpoint; never attach one mutable local image to two runners.
- [x] Define and implement runner-loss recovery: fence the lost generation, mark in-memory process state lost, preserve the last durable workspace checkpoint, and require a new generation before replacement.
- [x] Implement profile-driven disposable-compute and durable-coding lifecycles without hardcoded profile names or application-specific state machines.
- [x] Implement bounded snapshot, artifact, retained-byte, disk-pressure, and cleanup behavior with explicit failure states rather than silent partial success.
- [x] Add PostgreSQL/object-store/runner integration tests for service restart, runner restart, drain ordering, ping-versus-touch, active-session idleness, maximum duration, termination reasons, checkpoint interruption, running-checkpoint rejection, stale assignment, cross-runner resume, concurrent stop/start, retention, and garbage collection.

### Task 7: Complete command, terminal, filesystem, and port data planes

Expose the operations required by coding agents and framework adapters without turning transport timeouts into orphaned guest work. Every long-running operation must remain bounded, cancellable where promised, and fenced to the current Instance.

- [x] Implement separate shell-string and argv execution modes, cwd/env validation, non-zero exit results, signals, explicit deadlines, and typed spawn failures for missing executables, permission denial, invalid working directories, and malformed executables.
- [x] Implement buffered execution with explicit terminal outcomes for exit, spawn failure, deadline, cancellation, and output exhaustion; never encode service outcomes as synthetic guest exit codes or silently present incomplete output as complete.
- [x] Implement a backpressured streaming protocol for stdin, stdout, stderr, terminal outcome, cancellation, and disconnect; once bytes have streamed, terminate an exceeded output bound with an explicit event rather than attempting to retract output.
- [x] Implement PTY allocation, stable session IDs, terminal resize, detach/reattach policy, and deterministic cleanup when clients disconnect or leases expire.
- [x] Ensure timeout and cancellation reach the guest process and return stable timeout/cancel results; do not implement local `Promise.race` semantics that leave guest processes running.
- [x] Implement binary-safe read/write, stat, list, exists, mkdir, remove, and streaming transfer beneath a descriptor-pinned workspace root without symlink escape.
- [x] Implement artifact upload/download separately from ordinary workspace paths, with bounded size, content type, checksum, retention, and authorization.
- [x] Implement authenticated tunnels or proxy sessions for profile-approved exposed ports without publishing runner addresses or raw host ports.
- [x] Add cross-transport conformance tests for binary data, spawn-failure taxonomy, deadline and output-limit outcomes, large output, backpressure, slow clients, cancellation races, PTY detach/reattach and resize, path traversal, symlink replacement, stale leases, stale generations, API disconnect without Sandbox deletion, and runner disconnect.
- [x] Prove the complete Flue-required filesystem and command subset against the real service contract.

### Task 8: Publish SDKs, CLI, and the Flue adapter

Provide thin generated transports plus ergonomic lifecycle objects. Keep provider lifetime in application code and keep framework-specific types out of the server.

- [ ] Publish a Go SDK and TypeScript SDK from the versioned OpenAPI contract, with generated transport layers wrapped by small handwritten lifecycle, polling, streaming, and error helpers.
- [x] Keep the generated Python client syntactically and contract validated; add an ergonomic Python layer only when a real client requires behavior beyond generation.
- [x] Implement a CLI for authentication checks, projects, keys, profiles, runners, Sandbox lifecycle, exec/shell, files, checkpoints, logs, and diagnostic bundles.
- [x] Implement a TypeScript Flue adapter that accepts an already initialized SecondBox Sandbox handle and implements the exact current `SandboxApi` methods.
- [x] Honor Flue cwd/env/timeout behavior, UTF-8 and binary file reads, exact mkdir/rm options, stat fidelity, and non-zero command results; map structured SecondBox terminal outcomes into Flue conventions at the adapter boundary and reject unsupported semantics before mutation.
- [x] Ensure closing a Flue harness never stops or deletes a SecondBox Sandbox and document application-owned create/reuse/delete examples.
- [x] Add SDK contract tests against a live Compose service, TypeScript type tests, abort/timeout tests, and a minimal Flue workflow that persists files across separate harness initializations.
- [x] Add concise quick starts for same-host and remote-runner deployments without making SDK examples a second lifecycle specification.

### Task 9: Package secure self-hosted deployment and operations

Make a clean installation, upgrade, backup, and incident workflow reproducible without SecondStack scripts or environment machinery.

- [ ] Publish pinned control-plane and runner container images, standalone Linux runner binaries, systemd units, guest-agent artifacts, and signed Firecracker image bundles for supported architectures.
- [x] Provide a Compose deployment for the API, PostgreSQL, an S3-compatible development object store, and optional same-host runner; allow production PostgreSQL and object-store endpoints to be supplied explicitly.
- [x] Provide an idempotent bootstrap flow for the first administrator and runner trust without committed shared credentials, blank secrets, or application defaults.
- [x] Require every runtime setting explicitly, validate it at startup, and document secrets, rotation, ports, storage, TLS, runner pools, and capacity planning.
- [x] Add liveness, readiness, metrics, structured logs, audit inspection, runner diagnostics, and bounded support bundles with end-to-end correlation IDs.
- [x] Implement coordinated database/object-state backup and restore drills, including checksum manifests, quiescence/fencing, checkpoint reachability, and fresh-runner restore verification.
- [x] Write a threat model covering application tenancy, control-plane compromise, runner compromise, malicious guests, workspace data, network egress, artifact supply chain, and credential rotation.
- [x] Document how the unprivileged control plane may run on Kubernetes while Firecracker runners use dedicated KVM hosts or a future qualified node pool; do not ship or claim supported Kubernetes manifests in v1.
- [x] Add upgrade compatibility tests for database migrations, API clients, runner protocol skew, guest-agent generation skew, profile revisions, checkpoints, and rolling control-plane replacement.

### Task 10: Qualify and release Firecracker v1

Release only after the clean contract, one-host path, multi-runner path, durability, security boundary, and client integrations are proven against packaged artifacts.

- [x] Run all non-KVM tests in CI from a clean clone without reaching into the SecondStack repository.
- [ ] Run real KVM qualification on supported Linux hosts using the packaged runner, guest assets, systemd/Compose deployment, and documented network configuration.
- [x] Prove an ephemeral agent-turn profile and a durable coding-session profile created explicitly by the operator, without shipping them as hidden defaults.
- [x] Prove two independent application projects cannot list, inspect, execute in, read files from, attach to, or delete one another's Sandboxes.
- [ ] Prove multi-runner placement, drain, stopped-Sandbox relocation, runner crash, stale-runner rejection, control-plane restart, database restart, object-store interruption, and disk-pressure recovery.
- [ ] Prove buffered exec, streaming exec, typed spawn/deadline/cancellation/output-limit outcomes, PTY detach/reattach, file transfer, artifacts, exposed ports, Go/TypeScript clients, CLI, and Flue adapter against the release candidate.
- [ ] Prove default network policy blocks private, loopback, link-local, cloud-metadata, runner-host, rebinding, and unobserved domain destinations while explicit profile policy and authenticated tunnels permit only the intended paths.
- [x] Prove ping does not renew idle time, touch does, active data-plane sessions prevent idle stop, drain rejects new work and bounds existing work, API disconnect does not delete a Sandbox, and every Instance termination has a stable reason.
- [ ] Produce SBOMs, vulnerability reports, dependency-age evidence, licenses/notices, checksums, signatures, and provenance for images, binaries, guest assets, and SDK packages.
- [x] Freeze and document API, runner-protocol, guest-agent protocol, database, profile, checkpoint, and artifact compatibility policies.
- [ ] Publish the first versioned SecondBox release and immutable image/artifact digests only after the complete release matrix passes.
- [ ] Record the exact release version and digests as prerequisites in the SecondStack integration plan; never integrate an unversioned checkout.
