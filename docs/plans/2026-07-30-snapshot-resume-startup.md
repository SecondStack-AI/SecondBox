---
title: Snapshot-Resume Sandbox Startup
date: 2026-07-30
status: planned
owner: SecondStack
provenance: SecondBox timing qualification and repository-owner direction, 2026-07-30
---

# Plan: Snapshot-Resume Sandbox Startup

## Outcome

Make an unsaturated Sandbox reach `ready` in 100–200 ms by resuming an identity-neutral, post-boot memory snapshot instead of booting the guest kernel, init, and guest agent for every Instance. The full signed toolset remains in the guest.

The measured runner-local cold path is 2,609 ms p50 and 2,906 ms p95. Host setup is already about 13 ms: network setup is 10–13 ms, trust verification and launch preparation are about 1 ms each, and starting the compute process is 0–1 ms. Guest protocol negotiation consumes 2,592 ms p50 and 2,890 ms p95. Snapshot resume is therefore the correct path to the target; host micro-optimizations and rootfs trimming are not.

The target is not reachable with the path as currently composed. The previous public `artifact_verify` p50 of 277 ms and p95 of 8,463 ms was assignment delivery/admission time, not verification. After correcting that boundary, an isolated unsaturated concurrency-1 qualification measured `runner_admission` at 98 ms p50 and 118 ms p95. That exceeds the 25 ms gate below before snapshot load is counted. Snapshot load time is also unmeasured. The architecture remains the right way to remove the 2.6-second guest boot, but the implementation must first reduce admission and prove that the pinned Firecracker version can load an immutable file-backed memory snapshot copy-on-write.

The provisional no-saturation latency budget is:

| Step | Measurement supporting the estimate | Required budget |
|---|---|---:|
| API validation and durable admission | Stress API p95 was 5 ms | 5–10 ms |
| Scheduling, command delivery, and runner admission | Corrected isolated measurement is 98 ms p50 and 118 ms p95 | 98–118 ms now; 10–25 ms required |
| Signed-template lookup and Workspace attachment | Current trust and attachment stages are 0–1 ms | 1–3 ms |
| Guest IP, TAP, and host network policy | Current runner measurement is 10–13 ms | 10–15 ms |
| Process start and file-backed snapshot load | Process start is 0–1 ms; snapshot load is not measured | 30–70 ms |
| Post-resume hardening, identity bind, network configuration, and Workspace mount | Not measured; current entropy/secrets steps are about 2 ms after a cold boot | 10–30 ms |
| Assignment result persistence and `ready` projection | Current ready stage is sub-millisecond at the runner | 2–10 ms |
| Contingency | Required for filesystem and scheduler variance | 20 ms |
| **Total** | **Current admission plus unmeasured snapshot load; target after admission work** | **176–276 ms now; 88–183 ms required** |

## Fixed architecture

