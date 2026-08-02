---
title: Snapshot-Resume Sandbox Startup
date: 2026-07-30
status: gated
owner: SecondStack
provenance: SecondBox timing qualification and repository-owner direction, 2026-07-30
---

# Plan: Snapshot-Resume Sandbox Startup

## Outcome

Make an unsaturated Sandbox reach `ready` in 100–200 ms by resuming an identity-neutral, post-boot memory snapshot instead of booting the guest kernel, init, and guest agent for every Instance. The full signed toolset remains in the guest.

After the event-driven orchestration and reflinked Workspace-template work through `8703554`, a qualified concurrency-1 run measured `create_to_ready` at 657 ms p50 and 823 ms p95. The runner-local boot path is 381/426 ms, including 337/384 ms of guest negotiation. Snapshot resume remains the intended way to remove guest boot; host micro-optimizations and rootfs trimming are not.

A test-only KVM qualification now measures the missing low-level floor. Across 256, 512, and 2048 MiB shapes, Firecracker's immutable file-backed snapshot load was 3–4 ms p50/p95 and process-start-through-post-resume-hardening was 16–18 ms p50/p95. A cache-evicted sample completed in 39–45 ms and read only 84–86 MiB at every shape. Warm samples read at most 2.6 MiB, so no per-start full memory-image copy was observed.

The end-to-end target is still not reachable with the path as currently composed. A later 30-arrival candidate moved guarded ready projection into the fenced runner-result transaction, reducing `ready_projection` from 100/300 ms to 0/5 ms p50/p95. The next dispatch pass corrected Assignment-time attribution, batched the scheduler's durable writes, and removed per-connection claim preparation. Its quieter unsaturated baseline measured `runner_admission` at 17/31 ms and `pre_assignment` at 320/488 ms. An eager-dispatch experiment later measured admission at 11/34 ms, but repeated bursts proved that its connection-row lock inside placement could exhaust serialization retries and shut down the control plane. It was rejected in `05bbd8e`. The race-safe owner-claim path subsequently measured `runner_admission` at 39/66 ms, `pre_assignment` at 662/1,152 ms, and `create_to_ready` at 1,144/1,695 ms. Provider-neutral placement attribution then exposed perpetual polling of every healthy ready Sandbox as the dominant unsaturated queue. Scheduling those Sandboxes at their actual idle or maximum-duration deadline reduces the current path to 34/58 ms admission, 225/315 ms pre-assignment, and 692/800 ms end to end. Those spans still exceed the 200 ms ceiling before a production resume path performs template lookup, assignment bind, Workspace mount, or network identity activation. The plan therefore remains gated at Task 1 even though the low-level snapshot-load sub-gate passes and ready projection no longer blocks it.

The provisional no-saturation latency budget is:

| Step | Measurement supporting the estimate | Required budget |
|---|---|---:|
| API validation and durable admission | Stress API p95 was 5 ms | 5–10 ms |
| Pre-assignment orchestration | Current ready-deadline path is 225/315 ms, including 27/54 ms of placement and 204/264 ms of Workspace provisioning | Must fit inside the total target |
| Command delivery and runner admission | Current ready-deadline path is 34/58 ms; the owner-side claim remains the retained authority | 10–25 ms required |
| Signed-template lookup and Workspace attachment | Current trust and attachment stages are 0 ms | 1–3 ms |
| Guest IP, TAP, and host network policy | Current runner measurement is 12–16 ms | 10–15 ms |
| Process start and file-backed snapshot load | Qualified test-only floor is 1 ms process start and 3–4 ms warm load p50/p95 | 5–15 ms |
| First control response and post-resume hardening | Qualified test-only total, including process and load, is 16–18 ms p50/p95 | 10–30 ms before assignment bind and Workspace mount |
| Runner result production | Current runner `ready` stage is 2 ms | 2–10 ms |
| Contingency | Required for filesystem and scheduler variance | 20 ms |
| Ready projection and client visibility | Current 30-arrival measurements are 0/0 ms and 21/44 ms | Must fit inside the total target |
| **Total** | **Current ready-deadline path is 692/800 ms; low-level warm resume floor is 16–18 ms** | **100–200 ms required** |

## Feasibility gate result

The production snapshot implementation remains stopped at Task 1 as required by the plan. The low-level file-backed load sub-gate passes; the orchestration gate does not.

