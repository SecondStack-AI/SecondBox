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
