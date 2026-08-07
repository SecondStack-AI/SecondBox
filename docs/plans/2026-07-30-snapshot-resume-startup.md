---
title: Snapshot-Resume Sandbox Startup
date: 2026-07-30
status: in-progress
owner: SecondStack
provenance: SecondBox timing qualification and repository-owner direction, 2026-07-30
provenance-2026-08-06: Repository-owner direction to un-gate and implement, taken with the Phase-B lifecycle measurements on `perf/phase-b-experiments`
---

# Plan: Snapshot-Resume Sandbox Startup

## Outcome

Make an unsaturated Sandbox reach `ready` in 100–200 ms by resuming an identity-neutral, post-boot memory snapshot instead of booting the guest kernel, init, and guest agent for every Instance. The full signed toolset remains in the guest.

After the event-driven orchestration and reflinked Workspace-template work through `8703554`, a qualified concurrency-1 run measured `create_to_ready` at 657 ms p50 and 823 ms p95. The runner-local boot path is 381/426 ms, including 337/384 ms of guest negotiation. Snapshot resume remains the intended way to remove guest boot; host micro-optimizations and rootfs trimming are not.

A test-only KVM qualification now measures the missing low-level floor. Across 256, 512, and 2048 MiB shapes, Firecracker's immutable file-backed snapshot load was 3–4 ms p50/p95 and process-start-through-post-resume-hardening was 16–18 ms p50/p95. A cache-evicted sample completed in 39–45 ms and read only 84–86 MiB at every shape. Warm samples read at most 2.6 MiB, so no per-start full memory-image copy was observed.

The end-to-end target is still not reachable with the path as currently composed. A later 30-arrival candidate moved guarded ready projection into the fenced runner-result transaction, reducing `ready_projection` from 100/300 ms to 0/5 ms p50/p95. The next dispatch pass corrected Assignment-time attribution, batched the scheduler's durable writes, and removed per-connection claim preparation. Its quieter unsaturated baseline measured `runner_admission` at 17/31 ms and `pre_assignment` at 320/488 ms. An eager-dispatch experiment later measured admission at 11/34 ms, but repeated bursts proved that its connection-row lock inside placement could exhaust serialization retries and shut down the control plane. It was rejected in `05bbd8e`. The race-safe owner-claim path subsequently measured `runner_admission` at 39/66 ms, `pre_assignment` at 662/1,152 ms, and `create_to_ready` at 1,144/1,695 ms. Provider-neutral placement attribution then exposed perpetual polling of every healthy ready Sandbox as the dominant unsaturated queue. Scheduling those Sandboxes at their actual idle or maximum-duration deadline reduced that path to 34/58 ms admission, 225/315 ms pre-assignment, and 692/800 ms end to end. Bounded atomic lifecycle claims with sequential effects further reduce the current path to 26/50 ms admission, 192/223 ms pre-assignment, and 635/710 ms end to end. Those spans exceeded the 200 ms ceiling before a production resume path performed template lookup, assignment bind, Workspace mount, or network identity activation, and the plan stayed gated at Task 1 through that pass.

## Re-derived budget, 2026-08-06

The orchestration work that kept this plan gated has since landed. A Phase-B lifecycle qualification on `perf/phase-b-experiments` at base commit `e9ab1db` — 24 unsaturated arrivals at 0.25/s and one burst of 32, KVM, Btrfs, the signed qualified bundle, zero refusals — measures the path the resume work now attaches to. The evidence is `docs/plans/evidence/2026-08-06-lifecycle-b1-unsaturated-baseline.json` and `docs/plans/evidence/2026-08-06-lifecycle-b2-burst32-baseline.json`, with the post-wakeup-fix repeat in the matching `-wakeup-fix.json` reports.

| Qualified workload | `operation_total` p50/p95 | `guest_negotiation` p50/p95 | Non-guest overhead p50 |
|---|---:|---:|---:|
| Unsaturated `create → ready`, 24 arrivals at 0.25/s | 529/1,370 ms | 377/391 ms | **152 ms** |
| Unsaturated `start → ready`, 24 arrivals at 0.25/s | 447/489 ms | 380/396 ms | **67 ms** |
| Burst 32, `create → ready` | 3,897/4,172 ms | 2,233/2,673 ms | 1,664 ms |

Guest negotiation is 71% of unsaturated create and 57% of burst-32 create. Under burst it inflates 5.9× while the host pages in rootfs at 1.29 GiB/s: every Instance reflink-clones the rootfs, each clone is a distinct inode, and distinct inodes share no page cache, so 32 concurrent boots fault the same bytes from disk 32 times. A golden memory snapshot removes both terms at once. There is no boot, so guest negotiation collapses to a snapshot load; and every resume maps one shared golden memory file, so the resident set of the first resume is the page cache of every later resume. That is the mechanism this plan buys, not merely the removal of 377 ms.

The re-derived no-saturation budget replaces the earlier provisional one. Each row states the 2026-08-06 measurement it is derived from; the resume column is what the implemented path must hold.

| Step | Measured 2026-08-06 p50/p95 | Resume-path budget |
|---|---|---:|
| API validation, durable admission, and placement | `placement` 16/20 ms on create, 18/29 ms on start | unchanged, 16–30 ms |
| Workspace provisioning (create only) | `workspace_provision` 82/121 ms | unchanged, and the largest remaining non-guest term |
| Command delivery and runner admission | `runner_admission` 19/26 ms | unchanged, 16–30 ms |
| Signed-template lookup and Workspace attachment | `artifact_verify` 0/835 ms, `workspace_attach` 0/0 ms | 0–3 ms; template verification must be a stat-identity check, never a per-start rehash |
| Guest IP, TAP, and host network policy | `network_setup` 13/15 ms | unchanged, 13–20 ms |
| Process start and file-backed snapshot load | qualified test-only floor: 1 ms process start, 3–4 ms warm load | 5–15 ms, replacing `compute_launch` 7/16 ms |
| First control response and post-resume hardening | qualified test-only total through hardening, including process start and load, is 16–18 ms | 16–25 ms |
| Assignment bind, Workspace mount, network identity activation | not yet measured; no cold-path analogue exists | 10–30 ms, to be qualified by Task 9 |
| Runner result production | `ready` 2/2 ms | unchanged, 2–10 ms |
| Ready projection and client visibility | `ready_projection` 0/0 ms, `client_visibility` 16/20 ms | unchanged, 16–25 ms |
| **Total, `start → ready`** | **67 ms non-guest + 380 ms guest negotiation** | **67 + 18 + bind ≈ 95–115 ms** |
| **Total, `create → ready`** | **152 ms non-guest + 377 ms guest negotiation** | **152 + 18 + bind ≈ 180–200 ms** |

