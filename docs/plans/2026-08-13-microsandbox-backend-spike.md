# Plan: Linux-First Mergeable Microsandbox Backend Spike

> **Execution status (2026-08-13): Tasks 0L and 1L passed; Task 2L is next.** Host-daemon checks
> confirm that `deimos` is a real Linux KVM host and that Apple Silicon macOS hosts are available
> for the later macOS phase. The work is strictly sequential: complete and qualify Linux end to
> end, then begin macOS.
> No macOS implementation or qualification may run before Task 7L passes. See
> [Task 0 host-readiness evidence](evidence/2026-08-13-microsandbox-task-0-host-readiness.md).

Add Microsandbox as an explicitly selected experimental SecondBox compute backend alongside
Firecracker. The finished spike must run the same durable Sandbox and complete data-plane lifecycle
on a Linux KVM host and an Apple Silicon Mac while preserving one provider-neutral public API, one
runner-owned WorkspaceStore, generation fencing, and exclusive workspace ownership.

The implementation order is deliberately asymmetric. Linux is the proving ground for the entire
vertical slice. macOS work starts only after the normal control plane, runner protocol, helper,
WorkspaceStore, full data plane, lifecycle, network policy, and complete real-host scenario all pass
on deimos. macOS then ports and qualifies that working slice without redesigning it. Work on the two
platforms is never parallel or interleaved.

Profiles state integer vCPU count, guest memory, workspace capacity, and operation/output bounds;
they do not promise CPU-millis or guest PID enforcement. Assignments carry immutable digest-pinned
asset identity but no universal signature evidence. Runner isolation, local signature verification,
and additional resource controls are backend properties selected through homogeneous RunnerPools,
not universal admission requirements.

Each runner selects exactly one backend with
`SECONDBOX_COMPUTE_BACKEND=firecracker|microsandbox`. There is no per-assignment backend selection
and no fallback. A RunnerPool accepts only one backend kind, transactionally sealed by its first
admitted Runner and enforced for the pool's lifetime. Backend kind remains private runner/control-
plane evidence rather than a public Profile or Sandbox field. Microsandbox supports `cold_boot`
only during the spike.

The implementation uses a Go assignment adapter plus one Rust helper process per Instance. It does
not embed libkrun through cgo or adopt Microsandbox's SQLite lifecycle, named-volume, or Sandbox
directory ownership. WorkspaceStore remains authoritative for every durable Workspace and
Snapshot.

The evaluated baseline is Microsandbox v0.6.8 at commit
`5b335537afad433ad2c0308cb54de13b7015b4e7`, with `msb_krun = 0.1.30` and
`msb_krun_utils = 0.1.30`. Required ext4 descriptor and UUID support is maintained as a
SecondBox-owned, reproducible local patch and build input. Do not create or update external forks,
branches, pull requests, issues, comments, or releases. Every build and test runs locally in this
worktree or through explicit bb host terminals on deimos and the selected Apple Silicon host.

## Sequential gates and scope control

1. **Task 0L is the Linux feasibility gate.** Any mandatory failure is a NO-GO. Retain bounded
   evidence and stop before shared contracts or production composition.
2. **Tasks 1L through 7L are one Linux-only implementation phase.** Each task starts only after the
   preceding task and its validation pass. No macOS build, compile check, packaging, APFS test, or
   Hypervisor.framework probe runs during this phase.
3. **Task 7L is the Linux end-to-end gate.** macOS work remains closed until the complete scenario
   passes on deimos, including durability, fencing, snapshots, cleanup, and the full data plane.
4. **Task 8M is the macOS feasibility gate.** It reruns the risky mechanisms on a real Apple Silicon
   Mac and adds the APFS-specific proof. Any mandatory failure stops the macOS phase without
   weakening the qualified Linux backend.
5. **Tasks 9M and 10M port, package, and qualify macOS.** Final spike completion still requires both
   real-host suites and the unchanged Firecracker regression suite.

