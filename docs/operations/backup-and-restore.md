# Backup and recovery

SecondBox does not transport, catalog, or reconstruct runner-local Workspace
images. A PostgreSQL recovery cannot recover a Sandbox whose home runner
filesystem was lost.

`scripts/backup.sh` creates a quiescent control-plane recovery bundle containing
the `secondbox` PostgreSQL schema and database fencing evidence. That schema
includes Tenants, Subjects, authority lookup identifiers and one-way verifiers,
quota, cleanup Operations, desired state, and audit history. Existing delegated
credentials therefore retain their recorded state after a database restore;
their bearer secrets remain only with the callers that received them.

The bundle deliberately contains no bearer token, platform-token file,
Workspace image, local Snapshot, runner receipt directory, runner credential,
or host path. Back up the operator-owned platform-token file and Runner secrets
through the customer's secret-management system as separate recovery units.

## Runner recovery boundary

Use the infrastructure backup system chosen by the operator for each runner.
Before capturing a runner:

1. prevent new work on the runner;
2. stop or otherwise quiesce every affected Sandbox;
3. capture `SECONDBOX_RUNNER_WORKSPACE_ROOT` and the stable runner identity as
   one consistent recovery point;
4. include every operator-local immutable backend asset the runner's Sandboxes
   are pinned to in the same recovery unit: for a gVisor runner that is the
   build directory (`bin/runsc`, `bin/secondbox-guest-agent`, `rootfs/`), the
   materialization manifest, its pinned digest, and the runner's environment
   configuration - a restored Workspace root cannot resume its Sandboxes
   without the exact materialization they are pinned to. On the Kubernetes
   placement, the image digest reference, node-local build directory, and the
   identity Secret form that unit;
5. restore all of it to the same logical runner identity;
6. validate the WorkspaceStore inventory and start a stopped Sandbox before
   returning the runner to service.

Restoring the files under a different runner ID is not supported. Losing an
unbacked workspace filesystem permanently loses the affected Sandboxes and their
local Snapshots.

## Guided single-host installations

An ordinary guided-install uninstall is a service stop, not a backup or deletion operation. It preserves the accepted plan, receipt, manifest, generated authority, Runner identity, PostgreSQL volume, verified execution assets, installed binaries, CLI configuration, Workspace, and optional Btrfs image and mount unit. `secondbox-deploy install --resume DIRECTORY` restarts that preserved deployment after revalidating its evidence.

Back up the operation directory, Compose volume, Runner identity, and the operation-specific Runner storage tree as one coordinated recovery point. That tree contains verified execution assets, Runner state, Workspaces, and local Snapshots on one reflink-capable filesystem. For the filesystem-image choice, the fully allocated Btrfs image is the complete durable Runner storage filesystem and must be captured while the deployment and Sandboxes are quiescent. Restoring PostgreSQL or only the install receipt cannot reconstruct it. Permanent `uninstall --purge` is intentionally separate and is not a recovery mechanism. See [guided single-host installation](guided-single-host-install.md).

After restoring PostgreSQL and each matching Runner recovery unit, start the
control plane first and let Runners reconnect with their stable identities.
Inspect nonterminal Subject cleanup Operations and tenant usage before reopening
admission. Do not recreate missing Subjects, authorities, Workspaces, or quota
rows from application configuration.