- Add an immutable, provider-neutral startup policy to every Profile revision: `startup.mode` is explicitly either `cold_boot` or `snapshot_resume`. `snapshot_resume` is the new profile class used for the latency target. There is no implicit default and no `ephemeral` profile or lifecycle concept.
- Keep the current public Profile vocabulary provider-neutral. The public contract exposes only the startup mode and typed availability failures. Firecracker versions, memory files, host paths, CPU templates, TAP devices, vsock sockets, runner credentials, and snapshot-cache keys remain runner-private.
- Split guest startup into template readiness and assignment binding. Current generation-1 negotiation cannot be reused: `ProtocolIdentity` comes from per-Sandbox kernel arguments and `ConnectionBinding` is immutable after `Hello`/`Welcome`. A new guest protocol generation must support an identity-neutral template state, one template-readiness negotiation, and a one-time post-resume assignment bind.
- Capture the memory snapshot after the guest kernel, init, full toolset, control endpoint, and guest protocol server are ready and a template-readiness negotiation has proved the guest build, signed image digests, protocol generation, and feature set. Close that template stream, drain all protocol buffers, and receive an explicit guest quiescence acknowledgement before pausing the VM. Capture occurs before any assignment-bound `Hello`, runtime secret delivery, Workspace mount, guest IP configuration, or user operation.
- The template includes a network device with its link down, an unmounted Workspace block device, an empty RAM-backed runtime-private directory, and no tenant or assignment identity. The template build uses a sterile Workspace image only to establish the device shape. It never starts from a real Sandbox Instance and never captures a public Snapshot.
- Capture the post-boot rootfs disk state together with VM state and memory. Boot mutates the rootfs, so memory alone is not a coherent template. Seal an immutable post-boot rootfs image and create a per-Instance copy-on-write child at resume; do not reuse a writable template disk across Instances.
- Create and seal templates only in the separately privileged runner/image qualification boundary. The control plane neither invokes Firecracker nor sees local paths. Each home runner materializes a verified compatible template in its local execution-asset cache before advertising `snapshot_resume` capacity.
- Key every template by the signed runtime and toolchain bundle digests, guest build ID, guest protocol generation and features, kernel digest and arguments, post-boot rootfs digest, exact pinned Firecracker version, host CPU compatibility fingerprint, CPU/memory/process shape, runtime class, network-device shape, and guest control/protocol vsock ports. A mismatch is a cache miss, never a best-effort restore.
- Treat a bundle or compatibility-key change as a new immutable template generation. New Profile revisions may use it only after runners advertise the matching template as ready. Existing Profile revisions continue with their exact retained template; if it is unavailable, start fails with a typed retryable template-unavailable error. A `snapshot_resume` Profile never silently falls back to cold boot.
- Acquire the assignment fence and the home runner's opaque Workspace attachment before creating resume resources. `WorkspaceStore.Open` must hold the exclusive writer lock and prove the exact Sandbox generation. The runner stages that open image at the fixed jail-internal Workspace path recorded by the template; the compute layer never receives or resolves a host path.
- Preserve home-runner pinning. A resume template may be cached on many runners, but a Sandbox resumes only on its immutable home runner, against that runner's local Workspace. Loss of the home-runner filesystem remains loss of the Sandbox.
- Create the per-Instance guest IP, TAP device, and host network policy before loading the snapshot. Use Firecracker's network override only to bind the snapshotted interface to the new TAP. The resumed guest starts with the interface down, then the assignment-bind step sets a unique MAC, guest IP, route, and DNS configuration before bringing the link up. No packet is accepted before the host policy and guest identity agree.
- Keep the template's guest CID and control/protocol port numbers as compatibility-keyed constants. Use Firecracker's vsock UDS override to bind the restored device to the new per-Instance socket. Close all template-time vsock connections before capture and establish new control and assignment streams after resume.
- Load the template initially unavailable to the public data plane. After the VM is executing, use the already-present control endpoint to mix 64 fresh host-random bytes, force a CSPRNG reseed, correct the guest clock, and acknowledge completion before generating a connection nonce or accepting any operation. Reuse and strengthen the existing `HardenPostRestore` path rather than inventing a second entropy mechanism.
- Send one assignment-bind request carrying the Sandbox ID, Instance/compartment ID, Sandbox generation, assignment ID, fresh connection nonce, network identity, expected signed digests, and Workspace device expectation. The guest atomically installs that identity, mounts and validates the Workspace, applies runtime secrets into the empty RAM-backed private directory, and only then enables assignment-bound negotiation and data-plane handlers.
- Keep the fencing token runner-private. The runner proves it still owns the exact assignment before and after snapshot load and before publishing `ready`; the guest binds operations to assignment ID, Instance, Sandbox, generation, and nonce. Any fence loss, generation change, bind mismatch, policy failure, or timeout kills the resumed VM and releases the TAP, IP, Workspace writer lock, and capacity reservation.
- A shared template must contain no credentials, runtime secrets, runner credentials, tenant data, prior Sandbox identifiers, live protocol connections, user processes, mutable logs, or reusable random output. Template construction must scan the guest-visible filesystem and memory-facing configuration for forbidden material, zero transient buffers before capture, and restrict template files as privileged immutable execution assets.
- The existing low-level APIs are only building blocks. `FirecrackerAPIClient` can create and load snapshots with memory, network, and vsock overrides; `CreateGoldenSnapshot` can pause and create a diagnostic snapshot. There is no Manager restore composition path, and a source test explicitly prohibits reintroducing the removed unjailed `RestoreGoldenSnapshot`. `manager_toolvm.go` reuses a still-running VM for the same Sandbox/compartment; its `reusable`/`reused` fields do not mean snapshot resume, and its freeze timing only flushes the Workspace before teardown.