The spike remains explicitly experimental. It excludes automatic backend fallback, Microsandbox
memory snapshot resume, Windows, Developer ID distribution, notarization, a polished macOS
installer, and a performance release gate.

## Final validation commands

These commands are the final aggregate suite. Each task below also has its own required validation
commands and must be checked before the next task begins.

- `just verify-generated`
- `just lint`
- `just test`
- `just test-contract`
- `just test-non-kvm`
- `just test-firecracker`
- `cd runner && go test ./... -count=1`
- `cargo test --manifest-path runner/microsandbox-helper/Cargo.toml`
- `just test-workspacestore-linux`
- `just test-microsandbox-linux`
- `just test-scenario-microsandbox-linux`
- `cd runner && GOOS=darwin GOARCH=arm64 go test ./cmd/secondbox-runner -run '^$'`
- `just test-workspacestore-macos`
- `just test-microsandbox-macos`
- `just test-scenario-microsandbox-macos`
- `just test-scenario`

Real-host targets must fail clearly when invoked without the required platform or hypervisor. They
must never report a skipped suite as success. Cross-compilation is source-separation evidence only;
it never substitutes for a real Hypervisor.framework boot.

### Task 0L: Prove the risky mechanisms on Linux

Build a bounded standalone probe against the exact evaluated Microsandbox revision plus a
SecondBox-owned local patch. This task changes no public contract, database, deployment topology,
production composition, or WorkspaceStore implementation. The probe may live under
`runner/microsandbox-probe` and may use generated test fixtures.

- [x] Materialize the required ext4 changes as a reviewable local patch tied to exact upstream
  commit `5b335537afad433ad2c0308cb54de13b7015b4e7`; record the patch digest, Cargo lock digest,
  Microsandbox/libkrun versions, licenses, and deterministic local build command.
- [x] Build the patched Microsandbox and probe locally on deimos. Refuse a dirty, wrong-revision,
  or wrong-patch source tree rather than silently building different dependency code.
- [x] Start control threads before calling `Vm::enter()` and prove the inherited control socket
  remains responsive while the VMM owns the calling thread.
- [x] Pass an already-open writable raw-ext4 descriptor, reopen it through `/proc/self/fd/<n>`,
  attach it with a stable block ID, mount it in the guest, write a marker, stop, reopen the image,
  and verify the marker.
- [x] Prove the VMM does not replace the inode, close the parent-held descriptor, or take independent
  durable ownership of the descriptor-backed image.
- [x] Run one buffered agent command and one streaming command while simultaneously exchanging
  probe control frames, then request shutdown through the control channel.
- [x] Close an inherited lifecycle pipe and prove the guest shuts down, writable disks flush within
  a fixed deadline, and the process exits. Force-kill after the deadline and record that path
  separately.
- [x] Translate and enforce `deny_all` plus a representative domain/port allow-list with an allowed
  request, denied request, DNS change, private-address target, and metadata target.
- [x] Format through an inherited descriptor with an explicit caller-supplied filesystem UUID and
  safely change the UUID of a cloned image while preserving valid ext4 metadata checksums. Do not
  rewrite superblock bytes ad hoc from SecondBox code.
- [x] Record exact host OS/architecture, filesystem, source revisions, local patch/build digests,
  commands, deadlines, outcomes, and bounded logs in dated Linux evidence.
- [x] Declare Task 0L passed only if every Linux proof succeeds on deimos. On any mandatory failure,
  record NO-GO and stop the spike.

#### Task 0L validation

- `just build-microsandbox-probe-linux <clean-source> <new-output-directory>`
- `cargo test --locked --manifest-path <build-directory>/source/secondbox-probe/Cargo.toml`
- `just test-microsandbox-probe-ext4-linux <build-directory> <new-work-directory>`
- `just test-microsandbox-probe-linux <build-directory> <new-work-directory>`
- `git diff --check`

### Task 1L: Define portable contracts and homogeneous RunnerPools

After Task 0L passes, define the shared resource and runner contracts needed by both backends. This
task changes contracts and control-plane behavior but adds no production Microsandbox composition
and no macOS implementation.

