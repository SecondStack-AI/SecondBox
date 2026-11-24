# AWS elastic Code workers

This reference deployment runs an outbound-only, gVisor Runner on each protected
On-Demand EC2 worker. PostgreSQL, SecondBox, the fleet controller, the gateway
issuer and the egress proxy run independently. S3 stores workspace archives; the
control plane supplies short-lived object-specific transfer URLs. Workers have
no workspace-bucket credentials. Use a coordinated protocol-8 control-plane and
Runner release. The fleet manager and enrollment service must use the exact
same build as the control plane; their database and enrollment contracts are
private and versioned together.

## Controller host

The release includes `secondbox-fleet_<version>_<os>_<architecture>` and
`secondbox-fleet-enroll_<version>_<os>_<architecture>` alongside the other host
binaries. Verify their published checksums and `--version` identities against
the control-plane release. Install the controller binary on its trusted host;
the enrollment binary belongs on EC2 workers. The Linux amd64 worker topology
below is the qualified reference. A source build uses
`go build -o /persistent/path/secondbox-fleet ./cmd/secondbox-fleet`.

Copy `fleet.example.json` to a mode-0600 configuration file. Supply every referenced
file. Private inputs (token, database URL, CA signing key, env templates,
egress-contexts and every registry file) must be mode 0600. The Runner CA signs
only worker public CSRs; its private key stays on this controller. The gateway
issuer owns a separate issuer key and accepts the controller's mTLS identity.
Enrollment is server-authenticated TLS; allow its listener only from workers.
Do not expose PostgreSQL or gateway issuance publicly.

Use the existing deployment's generated Runner and gateway env/context files as
templates. Replace the exact Runner ID with `{{RUNNER_ID}}`. These files must
contain the actual candidate control endpoint, pool, logical capacities,
execution-image registries and gVisor settings. Keep host-internal paths matching
`compose.yaml`. Supply the registry `tenants.json` and its referenced credentials
and certificate files under `enrollment.registryDirectory`. Registry data is
returned only after EC2/STS enrollment verification; it is excluded from the
shared worker bundle and from the persisted certificate receipt.

The Runner env template pins the release: `SECONDBOX_RUNNER_SOFTWARE_VERSION`
and `SECONDBOX_GVISOR_MATERIALIZATION_DIGEST`. The controller does not update
them. When you update the fleet to a new release, set both to the new
release's version and verified gVisor materialization digest before workers
enroll. A worker whose digest differs from its bundle's materialization exits
before its Runner registers, and the controller then stops at the bootstrap
timeout.

The controller needs private connectivity to PostgreSQL, SecondBox and the
issuer. `controller-policy.example.json` describes its runtime AWS policy;
replace every angle-bracket placeholder and attach it to the controller's
existing instance/task role when hosted in AWS. On a non-AWS host, configure an
IAM Roles Anywhere shared profile with `credential_process` instead. The
control plane needs its own archive-signing role and the
[archive credential binding](../../docs/operations/workspace-storage-tiering.md).
Role trust and provisioning belong to the operator.
The runtime role needs no IAM write, PassRole, AMI creation or S3 access.
Its AWS principal needs DescribeAutoScalingGroups, DescribeInstances,
SetDesiredCapacity and TerminateInstanceInAutoScalingGroup for its exact owned
ASG, plus DescribeVolumes/DeleteVolume for receipted, tagged retired-worker
volumes. The ASG and instance ownership tag is
`secondstack.ai/fleet-ref=<fleetRef>`. Set the same stable reference in the
template's `FleetRef` parameter and controller configuration; changing it makes
existing resources foreign to that controller. Keep the node capacity
configuration below the measured allocatable Runner capacity. CPU, memory,
Instance and workspace-byte demand drive scaling. Physically admission-denied
workers contribute no free placement capacity. Set `maximumNodes` explicitly
and match it to the CloudFormation `MaximumNodes` value and account quota. The
example's two nodes are an example budget, not a product limit. `InitialNodes`
may be any nonnegative capacity up to that ceiling; `minimumNodes` controls the
controller's steady-state floor.

