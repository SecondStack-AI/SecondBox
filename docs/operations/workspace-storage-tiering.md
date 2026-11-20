# Workspace storage tiering

Enable the object tier to release stopped Workspace disks and restore them on
compatible Runners. Resident files still require a Runner-local reflink-capable
filesystem. The tier preserves disk data; it does not preserve running processes
or replace a coordinated backup of PostgreSQL, Runner state and archive objects.
See [Workspace durability](../design/workspace-durability.md) for the fencing
contract and [AWS workers](../../deploy/aws-fleet/README.md) for the elastic fleet.

## Configure and operate

Set `deployment.workspace_archive_config_file` to an absolute path in the
operator-owned deployment manifest. The compiler mounts this JSON read-only into
the control plane:

```json
{
  "bucket": "your-workspace-archives",
  "region": "us-east-1",
  "transferLifetimeSeconds": 900,
  "profileIdleSeconds": {"your-code-profile": 3600}
}
```

`endpoint` and `usePathStyle` optionally select an S3-compatible endpoint.
Transfer lifetime must be 60–3600 seconds; each configured Profile idle duration
must be positive and at most one year. Configuration is fixed at startup;
restart the control plane after changing it. Credentials use the AWS SDK chain;
prefer a workload role and explicitly provide any required credential-file
mounts. Only the trusted control plane receives bucket authority. Its principal
needs object read/write/delete and multipart abort permissions for the archive
prefix, plus the bucket's encryption-key permissions where applicable.

For a control host outside AWS, set
`deployment.workspace_archive_aws_config_directory` to a private directory
containing an AWS shared `config` file and only its archive credential-provider inputs. The
directory is mounted read-only at `/run/secondbox-archive-aws` in the control
plane only; it requires the archive configuration above. Use a real directory,
owner read/execute permission and no group/other access. Own the directory and
its private files with control-plane UID 65532 and make the helper executable
by that UID. Do not mount an operator's entire AWS configuration directory.
Startup verifies the shared config is a readable regular file under the
control-plane identity before constructing the archive provider; it does not
require an AWS request to pass that local prerequisite.

An IAM Roles Anywhere profile can use a checksum-pinned signing helper, its
client certificate and private key in this directory:

```ini
[default]
credential_process = /run/secondbox-archive-aws/aws_signing_helper credential-process --certificate /run/secondbox-archive-aws/client.crt --private-key /run/secondbox-archive-aws/client.key --trust-anchor-arn <trust-anchor-arn> --profile-arn <profile-arn> --role-arn <archive-role-arn>
```

Keep CA signing keys outside this directory. The AWS SDK refreshes expiring
credentials through the process; periodically rewriting a static credentials
file does not provide that contract. The binding disables EC2 metadata lookup
on the non-AWS host. Configure and qualify the archive role independently from
the fleet-controller role, including successful S3 access after the previous
session expires. Preserve the provider configuration and certificate recovery
materials outside generated Compose files; rendering recreates the binding.

**Configure an incomplete-multipart-upload lifecycle rule before enabling the
tier.** Interrupted or renewed uploads can leave uncommitted parts; deleting the
final object does not remove them. Keep committed archive objects until
SecondBox's receipted cleanup removes them. An external object-expiration rule
can destroy the only authoritative Workspace copy.

`POST /v1/sandboxes/{sandboxId}:archive` explicitly archives an eligible stopped,
Snapshot-free Sandbox. `profileIdleSeconds` opts named Profiles into automatic
cold offload, earlier offload under reported physical storage pressure, and
archive/restore for a pending start whose connected home is full or draining.
An empty map disables these automatic policies while retaining explicit archive
and restore. Active Instances, unsettled Assignments, retained Snapshots and
Workspace mutations prevent offload. Fleet evacuation also waits for safe
archival and retirement before terminating a worker.