- [x] Replace `SignedAssetReference` with `AssetReference`; remove `signature_key_id`; retain
  immutable digest, architecture, guest protocol generation, artifact identity, and required
  features.
- [x] Define a strict versioned backend-materialization manifest keyed by backend kind, guest
  architecture, runtime digest, and toolchain digest. Bind local launch-artifact digests,
  agent protocol/features, and backend/helper build identity.
- [x] Preserve Firecracker's local signature and trust-anchor verification as a backend-specific
  admission rule while removing signature evidence from the shared runner protocol.
- [x] Require Microsandbox materializations to name a digest-pinned source OCI manifest and a
  pre-materialized content-addressed flat root artifact. Assignment start never resolves a mutable
  tag or performs an implicit registry pull.
- [x] Advertise only locally present, revalidated materializations and reject a missing tuple before
  creating a Workspace or helper.
- [x] Transactionally seal each RunnerPool to the first healthy Runner's private backend kind and
  reject every later mismatch. Do not add a mutation/reset API.
- [x] Keep backend kind out of public Profile, Sandbox, Instance, operation, and data-plane schemas.
- [x] Replace `vcpu_millis` with integer `vcpu_count`; remove `process_limit` throughout profiles,
  assignments, quotas, accounting, projections, CLI output, fixtures, and tests.
- [x] Retain guest memory, workspace capacity, maximum duration/output/concurrency, architecture,
  capabilities, and startup mode.
- [x] Replace Firecracker-named universal capabilities and progress stages with provider-neutral
  evidence and compute-launch stages without changing lifecycle ordering.
- [x] Regenerate Go and TypeScript bindings and update migrations, fixtures, fake runners,
  scheduling, reconciliation, release inputs, and standard resources in the same task.
- [x] Update intended-design documents for profiles, protocol, assets, security, boundaries,
  observability, placement, and release operation. Do not claim macOS support yet.

#### Task 1L validation

- `just verify-generated`
- `just lint`
- `just test-contract`
- `just test-non-kvm`
- `just test-firecracker`
- `just test`

### Task 2L: Add the locally pinned Rust helper and private protocol

Create the minimal production helper before WorkspaceStore depends on it. The helper owns one
Instance's libkrun process lifetime but never owns durable Sandbox identity, fencing, or storage.

- [x] Create `runner/microsandbox-helper` as a locked Rust crate using the exact locally patched
  dependency input proven in Task 0L; commit its lockfile, patch/build manifest, and license
  inventory without publishing externally.
- [x] Define one private versioned helper protobuf covering start/ready, exec, files, PTY, TCP,
  shutdown, cancellation, terminal events, diagnostics, and stream credit. Generate Go and Rust
  bindings from the same schema.
- [x] Carry the protocol over one inherited Unix socket using bounded frames, multiplexed request
  and stream IDs, bounded diagnostic text, and no secret-bearing command-line arguments.
- [x] Pass the Workspace image and lifecycle pipe as inherited descriptors. Reopen the image only
  through `/proc/self/fd/<n>` and attach it with a stable block ID for `/workspace`.
- [x] Implement `format-workspace` using explicit UUID formatting. Accept a destination descriptor,
  logical capacity, label, and deterministic Workspace UUID; validate and durably flush before
  returning.
- [x] Configure integer VCPU count, guest memory, exact pre-materialized root artifact, startup
  environment, and translated network policy without a Microsandbox database, named volume,
  secondary workspace lock, or runtime registry pull.
- [x] Emit dependency/helper versions, host platform, agent protocol/features, supported operations,
  and active materialization digest before readiness.
- [x] Treat lifecycle-pipe EOF as parent loss; shut down, flush within a bound, and exit. Never
  daemonize or survive the runner parent.
- [x] Reject malformed, oversized, out-of-order, unknown-version, and stale-stream frames; add
  fuzz/property coverage for framing and stream state.
- [x] Keep cgo and in-process libkrun linkage out of the Go runner.

#### Task 2L validation

