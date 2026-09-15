---
title: Runner-Local Copy-on-Write Workspaces
date: 2026-07-29
status: completed
owner: SecondStack
provenance: SecondBox runner-independence simplification design, 2026-07-29
---

# Plan: Runner-Local Copy-on-Write Workspaces

## Outcome

Replace portable, S3-backed workspace checkpoints with a simpler model in which each Sandbox has one authoritative home runner and that runner's local copy-on-write filesystem is the authoritative durable workspace store. SecondBox continues to support multiple runner machines and keeps the control plane separate and unprivileged. Ordinary lifecycle and automatic recovery cannot move a Sandbox between runners; the later operator relocation Operation requires a stopped Sandbox and intact source. If its home runner is offline, the Sandbox is unavailable rather than restored elsewhere.

Keep the public Snapshot resource while changing its implementation to runner-local reflink snapshots. Snapshot create, delete, and in-place restore are asynchronous durable operations. Remove full-image hashing, full-image upload/download, cross-runner materialization, and workspace retained-byte accounting. S3-compatible storage remains for Artifacts and other immutable assets; external backup of runner-local workspace storage is an operator responsibility outside this implementation.

The runner-local workspace format is a raw ext4 image stored on a reflink-capable host filesystem such as XFS or Btrfs. The runner must prove at startup that the configured active-workspace and snapshot locations support `FICLONE` on the same filesystem and must fail readiness if they do not. There is no byte-copy fallback and no dm-thin alternative. The provider-neutral compute boundary receives a private workspace attachment handle, allowing a future smolvm backend to attach the same raw ext4 image without changing public APIs or control-plane lifecycle semantics.

## Fixed architecture

- `Sandbox` remains the durable public resource. `Instance` remains replaceable compute fenced to one Sandbox generation.
- Every Sandbox receives one `home_runner_id` during creation. That value is internal, never appears in public schemas, and changes only through the explicit stopped-Sandbox relocation Operation.
- The home runner's local workspace is the only authoritative durable workspace copy known to SecondBox. PostgreSQL stores desired state, the home-runner decision, generations, assignments, operation state, and snapshot metadata, but never a host path.
- A stopped Sandbox may restart only on its home runner. Runner draining prevents new Sandbox homes from being placed there. Runner loss does not trigger relocation, an empty workspace, or checkpoint recovery.
- Active workspace versions and snapshots are raw ext4 image files on one reflink-capable filesystem. An atomically replaced, fsynced local manifest selects the current writable version; Snapshots are read-only-by-policy reflink clones.
- Snapshot create and restore require a stopped Sandbox. Snapshot delete may run while the Sandbox is active because the immutable Snapshot is not attached to compute, but it must not race a restore that references it. All three are idempotent asynchronous Operations coordinated through durable effects and runner receipts.
- Restore applies a Snapshot in place to its original Sandbox only. It advances the Sandbox generation, stages a new writable reflink child, atomically swaps the current-image manifest, retains the previous image only until the control-plane commit is acknowledged, and creates no automatic safety Snapshot.
- PostgreSQL admits at most one local workspace mutation at a time. Its durable mutation barrier is checked by start, every explicit or reconciler-driven stop path, Snapshot, restore, and delete transactions so a pending restore cannot race another lifecycle request.
- Every workspace mutation transaction acquires rows in the global order Sandbox, then Workspace, then Snapshot when present. No endpoint or reconciler may introduce a different lock order.
- Every ordinary stop durably advances the runner-local workspace generation before PostgreSQL's existing `finish_stop` transition advances the Sandbox and Workspace rows. Replayed stop completion converges on the same generation.
- Public Snapshot metadata has no SHA-256. `sizeBytes` means logical image capacity, not physical blocks uniquely attributable to the Snapshot.
- The control plane never mounts a workspace or calls `FICLONE`. It forwards Workspace image bytes only in memory for the explicit stopped-Sandbox relocation Operation. Only the runner accesses local workspace paths and privileged compute facilities.
- Firecracker remains the only implemented backend. This work preserves a provider-neutral local-workspace port and compute-backend conformance suite but adds no smolvm implementation, placeholder, feature flag, or fallback.

## Non-goals

