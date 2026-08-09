# Workspace durability

Each Sandbox owns one durable Workspace and one authoritative `home_runner_id`.
The assigned runner's configured `SECONDBOX_RUNNER_WORKSPACE_ROOT` is the only
writable authoritative copy of that Workspace known to SecondBox. PostgreSQL records the
logical owner, generation, desired state, local mutation state, and durable
runner receipts; it never records a host path.

The WorkspaceStore keeps sparse raw ext4 images on a reflink-capable filesystem.
Startup proves that its active-image and Snapshot directories share a device,
that `FICLONE` works, and that later source mutation does not modify the clone.
Failure of any probe makes the runner unready. There is no byte-copy or alternate
storage-backend fallback.

Only the WorkspaceStore resolves local paths. Compute receives an opaque
attachment with the expected generation and holds the Workspace writer lock
until the VM and every host-side user are stopped. A manifest selected by atomic
rename names the current writable image and generation.

An ordinary stop first detaches compute, then durably advances the local
manifest generation. PostgreSQL advances the Sandbox and Workspace generations
only after recording the matching receipt. Replays converge on the same receipt,
and start remains blocked while local and database generations disagree.

Snapshots are immutable reflink clones local to the home runner. Create and
restore require a stopped Sandbox. Delete is allowed while compute runs when no
restore references the Snapshot. Public `sizeBytes` is the logical image
capacity, not uniquely allocated blocks, and Snapshot metadata contains neither
a Workspace-image digest nor a local reference.

Restore is in-place and same-Sandbox only. The runner prepares a writable
reflink child, atomically swaps the current manifest, and retains the previous
image as rollback evidence. PostgreSQL commits the new generation only after the
swap receipt, fences all old-generation authority, and then dispatches
idempotent finalize cleanup.

One PostgreSQL Workspace mutation slot serializes start, stop, Snapshot, restore,
relocation, and deletion. Runner-side locking independently enforces one writer.

An operator may relocate only a stopped Sandbox with no retained Snapshots. The
source Runner first seals the current generation and persists its receipt, so
the source image cannot be opened for a writer. The control plane then forwards
64 KiB chunks through a one MiB credit window directly from the source stream to
the target stream without persisting them in the control plane.
The target verifies the exact logical size, ext4 identity, and SHA-256 checksum,
fsyncs the imported image, publishes its manifest, and persists a receipt before
acknowledgement. PostgreSQL changes `home_runner_id` only after that receipt and
queues source deletion in the same transaction. Before that transaction the
source remains home and sealed; after it the target is home and the source
remains sealed until deletion.

Relocation refuses retained Snapshots rather than moving them. Snapshots are
reflink children whose locality and retention receipts are part of the source
WorkspaceStore, while relocation deliberately transfers only one sealed current
image. Requiring operators to delete them keeps the transfer single-image and
preserves the existing Snapshot lock and durability model.

If the home runner is unavailable, the Sandbox is unavailable. Scheduling never
automatically selects another runner, creates an empty replacement, or
reconstructs a Workspace from another service. Relocation requires the source
Runner and its intact WorkspaceStore to be online. Operators must preserve the
stable runner identity and workspace filesystem together through their own
backup system.
