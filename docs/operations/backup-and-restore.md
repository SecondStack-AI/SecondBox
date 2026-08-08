# Backup and recovery

SecondBox does not transport, catalog, or reconstruct runner-local Workspace
images. A PostgreSQL or S3 recovery alone cannot recover a Sandbox whose home
runner filesystem was lost.

`scripts/backup.sh` creates a quiescent control-plane recovery bundle containing:

- the `secondbox` PostgreSQL schema;
- database-derived metadata for published Artifacts;
- the matching verified Artifact object bytes.

The bundle deliberately contains no Workspace image, local Snapshot, runner
receipt directory, runner credential, or host path.

## Runner recovery boundary

Use the infrastructure backup system chosen by the operator for each runner.
Before capturing a runner:

1. prevent new work on the runner;
2. stop or otherwise quiesce every affected Sandbox;
3. capture `SECONDBOX_RUNNER_WORKSPACE_ROOT` and the stable runner identity as
   one consistent recovery point;
4. restore both to the same logical runner identity;
5. validate the WorkspaceStore inventory and start a stopped Sandbox before
   returning the runner to service.

Restoring the files under a different runner ID is not supported. Losing an
unbacked workspace filesystem permanently loses the affected Sandboxes and their
local Snapshots.

Artifact object storage retains its independent integrity and retention
requirements. Its provider backup, replication, and restore policy must preserve
object keys and bytes referenced by PostgreSQL.

## Guided single-host installations

An ordinary guided-install uninstall is a service stop, not a backup or deletion operation. It preserves the accepted plan, receipt, manifest, generated authority, Runner identity, PostgreSQL and object-store volumes, verified execution assets, installed binaries, CLI configuration, Workspace, and optional Btrfs image and mount unit. `secondbox-deploy install --resume DIRECTORY` restarts that preserved deployment after revalidating its evidence.

Back up the operation directory, Compose volumes, Runner identity/state, and Workspace as one coordinated recovery point. For the filesystem-image choice, the fully allocated Btrfs image is the durable Workspace filesystem and must be captured while the deployment and Sandboxes are quiescent. Restoring PostgreSQL, S3 objects, or only the install receipt cannot reconstruct it. Permanent `uninstall --purge` is intentionally separate and is not a recovery mechanism. See [guided single-host installation](guided-single-host-install.md).
