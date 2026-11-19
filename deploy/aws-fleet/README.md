# AWS elastic Code workers

This candidate deployment runs an outbound-only, gVisor Runner on each protected
On-Demand EC2 worker. PostgreSQL, SecondBox, the fleet controller, the gateway
issuer and the egress proxy run independently. S3 stores workspace archives; the
control plane supplies short-lived object-specific transfer URLs. Workers have
no workspace-bucket credentials. Use a coordinated protocol-8 control-plane and
Runner candidate build.

## Controller host

Build `go build -o /persistent/path/secondbox-fleet ./cmd/secondbox-fleet` and copy
`fleet.example.json` to a mode-0600 configuration file. Supply every referenced
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

The controller needs private connectivity to PostgreSQL, SecondBox and the
issuer. Its AWS principal needs DescribeAutoScalingGroups, DescribeInstances,
SetDesiredCapacity and TerminateInstanceInAutoScalingGroup for its exact owned
ASG, plus DescribeVolumes/DeleteVolume for receipted, tagged retired-worker
volumes. The ASG and instance ownership tag is
`secondstack.ai/qualification-run=<qualificationRun>`. Keep the node capacity
configuration below the measured allocatable Runner capacity. CPU, memory,
Instance and workspace-byte demand drive scaling. Physically admission-denied
workers contribute no free placement capacity.

Run `secondbox-fleet --config /etc/secondbox-fleet/fleet.json`. A PostgreSQL
session advisory lock permits one manager per pool. A controller outage stops
reconciliation and enrollment; it does not terminate workers. Current enrollment
certificates are bounded to 24 hours and immutable per instance; rotate by
safely evacuating and replacing workers before expiry.

## Worker bundle and infrastructure

Build candidate Runner and gateway images locally. Materialize the exact gVisor
base artifact and optionally prepare the exact Code execution-image cache.

```sh
deploy/aws-fleet/build-worker-bundle.sh \
  candidate-runner candidate-gateway /persistent/gvisor-artifacts \
  /etc/secondbox-fleet/enrollment-ca.pem /persistent/worker-bundle \
  /persistent/prewarmed-execution-images
```

The last argument is optional. Copying a prepared cache does not bypass Runner
execution-image verification. The printed SHA256 pins the whole bundle. Upload
`worker-bundle.tar.gz` to a private S3 bucket beneath a run-owned prefix. The
CloudFormation `WorkerBundlePrefix` is that single component; `WorkerBundleKey`
is relative to it (for example `artifacts/worker.tar.gz`). The worker role can
read exactly this object. No private signing keys belong in the bundle.

The builder requires root and GNU tar with pax, ACL and xattr support. It must
read every materialized artifact, including root-owned files
such as `etc/shadow` and private home directories. Run it with root access to
those inputs. Preserve their numeric owners and modes; changing artifact
permissions to make a normal-user copy succeed changes the pinned image.
For an isolated build, use a short-lived root container from a trusted build
image containing the pinned Go toolchain, Bash, GNU tar and Docker CLI. Mount the source,
materialized artifacts, enrollment CA and optional prepared cache read-only;
mount a task-specific persistent output/cache directory read-write and the
Docker socket for the builder's image inspect/save commands. Run the same
`build-worker-bundle.sh` command inside that container with these mounted paths.
Afterward, transfer ownership of the resulting `.tar.gz` archive to the operator
if needed. Do not recursively change ownership or modes inside the materialized
artifact or extracted bundle trees. The Docker socket grants host authority, so
use only the reviewed builder and trusted inputs in this container.
Artifact staging and the final archive preserve numeric owners, special modes,
nanosecond mtimes, symlinks, ACLs and all xattr namespaces. Before emitting the
shipping checksum the builder extracts the actual final archive and checks its
flat-root digest against the unmodified materialization manifest. Worker
extraction preserves the same metadata, and installation checks the copied XFS
artifact again. A metadata mismatch fails the build or worker startup; it never
rewrites the pinned materialization digest.

Deploy `fleet.cloudformation.yaml` with an Ubuntu 24.04 amd64 AMI, dedicated VPC
and public subnets (or supply equivalent outbound connectivity), explicit ports
and controller CIDR, bundle location/SHA256, workspace capacity, initial capacity
and UTC expiry. `EnrollmentPort` must match `EnrollmentURL`; `RegistryPort`
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
A pre-baked AMI can remove that cold-start work after qualification.
The installer records the XFS filesystem UUID in `fstab` and makes Docker require
that mount before starting. Docker also recreates the fetcher socket directory
with UID 10002 before restarting containers, so reboot cannot fall back to an
unmounted root-directory workspace or an inaccessible `/run` socket directory.

The bootstrap formats only a blank, single non-root volume as XFS with reflink.
Retained XFS requires the same fleet marker. Artifacts, cache, workspaces and
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
No Spot instances are used. The scheduled test expiry is a spend bound that
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