The Runner seals and uploads directly to object storage using expiring grants.
PostgreSQL commits object authority only after a verified receipt, then requests
source cleanup. A normal start restores on a compatible Runner in the pinned
pool, verifies image/archive digests and logical capacity, commits the new home,
and queues blob cleanup. The original start Operation remains pending through
restore and image preparation. `workspace.archivedBytes` reports compressed
archive size; resident storage observations are unavailable during archive
residency and restore.

## Recovery and disabling

A failed upload retains its source authority until acknowledged abort/unseal;
a failed restore retains the archive. A restore target lost before home commit
is quarantined from placement until its exact import cleanup is acknowledged.
The same pending start can retry elsewhere while its capacity deadline permits;
late replies cannot publish the abandoned target. Do not clear mutations, delete
receipts, reassign homes or fabricate empty disks to unblock an Operation.
Investigate the archive worker's retry error and restore the original Runner's
identity/connectivity for outstanding cleanup.

A permanently lost target leaves its orphan cleanup pending. Its archived
Workspace can restore elsewhere, but the tier cannot be disabled until the
cleanup receipt exists. Recover the target's original identity and retained
WorkspaceStore to complete cleanup. If that is impossible, keep the archive
provider configured; there is no forced-forget API. Do not remove database rows
or fabricate receipts to bypass this boundary.

Lost-target detection uses a 30-second bound. A long control-plane outage can
therefore abandon and repeat an in-flight restore before its Runner reconnects.
The committed archive remains authoritative through that retry.

Loss of an unarchived resident disk still loses its Workspace. Preserve each
Runner identity, WorkspaceStore and receipt directory together. An archived
Workspace depends on both the committed object and PostgreSQL descriptor; an
arbitrary old object is not a recoverable replacement. See
[backup and recovery](backup-and-restore.md).

To disable tiering, first remove automatic Profile policies and pause fleet
scale-in/evacuation while keeping the provider configured. Restore every archived
Workspace through normal starts and allow imports, aborts, source cleanup and
object cleanup to settle. Only then remove the configuration and restart. Startup
refuses an absent provider while archived/restoring Workspaces, active archive
mutations/transfers or queued object cleanup remain. Do not delete the bucket or
revoke its credentials before this boundary.

## Worker certificate availability

Fleet workers renew their original keys' certificate leaves automatically.
Unreadable, invalid or expired leaves refuse new TLS handshakes; established
streams can remain connected. In particular, an existing control stream can
keep its Runner visible while new direct sessions fail. Check the worker's
renewal timer and enrollment authority, restore same-key renewal and verify a
new direct and gateway connection before admitting more work. Static Runners
require operator-managed leaf renewal. An authority outage past leaf expiry is
an availability boundary, even when the Workspace disk remains intact.

## Capacity and upgrade boundary

Cold readiness includes cloud boot, enrollment, image preparation and Workspace
transfer. It is workload- and cache-dependent; no five-minute readiness bound is
implied. Align application wait budgets with measured readiness, retain a warm
floor where needed, and measure a pre-baked worker image before relying on
scale-to-zero. Spot interruptions, unplanned disk loss and controller HA require
separate qualification.

Finish pending operator relocations before upgrading. Quiesce admission and
capture a coordinated PostgreSQL/Runner/object recovery point. Apply migrations
`0039`–`0043` and replace the control plane and **all** Runners together: Runner
protocol generation 7 and generation 8 are incompatible. Keep the fleet manager
and enrollment service on the exact same build as the control plane; their
private contracts and privileged database, Runner CA, gateway issuance and cloud
credentials are trusted infrastructure, never application credentials.

The database migration is a rollback boundary. Returning to the prior release
requires restoring the pre-upgrade database and matching Runner/object recovery
units; running an old binary against the migrated database or copying individual
rows/disks is not rollback. Preserve that recovery point until the upgraded
fleet and restored Workspaces have been qualified. Release-specific steps and
qualification results belong in the release notes.
