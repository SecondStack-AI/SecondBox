# Workspace durability

A Workspace is durable Sandbox state. An Instance is disposable compute. Portable immutable checkpoints in S3-compatible storage and exclusive runner-local active materializations are distinct layers.

## Storage model

PostgreSQL records Workspace identity, active assignment, generation, last committed checkpoint, content hash, byte size, compatibility metadata, retention, and garbage-collection state. Object storage contains immutable checkpoint objects addressed by service-owned keys. A Snapshot is immutable PostgreSQL metadata that retains one of those verified checkpoint objects as a disk-state root without duplicating bytes. Public resources expose checksum and size, never bucket credentials or object keys.

A runner materializes the last committed checkpoint into local storage only after receiving a current assignment. That active image is writable by exactly one Sandbox generation and fenced Assignment. Runner-local storage is a cache and active working copy; it is never the only claimed durable copy.

## Checkpoint publication

A stopped-state checkpoint proceeds in this order:

1. reject new mutation and drain admitted data-plane work;
2. obtain the guest freeze token and flush filesystem state;
3. stop or otherwise prove the Instance cannot write;
4. stream ordered checkpoint chunks over the fenced Runner connection without exposing a runner-local path or object-store credential;
5. upload content under the control-plane-selected immutable object identity with checksums, byte counts, source generation, profile revision, image identity, guest generation, and format version;
6. move PostgreSQL evidence through `staging`, provider/content `verified`, and `published`;
7. transactionally make the published metadata the current committed checkpoint;
8. release the active materialization and thaw only when compute remains authorized.

An interrupted upload is unreachable staging data and is garbage-collected. Metadata is never published before byte verification. A published manifest never points to mutable object content.

The checkpoint effect persists checkpoint ID, opaque storage identity, Assignment fence, deadline, retry bound, command attempt, and sanitized terminal evidence. Chunk offsets are strict and replayed content must be byte-identical. The private control-plane spool survives process restart, and immutable put plus publication are idempotent for the same checksum and size.

A command that was delivered but produced no terminal before its effect deadline is not left waiting indefinitely. Reconciliation atomically expires that command, creates a uniquely identified retry with a renewed bounded deadline, and increments durable retry evidence. Exhaustion records a terminal failure class and moves the Sandbox lifecycle to failed instead of silently proceeding without a committed checkpoint.

The object-store port is provider-neutral and exposes immutable put, verified head/get, and delete. Its S3-compatible implementation uses explicit endpoint, region, static credentials, path-style selection, retry bound, HTTP timeout, private temporary directory, and maximum object size. Uploads and restores stream through private temporary files while hashing, so checkpoint size does not become control-plane memory use. No ambient AWS configuration, instance metadata, or credential chain participates.

Stop durability is selected only by the Sandbox's pinned immutable ProfileRevision. With `checkpoint.onStop=false`, reconciliation drains and stops the Instance without publishing its latest active materialization; any older published Workspace checkpoint remains the restart root. With `checkpoint.onStop=true`, reconciliation publishes the current generation before stopping. A later start uses that same pinned revision and materializes the current published root, or an empty Workspace when no root exists. Profile names and a Profile's newer current revision do not alter these decisions.

Running checkpoints are rejected in v1 unless the lifecycle has first completed the same bounded drain-and-freeze sequence and proved no writer remains. Snapshot creation requires a stopped Sandbox, its current published checkpoint, the current Sandbox revision, application lifecycle scope, and an idempotency key. Listing and inspection require application read scope. A Snapshot is an immutable named projection of committed disk state. It does not preserve RAM, processes, Leases, sockets, PTYs, port sessions, or in-memory command state.

## Restore and relocation

Start verifies checkpoint reachability, checksum, format, architecture, ProfileRevision, execution assets, and guest-protocol compatibility before allocating mutable local state. The control plane reads the current published object through its private object-store authority and sends a fenced restore-begin frame followed by strict-offset chunks and an end marker. The Runner persists those bytes in a private staging file, verifies the declared size and SHA-256, copies the verified image into a new generation-bound active image, and only then boots the Instance. The Runner receives neither object-store credentials nor provider URLs.

A stopped successfully checkpointed Sandbox may resume on any compatible Runner. The old Runner's materialization must be released before the stop transaction advances the Workspace generation. Scheduling then creates a new Assignment and a new exclusive materialization on the selected Runner; PostgreSQL rejects another `preparing` or `ready` materialization for the same Workspace. The portable checkpoint is copied into that Runner's private generation image rather than attaching or sharing the old Runner's mutable local image.

V1 has no live migration, active-active workspace access, or transparent process continuation after Runner loss. If a Runner disappears, heartbeat expiry alone leaves the generation and exclusive materialization authority unchanged. Only exact termination proof permits the control plane to terminate old-generation authority and advance the Workspace generation. The last committed checkpoint remains authoritative and is copied into a new materialization on a different compatible Runner; the replacement never attaches the lost Runner's mutable image. Uncheckpointed writes on that image are not claimed durable.

## Artifacts and retention

Artifacts are separate immutable objects with Project ownership, media type, checksum, size, source Sandbox generation, and expiry. Artifact APIs do not expose arbitrary workspace or runner paths.

Project and profile retained-byte quotas cover unique checkpoint and Artifact bytes with transactional reservation; Project, Profile, and pinned ProfileRevision limits separately bound active Snapshot records. A Snapshot does not reserve the same immutable checkpoint bytes twice, but it extends their reachability until its retention ends. Retention and garbage collection follow reachability: a current Workspace checkpoint, retained Snapshot, or pending restore prevents deletion. Artifact downloads copy and verify the complete retained object into bounded control-plane staging before exposing response bytes, so provider garbage collection cannot invalidate a response already in progress. Deletion marks metadata first, removes objects idempotently, and records terminal evidence. Missing reachable bytes are a hard integrity failure, not an empty workspace fallback.

Checkpoint and Artifact admission share Project and Profile retained-byte serialization. Checkpoint `staging` and `verified` bytes remain quota reservations until publication or cleanup, so concurrent uploads cannot each observe the same capacity. A rejected checkpoint records `quota_failed` metadata without publishing or becoming a restore root. Expired `staging`, `verified`, `integrity_failed`, and `quota_failed` records enter the same bounded two-phase garbage queue as unreachable published checkpoints; cleanup subtracts Workspace retained bytes only for objects that were actually published.

Garbage collection is two phase. PostgreSQL first marks expired unreachable objects, waits the configured grace, rechecks roots, and moves due records to `garbage_deleting`. New materializations can reference only the current `published` checkpoint and therefore cannot race a deletion claim. The collector deletes provider bytes before recording `deleted`; failed deletes remain claimed for an idempotent retry. Checkpoint completion also releases Workspace retained-byte evidence transactionally.

## Backup consistency

Backup establishes a quiesced metadata boundary, fences publication, records the PostgreSQL recovery position, and writes a manifest of every reachable object and checksum. Restore verifies database state and object reachability, then starts on a fresh Runner to prove portability. Runner-local active caches are not backup inputs.

See [Domain and lifecycle](domain-lifecycle.md), [Recovery and reconciliation](recovery-and-reconciliation.md), [Security](security.md), and [Compatibility policy](compatibility-policy.md).