Two conclusions follow, and both bind the implementation.

Warm `start → ready` reaches the target with headroom; the 100–200 ms outcome is met on the hot path the problem statement names as the ephemeral cycle. Cold `create → ready` lands at the 200 ms ceiling with none. Its 82/121 ms of Workspace provisioning is now the single largest non-guest term and is the next optimization after this plan, not part of it. This plan therefore claims the target for `start → ready` and reports `create → ready` honestly against the ceiling rather than assuming provisioning improves.

The 835 ms `artifact_verify` p95 on the create baseline is a trust-anchor re-verification, not a steady cost, and it is the shape of failure the template cache must not reproduce. Template admission verifies digests and signature once; every start after that proves only stable file identity, exactly as `trustedMicroVMArtifactsUnchanged` protects the launch artifacts today.

## Feasibility gate result

The low-level file-backed load sub-gate passed on 2026-07-31 and the orchestration gate passed with the 2026-08-06 Phase-B measurements above. Task 1 is complete and the plan is released to Tasks 2–9.

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
| Bounded-claim candidate, 30 fixed arrivals at 0.25/s | **635/710 ms** | **192/223 ms** | 26/50 ms | 407/448 ms | 0/9 ms |

The original concurrency-1 report is `.tmp/lifecycle-workspace-template-c1-result.json`; the original burst report is `.tmp/lifecycle-workspace-template-burst32-result.json`. They qualified the Workspace-template candidate subsequently committed as `8703554`. The ready-projection reports are `.tmp/lifecycle-ready-fastpath-c1-30-result.json` and `.tmp/lifecycle-ready-fastpath-burst32-result.json`; they identify base commit `c1cfcaf` and qualified the dirty implementation candidate. The dispatch reports are `.tmp/lifecycle-claim-exec-c1-30-result.json` and `.tmp/lifecycle-scheduler-batch-claim-exec-burst32-result.json`; they identify base commit `de574bf` and qualified the dirty retained candidate. The loaded-host control and rejected eager-dispatch reports are `.tmp/lifecycle-relay-claim-c1-30-result.json` and `.tmp/lifecycle-eager-assignment-c1-30-result.json`; both use the fixed 30-arrival workload and the latter identifies base commit `e947eb7` with the implementation diff dirty. The race-safe report is `.tmp/lifecycle-post-race-fix-c1-30-result.json`. The placement observer and ready-deadline reports are `.tmp/lifecycle-placement-attribution-c1-30-result.json`, `.tmp/lifecycle-ready-deadline-c1-30-result.json`, and `.tmp/lifecycle-ready-deadline-burst32x10-result.json`; they identify base commit `b45c4a7` with the instrumentation and implementation candidate dirty. The retained bounded-claim reports are `.tmp/lifecycle-claim-batch8-c1-30-result.json` and `.tmp/lifecycle-claim-batch8-burst32x10-result.json`. All listed runs used KVM, Btrfs, the signed qualified bundle, and zero refusals or failures. The bounded-claim burst qualification admitted 320/320 arrivals across ten rungs with no shutdown while absorbing 14 serialization failures, compared with 21 for ready-deadline scheduling alone.

The standalone snapshot-load evidence is `.tmp/snapshot-resume-feasibility-qualified-20260731.json`. It used Firecracker v1.16.1 on Linux 7.1.4, Btrfs, KVM, one cache-evicted sample, and 20 warm samples per shape. The report truthfully records source commit `8703554` with `sourceTreeDirty: true` because it qualified the uncommitted harness itself.

| Memory shape | Warm load p50/p95 | Warm total p50/p95 | Cache-evicted load/total | Cache-evicted reads | Full copy observed |
|---:|---:|---:|---:|---:|---:|
| 256 MiB | 4/4 ms | 16/17 ms | 5/45 ms | 85.7 MiB | no |
| 512 MiB | 3/4 ms | 16/18 ms | 5/39 ms | 83.6 MiB | no |
| 2048 MiB | 3/4 ms | 16/17 ms | 5/39 ms | 83.7 MiB | no |

This is a feasibility floor, not a reusable template. The test snapshots an already assignment-bound guest, keeps its disks and memory coherent by capturing while paused, reloads it with a fresh vsock UDS, proves the first control response, and invokes post-resume hardening. It deliberately does not connect restore to Manager lifecycle, public Profile schemas, runner assignment fencing, networking, or a tenant-neutral template cache. A 64 MiB exploratory source boot was rejected because the signed guest cannot boot at that memory size; the qualified scenario uses 256 MiB of memory and a separate 64 MiB Workspace.

Snapshot resume can remove only the runner boot portion of these measurements. It cannot remove pre-assignment orchestration, runner admission, ready projection, or client visibility. The orchestration passes that had to land first were:

1. Receipt-directory pipelining reduced the qualified unsaturated `workspace_provision` span from 148/192 ms to 129/154 ms p50/p95 over 30 arrivals. The full runner-local mutation is 108/132 ms; the earlier 28/40 ms figure covered only UUID rewrite, not manifest and receipt durability.
2. Guarded transactional ready projection reduced the qualified unsaturated `ready_projection` span from 100/300 ms to 0/5 ms p50/p95 and burst-32 from 495/621 ms to 0/0 ms. `ready_event_ingest` remains separately attributed at 0/13 ms unsaturated and 10/37 ms under burst.
3. Correct Assignment-time attribution and batch dispatch reduced unsaturated `runner_admission` from 26/43 ms to 17/31 ms p50/p95. Follow-up attribution isolated PostgreSQL execution as the claim cost, while pool acquisition, decode, and stream send remained 0 ms. Empty command and relay polls now avoid idle connection-row locks and writes. Eager Assignment dispatch reduced a consecutive loaded-host comparison from 51/90 ms to 11/34 ms, but it put the runner's single connection row into every concurrent placement transaction and was rejected after a repeated burst could shut down the control plane. The retained owner-side claim remains the only Assignment delivery authority and currently measures 34/58 ms end to end.
4. Provider-neutral placement milestones isolated lifecycle pickup at 327/839 ms of a 337/859 ms placement span. Scheduling healthy ready Sandboxes at their real idle or maximum-duration deadline reduced pickup to 12/29 ms and placement to 27/54 ms without changing scheduler authority. Bounded atomic claims with sequential effects then reduced repeated burst mean pickup from 1,738/1,933 ms to 1,156/1,342 ms p50/p95. Independent two-worker and eight-worker variants were rejected because they increased scheduler serialization contention and end-to-end latency. The ten-rung burst and full qualified scenario passed.
5. Optimize the remaining 160/197 ms Workspace provisioning, 359/390 ms guest negotiation, and 26/50 ms runner admission without moving connection binding or sequence allocation back into placement. First split runner-local burst queue time from actual Workspace mutation and guest negotiation; do not add control-plane workers while the scheduler's serializable transaction remains the contention point.
6. The 2026-08-06 Phase-B pass re-ran the unsaturated gate and the burst ladder and recorded the spans in the re-derived budget above. Unsaturated `start → ready` non-guest overhead reached 67 ms and `create → ready` reached 152 ms, both inside the 200 ms ceiling before the resume path is added. Tasks 2–9 are released.

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
- Preserve ordinary-lifecycle home-runner pinning. A resume template may be cached on many runners, but a Sandbox resumes only on its current home runner, against that runner's local Workspace. Loss of the home-runner filesystem remains loss of the Sandbox; the explicit stopped-Sandbox relocation path requires an intact source.
- Create the per-Instance guest IP, TAP device, and host network policy before loading the snapshot. Use Firecracker's network override only to bind the snapshotted interface to the new TAP. The resumed guest starts with the interface down, then the assignment-bind step sets a unique MAC, guest IP, route, and DNS configuration before bringing the link up. No packet is accepted before the host policy and guest identity agree.
- Keep the template's guest CID and control/protocol port numbers as compatibility-keyed constants. Use Firecracker's vsock UDS override to bind the restored device to the new per-Instance socket. Close all template-time vsock connections before capture and establish new control and assignment streams after resume.
- Load the template initially unavailable to the public data plane. After the VM is executing, use the already-present control endpoint to mix 64 fresh host-random bytes, force a CSPRNG reseed, correct the guest clock, and acknowledge completion before generating a connection nonce or accepting any operation. Reuse and strengthen the existing `HardenPostRestore` path rather than inventing a second entropy mechanism.
- Send one assignment-bind request carrying the Sandbox ID, Instance/compartment ID, Sandbox generation, assignment ID, fresh connection nonce, network identity, expected signed digests, and Workspace device expectation. The guest atomically installs that identity, mounts and validates the Workspace, applies runtime secrets into the empty RAM-backed private directory, and only then enables assignment-bound negotiation and data-plane handlers.
- Keep the fencing token runner-private. The runner proves it still owns the exact assignment before and after snapshot load and before publishing `ready`; the guest binds operations to assignment ID, Instance, Sandbox, generation, and nonce. Any fence loss, generation change, bind mismatch, policy failure, or timeout kills the resumed VM and releases the TAP, IP, Workspace writer lock, and capacity reservation.
- A shared template must contain no credentials, runtime secrets, runner credentials, tenant data, prior Sandbox identifiers, live protocol connections, user processes, mutable logs, or reusable random output. Template construction must scan the guest-visible filesystem and memory-facing configuration for forbidden material, zero transient buffers before capture, and restrict template files as privileged immutable execution assets.
- The existing low-level APIs are only building blocks. `FirecrackerAPIClient` can create and load snapshots with memory, network, and vsock overrides; `CreateGoldenSnapshot` can pause and create a diagnostic snapshot. There is no Manager restore composition path, and a source test explicitly prohibits reintroducing the removed unjailed `RestoreGoldenSnapshot`. `manager_toolvm.go` reuses a still-running VM for the same Sandbox/compartment; its `reusable`/`reused` fields do not mean snapshot resume, and its freeze timing only flushes the Workspace before teardown.

## Snapshot resume requires the jailer, 2026-08-06

A first attempt at a composed multi-Instance resume qualification produced concurrency numbers that were **withdrawn**, because the setup was invalid. Recording why, since the mistake is easy to repeat.

Firecracker opens every block device at the path the VM state recorded, and it does so **during the load**, not afterwards:

```
Load snapshot error: Failed to restore from snapshot: Failed to restore devices:
Error restoring MMIO devices: Block: Virtio backend error: Error manipulating the
backing file: No such file or directory (os error 2) /.../rootfs.ext4
```

Two consequences follow. `PATCH /drives` cannot repoint a restored Instance's disks, because the load has already failed by the time it could be called — the API supports the call, but not for this purpose. And an unjailed restore records the template source's absolute paths, which at most one Instance can own. The first qualification ran unjailed and its resumed Instances silently opened the **template source's** rootfs and Workspace, which happened to still exist; the per-Instance staging it appeared to prove was inert, and its 19–25 ms concurrency figures describe Instances sharing one disk. They are not evidence of anything and are not carried forward.

The design consequence is not a workaround, it is a constraint: **snapshot resume requires the jailer.** Under the jailer the recorded drive paths are chroot-relative names that each Instance resolves inside its own jail, so staging — not an API call — is what delivers a per-Sandbox Workspace. `prepareSnapshotResumeLaunch` therefore refuses an unjailed resume before staging any file, and the runner's unjailed mode stays what it always was: a test-only escape hatch, now explicitly incompatible with `snapshot_resume`.

What is qualified is the template artifact itself. The evidence is `docs/plans/evidence/2026-08-06-snapshot-resume-template-lifecycle.json`, recording source commit `76b73bb` with `sourceTreeDirty: true` because it qualified the harness that produced it, and `identityNeutralTemplate: false` because the shipped guest still takes its Sandbox identity from kernel arguments. The run builds a template from a real signed boot at 512 MiB with a 10.7 GiB sealed post-boot rootfs captured at one paused point, publishes it atomically, admits it through the runner-local cache with full digest verification, proves the per-start stable-identity check is stat-only, and proves the unjailed refusal fails closed before staging.

| Stage | Measured |
|---|---:|
| Template build, boot through sealed publish | 28,822 ms |
| One-time cache admission, digesting 11.2 GiB | 5,187 ms |
| Per-start stable-identity check | **5,911 ns** |