- `cargo fmt --manifest-path runner/microsandbox-helper/Cargo.toml -- --check`
- `cargo clippy --manifest-path runner/microsandbox-helper/Cargo.toml --all-targets -- -D warnings`
- `cargo test --manifest-path runner/microsandbox-helper/Cargo.toml`
- `cd runner && go test ./internal/microsandboxprotocol/... -count=1`
- `just verify-generated`

### Task 3L: Make WorkspaceStore portable in design and complete on Linux

Extract durable workspace ownership behind a narrow platform driver while implementing and
qualifying only the Linux driver. Common code owns manifests, generations, receipts, retention,
snapshot/restore/delete, relocation framing, and path confinement.

- [x] Make only WorkspaceStore resolve host paths. Compute backends receive an opaque attachment
  containing the generation fence, exclusive writer lock, inherited descriptor, stable block ID,
  capacity, and filesystem identity.
- [x] Extract common manifest, generation, receipt, retention, snapshot, restore, delete, and
  relocation logic from platform operations.
- [x] Implement the Linux driver with `FICLONE`, `flock`, durable directory `fsync`, helper-based
  ext4 formatting, and `/proc/self/fd/<n>` child attachment.
- [x] Preserve deterministic per-Workspace ext4 UUID validation on open, clone, restore, relocation,
  and replay. A virtio block ID is mount addressing, not image identity.
- [x] Keep workspace creation, snapshots, restores, and relocation destination creation reflink-only.
  Return storage incompatibility on failure and never copy or reconstruct empty state.
- [x] Prove cross-process writer exclusion, crash release, receipt replay, UUID validation, atomic
  publication, retention, relocation framing, sparse-file preservation, capacity accounting, and
  path confinement.
- [x] Make readiness create a source and clone under the configured final roots, mutate both, and
  prove isolation before advertising storage readiness.
- [x] Preserve Firecracker WorkspaceStore behavior and existing recovery semantics.

#### Task 3L validation

- `just test-workspacestore-linux`
- `cd runner && go test ./internal/workspacestore/... -count=1`
- `just test-firecracker`
- `just test-non-kvm`

### Task 4L: Compose the Linux Microsandbox assignment backend

Refactor the Linux runner composition root only enough to select one concrete backend and bind it
to the existing runner protocol and WorkspaceStore. There is no runtime fallback.

- [ ] Add strict `SECONDBOX_COMPUTE_BACKEND=firecracker|microsandbox` parsing with no production
  default.
- [ ] Separate common runner, protocol, logging, WorkspaceStore, materialization cache, and capacity
  configuration from Firecracker-only jail, TAP, bridge, trust-anchor, and cgroup settings.
- [ ] Add `runner/internal/microsandbox` implementing `AssignmentBackend`, startup timing, terminal
  events, local Workspace, relocation, and data-plane integration.
- [ ] Report private backend kind/version evidence so control-plane registration enforces the
  homogeneous-pool invariant before readiness.
- [ ] Admit only `cold_boot`, supported architecture, available integer VCPU/memory, an exact cached
  materialization tuple, expressible policy, and required agent features.
- [ ] Keep the complete assignment fence and Workspace attachment in Go for the Instance lifetime.
- [ ] Start one helper per Instance with inherited socket, Workspace descriptor, and lifecycle pipe.
  Publish ready only after helper/agent identity matches the materialization and `/workspace`
  passes a read/write probe.
- [ ] Reserve and release VCPU, memory, Instance, and operation capacity without CPU-millis or PID
  accounting.
- [ ] On fence, reject new work atomically, cancel streams, request shutdown, enforce the deadline,
  reap the helper, then release the Workspace.
- [ ] Convert unexpected post-ready helper exit into exactly one provider-neutral terminal event;
  never adopt a helper after runner restart.
- [ ] Keep Firecracker jailer, cgroup, network, snapshot-resume, signature verification, and
  cleanup behavior passing.

#### Task 4L validation