Run `secondbox-fleet --config /etc/secondbox-fleet/fleet.json`. A PostgreSQL
session advisory lock permits one manager per pool. A controller outage stops
reconciliation and enrollment; it does not terminate workers.

Enrollment certificates are bounded to 24 hours. The worker's
`secondbox-fleet-renew.timer` invokes `secondbox-fleet-enroll --renew` two
minutes after boot and every five minutes, with three bounded one-minute
retries. The authority renews within the final third of the lifetime, using the
same keys, fresh same-role STS proof and current owned-ASG membership. Renewal
refreshes the Runner leaf and both gateway leaf locations atomically while
preserving keys, CA and environment. New TLS handshakes reload the leaves
without restarting guests. An expired leaf can recover only through its
existing key binding; retired identities and substituted keys are refused.

## Worker bundle and infrastructure

Build candidate Runner and gateway images locally. Materialize the exact gVisor
base artifact and optionally prepare the exact Code execution-image cache.

```sh
deploy/aws-fleet/build-worker-bundle.sh \
  candidate-runner candidate-gateway /persistent/gvisor-artifacts \
  /etc/secondbox-fleet/enrollment-ca.pem /persistent/secondbox-fleet-enroll /persistent/worker-bundle \
  /persistent/prewarmed-execution-images
```

Use the checksum-verified Linux amd64 enrollment binary from the same release
as the control plane and Runner. The builder prints its version; it does not
build an enrollment client with a development identity. The last argument is
optional. Copying a prepared cache does not bypass Runner
execution-image verification. The printed SHA256 pins the whole bundle. Upload
`worker-bundle.tar.gz` to a private S3 bucket beneath a run-owned prefix. The
CloudFormation `WorkerBundlePrefix` is that single component; `WorkerBundleKey`
is relative to it (for example `artifacts/worker.tar.gz`). The worker role can
read exactly this object. No private signing keys belong in the bundle.

Supply a quiesced cache or offline copy. The builder exports only complete
digest directories and their fixed execution-bundle artifact files, verifies
their checksums, and excludes registry authentication, operation records,
preparation retention markers and partial downloads. It never packages the
whole mutable Runner cache.

The builder requires root and GNU tar with pax, ACL and xattr support. It must
read every materialized artifact, including root-owned files
such as `etc/shadow` and private home directories. Run it with root access to
those inputs. Preserve their numeric owners and modes; changing artifact
permissions to make a normal-user copy succeed changes the pinned image.
For an isolated build, use a short-lived root container from a trusted build
image containing the pinned Go toolchain, Bash, GNU tar and Docker CLI. Mount the source,
materialized artifacts, enrollment binary/CA and optional prepared cache read-only;
mount a task-specific persistent output/cache directory read-write and the
Docker socket for the builder's image inspect/save commands. Run the same
`build-worker-bundle.sh` command inside that container with these mounted paths.
Afterward, transfer ownership of the resulting `.tar.gz` archive to the operator
if needed. Do not recursively change ownership or modes inside the materialized
artifact or extracted bundle trees. The Docker socket grants host authority, so
use only the reviewed builder and trusted inputs in this container.
Artifact staging and the final archive preserve numeric owners, special modes,
nanosecond mtimes, symlinks, ACLs and all xattr namespaces. The shipping archive
records zero regions of execution-image disk files as sparse extents. Their
logical sizes and content hashes are unchanged; AMI extraction and subsequent
cache copies avoid allocating and reading those zeros. Before emitting the
shipping checksum the builder extracts the actual final archive and checks its
flat-root digest against the unmodified materialization manifest. Worker
extraction preserves the same metadata, and installation checks the copied XFS
artifact again. A metadata mismatch fails the build or worker startup; it never
rewrites the pinned materialization digest.