The last row is the one that matters for the budget. Admission verifies every digest once and costs seconds; every start afterwards costs six microseconds, four orders of magnitude below the 0–3 ms the re-derived budget allows for template lookup. That is the difference between extending the signed-asset model and reproducing the 835 ms `artifact_verify` p95 on every start.

The resume floor remains the separately qualified low-level load: 3–4 ms warm load and 16–18 ms through post-resume hardening at 256, 512, and 2048 MiB, with no full memory-image copy. Qualifying the composed multi-Instance resume needs a privileged jailed gate; it is built and passing below.

## Implementation decisions, 2026-08-06

These resolve questions the fixed architecture left open. Each states what the pinned VMM and the shipped guest actually support, because two of them contradict an assumption in the section above.

**Identity reaches a resumed guest over the existing guest control endpoint, before the first `Hello`.** Settled by the repository owner on 2026-08-06, superseding the fixed architecture's call for a new guest protocol generation. The resumed guest accepts one `/assignment/bind` control request after hardening succeeds and before its protocol listener accepts a connection. The fixed architecture calls for a new guest protocol generation carrying a one-time assignment bind, on the premise that `ProtocolIdentity` comes from kernel arguments and `ConnectionBinding` is immutable after `Hello`. Both remain true, and neither requires a new generation. The guest already exposes a host-only HTTP control endpoint on its own vsock port — the surface that serves `/restore/harden` and already delivers per-Sandbox runtime secrets. Delivering identity there, after hardening and before the guest protocol listener accepts a connection, gives the same one-time, generation-fenced bind with no change to `contracts/guest/v1`, no descriptor or fixture churn, and no widened negotiation window. It is no weaker: the control endpoint refuses any connection whose vsock CID is not the host, and it already carries secrets, which are strictly more sensitive than identifiers. MMDS is not used. It would require the guest to have a configured network before it has an identity, and the runner's egress policy treats the whole link-local range as protected.

**The Workspace reaches a resumed guest by staging, and `PATCH /drives` cannot substitute for it.** Firecracker v1.16.1's `SnapshotLoadParams` carries `network_overrides`, `vsock_override`, and `clock_realtime`, and no drive override; the snapshot restores each block device by opening the `path_on_host` it recorded, during the load. `PATCH /drives/{id}` exists and is post-boot only, but a load whose recorded paths are absent has already failed before it could be called, so it is not a mechanism for attaching per-Instance disks to a restored guest. Under the jailer the recorded path is a chroot-relative name, so a restored Instance opens its own jail's file at the same name and the per-Sandbox Workspace arrives by being staged there. That is the only mechanism, and it is why resume requires the jailer.

**The generation fence and the exclusive writer lock are unchanged by resume.** `WorkspaceStore.Open` already runs before compute in `StartAssignment`, holds the cross-process writer lock for the attachment's lifetime, and fails closed on a stale generation. The resume path forks below that call, exactly where cold start forks, and re-proves the attachment's generation before staging. Nothing about resume moves, weakens, or reorders it.

**The golden memory file is hard-linked into each jail, never reflinked.** This is the mechanism, not an optimization. A reflink clone is a distinct inode, and page cache is per inode; cloning the memory file per Instance would reproduce exactly the per-inode page-in that makes 32 concurrent boots read the same rootfs bytes 32 times. One inode means the first resume's resident set is every later resume's cache hit. The link is never chowned, so no Instance's jailer UID takes ownership of an artifact every other Instance depends on. Only the sealed post-boot rootfs is cloned per Instance, because the restored guest writes to it.

**Template integrity extends the signed-asset model rather than paralleling it.** A template is not separately signed — the runner has no signing key. Its compatibility key carries the signing-key fingerprint and the signed manifest digest of the bundle it was built from, so a template is only ever admitted against the bundle the runner has already verified. Cache admission then verifies every recorded file digest once, and every start after that proves stable file identity with the same dev+ino+size+mtime+ctime check that protects launch artifacts today. Publication is one rename of a fully synced staging directory, so no reader opens a partial generation.

**A failed resume is a failed start.** There is no retry inside the load, no second attempt with different parameters, and no cold boot. A template that is absent, incompatible, corrupted, or changed since admission fails with a typed retryable unavailability error and the Instance is torn down with its TAP, policy, guest IP, staged files, and Workspace attachment released.

## Guest rebuild feasibility, 2026-08-06

An earlier report from this work claimed an identity-neutral template was blocked because a rebuilt, re-signed rootfs could not be produced on the qualification host. That claim was wrong and is corrected here, because acting on it would have stalled Task 3 and Task 4 indefinitely.

The private key for the currently qualified bundle's signing key `acf1b3a8…` is genuinely absent from this host. That is not the blocker it appeared to be. The runner takes its trust anchor as explicit operator-supplied runtime input — `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY` and `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256` — so a locally rebuilt bundle signed by a locally held key verifies through exactly the same three gates as the shipped one: checksum manifest, detached signature, and pinned key fingerprint. Nothing is weakened; the anchor is simply pinned to a key the qualification operator holds.

Such a key is present, with its private half: `secondbox-task5-signing/signing.pem`, deriving public fingerprint `d2bde8b7…`. Two complete bundles on this host were built and signed with it, and the second one proves the incremental path this plan needs. `secondbox-task5-artifacts` and `secondbox-task5-artifacts-shutdown-fix` carry byte-identical `kernel`, `shared.img`, package locks, and license inventories, but different `rootfs.ext4` images — `5ec30581…` against `48383ded…` — with regenerated `manifest.json`, `manifest.sig`, `SHA256SUMS`, and contract files timestamped 1.5 hours later. Its signature verifies against its own `signing.pub`.

A guest-agent-only rebuild is therefore a supported, already-exercised operation on this host: rebuild the rootfs with the new agent, re-sign with a held key, and qualify against that anchor. Task 3 and Task 4 are not blocked.

The concrete recipe, so the next pass does not rediscover it:

- The builder is `runner/scripts/microvm-image/build.sh`, wrapped by `just build-microvm-images` and `just verify-microvm-images`. `just prepare-stress` is the documented end-to-end local workflow and already generates its own RSA key, builds, signs, and verifies. Its output pair is on disk and is re-signable: bundle `.secondbox/stress/artifacts/` with the matching private key at `.secondbox/stress/trust/manifest-private.pem`, mode 0600.
- Signing is `openssl dgst -sha256 -sign` over `manifest.json`, keyed by `SECONDBOX_RUNNER_MICROVM_SIGNING_KEY`. The key must be RSA; `Config.ValidateMicroVMTrustAnchor` rejects anything else, which is what the `secondbox-task5-signing-ed25519-failed` directory on this host records. `manifest.json` binds the kernel, rootfs, shared-image, and component-manifest digests, so one signature covers the set.
- The expensive stages are skippable. The Docker/debootstrap rootfs source build is a separate script and does not need to run: two prepared trees exist with their `.fakeroot-state` sidecars. The kernel build is skippable with `SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL=false` plus an explicit `_KERNEL_PATH` and `_KERNEL_CONFIG`. What remains is the guest-agent compile, the ext4 image creation, and hashing.
- One trap: use the `-f781` prepared source tree, not the original one. The original predates the `source.browserPolicy` field, and `build.sh` reads it with `jq -er ... select(. == "allow" or . == "forbid")` under `set -e`, so an absent field aborts the build. `verify.sh` has a legacy-v1 escape hatch for that manifest shape; `build.sh` does not.
- Every consumer that pins `acf1b3a8…` must be repointed at the new fingerprint when re-keying: the deployment manifest, its generated env file, the runner identity env, and the Phase-B env script. That last one currently points the anchor at the bundle's own `signing.pub`, which the pipeline documentation explicitly warns against; it degrades the anchor to the pinned fingerprint alone. Pre-existing, but worth fixing while re-keying.

What remains genuinely unproven is whether the jailed resume gate can run here. The jailer needs to chroot, chown, and drop UID, which the unprivileged test suite cannot do — but the scenario suite already runs every other privileged gate inside its privileged runner container, so the resume gate should be built there rather than as an unprivileged `go test`. That is the first thing to settle in Task 9.

## Guest rebuild result, 2026-08-06

The rebuild was performed and the recipe above holds exactly as written. The bundle is `secondbox-template-mode-artifacts`, artifact version `secondbox-local-template-mode`, built from the `-f781` prepared source tree with `SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL=false` and the kernel supplied from `secondbox-task5-artifacts-shutdown-fix`, and signed with the RSA-3072 key at `.secondbox/stress/trust/manifest-private.pem`. Its trust anchor is the independently held `manifest-public.pem` in that same `trust/` directory, fingerprint `a02f2488…` — never the bundle's own `signing.pub`. `verify.sh` passes against that anchor.

That directory layout is the fix for the self-referential anchor the recipe flags. `.tmp/phase-b/env.sh` pointed `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY` at `${bundle}/signing.pub`, which degrades the anchor to the pinned fingerprint alone; the same defect exists in the deployment manifest and its generated env, which both point at `/opt/secondbox-artifacts/signing.pub` inside the artifact mount. Every gate in this pass takes its anchor from `trust/`, matching `scripts/prepare-stress.sh`, `scripts/test-stress.sh`, and `docs/operations/scenario-qualification.md`. The four `acf1b3a8…` consumers are host deployment state outside this repository and are not repointed by this branch: repointing the live deployment's anchor requires shipping it the matching bundle, which is a deployment operation, not a code change.

The rebuild is not guest-agent-only in one respect the recipe did not anticipate. `runner/scripts/microvm-image/init` also changes, because a template must not mount its Workspace before capture. Both files are inside the rootfs, so the same single rebuild covers them.

## Identity-neutral template qualified, 2026-08-06

The template PR #57 qualified recorded `identityNeutralTemplate: false`, because the shipped guest took its Sandbox identity from kernel arguments. That is no longer true. The evidence is `docs/plans/evidence/2026-08-06-snapshot-resume-identity-neutral-template.json`, recording source commit `dc1a0a3` with a clean tree, and the low-level floor re-measured against the same rebuilt bundle is `docs/plans/evidence/2026-08-06-snapshot-load-512mib-template-mode.json`.

| Stage | Measured |
|---|---:|
| Template guest boot, process start through control endpoint answering | 402 ms |
| Template build, boot through sealed publish | 29,662 ms |
| One-time cache admission, digesting 11.2 GiB | 5,622 ms |
| Per-start stable-identity check | **6,603 ns** |

The template guest is `secondbox.template_mode=1`: no Sandbox ID, no Instance ID, no generation, no digests, no heartbeat interval, no runtime secrets, an unmounted `/dev/vdb`, and a protocol listener that is bound but refuses every connection. Its `/heartbeat` reports empty identifiers. That 402 ms of boot is exactly what a resume removes; the low-level floor for replacing it remains 3–4 ms of warm load and 16–18 ms through post-resume hardening.

Two design points are worth recording because they are not obvious from the fixed architecture.

**The Workspace must be mounted at bind, not at boot.** It is the one device whose backing file differs per Instance. A template that mounted its sterile Workspace before capture would seal that superblock and page cache into every resumed Instance, which no drive override can correct — the guest would be reading a filesystem it thinks it already knows. `init` therefore skips the mount in template mode and the guest agent mounts the staged image inside the one atomic bind. The rootfs and shared image need no such treatment: each Instance's copies are content-identical to the sealed ones.

**A template capture has nothing to quiesce.** The fixed architecture called for an explicit prepare-for-snapshot request that drains frames, closes streams, and clears session state. An identity-neutral guest has never accepted a protocol connection, so there is no stream, no session, no outbound frame, and no mounted mutable disk. The quiescent point is structural rather than negotiated, which is why Task 3 records that item as unnecessary rather than done.

## Jailed resume qualified, 2026-08-07

The gating unknown is closed. `scripts/test-snapshot-resume-jailed.sh` compiles `TestSmokeJailedSnapshotResume` on the host and runs it as root inside a privileged container with `/dev/kvm`, host cgroups, the signed bundle, and a Btrfs qualification root, exactly as the scenario suite runs every other privileged gate. Inside, it builds an identity-neutral template under the jailer, admits it through the runner-local cache, and resumes real Instances at concurrency 1, 2, 4, 8, and 16, composing the landed primitives with no Manager surgery: `prepareSnapshotResumeLaunch` → start the jailer → `resumeSnapshotTemplate` → `HardenPostRestore` → `BindAssignment` → `NegotiateGuestProtocol`. Each Instance holds its own `WorkspaceStore.Open` attachment with the exclusive writer lock and the generation fence, so the fork point the plan names is exercised rather than asserted.