- `cd runner && go test ./cmd/secondbox-runner ./internal/microsandbox/... -count=1`
- `just test-microsandbox-linux`
- `just test-firecracker`
- `just lint`

### Task 5L: Implement the complete data plane through agentd

Map the existing provider-neutral runner interfaces onto Microsandbox's agent client. Do not add a
backend-specific public operation surface.

- [ ] Implement bounded buffered exec with typed spawn failures, deadlines, cancellation, exact
  stdout/stderr limits, exit status, and signal results.
- [ ] Implement streaming exec with stdin, signals, caller credit, bounded queues, output-channel
  preservation, terminal delivery, and guest cancellation.
- [ ] Implement binary-safe file stat, read, write, list, exists, mkdir, and remove with the existing
  workspace-relative path contracts.
- [ ] Implement PTY open, input/output, resize, detach/reattach where the public contract supports
  it, cancellation, and terminal delivery.
- [ ] Implement Port open as a generation-fenced guest TCP connection with bounded bidirectional
  relay and no backend-created host listener.
- [ ] Check the complete assignment generation before every operation and again before terminal
  publication. Stale work must not reach agentd.
- [ ] Map every known helper/agentd outcome to an existing provider-neutral result. Treat unknown
  outcomes as infrastructure failures.
- [ ] Prove cancellation races, helper death, caller disconnect, fencing, output exhaustion, stream
  credit, and duplicate terminal delivery through conformance tests shared with Firecracker.

#### Task 5L validation

- `cd runner && go test ./internal/dataplane/... ./internal/microsandbox/... -count=1`
- `just test-contract`
- `just test-microsandbox-linux`
- `just test-firecracker`

### Task 6L: Translate network policy and complete lifecycle evidence

Use Microsandbox's user-space network stack as the Linux backend implementation while preserving
SecondBox's fail-closed resolved-policy contract and bounded operational evidence.

- [ ] Translate deny-all and every supported domain, CIDR, protocol, and port allow-list exactly.
  Reject any rule without an exact representation; never omit or broaden it.
- [ ] Preserve DNS pinning, rebinding defense, private/metadata blocking, TLS behavior, and resolver
  discovery as documented backend properties rather than universal guarantees.
- [ ] Make readiness prove the user-space policy engine and agent relay without TAP, nftables,
  bridge, or elevated network privileges.
- [ ] Record bounded lifecycle evidence containing runner, Sandbox, Instance, assignment,
  generation, helper PID, backend/platform versions, materialization digest, stage, and stream ID,
  excluding environment values, payloads, file contents, and network bodies.
- [ ] Use a reverse-order cleanup stack for capacity, Workspace attachment, helper, socket, and
  network state on every pre-ready failure.
- [ ] Map unsupported shapes/policies, absent capacity, digest mismatch, helper/hypervisor failure,
  and guest negotiation/mount failures to their provider-neutral rejection classes.
- [ ] Record exit status, signal, helper reason, and bounded stderr/event-tail digest after
  unexpected exit without inventing cgroup OOM or PID-limit attribution.
- [ ] Add fixed-cardinality backend and host-platform dimensions to diagnostics and metrics.

#### Task 6L validation

- `cd runner && go test ./internal/microsandbox/... ./internal/networkpolicy/... -count=1`
- `just test-microsandbox-linux`
- `just lint`
- `just test`

### Task 7L: Qualify the complete Linux vertical slice

Run the full product scenario on deimos through the normal control plane, authenticated runner
protocol, direct data plane, and durable WorkspaceStore. This is the hard gate before any macOS
work.

- [ ] Add a real-KVM Linux scenario driver that fails when KVM or the local patched dependency build
  is unavailable; it must not skip successfully.
- [ ] Create a durable Sandbox and run buffered and streaming exec, binary file operations, PTY
  open/resize/close, Port relay, deny-all, and an exact network allow-list.
- [ ] Stop and restart while preserving Workspace data; create a Snapshot, mutate, restore, and
  prove the earlier contents return.
