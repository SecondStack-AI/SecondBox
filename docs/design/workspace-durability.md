# Workspace durability

## Storage observations

The WorkspaceStore periodically reads current-image manifests and inode block
counts and bounded extent metadata without acquiring the compute writer lock,
opening a compute attachment, mounting an image, or enumerating guest files.
One background scan visits at most 64 Workspace directories, continuing from
its cursor on later scans. Heartbeats collect completed results and start at
most one scan at a time; slow FIEMAP calls do not block heartbeat delivery.
Reports contain logical identities, generation, measurement time, allocated
bytes, optional exclusive bytes, and bounded failure reasons on the authenticated
control channel. Pressure evidence reuses the backend's admission controller.

PostgreSQL accepts measurements only from the Workspace's current home and for
its current generation. Observation updates do not change Sandbox revision,
activity, generation, or lifecycle timestamps. Busy Workspace rows are skipped
to keep telemetry out of lifecycle lock waits; a later scan revisits them.
Public reads use persisted evidence, preserve its original observation time,
and never dispatch Runner work. Deleted Workspaces report unavailable storage.
See [operational measurement semantics](../operations/observability-and-diagnostics.md#application-workspace-capacity).

Each Sandbox owns one durable Workspace. A resident Workspace has one authoritative
`home_runner_id`; an archived Workspace has no home and one committed object.
The assigned runner's configured `SECONDBOX_RUNNER_WORKSPACE_ROOT` is the only
writable authoritative copy of a resident Workspace. PostgreSQL records the
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
manifest generation. Before advancing it, and with the writer lock held, the
runner releases the host allocation behind ext4 blocks the guest has freed: it
reads the stopped image's block bitmaps and punches holes over free runs of at
least 1 MiB, aligned inward to 64 KiB. Punching drops only this image's
reference, so a Snapshot sharing those blocks keeps them. The pass punches
nothing when the filesystem awaits journal recovery, records errors, uses an
unsupported layout, fails a checksum or free-count check, or has no journal and
was not cleanly unmounted (a guest that removes its journal therefore never
reclaims on Firecracker, whose stop freezes rather than unmounts). One pass
punches at most 4,096 ranges that still hold host data, and a host filesystem
too full to record a punch ends the pass early without failing the stop; the
next stop continues. Other punch failures fail the generation advance.
PostgreSQL advances the Sandbox and Workspace generations
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
relocation, archive, and deletion. Runner-side locking independently enforces one writer.

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

## Object-store archive tier

An operator can archive a stopped, Snapshot-free Sandbox with `POST
/v1/sandboxes/{sandboxId}:archive`. An explicit per-Profile cold idle policy can
request the same operation. The Runner seals the current image and uploads a
compressed sparse archive directly to S3 through bounded multipart grants.
The receipt contains compressed size and SHA-256 digests for both archive and
logical image. PostgreSQL commits archived residency and releases the home
before independently deleting the sealed source. Upload failure keeps the
source authoritative until an acknowledged abort unseals it.

For configured Profiles, a physical-storage warning or admission denial can archive an eligible stopped Workspace before its idle age.
The policy uses the Runner's reported pressure state and existing operator thresholds, including recovery hysteresis.
It selects one Workspace per pressured Runner and waits for archive and source deletion before staging another.
Active Instances, unsettled Assignments, retained Snapshots and mutations still prevent offload.

Starting an archived Sandbox waits for a compatible Runner in its pinned pool.
Restore verifies the archive and image digests and exact logical capacity before
publishing a local manifest. PostgreSQL commits the new home before deleting
the blob. A concurrent stop suppresses compute admission; delete waits for the
transfer receipt. An expired capacity wait leaves the committed archive intact.
Pending transfer grants are refreshed after expiry, including after reconnect.
Storage observations become unavailable while archived or restoring;
`workspace.archivedBytes` reports the committed compressed size separately.

For Profiles explicitly configured for cold tiering, a pending start of a
resident stopped Workspace can use the same archive/restore path when its
connected home is full or draining. Other Profiles keep their resident home;
enabling object storage alone does not opt them into automatic re-homing.
The worker adopts the queued start mutation only while there is no Instance,
unsettled Assignment or retained local Snapshot. Home capacity excludes that
start's own pending reservation, so ordinary image preparation does not trigger
an offload. An offline source remains unavailable; no replacement is fabricated.

The original start Operation stays pending across verified export and restore.
Each transfer phase has its own archive record and authorized Runner command;
source cleanup may finish independently after a target import. Restore excludes
every former home whose exact source cleanup has not succeeded. A cleaned home
may be selected again if it is connected, ready and has compatible capacity.
Restore preserves the pinned pool, resources,
architecture, backend and egress constraints. The new home verifies its own
selected image under a private preparation identity bound to the original start.
If that start already resolved an image on a previous home, the target prepares
the same immutable digest while retaining the public requested reference.
Old source replies cannot complete the target's preparation or change its home.
Relocation-source deletion retains its local mutation receipts until ordinary
Workspace deletion, so even a cleanup replay from an earlier tiering cycle cannot
delete a restored image on the same Runner and generation.
Reconnect does not advertise that historical ledger. The control plane requests
only exact receipts for outstanding ordinary mutations or unfinished Snapshot
restore phases, and the Runner reads those receipt files directly. All live
Workspaces remain in inspection inventory. Reporting neither deletes tombstones
nor imposes an age cutoff on their replay protection.

Stop and delete remain admissible while a start holds the archive mutation.
Cancellation or capacity-wait expiry suppresses compute without clearing the
source seal before an abort/unseal receipt or committed blob authority. Failed
upload retains the source; failed restore retains the authoritative archive.
Only the eventual compute readiness completes a continuing start successfully.

Enable the tier through `deployment.workspace_archive_config_file` in the deployment
manifest. The referenced JSON contains `bucket`, `region`, optional `endpoint`
and `usePathStyle`, `transferLifetimeSeconds` (60–3600), and explicit
`profileIdleSeconds`. The deployment compiler binds it read-only into the control
plane. AWS credentials use the official SDK credential chain; use a workload IAM
role in cloud deployments. Configure an incomplete-multipart lifecycle rule on
the bucket so interrupted and renewed upload grants cannot retain orphan parts.
Object bytes never pass through the control plane or guest credential boundary.

If a resident home runner is unavailable, the Sandbox is unavailable. Scheduling
never creates an empty replacement or assumes a stale archive exists. Relocation requires the source
Runner and its intact WorkspaceStore to be online. Operators must preserve the
stable runner identity and workspace filesystem together through their own
backup system.