| Qualified workload | `create_to_ready` p50/p95 | `pre_assignment` p50/p95 | `runner_admission` p50/p95 | `runner_boot` p50/p95 | `ready_projection` p50/p95 |
|---|---:|---:|---:|---:|---:|
| Unsaturated, concurrency 1, 10 fixed arrivals at 0.25/s | 657/823 ms | 190/220 ms | 26/39 ms | 381/426 ms | 23/133 ms |
| Burst 32, 16 concurrent Workspace creates | 2,797/3,195 ms | 1,481/2,237 ms | 84/1,020 ms | 699/1,695 ms | 454/587 ms |
| Ready-projection candidate, 30 fixed arrivals at 0.25/s | 752/954 ms | 338/503 ms | 26/43 ms | 394/443 ms | 0/5 ms |
| Ready-projection candidate, burst 32 | 2,070/2,639 ms | 1,413/2,169 ms | 63/960 ms | 540/1,497 ms | 0/0 ms |
| Scheduler-batch and claim-exec candidate, 30 fixed arrivals at 0.25/s | 745/929 ms | 320/488 ms | 17/31 ms | 393/438 ms | 0/0 ms |
| Scheduler-batch and claim-exec candidate, burst 32 | 2,191/2,733 ms | 1,596/2,291 ms | 62/1,046 ms | 529/1,652 ms | 0/0 ms |
| Empty-poll follow-up, loaded host, 30 fixed arrivals at 0.25/s | 1,571/2,057 ms | 1,046/1,512 ms | 51/90 ms | 444/491 ms | 0/0 ms |
| Rejected eager Assignment dispatch, loaded host, 30 fixed arrivals at 0.25/s | 1,015/1,637 ms | 602/1,181 ms | 11/34 ms | 399/435 ms | 0/0 ms |
| Current race-safe owner claim, 30 fixed arrivals at 0.25/s | 1,144/1,695 ms | 662/1,152 ms | 39/66 ms | 433/479 ms | 0/0 ms |
| Ready-deadline candidate, 30 fixed arrivals at 0.25/s | **692/800 ms** | **225/315 ms** | 34/58 ms | 416/445 ms | 0/0 ms |

The original concurrency-1 report is `.tmp/lifecycle-workspace-template-c1-result.json`; the original burst report is `.tmp/lifecycle-workspace-template-burst32-result.json`. They qualified the Workspace-template candidate subsequently committed as `8703554`. The ready-projection reports are `.tmp/lifecycle-ready-fastpath-c1-30-result.json` and `.tmp/lifecycle-ready-fastpath-burst32-result.json`; they identify base commit `c1cfcaf` and qualified the dirty implementation candidate. The dispatch reports are `.tmp/lifecycle-claim-exec-c1-30-result.json` and `.tmp/lifecycle-scheduler-batch-claim-exec-burst32-result.json`; they identify base commit `de574bf` and qualified the dirty retained candidate. The loaded-host control and rejected eager-dispatch reports are `.tmp/lifecycle-relay-claim-c1-30-result.json` and `.tmp/lifecycle-eager-assignment-c1-30-result.json`; both use the fixed 30-arrival workload and the latter identifies base commit `e947eb7` with the implementation diff dirty. The race-safe report is `.tmp/lifecycle-post-race-fix-c1-30-result.json`. The placement observer and retained reports are `.tmp/lifecycle-placement-attribution-c1-30-result.json`, `.tmp/lifecycle-ready-deadline-c1-30-result.json`, and `.tmp/lifecycle-ready-deadline-burst32x10-result.json`; they identify base commit `b45c4a7` with the instrumentation and implementation candidate dirty. All listed runs used KVM, Btrfs, the signed qualified bundle, and zero refusals or failures. The ready-deadline burst qualification admitted 320/320 arrivals across ten rungs with no shutdown while absorbing 21 serialization failures.

The standalone snapshot-load evidence is `.tmp/snapshot-resume-feasibility-qualified-20260731.json`. It used Firecracker v1.16.1 on Linux 7.1.4, Btrfs, KVM, one cache-evicted sample, and 20 warm samples per shape. The report truthfully records source commit `8703554` with `sourceTreeDirty: true` because it qualified the uncommitted harness itself.

| Memory shape | Warm load p50/p95 | Warm total p50/p95 | Cache-evicted load/total | Cache-evicted reads | Full copy observed |
|---:|---:|---:|---:|---:|---:|
| 256 MiB | 4/4 ms | 16/17 ms | 5/45 ms | 85.7 MiB | no |
| 512 MiB | 3/4 ms | 16/18 ms | 5/39 ms | 83.6 MiB | no |
| 2048 MiB | 3/4 ms | 16/17 ms | 5/39 ms | 83.7 MiB | no |