- [ ] Reject a stale generation during active work; kill the runner and prove the helper exits and
  the Workspace lock becomes available without reconstructing empty state.
- [ ] Reject wrong logical/materialization digests, a backend mismatch in a sealed RunnerPool, and
  unsupported `snapshot_resume` before creating compute.
- [ ] Run at least two concurrent Instances with independent operations, streams, disks, and
  terminal events; prove no cross-Instance data or frame delivery.
- [ ] Exercise relocation only between stopped, Snapshot-free compatible Linux runners and prove
  image bytes never persist in the control plane.
- [ ] Record 30 cold starts with start-to-ready p50/p95, stage breakdown, and peak helper memory as
  observations rather than a release gate.
- [ ] Run the complete Firecracker regression suite and scenario; prove selection introduced no
  fallback and preserved Firecracker-specific controls.
- [ ] Record exact commands, versions, materialization/build digests, bounded logs, and outcomes in
  dated Linux qualification evidence.
- [ ] Mark Task 7L passed only when every Linux scenario and regression command succeeds. Begin no
  macOS work on partial success.

#### Task 7L validation

- `just verify-generated`
- `just lint`
- `just test`
- `just test-contract`
- `just test-non-kvm`
- `just test-firecracker`
- `just test-microsandbox-linux`
- `just test-workspacestore-linux`
- `just test-scenario-microsandbox-linux`
- `just test-scenario`

### Task 8M: Prove the completed Linux mechanisms on Apple Silicon

Only after Task 7L passes, select one real Apple Silicon host and run a bounded macOS feasibility
probe against the same dependency source, local patch, helper protocol, and guest assets. This task
does not redesign shared contracts.

- [ ] Build the exact locally patched Microsandbox dependency and probe on the selected Mac; record
  OS, architecture, Hypervisor.framework, libkrun, signing/entitlement mode, filesystem, source,
  patch, Cargo lock, and binary digests.
- [ ] Prove the pre-created control threads and inherited control socket remain responsive while
  `Vm::enter()` owns the calling thread.
- [ ] Reopen an inherited Workspace descriptor through `/dev/fd/<n>`, mount it, write and persist a
  marker, and prove inode plus parent-descriptor ownership remain stable.
- [ ] Run simultaneous buffered/streaming commands and control frames, then shut down through the
  control channel.
- [ ] Close the lifecycle pipe and prove bounded guest shutdown, disk flush, and process exit;
  record the deadline force-kill path separately.
- [ ] Prove deny-all and the representative allow-list, including DNS change, private target, and
  metadata target behavior.
- [ ] Perform a real APFS `clonefile`, mutate source and destination independently, and prove
  copy-on-write isolation plus stable sparse logical size.
- [ ] Format through an inherited descriptor with the deterministic UUID and rewrite a cloned
  image UUID while preserving valid ext4 metadata checksums.
- [ ] Record bounded macOS feasibility evidence and declare Task 8M passed only if every proof
  succeeds on a real Hypervisor.framework boot.

#### Task 8M validation

- `just build-microsandbox-probe-macos`
- `cargo test --manifest-path runner/microsandbox-probe/Cargo.toml`
- `just test-microsandbox-probe-macos`
- `git diff --check`

### Task 9M: Port WorkspaceStore, runner composition, and packaging to macOS

Port the already-qualified Linux design without changing public contracts or weakening Linux. The
Darwin source graph must exclude Linux-only Firecracker, jailer, cgroup, namespace, TAP, and
nftables code.

- [ ] Split production backend factories and process/syscall supervision by build target. Linux
  implementations remain unchanged; Darwin compiles only common runner and Microsandbox paths.
- [ ] Add a Darwin WorkspaceStore driver using APFS `clonefile`, `flock`, platform-correct durable
  directory synchronization, helper formatting, and `/dev/fd/<n>` attachment.
- [ ] Preserve clone-only snapshots/restores, deterministic ext4 UUID validation, receipts,
  retention, atomic publication, sparse files, path confinement, and cross-process writer locks.
