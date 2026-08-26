# Task 3L evidence: portable WorkspaceStore with qualified Linux driver

Date: 2026-08-13

Host: `deimos` (`linux/amd64`)

Result: pass

## Implemented boundary

- The common `Store` retains ownership of logical IDs, path confinement, manifests, generation
  fences, receipts and replay, snapshot/restore/delete state, retention cleanup, relocation framing,
  deterministic filesystem identity, and atomic publication.
- The private `platformDriver` owns cloning, descriptor-only format and UUID operations, writer
  locks, directory durability, child descriptor addressing, descriptor opening, and descriptor-
  authorized jail links.
- The Linux driver uses `FICLONE`, nonblocking exclusive `flock`, directory `fsync`,
  `/proc/self/fd/<n>`, and `linkat(..., AT_SYMLINK_FOLLOW)` from the held descriptor. There is no
  byte-copy fallback. Failed clones preserve the underlying error and also report
  `ErrStorageIncompatible`.
- Production ext4 creation invokes the pinned local `secondbox-microsandbox-helper` with an
  inherited control socket and Workspace descriptor. The helper formats the explicit 64 MiB-or-
  larger capacity and deterministic UUID, disables lazy initialization, runs `e2fsck`, and flushes
  before success. Per-Workspace UUID rewrites remain descriptor-only through `tune2fs`.
- `ComputeAttachment` now contains the generation fence, held writer lock, neutral open
  descriptor, stable block ID `workspace`, exact capacity, deterministic filesystem UUID, child
  descriptor address, and a same-filesystem private-link capability. Its descriptor display name
  is `workspace`, not an authoritative host path.
- Firecracker no longer recovers the authoritative WorkspaceStore path through `os.File.Name()`.
  Cold boot and snapshot resume stage descriptor-authorized hard links into their private run/jail
  roots and retain the original attachment until teardown. Existing Firecracker recovery and
  snapshot behavior remains intact.

## Qualification coverage

The Linux-specific suite proves:

- real helper-based ext4 formatting with the deterministic UUID;
- real source/clone creation under the configured Btrfs roots, sparse allocation, mutation
  isolation, snapshot isolation, and restore isolation;
- descriptor-authorized private linking without revealing the backing path;
- cross-process writer exclusion and kernel release of a leaked lock when its owner exits;
- attachment generation, capacity, block ID, UUID, and child descriptor metadata;
- all existing receipt replay, crash recovery, UUID corruption, atomic publication, retention,
  relocation checksum/framing, capacity, deletion, and traversal-confinement tests.

The helper used for qualification was the Task 2L local build at Microsandbox commit
`5b335537afad433ad2c0308cb54de13b7015b4e7`; its binary SHA-256 is
`d4422b2ae330e3038af1ed80724d9674c20a9f3ed4f9503605540ed8ad01b784`.

## Validation

- `just test-workspacestore-linux`: passed on deimos's qualified Btrfs root.
- `cd runner && go test ./internal/workspacestore/... -count=1`: passed.
- `just test-firecracker`: passed against Firecracker 1.16.1 on real `/dev/kvm` using the existing
  locally signed VM assets; 3 qualified lifecycle tests passed in 25.605 seconds.
- `just test-non-kvm`: passed with the disposable local PostgreSQL fixture. The host-filesystem
  preflight test used its intentional disposable `/var/tmp` fixture.
- `git diff --check`: passed.

All builds and tests were local. No external repository, pull request, issue, or remote branch was
created or modified.