Worker bootstrap (or, for a pre-baked fleet, the AMI build) stages the execution
cache privately, verifies checksums and publishes complete digest directories
atomically under the Runner's digest lock. An interrupted copy cannot publish a
partial image. Retries retain existing image directories and live lock inodes.

Deploy `fleet.cloudformation.yaml` with an Ubuntu 24.04 amd64 AMI, dedicated VPC
and public subnets (or supply equivalent outbound connectivity), explicit ports
and controller CIDR, bundle location/SHA256, workspace capacity and explicit
initial/maximum capacity. `EnrollmentPort` must match `EnrollmentURL`; `RegistryPort`
allows image pulls to the controller CIDR's authenticated TLS registry. There is no inbound
worker security-group rule. Workers need outbound package/image/S3 access; the
gateway sends guest upstream traffic through the separately authenticated proxy.
The initial bootstrap installs Docker, XFS tools and AWS CLI before enrolling.
Ubuntu 24.04 has no `awscli` package in the configured archive, so bootstrap uses
the official AWS CLI v2.38.0 amd64 zip, pinned to SHA256
`cb037d35cca5cefc3eb89ebabc471a0b4e4fe44135b72228c7eb0da2d2fae9f6`.
The pin was verified against its detached signature and the AWS CLI Team key
published in the AWS install guide (fingerprint
`FB5DB77FD5C118B80511ADA8A6310ACC4672475C`). Downloads, extraction and `TMPDIR`
stay in `/var/lib/secondbox-bootstrap` on persistent root storage.
Alternatively, explicitly select the pre-baked AMI workflow below.
Production deployments leave `ExpiresAt` empty (the default). No expiry Lambda,
schedule or expiry IAM roles are then created. Disposable qualification runs
can opt in by supplying a UTC timestamp such as `2026-11-19T03:00:00`; that
creates the one-time spend-bound shutdown. It removes scale-in protection and
requests zero workers even if archival or execution is active. It preserves
retained data volumes, so inventory and cleanup remain required. This
destructive test expiry is not a normal retirement or durability policy.

## Pre-baked worker AMI

A pre-baked fleet uses two build outputs: an AMI and a worker data snapshot.
The AMI root contains Docker, XFS tools, AWS CLI, the immutable runtime images
and the unpacked worker bundle without its execution images. The data snapshot
is a 64 GiB reflink XFS filesystem that already holds the base artifacts, the
optional execution-image cache owned by the image fetcher, and the Runner's
verification records for that cache. The build runs the bundle's own Runner
image with `-record-execution-image-verifications` under the worker's container
paths, so a worker's Runner admits the cached images from records instead of
hashing them. The Runner still verifies the small base root when it starts. Records bind files to their filesystem rather than a device number,
so the clone keeps them.

Each worker's data volume is created from that snapshot at the template's
`WorkspaceGiB`. The volume keeps the bundle marker of the snapshot, and every
pre-baked bootstrap refuses a volume of another bundle. Bootstrap claims a
volume without a fleet marker: it grows the filesystem, writes the fleet marker
and skips the artifact copy and base-root check, cache staging and cache
ownership change. A
pre-baked worker's root and data volumes initialize from their snapshots at a
provisioned 300 MiB/s, which EBS bills per restored GiB; without it, blocks
first read before initialization load at a few MB/s, which dominated earlier
cold starts. All clones share one XFS UUID; mount two on one host only
with `nouuid`, for example during operator recovery of a retained volume.

Neither output contains an enrolled Runner or gateway identity, registry
credentials, a Workspace or application data. Each worker enrolls
independently. The enrollment CA is deployment-specific; rebuild both outputs
when changing it or any pinned bundle input. They are a cache, not a Workspace
recovery set.