- [ ] Add a portable raw-ext4 fixture created on one platform and structurally validated on the
  other. Do not call cross-architecture live Sandbox movement portable relocation.
- [ ] Add an explicit operator-defined `arm64` Microsandbox RunnerPool, materialization, and Profile
  fixture without changing existing `amd64` standard Profile semantics.
- [ ] Build `secondbox-runner` and the helper for `darwin/arm64` with pinned `libkrunfw.dylib` and
  required runtime libraries.
- [ ] Add a repository-owned Hypervisor entitlement and deterministic signing step. Local builds
  may use ad-hoc signing but must verify the helper's effective entitlement.
- [ ] Resolve runtime libraries relative to the installed bundle without global mutable search
  paths or a user-specific Microsandbox home.
- [ ] Add macOS readiness for architecture, Hypervisor.framework, signature/entitlement, libraries,
  APFS cloning, materialization cache, descriptor reopening, network engine, and cleanup.
- [ ] Use inherited socketpairs and short runtime paths so Unix socket limits and `/var` versus
  `/private/var` canonicalization do not destabilize identity.
- [ ] Add an explicit experimental macOS install/run document without altering the qualified Linux
  Firecracker installer path.

#### Task 9M validation

- `cd runner && GOOS=darwin GOARCH=arm64 go test ./cmd/secondbox-runner -run '^$'`
- `cargo test --manifest-path runner/microsandbox-helper/Cargo.toml`
- `just test-workspacestore-macos`
- `just test-microsandbox-macos`
- `just test-workspacestore-linux`
- `just test-microsandbox-linux`

### Task 10M: Qualify macOS and close the dual-platform spike

Run the same complete vertical slice on Apple Silicon, then rerun Linux and Firecracker regressions.
The spike is complete only when both real-host suites pass with comparable evidence.

- [ ] Add a real Hypervisor.framework scenario driver that fails rather than skips when the Mac,
  entitlement, runtime libraries, APFS, or local dependency build is unavailable.
- [ ] Repeat buffered/streaming exec, binary files, PTY, Port, deny-all, and exact allow-list through
  the normal control plane and authenticated runner protocol.
- [ ] Repeat stop/restart persistence, Snapshot mutation/restore, stale-generation rejection,
  runner death/helper exit, and Workspace lock recovery.
- [ ] Reject wrong logical/materialization digests, sealed-pool backend mismatch, unsupported
  `snapshot_resume`, and unsupported architecture before creating compute.
- [ ] Run two concurrent Instances and prove isolation of operations, frames, storage, streams, and
  terminal events.
- [ ] Exercise cross-platform raw-ext4 structural compatibility and same-architecture relocation
  only where both runners support the identical immutable Profile/materialization tuple.
- [ ] Record 30 macOS cold starts with p50/p95, stage breakdown, and peak helper memory as
  observations rather than a release gate.
- [ ] Rerun the complete Linux Microsandbox, WorkspaceStore, scenario, and Firecracker suites after
  macOS changes.
- [ ] Record exact macOS commands, versions, entitlements, build/materialization digests, bounded
  logs, and outcomes alongside the Linux evidence.
- [ ] Update current architecture, operations, qualification, distribution, and security documents
  only after both platforms pass. Describe backend-specific isolation and trust accurately and keep
  `snapshot_resume` absent from Microsandbox capabilities.
- [ ] Keep Microsandbox experimental. Removing that label requires a later decision covering
  production distribution, sustained stress, upgrades/recovery, and support policy.

#### Task 10M validation

- `just verify-generated`
- `just lint`
- `just test`
- `just test-contract`
- `just test-non-kvm`
- `just test-firecracker`
- `cd runner && go test ./... -count=1`
- `cargo test --manifest-path runner/microsandbox-helper/Cargo.toml`
- `just test-workspacestore-linux`
- `just test-microsandbox-linux`
- `just test-scenario-microsandbox-linux`
- `just test-workspacestore-macos`
- `just test-microsandbox-macos`
- `just test-scenario-microsandbox-macos`
- `just test-scenario`