The template must itself be built under the jailer, which the earlier passes did not have to know. A jailed Firecracker resolves every API path inside its chroot, in both directions: the capture is requested at jail-relative names and moved into the cache afterwards, and the resulting VM state records chroot-relative drive names — which is exactly what lets a restored Instance open its own disks. A template captured unjailed records the source's absolute paths and cannot be resumed into a jail at all.

The evidence is `docs/plans/evidence/2026-08-07-snapshot-resume-jailed-gate.json`, recording source commit `28f066e` with a clean tree. Every rung passed. Stage p50/p95 in milliseconds:

| Stage | c1 | c2 | c4 | c8 | c16 |
|---|---:|---:|---:|---:|---:|
| Template stable-identity check | 0.010/0.010 | 0.011/0.029 | 0.009/0.011 | 0.011/0.068 | 0.020/0.268 |
| Jail staging | 0.7/0.7 | 1.0/1.4 | 1.4/2.2 | 3.3/6.0 | 5.8/11.6 |
| Jailer process start through API socket | 32/32 | 48/48 | 60/64 | 103/109 | 179/202 |
| Snapshot load | 2.7/2.7 | 2.3/3.3 | 3.9/6.1 | 7.8/13.5 | 4.1/7.0 |
| First control response | 7.3/7.3 | 6.4/7.0 | 6.6/7.3 | 6.9/7.7 | 7.1/9.9 |
| Post-resume hardening | 1.8/1.8 | 1.7/2.5 | 1.8/2.4 | 2.7/3.3 | 2.8/3.3 |
| Assignment bind | 5.7/5.7 | 4.5/5.0 | 4.9/5.5 | 6.9/9.8 | 5.1/6.1 |
| Guest protocol handshake | 7.5/7.5 | 6.5/6.6 | 5.7/6.4 | 6.9/7.2 | 6.6/7.9 |
| **Resume total** | **58/58** | **72/72** | **87/88** | **138/139** | **219/232** |

Three things the measurement settles.

**The resume mechanism does not scale with concurrency; the jailer does.** Snapshot load stays between 2.3 and 7.8 ms across the whole ladder, and the four guest-side stages — first control response, hardening, assignment bind, guest handshake — are flat at roughly 7, 2, 5, and 7 ms, totalling about 22 ms at every rung. Everything that grows is the jailer's process start: chroot, device nodes, exec-file copy, cgroup creation, and UID drop, from 32 ms at one Instance to 179 ms at sixteen. That is not a resume cost and cold boot pays it too; it is simply hidden inside cold boot's 377 ms `guest_negotiation` span, where nothing measured it separately.

**The golden memory file is one inode and one page cache, exactly as the mechanism requires.** Every rung shares the template's memory inode, with a link count of exactly concurrency plus one, while per-Instance rootfs children and Workspace images are distinct inodes. Aggregate Firecracker block reads across sixteen concurrent resumes are 8.9 MiB against a nominal 16 × 512 MiB of guest memory — 0.1%, with no per-start memory copy at any rung. The 32-boot rootfs re-read that motivated this plan does not have an analogue here.

**The budget's premise holds and one of its rows does not.** The re-derived budget allowed 5–15 ms for process start plus file-backed load. The measured pair is 35 ms at concurrency 1, because the qualified low-level floor that produced the 1 ms process-start figure started Firecracker unjailed with `exec`, and a jailed start costs 32 ms. Every other row came in at or under budget: template lookup 10 µs against 0–3 ms, first control plus hardening 9.1 ms against 16–25 ms, and assignment bind 5.7 ms against 10–30 ms. The resume total of 58 ms therefore exceeds the 28–48 ms the budget derived, entirely in the jailer.

That does not threaten the outcome, because the term resume removes is much larger than the term it adds. Resume replaces `compute_launch` at 7/16 ms and `guest_negotiation` at 377/391 ms with one 58 ms span. Carried onto the 2026-08-06 unsaturated spans, `start → ready` projects to 67 − 7 + 58 = **118 ms** and `create → ready` to 152 − 7 + 58 = **203 ms**. Those are arithmetic on two separately measured spans, not an end-to-end measurement: the production start path is not landed, so no `create_to_ready` has been observed through resume. The projection is what releases the remaining work, and the 100–200 ms outcome is claimed only once a qualified end-to-end run shows it.

One defect had to be fixed before any number existed. `AdmittedSnapshotTemplate.VerifyStableIdentity` pinned the same `dev+ino+size+mtime+ctime` identity that protects launch artifacts, but a template file is hard-linked into every Instance's jail and both `link(2)` and `unlink(2)` mark the inode's status-change time. The second concurrent resume therefore failed its own identity check for a file nothing had modified, and so did the first resume after any teardown. Launch artifacts are never linked, so they keep ctime; a template's identity now pins device, inode, size, and modification time only. A replaced, truncated, or rewritten template still fails closed, and `TestSnapshotTemplateStableIdentitySurvivesPerInstanceLinking` and `TestSnapshotTemplateStableIdentityFailsAfterRewrite` pin both halves of that.

## The `snapshot_resume` Profile class and its capability binding, 2026-08-07

Task 2 is complete. `ProfileRevisionSpec` now carries a required `startup` policy whose `mode` is explicitly `cold_boot` or `snapshot_resume`. There is no application default: `validateProfileRevisionSpec` refuses a blank mode, the contract lists `startup` as required, and every producer in the repository — both standard bundles, the declarative fixtures, the scenario and stress drivers, the live SDK fixtures, and the placement fixtures — states one.

**Migration.** `spec_json` is schemaless, so nothing forced a migration; the honest reading did. A revision written before the field decodes with an empty mode, and an empty mode is not a policy an operator ever stated. `0015_profile_startup_mode` stamps every revision without a `startup` key `cold_boot`, which is a statement of the behavior those revisions already had rather than a default invented for them. The guard `WHERE NOT (spec_json ? 'startup')` makes it idempotent and keeps it off a revision that already states a mode. `TestProfileStartupModeConvergesOnFreshAndUpgradedSchemas` proves an upgraded database ends with exactly the `spec_json` a fresh one writes.

**The capability binding, derived from #57's compatibility key.** A `snapshot_resume` Profile is placeable only onto a Runner advertising the provider-neutral `snapshot-resume` capability, and the capability means one specific thing:

1. `RunnerCapabilities.snapshot_resume_ready` is a new runner-protocol field. It is *not* a registration prerequisite — a Runner without it registers normally and stays fully schedulable for `cold_boot`. `RecordRegistration` appends `snapshot-resume` to the Runner's capability set only when it is reported.
2. The Runner reports it only when `SECONDBOX_RUNNER_SNAPSHOT_TEMPLATE_CACHE_ROOT` is configured, the jailer is required (`MicroVMAllowUnjailed` false, because a resume is impossible unjailed), and `SnapshotTemplateCache.HasTemplateForBundle` finds a published template whose key's `artifactVersion`, `architecture`, `runtimeBundleDigest`, and `toolchainBundleDigest` equal the Runner's own verified signed bundle.
3. Those four fields are the exact bridge to the Profile. A Profile pins `runtimeBundleDigest` and `toolchainBundleDigest`; the control plane resolves them through the asset catalog into the `SignedAssetReference`s the Assignment carries; `matchSignedAssignmentComponent` already refuses any Assignment whose component digests differ from the Runner's local manifest. So a template keyed to this Runner's bundle is a template compatible with every Profile this Runner can serve at all. That is why the capability is bundle-scoped rather than Profile-scoped, and why no separate shape advertisement is needed: the remaining compatibility-key fields — memory size, vCPU count, workspace size, process limit, CPU template, host CPU fingerprint — are already bounded by the Runner's `sandbox_max_*` capacity admission and are re-proved exactly by `SnapshotTemplateCache.Resolve` on the start path, which is a cache miss rather than a best-effort restore. A shape mismatch is therefore a typed start failure, not a mis-placement.
4. The capability is a hard filter in *both* placement implementations, because they do not share code: `scheduler.compatible` through `Requirements.RequiredCapabilities`, and `runnerPlacementCompatible` for the initial home choice. A Sandbox never leaves its home Runner, so the Runner chosen at creation must satisfy the mode for every Instance it will ever host.

**Two-tier refusal.** A pool's operator-declared `capabilities_json` is the operator's statement of what the pool serves. A `snapshot_resume` Profile aimed at a pool that does not declare `snapshot-resume` is a standing incompatibility, not a shortage: `ensureCompatibleRunnerPool` returns `ports.ErrStartupModeUnsupported`, mapped to the new non-retryable `startup_mode_unsupported` problem code (409). A declared pool with no currently advertising Runner keeps the existing retryable refusals, because a Runner may still materialize the template.

**Operator surface.** `snapshot_template_cache_root` is a required runner manifest key, validated as an absolute Runner-host path and constrained to `/var/lib/secondbox-runner` for same-host Compose placement, exactly like `firecracker_jail_root`. It is deliberately required rather than optional: the golden memory file is hard-linked into every jail, so the cache must share a filesystem with the jail root, and that is a decision an operator makes rather than one the code guesses. The scaffold (`secondbox-deploy runner-template`), its byte-identical documentation block, the same-host Compose overlay, the scenario Compose file, both container entrypoints, and the runtime-config path check all carry it.

**No silent cold boot.** The Assignment now carries `ProfileRequirements.startup_mode`, and `ValidateAssignment` refuses a mode this Runner cannot honour before creating any Workspace, TAP, or jail. Today `snapshot_resume` is refused with `ErrSnapshotTemplateUnavailable`, because the production resume start path is the next item. That refusal is the point: a `snapshot_resume` Profile that quietly cold booted would be exactly the fallback this plan forbids, and the Sandbox would come up without the identity-neutral template, the shared golden memory inode, or the one-time assignment bind that define the mode. When `createAndStartResume` lands it replaces that one refusal, which is why the gate is a fork point rather than a stub.

The consequence, stated plainly: an operator can now declare, place, and refuse a `snapshot_resume` Profile end to end, and cannot yet start one. In a production deployment no Runner advertises the capability at all, because templates are still built by the qualification harness rather than by a Runner, so the observable behavior is a typed refusal at admission. Nothing about the mode is dormant — every branch above is reachable and tested — but the mode does not yet produce a running Sandbox.

## Remaining work after the jailed resume gate, 2026-08-07

Tasks 1, 2, 3, and Task 4 minus its generation-time forbidden-material scan are complete, the jailed resume gate that blocked everything downstream is qualified, and the `snapshot_resume` Profile class and its operator surface are landed. What remains, in the order it must be done:

1. **The production resume start path.** `createAndStartResume` alongside `createAndStartCold`, forking below `WorkspaceStore.Open` where the plan says it does, with the same jailer UID, guest IP, TAP, host policy, instance registration, reaper, and teardown. It replaces the one refusal in `validateAssignmentStartupMode`. The jailed gate proves the composition works and how long each stage takes; what it does not carry is TAP creation, host network policy, guest network identity activation, and runtime secret delivery, because it resumes with no network device.

   **The open question this item must settle first**, before writing the start path: whether a resumed guest can be given a network device at all, or whether the template must be captured *with* one. Firecracker v1.16.1's `SnapshotLoadParams` carries `network_overrides`, which rebinds a snapshotted interface to a new TAP — it does not add an interface that the VM state never recorded, and `PUT /network-interfaces` is pre-boot only. The fixed architecture already assumes the answer ("the template includes a network device with its link down"), and the drive finding in #57 is the same shape: a restored device must exist in the recorded VM state, and only its host-side binding is overridable. The jailed gate resumed with no NIC, so this is asserted rather than measured. The next pass must confirm it against the pinned VMM's API and record the finding in this plan either way, because it decides whether the template builder changes.
2. **Template generation as a runner operation.** Templates are still built by the qualification harness, so in a production deployment no Runner advertises `snapshot-resume` and every `snapshot_resume` Profile refuses at admission. A Runner that advertises resume capacity must materialize the template itself, including Task 4's generation-time scan for forbidden identity and secret material.
3. **Tasks 5, 6, 7, 8, and the rest of Task 9**, including the end-to-end qualified scenario gate that turns the 118 ms projection above into a measurement.

The `identityNeutralTemplate` and `jailedResume` fields in the evidence are the honest markers of where the work is, and both are now `true`. The `unjailedResumeRefusal` field stays in the template-lifecycle evidence, where it still proves an unjailed resume fails closed before staging a file; it is no longer standing in for a jailed number nobody had.

## Non-goals