Copy `ami-build.example.json` outside the checkout and replace every placeholder.
Select an available Ubuntu 24.04 amd64 base AMI and its expected owner explicitly.
The base must have only one EBS root at `/dev/sda1`, with no other disk mappings.
Upload the verified bundle to a private immutable object key. Supply a dedicated
builder instance profile with `AmazonSSMManagedInstanceCore` and `s3:GetObject`
for only the bundle and preparer keys. Use a security group with no inbound
rules and outbound HTTPS/DNS, plus outbound connectivity to Ubuntu, AWS CLI,
S3 and Systems Manager. The builder never needs controller or enrollment access.

```sh
export TMPDIR=/persistent/ami-build-tmp
uv run --no-project deploy/aws-fleet/build-worker-ami.py \
  --config /persistent/worker-ami-build.json \
  --output-directory /persistent/new-ami-build
```

Use an existing task-scoped AWS profile or an operator-assumed role. The script
checks the account and base owner, launches at most one owned builder, uploads
and pins the preparer, and prepares the checksum-pinned cache and the builder's
blank 64 GiB data disk without enrolling. It verifies the cache, stops and
removes SSM registration, clears cloud-init identity and SSH host keys, then
powers off. It creates the AMI from the root only and a separate snapshot of the
data disk, records the AMI, its root snapshot and `dataSnapshotId` in
`ami-build.json`, and requests termination of the builder, whose disks are
deleted with it, even on failure. The explicit build deadline is at most two hours;
the guest also schedules shutdown. Inspect the receipt after interruption;
never blindly rerun against a live builder. A launch token is persisted before
allocation; an ambiguous CLI response triggers bounded recovery and termination
of only that request. If AWS cannot confirm cleanup, inspect the recorded token
and ownership tags. Failed image/snapshot outputs are
retained for inspection and must be inventoried and removed by the operator.

Before imaging, the builder observes a per-build nonce acknowledgement written
to the serial console after identity scrubbing, then requests an EC2 stop and
confirms that the stopped instance has exactly its root and data disks. It
refuses an AMI that kept the data disk mapping. Console output
is read while the instance runs because EC2 can return empty output after
shutdown. The sealed guest powers off after five minutes if the provisioner
disappears. An unrelated stop or a stale console acknowledgement does not
satisfy this check. Catchable SIGTERM follows the same
receipted cleanup path; after SIGKILL or host loss, inspect the receipt and AWS
inventory explicitly.

The provisioning role needs EC2 describe, scoped launch/tag/stop/terminate/create-image
and create-snapshot authority (including `ec2:DescribeSnapshots`), `iam:PassRole` for only the builder profile's role, `s3:PutObject` for
the preparer key and SSM RunShellScript on only owned builders. It needs no IAM
write authority or archive-bucket credentials. Existing account-wide SSM
associations may run on the builder; account operators must account for these
when assessing AMI provenance and package-manager contention.

The provisioner also needs `ec2:GetConsoleOutput` on its owned builder instances
to verify the seal acknowledgement; worker roles do not need that permission.

Deploy `fleet.cloudformation.yaml` with `WorkerAMI` and
`PrebakedWorkerDataSnapshot` set to the build's results, and both
`PrebakedWorkerBundleSHA256` and `WorkerBundleSHA256` set to the bundle's exact
SHA256. Keep `WorkerBundleBucket`, prefix and key pinned to the same input.
The root volume is 32 GiB and must accommodate the unpacked bundle and loaded
images. The template rejects unequal SHA values and a pre-baked SHA256 without
its data snapshot, or the reverse. Bootstrap checks the cache marker, the exact
Docker image IDs and the data volume's bundle marker before enrollment. A
missing or mismatched cache or data volume fails explicitly, with no download
fallback. Empty `PrebakedWorkerBundleSHA256` selects the existing download mode
and refuses a pre-baked cache or data volume. Switching an idle fleet changes subsequent workers; it does
not roll active Workspaces or authorize forced ASG scale-in.

