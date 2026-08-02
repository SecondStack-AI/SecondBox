# Workspace durability

Each Sandbox owns one durable Workspace and one immutable `home_runner_id`. The
home runner's configured `SECONDBOX_RUNNER_WORKSPACE_ROOT` is the only
authoritative copy of that Workspace known to SecondBox. PostgreSQL records the
logical owner, generation, desired state, local mutation state, and durable
runner receipts; it never records a host path. S3-compatible storage is not a
Workspace persistence layer.

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
and deletion. Runner-side locking independently enforces one writer.

If the home runner is unavailable, the Sandbox is unavailable. Scheduling never
selects another runner, creates an empty replacement, or reconstructs a
Workspace from object storage. Operators must preserve the stable runner
identity and workspace filesystem together through their own backup system.