- Do not trim the rootfs, remove packages, reduce the standard toolset, or treat image size reduction as the startup strategy.
- Add no placeholder backend, dormant restore function, feature flag, or stub resume branch. Each task lands as a working, tested path or it does not land.
- Do not turn public Workspace Snapshots into VM-memory snapshots. Public Snapshots remain runner-local reflink copies of one Sandbox Workspace.
- Do not capture a warmed tenant VM, a warm tool lease, or an assignment-bound guest connection as the shared template.
- Do not automatically relocate a Sandbox, copy a Workspace across runners outside the operator relocation Operation, reconstruct missing local data, or add a non-reflink fallback.
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
- `just test-snapshot-resume` with the explicit signed bundle, KVM, Btrfs/XFS Workspace root, shapes, iterations, and absent evidence output paths. It runs both the low-level load floor and the composed template lifecycle
- `just test-snapshot-resume-jailed` with the explicit signed bundle, KVM, a cgroup v2 host, a Btrfs/XFS Workspace root, the concurrency rungs, and an absent evidence output path. It runs the privileged jailed gate and fails if resumed Instances stop sharing one golden memory inode, if their rootfs children or Workspace images stop being distinct, or if aggregate reads approach one memory image per Instance
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

- [x] Add required `startup` policy to `ProfileRevisionSpec` and generated SDKs, with `mode` explicitly `cold_boot` or `snapshot_resume`; update every built-in Profile, fixture, example, and caller rather than supplying an application default. `StartupMode` is a named contract enum so both SDKs emit typed constants; migration `0015_profile_startup_mode` stamps pre-field revisions `cold_boot`.
- [x] Pin `startup.mode` in immutable Profile revisions and carry it through scheduling as a provider-neutral requirement. It reaches `scheduler.Requirements.RequiredCapabilities`, `runnerPlacementCompatible`, and the Assignment's `ProfileRequirements.startup_mode`.
- [x] Advertise provider-neutral snapshot-resume readiness and compatible shape capacity from runners. Schedule a `snapshot_resume` Profile only to its home runner and only when the exact template is ready. Shape capacity is not separately advertised, and the reason is recorded in the capability binding below.
- [x] Add typed retryable errors for template generation in progress or temporarily unavailable, and a typed non-retryable incompatibility error for a Profile shape no runner can support. Non-retryable `startup_mode_unsupported` (`ports.ErrStartupModeUnsupported`) for a pool that never declares resume capacity; the existing retryable `execution_node_unavailable`/`home_runner_unavailable` for a declared pool with no advertising Runner; retryable `ErrSnapshotTemplateUnavailable` at the Runner.
- [x] Prove public schemas contain no backend, VMM, host path, snapshot-cache, CPU, TAP, or vsock implementation fields, and prove no `ephemeral` Profile was introduced. `tests/contract/openapi_contract_test.go` pins both.
- [x] Run `just test-contract`, SDK tests, Profile validation tests, and scheduler tests.

### Task 3: Split template readiness from assignment-bound guest negotiation

- [x] ~~Introduce a new guest protocol generation~~ superseded by the settled decision above: identity reaches a resumed guest through `POST /assignment/bind` on the existing host-only vsock control endpoint, one time and generation-fenced. `contracts/guest/v1` is untouched and its frozen descriptor and fixtures are unchanged.
- [x] Remove per-Sandbox identity from template boot arguments. `secondbox.template_mode=1` carries only the compatibility-keyed control and protocol vsock ports; `runner/scripts/microvm-image/tool-entrypoint.sh` execs the template branch before it reads any identity argument, and `runner/scripts/entrypoint_contract_test.go` pins that ordering.
- [x] Prove template readiness without installing identity. The template's readiness is its control endpoint answering `/heartbeat`, which reports no Instance or Sandbox ID until the bind. Build ID and signed digests are proved at bind time against the runner's assignment, not negotiated by the template.
- [x] ~~Add an explicit prepare-for-template-snapshot request~~ unnecessary as specified, and recorded here rather than implied away. The capture point is structurally quiescent: an identity-neutral guest has never accepted a protocol connection, holds no session or connection state, has no outbound frames, and leaves `/dev/vdb` unmounted, so there is nothing to drain, freeze, or clear. `runner/scripts/microvm-image/init` skips the Workspace mount in template mode for exactly this reason.
- [x] Make the resumed guest reject exec, PTY, file, port, and workspace operations until hardening and the one-time assignment bind both succeed. The protocol listener is bound before capture but refuses every connection until the bind, and both the control endpoint's Workspace handlers and `/tool/exec` fail closed with "guest has no Workspace until its assignment bind".
- [x] Prove a second bind, a bind before hardening, an incomplete identity, an unavailable Workspace, and a stale generation fail closed. `runner/internal/guest/assignment_bind_test.go` covers each; the stale-generation case is fenced by the unchanged `Hello` binding check against the bound identity.
- [x] Run guest protocol, frozen descriptor, and upgrade-compatibility tests. Unchanged and passing, because no protocol generation was added.

Remaining in Task 3: nothing. The identity-neutral template is qualified below.

### Task 4: Build and seal identity-neutral templates

- [x] Boot the full signed bundle in template mode, reach the quiescent point, pause the VM, and create full VM state and memory snapshots. `TestSmokeSnapshotResumeTemplateLifecycle` does this against a real signed boot.
- [x] Capture and seal the coherent post-boot rootfs image alongside memory and VM state, with an unmounted sterile Workspace device preserving the device topology.
- [x] Never call `CreateGoldenSnapshot` against a tenant Instance for this purpose. The template path is separate and the diagnostic create-only behavior is unchanged.
- [x] Create a template manifest carrying the complete compatibility key and the digest and size of VM state, memory, and post-boot rootfs. It is not separately signed: the runner has no signing key, so the key carries the signing-key fingerprint and signed manifest digest of the bundle the template was built from.
- [x] Durably sync template files and their parent directory and publish atomically by one rename.
- [x] Prove deterministic compatibility identity independent of output paths and runner-local names.
- [ ] Scan the capture for forbidden identity and secret material and fail generation if the guest has mounted a tenant Workspace, configured a tenant IP, retained a live connection, or written runtime secrets. The template is structurally free of all four, but the generation-time scan that proves it is not implemented.

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
- [ ] Keep the Sandbox on its current home and unavailable when its home runner or exact template is absent. Never relocate automatically, reconstruct an empty Workspace, or use another runner's local cache as authority for the Workspace.
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