## Non-goals

- Do not trim the rootfs, remove packages, reduce the standard toolset, or treat image size reduction as the startup strategy.
- Do not implement this plan while writing or reviewing it. Add no placeholder backend, dormant restore function, feature flag, or stub resume branch.
- Do not turn public Workspace Snapshots into VM-memory snapshots. Public Snapshots remain runner-local reflink copies of one Sandbox Workspace.
- Do not capture a warmed tenant VM, a warm tool lease, or an assignment-bound guest connection as the shared template.
- Do not relocate a Sandbox, copy a Workspace across runners, stream image bytes through the control plane, reconstruct missing local data, or add a non-reflink fallback.
- Do not expose Firecracker, host CPU details, host paths, TAP names, vsock sockets, memory files, or template cache identities in public schemas.
- Do not weaken signed-bundle verification, post-resume entropy hardening, host network policy, generation fencing, or the exclusive Workspace writer lock for latency.
- Do not promise 100–200 ms under saturation. Queue time remains separately observable through `runner_admission`; the target applies to a compatible ready runner with available capacity and a warm local template cache.

## Validation Commands

Run the focused tests introduced by each task, then run all repository-wide and qualified gates below before marking the plan complete.

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- `just test-deployment`
- `git diff --check`
- `(cd runner && go test ./internal/firecracker ./internal/guest ./internal/runnercontrol)`
- `(cd runner && SECONDBOX_RUNNER_QUALIFY_SNAPSHOT=1 go test ./internal/firecracker -run 'TestSmokeGoldenSnapshot' -count=1)` with the signed bundle, pinned Firecracker binary, and qualified KVM host
- `just test-scenario` with `SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1`
- `just test-stress` with `SECONDBOX_REQUIRE_QUALIFIED_STRESS=1`
- Run the new snapshot-resume qualification repeatedly at concurrency 1, 2, 4, 8, and 16; retain machine-readable per-stage evidence, host process I/O counters, cache identity, and failure classifications

### Task 1: Freeze the startup contract and measure the two remaining gates

- [ ] Record corrected cold-boot and runner-admission p50/p95/p99 at concurrency 1 and under saturation, using `runner_admission`, `artifact_verify`, `workspace_attach`, `network_setup`, `compute_launch`, `guest_negotiation`, and `ready` as separate boundaries.
- [ ] Add provider-neutral resume milestones for template lookup, snapshot load, post-resume hardening, assignment binding, Workspace readiness, and final readiness. Keep backend and host details in runner-local logs only.
- [ ] Build a one-off qualified measurement harness around the existing low-level snapshot load API and record process start, API load, first control response, hardening, and total time without connecting it to lifecycle or public startup.
- [ ] Measure file-backed memory load versus any full-copy behavior with `/proc/<pid>/io`, major faults, and wall time at every supported Profile memory shape.
- [ ] Stop the plan and report the floor if unsaturated runner admission exceeds 25 ms, snapshot load exceeds 70 ms, or load performs a per-start full memory-image copy that makes the 200 ms ceiling impossible.
- [ ] Run the runner timing tests and retain the raw qualified evidence with the implementation report.

### Task 2: Add the provider-neutral snapshot-resume Profile class

- [ ] Add required `startup` policy to `ProfileRevisionSpec` and generated SDKs, with `mode` explicitly `cold_boot` or `snapshot_resume`; update every built-in Profile, fixture, example, and caller rather than supplying an application default.
- [ ] Pin `startup.mode` in immutable Profile revisions and carry it through scheduling as a provider-neutral requirement.
- [ ] Advertise provider-neutral snapshot-resume readiness and compatible shape capacity from runners. Schedule a `snapshot_resume` Profile only to its home runner and only when the exact template is ready.
- [ ] Add typed retryable errors for template generation in progress or temporarily unavailable, and a typed non-retryable incompatibility error for a Profile shape no runner can support.
- [ ] Prove public schemas contain no backend, VMM, host path, snapshot-cache, CPU, TAP, or vsock implementation fields, and prove no `ephemeral` Profile was introduced.
- [ ] Run `just test-contract`, SDK tests, Profile validation tests, and scheduler tests.

### Task 3: Split template readiness from assignment-bound guest negotiation