For a permanent deployment, place the controller, PostgreSQL, issuer and egress
proxy on stable private connectivity and use an attached instance/task role for
the controller instead of a long-lived operator key. Configure archive storage
and lifecycle rules separately. Leave `ExpiresAt` empty, set the Code pool's
archive TTL and compute-idle policy deliberately, and match the controller's
minimum/maximum nodes to the chosen cost/latency tradeoff. A minimum of zero
removes worker compute and data disks after safe evacuation, while controller,
database, AMI snapshot, object storage and networking costs remain. Qualify the
exact AMI with a real Code build, authenticated HTTP/WebSocket preview, safe
archive/retirement to actual AWS zero, fresh-worker restore and native session
continuation before using it for a permanent site.

Validate the deployment helpers with `just test-deployment` and
`just test-aws-worker-ami`, then run the live AMI/workload qualification. The
fault-boundary tests do not contact AWS and do not substitute for a cloud pass.

The installer records the XFS filesystem UUID in `fstab` and makes Docker require
that mount before starting. Docker also recreates the fetcher socket directory
with UID 10002 before restarting containers, so reboot cannot fall back to an
unmounted root-directory workspace or an inaccessible `/run` socket directory.

The download-mode bootstrap formats only a blank, single non-root volume as XFS
with reflink; a pre-baked bootstrap requires the snapshot's volume. Retained XFS
requires the same fleet marker. Artifacts, cache, workspaces and
archive staging share the XFS disk. Budget for the materialized base, image
cache, active workspace capacity and an additional full logical image per
concurrent incompressible export; archive concurrency defaults to one in the
example. Do not rely on root-volume storage for workspace durability.

## Scale-in and limitations

The controller observes admitted starts even at zero workers, then increases ASG
capacity. To remove capacity it drains one idle Runner, archives each stopped
workspace, waits for zero local references, revokes gateway authority, permanently
retires the Runner identity and only then terminates its exact instance. Active
assignments, snapshots, pending mutations, failed archives and truncated inventory
block evacuation. `scaleInAfterSeconds` measures inactivity since the later of
instance launch, its last assignment update and its last homed Sandbox update.
An existing stopped Workspace in a Profile configured for cold tiering whose
connected home is full or draining follows
fenced export/restore while preserving its admitted start Operation. Retained
Snapshots and unsettled Assignments prevent this move. The target must satisfy
the original placement constraints and verify its own execution image, retaining
any digest already resolved for the same start. Former homes become eligible
again only after their exact source cleanup succeeds and capacity is available.
A start
waiting only for its own image preparation keeps its home.
A cloud instance still missing from the Runner inventory after
`bootstrapTimeoutSeconds` causes an explicit operator-recovery error. It stops
counting as indefinitely in-flight capacity. The controller does not terminate
or replace an unknown node whose enrollment or local-data history is unproven.

ASG scale-in protection is enabled. It does not prevent health replacement or an
operator termination. Data EBS volumes survive termination initially. For a
planned, fully evacuated and retired worker the controller first commits a
receipt with exact non-root EBS volume IDs. After termination it deletes only
those detached, ownership-matching volumes. Unexpected-loss volumes have no such
receipt and remain available for operator recovery of local-only generations.
No Spot instances are used. When explicitly enabled, scheduled test expiry is a spend bound that
unprotects and stops workers independently of archival; unreceipted EBS and S3
continue billing until explicitly inventoried and cleaned up. Expiry is not a
durability boundary.

The current manager uses the existing platform control token, which is broader
than one pool. The fleet manager therefore belongs in the trusted control-plane
boundary alongside PostgreSQL and the Runner signing authority; restrict its
configuration, database and platform-token access to that operator. No platform
token, CA signing key, gateway issuer key or controller AWS credential is handed
to workers or Code. Workers receive instance-profile credentials scoped to the
exact bootstrap object and short-lived certificate/registry handoff. A
pool-scoped service credential is still required before delegating fleet
administration to a less-trusted service. No cloud qualification is implied by
local tests.

The fleet controller owns desired capacity after deployment. CloudFormation
ignores unchanged group-size parameters during worker-bundle updates so an
initial capacity of zero does not overwrite the controller's current capacity.