This is a feasibility floor, not a reusable template. The test snapshots an already assignment-bound guest, keeps its disks and memory coherent by capturing while paused, reloads it with a fresh vsock UDS, proves the first control response, and invokes post-resume hardening. It deliberately does not connect restore to Manager lifecycle, public Profile schemas, runner assignment fencing, networking, or a tenant-neutral template cache. A 64 MiB exploratory source boot was rejected because the signed guest cannot boot at that memory size; the qualified scenario uses 256 MiB of memory and a separate 64 MiB Workspace.

Snapshot resume can remove only the runner boot portion of these measurements. It cannot remove pre-assignment orchestration, runner admission, ready projection, or client visibility. The next optimization pass must therefore:

1. Receipt-directory pipelining reduced the qualified unsaturated `workspace_provision` span from 148/192 ms to 129/154 ms p50/p95 over 30 arrivals. The full runner-local mutation is 108/132 ms; the earlier 28/40 ms figure covered only UUID rewrite, not manifest and receipt durability.
2. Guarded transactional ready projection reduced the qualified unsaturated `ready_projection` span from 100/300 ms to 0/5 ms p50/p95 and burst-32 from 495/621 ms to 0/0 ms. `ready_event_ingest` remains separately attributed at 0/13 ms unsaturated and 10/37 ms under burst.
3. Correct Assignment-time attribution and batch dispatch reduced unsaturated `runner_admission` from 26/43 ms to 17/31 ms p50/p95. Follow-up attribution isolated PostgreSQL execution as the claim cost, while pool acquisition, decode, and stream send remained 0 ms. Empty command and relay polls now avoid idle connection-row locks and writes. Eager Assignment dispatch reduced a consecutive loaded-host comparison from 51/90 ms to 11/34 ms, but it put the runner's single connection row into every concurrent placement transaction and was rejected after a repeated burst could shut down the control plane. The retained owner-side claim remains the only Assignment delivery authority and currently measures 34/58 ms end to end.
4. Provider-neutral placement milestones isolated lifecycle pickup at 327/839 ms of a 337/859 ms placement span. Scheduling healthy ready Sandboxes at their real idle or maximum-duration deadline reduced pickup to 12/29 ms and placement to 27/54 ms without changing scheduler authority. The ten-rung burst and full qualified scenario passed.
5. Optimize the remaining 204/264 ms Workspace provisioning and 34/58 ms runner admission without moving connection binding or sequence allocation back into placement. Burst pickup is a separate worker-throughput problem: one lifecycle worker serially consumes a simultaneously due cohort.
6. Re-run the 30-arrival gate, repeated burst ladder, and full scenario; only after the unaffected spans fit inside the 200 ms budget should production work proceed to Tasks 2–9.

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
- `just test-snapshot-resume` with the explicit signed bundle, KVM, Btrfs/XFS Workspace root, shapes, iterations, and absent evidence output path
- `git diff --check`
- `(cd runner && go test ./internal/firecracker ./internal/guest ./internal/runnercontrol)`
- `just test-scenario` with `SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1`
- `just test-stress` with `SECONDBOX_REQUIRE_QUALIFIED_STRESS=1`
- Run the new snapshot-resume qualification repeatedly at concurrency 1, 2, 4, 8, and 16; retain machine-readable per-stage evidence, host process I/O counters, cache identity, and failure classifications

### Task 1: Freeze the startup contract and measure the two remaining gates

- [x] Record corrected cold-boot and runner-admission p50/p95/p99 at concurrency 1 and under saturation, using `runner_admission`, `artifact_verify`, `workspace_attach`, `network_setup`, `compute_launch`, `guest_negotiation`, and `ready` as separate boundaries.
- [ ] Add provider-neutral resume milestones for template lookup, snapshot load, post-resume hardening, assignment binding, Workspace readiness, and final readiness. Keep backend and host details in runner-local logs only.
- [x] Build a one-off qualified measurement harness around the existing low-level snapshot load API and record process start, API load, first control response, hardening, and total time without connecting it to lifecycle or public startup.
- [x] Measure file-backed memory load versus any full-copy behavior with `/proc/<pid>/io`, major faults, and wall time at the currently deployed and qualified 256, 512, and 2048 MiB Profile/runner shapes.
- [x] Stop the plan and report the floor if unsaturated runner admission exceeds 25 ms, snapshot load exceeds 70 ms, or load performs a per-start full memory-image copy that makes the 200 ms ceiling impossible.
- [x] Run the runner timing tests and retain the raw qualified evidence with the implementation report.

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