- Do not implement live migration, automatic relocation, general cross-runner copy, cross-Sandbox cloning, or Snapshot export/import. The explicit stopped-Sandbox relocation Operation is the sole control-plane Workspace streaming path.
- Do not implement a SecondBox-managed backup transport, backup catalog, or automatic runner replacement. Operators back up and restore the runner filesystem and runner identity through separate infrastructure.
- Do not merge the control plane and runner, grant the control plane host-storage access, or change the outbound authenticated runner connection model.
- Do not expose a Sandbox's home-runner linkage, host paths, local storage references, filesystem type, reflink details, Firecracker, or a future backend name through Sandbox, Workspace, Snapshot, or Operation schemas. Preserve the separate administrative Runner API.
- Do not retain the existing checkpoint API, checkpoint compatibility path, dm-thin backend, SHA compatibility field, or full-copy fallback.
- Do not implement smolvm in this plan. A future adapter may make the small mandatory smolvm API change needed to attach an externally supplied raw ext4 workspace and mount it by filesystem label or UUID.

## Validation Commands

Run the focused commands listed in each task while implementing it. Before handoff, run all repository-wide gates below from the repository root.

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- `just test-non-kvm`
- `just test-deployment`
- `just preship`
- `git diff --check`
- `just test-firecracker` on a qualified KVM runner whose workspace root is on supported reflink storage
- `just test-multirunner` against two qualified runner machines after that target is introduced (the independent second-host run was waived for this plan's closeout on 2026-07-29; the recorded single-host Btrfs/KVM run remains the qualification evidence)

### Task 1: Replace the public checkpoint contract with local Snapshot operations

Change the public contract first so the implementation has one intended lifecycle rather than preserving portable-checkpoint compatibility. Ordinary Sandbox stop/start becomes sufficient to retain workspace data. Snapshot create, delete, and restore are the only explicit point-in-time workspace operations, and all three use the existing Operation polling model.

- [x] Remove `POST /v1/sandboxes/{sandboxId}:checkpoint`, the `checkpoint` Operation kind, and checkpoint-specific request/response schemas from `contracts/openapi/v1/secondbox.openapi.json`.
- [x] Change Snapshot creation and deletion from immediate `201`/`204` responses to `202 Operation` responses. Require `Idempotency-Key` for both, retain the parent Sandbox `If-Match` precondition for create, and deliberately omit `If-Match` from delete because Snapshot has no independent revision/ETag.
- [x] Add `POST /v1/sandboxes/{sandboxId}:restore` with a body containing the logical `snapshotId`; return `202 Operation`, require `Idempotency-Key` and `If-Match`, and document that restore is stopped-only and limited to a Snapshot belonging to that Sandbox.
- [x] Add provider-neutral Operation kinds for Snapshot create, Snapshot delete, and Snapshot restore, with stable typed errors for running Sandbox, wrong Sandbox, unavailable home runner, missing local Snapshot, storage incompatibility, and stale revision/generation.
- [x] Extend Operation with an optional Snapshot result/projection. Reserve and return the stable Snapshot ID in the create Operation from its first response, and populate the ready Snapshot on success so clients never have to discover a non-unique Snapshot by listing.
- [x] Remove `sha256`, checkpoint IDs, compatibility metadata, runner identity, host storage references, and backend terminology from the public Snapshot schema. Retain logical `sizeBytes`, creation time, optional expiration, bounded metadata, and lifecycle state only where clients need it.
- [x] Remove backend selection from public Profile/ProfileRevision schemas and generated clients. Replace backend-specific `firecracker`, `kvm`, storage, and virtualization capability strings in public Runner/RunnerPool projections with provider-neutral SecondBox feature vocabulary; keep backend resolution private to control-plane scheduling and runner execution while Firecracker remains the only implemented backend.
- [x] Replace the profile `CheckpointPolicy` with an explicitly configured provider-neutral Snapshot/Artifact retention policy. Retain `snapshotLimit`, introduce an explicit `snapshotRetentionSeconds`, retain artifact retention, and remove `onStop` and checkpoint retention semantics.
- [x] Remove workspace bytes from the public retained-byte quota. Rename or redefine the remaining quota field as artifact-only rather than silently changing its meaning, and update API descriptions and typed quota errors.
- [x] Regenerate Go and TypeScript transport types and clients; delete generated and handwritten checkpoint methods rather than retaining aliases.
- [x] Update CLI surfaces to remove `sandboxes checkpoint` and `checkpoints create`, and define Snapshot create/list/get/delete/restore commands that wait for or print the returned Operation consistently with other async commands.
- [x] Add OpenAPI fixture and negative tests proving Snapshot has no workspace-image SHA/checkpoint fields; Sandbox, Workspace, Snapshot, and their Operations expose no home-runner ID, host path, or storage reference; and Profile/ProfileRevision/Runner/RunnerPool plus all generated representations expose no KVM, reflink, ext4, Firecracker, smolvm, or other backend/host-storage detail. Preserve SHA fields for Artifacts/bundle digests and runner IDs in the administrative Runner API.
- [x] Run `just verify-generated` and `just test-contract`.

### Task 2: Replace portable workspace persistence with home-runner metadata

Make PostgreSQL describe durable ownership and operation intent without pretending to contain or locate workspace bytes. Replace the canonical pre-release baseline instead of carrying obsolete checkpoint and materialization tables through compatibility migrations. Cross-resource IDs remain logical strings without foreign keys or CHECK constraints.

- [x] Update `migrations/postgres/0001_secondbox.sql` and its schema fixtures to add authoritative internal `home_runner_id` ownership to `workspaces`, including explicit creation/readiness/deletion state and logical image capacity. Derive a Sandbox's home through its logical Workspace ID rather than duplicating the field on `sandboxes`.
- [x] Select field names that distinguish a runner's stable logical ID from private storage evidence; never persist a host path. Use a private opaque local reference only if the runner protocol cannot address storage deterministically by Workspace or Snapshot ID.
- [x] Remove `workspace_checkpoints`, `workspace_materializations`, current-checkpoint hash/size fields, checkpoint compatibility fields, retained workspace bytes, checkpoint garbage-collection state, and cross-runner source-checkpoint state.
- [x] Replace Snapshot persistence with local lifecycle metadata: Sandbox/Workspace identity, authoritative home runner, logical size, state, retention deadline, operation/effect correlation, timestamps, and optional private runner receipt/reference. Do not store a full-image digest.
- [x] Model Snapshot states needed for crash-safe asynchronous create/delete/restore, and make repository methods idempotent under duplicate API requests, duplicate effects, reordered runner results, and control-plane restart.
- [x] Add durable restore bookkeeping sufficient to distinguish requested, staged, runner-swapped, database-committed, finalized, and failed work without storing an image path.
- [x] Add one durable Workspace mutation slot containing kind, stable mutation/effect ID, optional public Operation ID, expected/current generation, and state. Acquire it while locking rows in the invariant order Sandbox → Workspace → Snapshot when present; make conflicting start, stop, Snapshot, restore, and delete transitions fail or wait deterministically. A pending start owns the slot from admission until ready or terminal failure so later reconciliation cannot race a Snapshot/restore admitted from stale stopped state.
- [x] Centralize the row-locking helper/order in the PostgreSQL store and add concurrent transaction tests for create/delete Snapshot, restore, start, every stop path, and Sandbox delete so no caller can invert Workspace/Snapshot acquisition or deadlock.
- [x] Delete checkpoint and materialization store interfaces and implementations in `internal/store`; introduce narrow home-workspace and local-Snapshot repository methods used by lifecycle code.
- [x] Update quota transactions so Snapshot count and Artifact bytes remain enforceable while workspace/Snapshot filesystem allocation is not charged as uniquely retained bytes.
- [x] Preserve assignment, generation, fencing, lease, audit, and Operation records. Add indexes for home-runner reconciliation and pending local-storage effects without adding physical foreign keys or CHECK constraints.
- [x] Add store conformance tests for ordinary-lifecycle home assignment, concurrent initial placement, Snapshot state transitions, restore recovery points, deletion retries, retention selection, and artifact-only quota accounting.
- [x] Run the PostgreSQL store tests and `just test-contract`.

### Task 3: Implement the runner-local reflink workspace store

Introduce a provider-neutral runner-owned `WorkspaceStore` below lifecycle and beside the compute backend. Its production implementation owns raw ext4 active images, immutable Snapshot clones, writable restore clones, atomic publication, runner-side locking, startup recovery, and local deletion. Callers receive opaque handles rather than paths.

- [x] Define a small private `WorkspaceStore` port for create/open, generation advance, Snapshot clone, restore stage/swap/finalize, Snapshot delete, workspace delete, inspect, and reconcile. Require operation IDs on every mutating method so retries return the same result.
- [x] Define a private opaque `WorkspaceHandle`/attachment value that a compute backend can consume without exposing a path outside the runner process or coupling lifecycle code to Firecracker.
- [x] Implement a deterministic, traversal-safe directory layout beneath one required absolute `SECONDBOX_RUNNER_WORKSPACE_ROOT`, separating versioned workspace images, the current-image manifest, Snapshot images, staged images, rollback images, locks, and durable operation receipts.
- [x] At runner startup, create/probe the configured directories, prove active and Snapshot files have the same device ID, issue a real `FICLONE`, verify source/destination mutation isolation, and fail readiness on unsupported storage or inconsistent mounts.
- [x] Create sparse raw ext4 images at the profile-resolved logical capacity, format them deterministically, fsync file and parent directory metadata, and atomically publish the active image.
- [x] Implement Snapshot creation with `FICLONE` to a temporary file followed by fsync and atomic rename. Never read the complete image, calculate a full-image digest, call a byte-copy helper, or fall back when reflink fails.
- [x] Implement restore preparation by reflinking a Snapshot into a new writable versioned image; implement the swap by fsyncing and atomically renaming a complete current-image manifest that names the new version and preserves the previous version as operation-scoped rollback state until finalize. Fsync the manifest's parent directory after rename and before recording or acknowledging the swap receipt.
- [x] Implement idempotent local generation advance for ordinary stop. After compute is detached, atomically replace the current-image manifest with the same image reference and next generation, fsync its parent directory, then persist a replayable receipt before acknowledging stop completion.
- [x] Persist runner-local receipts before acknowledging create, generation advance, delete, swap, or finalize. On restart, treat the current-image manifest plus receipts as authoritative evidence and safely finish or report interrupted operations.
- [x] Enforce one writer with a runner-side Workspace lock independent of control-plane correctness. Reject Snapshot creation and restore while a compute attachment remains open; allow deletion of an unattached immutable Snapshot after checking restore references.
- [x] Make Snapshot deletion and workspace deletion idempotent, bounded to validated exact paths, and safe in the presence of a staged or unfinalized restore.
- [x] Remove `ext4|dm-thin` backend selection, dm-thin configuration, pool management, and tests. Replace them with the single explicit reflink-root configuration and startup validation.
- [x] Add syscall abstraction unit tests plus real-filesystem qualification tests covering sparse creation, reflink-only behavior, copy-on-write isolation, atomic manifest publication, duplicate generation advance, duplicate operations, crashes at each rename/receipt boundary, locking, path validation, and disk-full errors.
- [x] Run the focused runner storage tests on tmpfs/local CI fakes and on an XFS or Btrfs qualification filesystem.

### Task 4: Replace workspace streaming in the runner protocol with local-storage commands

The control plane should coordinate local storage state, not carry disk bytes. Replace chunked checkpoint upload/download with small idempotent commands and durable results that refer only to logical resource IDs, generations, and operation/effect IDs.

- [x] Remove `RUNNER_FEATURE_CHECKPOINT`, `source_checkpoint_id`, checkpoint chunk/result messages, restore begin/chunk messages, and every protocol field used to stream portable checkpoint bytes. Protocol generation 2 relocation messages remain the sole stopped-Sandbox exception.
- [x] Add a provider-neutral local-workspace capability and versioned commands/results for workspace create/inspect/delete, generation advance, Snapshot create/delete, restore prepare/swap/finalize, and reconciliation.
- [x] Keep protocol payloads logical: Sandbox ID, Workspace ID, Snapshot ID, expected generation, fencing token, operation/effect ID, logical capacity, and result evidence. Do not transmit host paths or make the control plane understand runner-local handles.
- [x] Require every command to carry the home runner identity implicitly through its authenticated session and reject it if the target workspace is not locally owned by that runner.
- [x] Return stable typed failures for absent local data, active writer, stale generation/fence, unsupported reflink store, insufficient space, corrupt receipt, and conflicting replay.
- [x] Update runner session reconnect logic so unacknowledged local-storage results are replayed until the control plane durably records them.
- [x] Update capability negotiation and frozen protobuf fixtures; explicitly reject old runners that only implement portable checkpoints rather than retaining a mixed-mode compatibility path.
- [x] Delete checkpoint sender/receiver composition and streaming backpressure code from `internal/lifecycle`, `internal/runnercontrol`, and `runner/internal/runnercontrol`.
- [x] Add protocol tests for duplicate/reordered commands, disconnect before/after receipt, reconnect replay, stale fencing, wrong home runner, protocol skew, and payload checks proving no image bytes or local paths cross ordinary lifecycle messages.
- [x] Run protobuf generation verification and the runner-control conformance suite.

### Task 5: Pin each Sandbox to one home runner during creation

Change placement from per-Instance scheduling with portable materialization to one durable home selection. Creation succeeds only after the chosen runner has durably created the local Workspace; all later Instance assignments are constrained to that runner.

- [x] Split scheduler behavior into initial home placement and subsequent exact-home assignment. Use immutable profile requirements, compatible pool, health, drain state, storage readiness, free capacity, and deterministic tie-breaking only for the initial decision.
- [x] Persist the chosen `home_runner_id` transactionally before dispatching workspace creation, and make concurrent/replayed Sandbox creation converge on the same home.
- [x] Keep Sandbox creation asynchronous until the runner returns a durable workspace-create receipt; expose a typed failed Operation if local creation fails and reconcile ambiguous results on reconnect.
- [x] Never change the home runner during start, stop, restore, control-plane restart, ordinary runner reconnect, drain, or loss. Provide no internal scheduler fallback that selects another runner.
- [x] Constrain every Instance assignment to the home runner while preserving assignment IDs, generations, fencing tokens, capacity admission, and exactly-one-writer rules.
- [x] Make lifecycle admission lock and inspect the Workspace mutation slot. Acquire it for start before changing desired state and retain it through assignment/ready or terminal startup failure; a pending local mutation must prevent a conflicting start, stop, Snapshot, restore, or Sandbox delete from being accepted under the same stopped revision.
- [x] Before the reconciler dispatches any compute stop, transactionally acquire or adopt the Workspace mutation slot under a stable stop effect ID. Cover explicit stop and drain Operations plus idle timeout, maximum duration, guest shutdown, guest-agent/liveness loss, runner recovery, deletion, and every other decision that can lead to `finish_stop` without a public Operation.
- [x] Define drain semantics: a draining runner receives no new Sandbox homes and no new Instances; its existing stopped Sandboxes remain unavailable for start until the runner is undrained or an operator explicitly relocates an eligible Workspace.
- [x] Define offline semantics: retain the Sandbox and workspace metadata, report a stable home-runner-unavailable state/error, and do not advance to an empty workspace or another runner.
- [x] Reconcile returning runners by comparing authenticated runner identity, local Workspace inventory/receipts, current assignment, and generation. Treat missing or conflicting local data as an explicit operator-visible failure, not a recreation request.
- [x] Make Sandbox deletion wait for the home runner to stop compute and durably delete its Snapshots, active image, staged/rollback files, and receipts before finalizing the public tombstone. Keep deletion pending while the home runner is unavailable.
- [x] Document the operator recovery boundary: machine restoration must preserve or deliberately restore both stable runner identity and its workspace root; automatic rebind to a new runner ID is out of scope.
- [x] Remove locality scoring, portable materialization scheduling, and cross-runner recovery branches made unreachable by exact-home assignment.
- [x] Add lifecycle tests with two fake runners for deterministic initial placement, ordinary-lifecycle pinning, drain, offline/online recovery, missing local data, no automatic relocation, concurrent create/start, and multiple control-plane replicas.
- [x] Run scheduler, lifecycle, and runner-control integration tests.

### Task 6: Attach the local raw ext4 workspace through the compute port

Make compute consume a provider-neutral local attachment produced by `WorkspaceStore`. Firecracker continues to be the only implementation, but no lifecycle code should depend on Firecracker device naming or on a future backend's disk API.

- [x] Change the compute backend start contract to require a resolved local `WorkspaceHandle`/attachment, expected generation, and fence; remove source-checkpoint and materialization inputs.
- [x] Update the Firecracker adapter to resolve the private handle inside the runner and attach the existing raw ext4 image as the guest workspace without copying or reformatting it.
- [x] Preserve generation-bound attachment, jailer path preparation, guest mount validation, cgroup/network setup, and teardown ordering. Release the Workspace writer lock only after the VM and all host-side users are conclusively stopped.
- [x] Make ordinary stop flush and detach the active image, then execute the idempotent runner-local generation advance and return its durable receipt before `finish_stop` commits the matching next generation in PostgreSQL. Perform no Snapshot, hash, upload, retention update, or S3 request.
- [x] Hold the durable Workspace mutation slot from the stop-producing lifecycle transition through detach, local generation-advance receipt, and the `finish_stop` database transaction. A failure before local advance may clear the slot transactionally; once an advance receipt exists, retain and retry the slot until PostgreSQL commits the matching generation.
- [x] Reconcile a stop receipt when the runner advanced locally but the control-plane transaction did not commit; block start until PostgreSQL and the current-image manifest converge on the same generation.
- [x] Make start resolve the current-image manifest after runner or control-plane restart and reject startup when the image is missing, already locked, wrong-sized, unformatted, or inconsistent with the expected generation.
- [x] Remove streaming checkpoint and full-copy restore paths from `runner/internal/firecracker/manager.go`, `manager_launch.go`, `assignment_backend.go`, and related helpers. Retain unrelated Firecracker VM-state diagnostic snapshot code only if it is not part of Sandbox workspace persistence.
- [x] Extend the provider-neutral compute conformance suite so any future backend must attach the supplied raw ext4 workspace, preserve mutations across stop/start, reject absent attachment, and honor exclusive ownership.
- [x] Record the future smolvm integration contract in an architecture note: its current high-level `MachineSpec` creates `storage.raw`/`overlay.raw` itself and the guest assumes `/dev/vda`/`/dev/vdb`, so a future adapter must add a mandatory externally supplied raw ext4 disk, attach it through `krun_add_disk2`, and mount by filesystem label or UUID at `/workspace`. Add no current smolvm code or conditional branch.
- [x] Add real Firecracker tests that write data, stop through explicit stop, drain, idle timeout, maximum duration, guest shutdown, and liveness-loss paths, restart on the same runner, verify generation/data/filesystem integrity, and prove no object-store request or image-sized I/O occurs.
- [x] Run the compute conformance tests and `just test-firecracker` on a qualified host.

### Task 7: Implement asynchronous local Snapshot create and delete

Wire the retained public Snapshot resource through PostgreSQL effects, the runner protocol, and `WorkspaceStore`. The control plane owns authorization, idempotency, retention, and lifecycle state; the home runner owns all image operations.

- [x] Implement Snapshot create validation for ownership, stopped Sandbox, ready home Workspace, Snapshot-count quota, optimistic revision, and available home runner.
- [x] In one transaction, resolve idempotency, lock Sandbox/Workspace state, acquire the Workspace mutation slot, reserve the Snapshot ID/count, create the Operation with its Snapshot projection, and enqueue a durable runner effect without publishing the Snapshot as ready.
- [x] Have the reconciler dispatch local Snapshot creation to the home runner and finalize PostgreSQL state only after a durable runner receipt. Recover correctly if either side restarts before or after the receipt.
- [x] Return logical `sizeBytes` from the Workspace capacity and never calculate physical unique blocks or a full-image digest.
- [x] Implement Snapshot delete as an async Operation: resolve IDs, lock Sandbox → Workspace → Snapshot in the global order, acquire the mutation slot, mark deletion intent transactionally, block new restore use, dispatch idempotent local deletion, and finalize metadata only after the runner receipt.
- [x] Keep deletion pending while the home runner is unavailable. Do not report success or discard the only metadata needed to retry.
- [x] Implement expiration/retention selection as local Snapshot-delete effects. Enforce the explicit Snapshot count limit and retention duration without retained-byte accounting.
- [x] Release the Workspace mutation slot in the same transaction that finalizes Snapshot create/delete. Prevent deletion of a Snapshot referenced by a pending or unfinalized restore, and define deterministic conflict handling for simultaneous create/delete/restore requests.
- [x] Update list/get visibility rules so clients see stable lifecycle state without local references, and ensure expired/deleted resources follow the service's existing not-found/tombstone convention.
- [x] Add API and integration tests for idempotent create/delete, duplicate effects, stopped-only validation, count quota races, offline home runner, disk full, service restart, runner restart, expiry, and absence of SHA/path/backend data.
- [x] Run focused Snapshot API/lifecycle tests, `just test-contract`, and `just test-compose`.

### Task 8: Implement crash-safe in-place Snapshot restore

Restore replaces the stopped Sandbox's active workspace with a writable reflink child of one of its own Snapshots. Use a staged/swap/finalize protocol so retries are deterministic and the old active image remains available only until PostgreSQL commits the new generation.

- [x] In one transaction, resolve IDs, lock Sandbox → Workspace → Snapshot in the global order, and validate Snapshot ownership, ready state, stopped Sandbox, home-runner match, optimistic revision, no active writer, no conflicting Workspace mutation, and available home runner.
- [x] In that admission transaction, advance the Sandbox revision, acquire the durable Workspace mutation slot, create the restore Operation, reserve the next generation, and enqueue a prepare effect without making the new generation current. Start, stop, Snapshot mutation, and Sandbox delete must observe the slot or new revision and cannot race the restore.
- [x] Dispatch restore prepare to create and durably publish the writable staged reflink child while leaving the current active image untouched.
- [x] After a prepare receipt, dispatch the fenced swap. The runner must atomically publish a current-image manifest naming the new version/generation, durably record the result, and retain the previous version as operation-scoped rollback state before acknowledging.
- [x] Commit the new Sandbox generation, active-workspace evidence, audit entry, successful Operation, and mutation-slot release in PostgreSQL only after the durable swap receipt; advance the revision again for the observable completion.
- [x] Dispatch a finalize effect after database commit to delete the rollback image and operation staging data. Treat finalize as idempotent cleanup that may retry indefinitely without reverting the committed restore.
- [x] On control-plane or runner restart, reconcile every prepare/swap/commit/finalize boundary from PostgreSQL state and runner receipts. Never guess based only on file presence and never perform the swap twice with different results.
- [x] For every failure before the current-manifest rename, leave the original active image unchanged, idempotently delete any published staged version, record an abort receipt, and only then transactionally fail the Operation/release the mutation slot. If the runner cannot yet prove that abort cleanup, keep the Operation pending. If swap has occurred but the control-plane commit cannot yet be proven, preserve both the new active and rollback images, retain the mutation slot, and report/retry rather than automatically choosing one.
- [x] Create no implicit safety Snapshot. Once finalize succeeds, the pre-restore active image is deleted and recoverability depends only on explicit Snapshots and the operator's external backup.
- [x] Fence all pre-restore assignment, Instance, lease, stream, and operation authority through the generation advance even though restore is stopped-only.
- [x] Add fault-injection tests at every receipt, manifest rename, dispatch, and database-commit boundary; add tests for stale revisions, wrong Sandbox, running Sandbox, start/stop/delete after restore admission, delete/restore race, multiple restore requests, offline/reconnecting runner, missing Snapshot file, and finalize retry.
- [x] Add a real Firecracker test that writes state A, creates Snapshot A, writes state B, restores A, restarts, observes A, and proves stale generation authority is rejected.
- [x] Run restore lifecycle tests, `just test-compose`, and `just test-firecracker` on a qualified host.

### Task 9: Remove portable-workspace S3, retention, and backup machinery

Delete the old path completely after local lifecycle tests pass. Preserve S3-compatible storage for Artifacts and immutable execution assets, but ensure neither control-plane nor runner composition can upload, download, garbage-collect, or account for workspace images.

- [x] Delete checkpoint sender/receiver, checkpoint object keys, workspace checkpoint store methods, compatibility/hash helpers, checkpoint garbage collection, materialization cleanup, and unreachable reconciliation branches.
- [x] Refactor object-store composition and configuration so S3 remains explicitly required only by features that still use it, especially Artifacts; do not remove or weaken Artifact hashing, retention, authorization, or garbage collection.
- [x] Replace workspace retained-byte metrics and quota labels with meaningful Artifact and runner-storage signals. Keep metric cardinality fixed and do not label host paths, Sandbox IDs, or Snapshot IDs.
- [x] Update runner disk-pressure reporting to use filesystem-wide capacity/reservation evidence for the local workspace root. Admission may reject new homes or Snapshot operations, but must not claim exact per-Snapshot physical ownership.
- [x] Remove `just test-backup-restore` and the fresh-runner checkpoint restoration test. Add a deployment test proving a control plane with S3 available cannot automatically relocate or reconstruct a Sandbox whose home runner is absent.
- [x] Update Compose, systemd, environment validation, examples, support bundles, and operations documentation to require the reflink workspace root and remove dm-thin/checkpoint settings.
- [x] Document external backup requirements without implementing them: stop or otherwise quiesce affected Sandboxes, back up the home runner's workspace filesystem and stable identity consistently, and validate restoration through the operator's chosen system.
- [x] Update threat, recovery, and durability documentation to state plainly that loss of an unbacked home-runner filesystem loses those Sandboxes, while control-plane or S3 recovery alone is insufficient.
- [x] Update SDK, CLI, quick-start, profile, lifecycle, runner, storage, backup, and failure-mode documentation. Remove every claim of portable checkpoints or fresh-runner recovery and identify the explicit stopped-Sandbox relocation Operation as the only cross-Runner exception.
- [x] Use `rg` checks to remove obsolete exported names, environment variables, protocol messages, schema fields, and error prefixes rather than retaining compatibility shims.
- [x] Run `just verify-generated`, `just test`, `just test-contract`, `just test-compose`, and `just test-deployment`.

### Task 10: Qualify the simplified multi-runner system

Close the migration only after both correctness and the intended simplification are demonstrated. Qualification must prove that multiple independent runner machines still work, that each Sandbox remains pinned, and that local COW operations never degrade into full-image copies or hidden portability.

- [x] Add `just test-multirunner` and a documented two-runner fixture with distinct stable identities and distinct reflink workspace roots.
- [x] Prove initial placement can distribute different Sandboxes across runners, while every start, stop, Snapshot, restore, and delete for each Sandbox is sent only to its current home.
- [x] Prove runner drain and loss never automatically relocate a Sandbox, create an empty replacement, or read a workspace image from S3; verify the API reports the typed unavailable state and recovers when the same runner returns.
- [x] Prove control-plane restart, PostgreSQL restart, runner restart, duplicate delivery, and network partition at every local-storage operation converge through durable state and receipts.
- [x] Prove Snapshot create and restore are reflink operations by combining syscall instrumentation with allocation/mutation checks; assert there is no fallback code path that performs image-sized reads or writes.
- [x] Prove one active writer under concurrent API replicas, stale runner sessions, stale assignments, stale generations, and delayed results.
- [x] Prove Snapshot restore changes only the original Sandbox, advances generation, invalidates stale authority, preserves the chosen Snapshot, deletes temporary rollback state after commit, and creates no extra Snapshot.
- [x] Prove Artifact upload/download, retention, hashing, backup, and garbage collection remain functional through S3 independently of workspace persistence.
- [x] Run public-schema leak tests and inspect generated SDKs to confirm home-runner identity, paths, storage references, and workspace-image SHA are absent from Sandbox/Workspace/Snapshot operations, and backend/host-storage details are absent from Profile/ProfileRevision/Runner/RunnerPool and generated types, while administrative Runner IDs and Artifact/asset SHA fields remain intact.
- [x] Run `just verify-generated`, `just test`, `just test-contract`, `just test-compose`, `just test-non-kvm`, `just test-deployment`, `just preship`, and `git diff --check` from a clean checkout.
- [x] Record qualified `just test-firecracker` and `just test-multirunner` filesystem/mount and exact build-artifact evidence. The project owner accepted the recorded single-host Btrfs/KVM evidence and waived the independent second-host run for this plan's closeout on 2026-07-29.
- [x] Update `AGENTS.md` and architecture decision records so future changes preserve home-runner pinning, reflink-only local durability, provider-neutral attachment, Firecracker-only implementation, and the prohibition on placeholder backends.
- [x] Delete superseded design text and mark this plan complete only after all portable-workspace code and claims are gone.