- [ ] Introduce a new guest protocol generation whose first state is identity-neutral template readiness and whose assignment bind is one-time and generation-fenced.
- [ ] Remove per-Sandbox identity from template boot arguments. Retain only signed build identity, template compatibility identity, fixed control/protocol ports, and values included in the template key.
- [ ] Make the template readiness exchange prove guest build ID, signed runtime/toolchain digests, supported features, and control endpoint health without installing a Sandbox ID, Instance ID, generation, assignment ID, nonce, secrets, or network identity.
- [ ] Add an explicit prepare-for-template-snapshot request that refuses active operations, freezes or leaves unmounted all mutable disks, closes protocol streams, clears connection/session state, drains outbound frames, and acknowledges a safe capture point.
- [ ] Make the resumed guest reject exec, PTY, file, port, heartbeat-ready, and useful-activity operations until hardening and the one-time assignment bind both succeed.
- [ ] Prove a second bind, stale generation, changed digest, wrong assignment, replayed nonce, or operation before bind fails closed.
- [ ] Run guest protocol generation, frozen descriptor, cancellation, flow-control, and upgrade-compatibility tests.

### Task 4: Build and seal identity-neutral templates

- [ ] Add a privileged runner/image-qualification workflow that boots the full signed bundle in template mode, performs template negotiation, reaches the explicit quiescent point, pauses the VM, and creates full VM state and memory snapshots.
- [ ] Capture and seal the coherent post-boot rootfs image alongside memory and VM state. Use an unmounted sterile Workspace device only to preserve the expected device topology.
- [ ] Never call the current `CreateGoldenSnapshot` against a tenant Instance for this purpose. Refactor shared validation primitives only after tests preserve the diagnostic create-only behavior.
- [ ] Create a signed template manifest containing the complete compatibility key, hashes and logical sizes for VM state, memory, and post-boot rootfs, creation tool versions, and security scan result.
- [ ] Durably sync template files and their parent directory before advertising readiness, and publish them atomically so a runner never opens a partial generation.
- [ ] Scan the capture for forbidden identity and secret material and fail generation if the guest has mounted a tenant Workspace, configured a tenant IP, retained a live connection, or written runtime secrets.
- [ ] Run template creation twice and prove deterministic compatibility identity, independent of output paths and runner-local names.

### Task 5: Version and manage the runner-local template cache

- [ ] Implement a runner-owned immutable execution-asset cache keyed by the complete compatibility identity; PostgreSQL stores desired Profile/bundle identity, not cache paths or memory artifacts.
- [ ] Verify the signed bundle and template manifest before cache admission, then verify stable file identity before every load exactly as launch artifacts are protected today.
- [ ] Coalesce concurrent materialization of the same template without allowing one partially verified result to become visible.
- [ ] Keep templates referenced by immutable Profile revisions and active assignments. Evict only unreferenced compatible generations under explicit storage-pressure policy.
- [ ] On signed-bundle, Firecracker, kernel, protocol, CPU-compatibility, or shape change, generate a new key and never load the old template for the new Profile revision.
- [ ] Prove corrupted, replaced, truncated, stale, unsupported-CPU, and partially published templates fail closed without a cold-boot fallback.
- [ ] Run cache concurrency, trust mutation, restart recovery, and storage-pressure tests.

### Task 6: Compose per-Sandbox resources around snapshot load

- [ ] Acquire the exact home-runner Workspace attachment and exclusive generation writer lock before template lookup, and retain both until definitive VM teardown.
- [ ] Create the per-Instance run/jail directories and stage the immutable post-boot rootfs child and opaque Workspace image at the fixed jail-internal paths expected by VM state. Use reflinks/hard links only where ownership rules permit; add no byte-copy fallback.
- [ ] Reserve a unique guest IP and MAC, create the TAP, and install default-deny or allow-list host policy before loading the snapshot.
- [ ] Start the pinned Firecracker process, load VM state using the immutable file-backed memory backend, override `eth0` to the new TAP and vsock to the new per-Instance UDS path, and resume into the guest's assignment-bind gate.
- [ ] Reconfigure the resumed guest interface from link-down to the unique MAC, IP, route, and DNS identity before allowing traffic; prove no template MAC/IP leaks onto the bridge.
- [ ] Keep guest CID and vsock port values fixed by the template key, replace only the UDS path, and prove no live template connection or frame survives resume.
- [ ] On every failure boundary, terminate Firecracker and release policy, TAP, IP, staged files, capacity, and the Workspace attachment without reporting `ready`.
- [ ] Run jailed and unjailed load tests, network isolation tests, process-I/O tests, and repeated cleanup/orphan-recovery tests.

### Task 7: Harden and bind identity before exposing the data plane

- [ ] Make the first permitted post-resume control action accept 64 fresh host-random bytes, mix and credit them into the kernel RNG, force reseed, correct the guest clock, and return an acknowledgement without producing guest-random output first.
- [ ] Zero template-era entropy buffers and generate the assignment connection nonce only after hardening succeeds.
- [ ] Bind Sandbox ID, Instance/compartment ID, generation, assignment ID, nonce, expected signed digests, network identity, and Workspace expectation atomically and exactly once.
- [ ] Mount the per-Sandbox Workspace only after bind, validate its expected filesystem/device contract, and reject missing, wrong-sized, read-only, stale-generation, or already-mounted images.
- [ ] Apply per-Sandbox runtime secrets only after hardening and bind, exclusively into the empty RAM-backed private directory. Ensure teardown zeroes the secret bundle and no secret enters template files, logs, evidence, or public responses.
- [ ] Establish a fresh assignment-bound guest protocol stream and require its binding to match the runner's current assignment fence before reporting guest negotiation complete.
- [ ] Add cloned-template tests proving two concurrent Sandboxes receive different random streams, nonces, MAC/IP identities, Workspaces, secrets, and generation bindings.

### Task 8: Preserve lifecycle reconciliation and fencing

- [ ] Add snapshot-resume as the single start path for `snapshot_resume` Profile revisions inside the existing provider-neutral compute port; do not add a second backend or bypass its conformance suite.
- [ ] Preserve the durable assignment, current Instance, Workspace mutation slot, home-runner restriction, capacity reservation, assignment fencing token, and generation checks used by cold start.
- [ ] Recheck assignment ownership after template load, after guest bind, and immediately before the ready result. Fence and tear down if any check changed.
- [ ] Make retries idempotent across control-plane restart, runner reconnect, lost assignment acknowledgement, Firecracker exit, partial load, post-resume timeout, and ready-result replay.
- [ ] Ensure stop, drain, delete, Snapshot create/restore, idle expiry, maximum duration, and runner-loss paths cannot release the Workspace writer or advance generation while a resumed VM may still write.
- [ ] Keep the Sandbox pinned and unavailable when its home runner or exact template is absent. Never relocate, reconstruct an empty Workspace, or use another runner's local cache as authority for the Workspace.
- [ ] Extend compute conformance and lifecycle fault-injection tests through every load, bind, fence, and ready persistence boundary.

### Task 9: Qualify latency, isolation, and operational rollout

- [ ] Add a qualified snapshot-resume scenario gate that creates a new Sandbox and starts a stopped Sandbox repeatedly, proving real exec, PTY, exact-boundary file transfer, network policy, stop/start persistence, Snapshot restore, generation fencing, and cleanup after resume.
- [ ] Run at concurrency 1, 2, 4, 8, and 16 with a pre-materialized template. Report runner admission separately from resume time and classify capacity refusals instead of folding them into latency.
- [ ] Meet p95 budgets for every row in the Outcome table and p95 end-to-end `ready` at or below 200 ms without saturation. Report p50, p95, p99, cache-hit rate, page-fault counts, and process read/write bytes.
- [ ] Prove template load performs no full memory or rootfs copy and no Workspace byte stream; fail qualification if I/O scales with the full memory image on every start.
- [ ] Reboot the runner, restart the control plane and PostgreSQL, rotate to a new signed bundle/template generation, and prove exact-key selection, old-Profile behavior, and no cross-generation restore.
- [ ] Perform adversarial scans and two-tenant concurrency tests proving no credentials, entropy state, identity, process, open connection, log content, page-cache data, or Workspace bytes cross Sandboxes.
- [ ] Document template generation, prewarming, compatibility rollout, retention, storage pressure, incident invalidation, and the explicit no-cold-fallback behavior for operators.
- [ ] Run every Validation Command, retain the raw evidence, and mark the plan complete only if both correctness and the 200 ms unsaturated p95 are demonstrated.
