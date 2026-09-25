# gVisor runner on Linux hosts without KVM

The gVisor runner is the supported backend for Linux x86_64 hosts that cannot expose
`/dev/kvm`, Kubernetes nodes included. It is a separate, operator-managed Runner deployment; it does not alter or replace the qualified
Linux Firecracker installer, container, systemd units, network setup, or standard Profiles.

Sandboxes use the Profile's fixed flat root unless create or start selects a signed execution image.
A gVisor Runner consumes the same signed image an application publishes for Firecracker: the image's
`rootfs.ext4` becomes the sandbox root, while the Runner's pinned `runsc` and guest agent still launch it.
[Client-selected execution images](../design/client-selected-execution-images.md#gvisor-materialization)
defines the trust and materialization contract.
The control plane and Runner must use the same supported Runner protocol generation.

## Host contract

Use a Linux x86_64 host with loop-device support (`/dev/loop-control`), `nftables`, `iproute2`,
`e2fsprogs`, and a Btrfs or XFS volume with reflink support for the complete WorkspaceStore root
and the execution image cache.
The backend requires no KVM device, no TUN/TAP device, and no hardware-virtualization CPU flags:
the sentry runs on its systrap platform. The runner needs the authority to create network
namespaces, veth pairs, loop attachments, and nftables tables.

Check a candidate host with the installer preflight in its gVisor mode:

```sh
secondbox-deploy install --check --backend gvisor
```

The gVisor mode enforces the same host baseline as every guided preflight - an active systemd,
the minimum host CPU count and memory, and a dedicated reflink-capable filesystem with enough
free capacity and compatible mount options for the WorkspaceStore root - plus loop-device,
`nft`, `ip`, and ext4-toolchain (`mkfs.ext4`, `e2fsck`) availability, and it reports absent
KVM, TUN, and virtualization as passing observations rather than blockers. It skips the
Firecracker-only container-engine and jailer UID-range checks. Run `--check` and read every
finding: each carries its own remedy. The Firecracker preflight is
unchanged.

The host must run the unified cgroup v2 hierarchy with the `cpu`, `memory`, and `pids`
controllers enabled on the runner's branch, and the runner's service identity must be able to
create sandbox cgroups there with writable `cpu.max` and `memory.max` controls (under systemd,
run the runner with `Delegate=yes` or as root). Readiness proves this by creating and removing
a disposable sandbox cgroup with those controls on every pass; a host without delegated,
writable controllers stays permanently unready.

When Docker shares the host, its firewall sets the forward hook to a drop policy. The runner
inserts one admission rule into Docker's designated `DOCKER-USER` extension chain accepting only
connections the runner's own fail-closed policy tables have already marked; hosts without Docker
need and receive no such rule.

## Build contract

Assemble a local build directory with exactly three inputs:

```text
bin/runsc
bin/secondbox-guest-agent
rootfs/
```

- `runner/scripts/fetch-runsc.sh` downloads the pinned `runsc` release and refuses any binary
  whose SHA-512 differs from the reviewed pin recorded in the script.
- Build the guest agent from the repository with CGO disabled:
  `CGO_ENABLED=0 go build -o bin/secondbox-guest-agent ./cmd/secondbox-guest-agent` under
  `runner/`.
- `rootfs/` is the flattened extract of the reviewed source OCI image. Extract it
  provenance-preserving from the exact digest-pinned image rather than a rebuilt tag:

  ```sh
  container="$(docker create <source-image>@sha256:<reviewed digest>)"
  mkdir rootfs && docker export "$container" | tar -x -C rootfs
  docker rm "$container"
  ```

  A normal exported image is not required to contain gVisor's bind destinations. Prepare the
  flat root once, before computing or recording any digest:

  ```sh
  cd runner
  go run ./cmd/secondbox-prepare-gvisor-flat-root /absolute/path/to/rootfs
  go run ./cmd/secondbox-flat-root-digest /absolute/path/to/rootfs
  ```

  Preparation creates only absent `/secondbox-guest-agent`, `/workspace`,
  `/secondbox-sockets`, `/runtime-private`, `/etc/resolv.conf`, `/proc`, and `/tmp` targets.
  It rejects symlinks and incompatible existing node types and is idempotent. The runner only
  validates this contract; it never repairs or mutates a flat root after the resulting digest is
  pinned. The digest must equal the materialization's `flatRootDigest`. The qualification drivers
  below consume and prepare this same build directory before calculating identity: local build directory is for source qualification. For release enrollment, use the
  published `runner-gvisor` and `gvisor-artifacts` images as described below.

## Operator-owned resources

Copy and review both explicit fixtures:

- `runner/deploy/gvisor-linux-amd64.resources.json` declares a separate gVisor RunnerPool and
  Profile.
- `runner/deploy/gvisor-linux-amd64.materialization.json` shows the backend materialization
  shape, recording the `runsc` and guest-agent digests as launch artifacts.

The repeated numeric SHA-256 values in the fixtures are deliberate non-release fixture
identities. Replace every runtime, toolchain, source OCI, flat-root, and launch-artifact digest
with the exact values from the operator's reviewed assets, then compute and pin the canonical
materialization digest. The backend verifies the manifest digest, both launch artifacts, and the
flat root against these pins before it advertises, and its readiness probe proves `runsc` can
boot on the host. Do not attach a gVisor runner to `standard-amd64` or change a published
standard Profile revision.

## Explicit runner environment

The following values are examples of required names, not runtime defaults. Materialize every
path, identity, capacity, and credential for the actual installation.

```sh
export SECONDBOX_COMPUTE_BACKEND=gvisor
export SECONDBOX_RUNNER_WORKSPACE_ROOT=/var/lib/secondbox/workspaces
export SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS=256
export SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL=5m
export SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES=10.210.2.1
export SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS=10.211.0.0/16
export SECONDBOX_RUNNER_EGRESS_CONTEXT_CONFIG=/etc/secondbox-runner/egress-contexts.json
export SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM=10.0.0.53:53
export SECONDBOX_GVISOR_RUNSC_PATH=/opt/secondbox-gvisor/bin/runsc
export SECONDBOX_GVISOR_AGENT_PATH=/opt/secondbox-gvisor/bin/secondbox-guest-agent
export SECONDBOX_GVISOR_FLAT_ROOT_PATH=/opt/secondbox-gvisor/rootfs
export SECONDBOX_GVISOR_MATERIALIZATION_PATH=/etc/secondbox/gvisor-linux-amd64.materialization.json
export SECONDBOX_GVISOR_MATERIALIZATION_DIGEST=sha256:OPERATOR_CANONICAL_DIGEST
# Dedicated and disposable: startup reconciliation recursively removes every
# child of this directory. Never share it with other services or point it at
# a Workspace or asset path; the runner refuses /, first-level, symlinked,
# overlong, and WorkspaceStore- or flat-root-overlapping values.
export SECONDBOX_GVISOR_RUNTIME_DIR=/run/secondbox-gvisor
export SECONDBOX_GVISOR_NETWORK_PROFILE=0
export SECONDBOX_GVISOR_MAXIMUM_VCPUS=8
export SECONDBOX_GVISOR_MAXIMUM_MEMORY_BYTES=8589934592
export SECONDBOX_GVISOR_MAXIMUM_DISK_BYTES=34359738368
export SECONDBOX_GVISOR_MAXIMUM_INSTANCES=4
export SECONDBOX_GVISOR_MAXIMUM_OPERATIONS=32
export SECONDBOX_GVISOR_WORKSPACE_TEMPLATE_CAPACITY_BYTES=8589934592
# Client-selected execution images: the same generic names the Firecracker
# Runner uses. The cache must be on a reflink-capable filesystem and disjoint
# from the runtime directory; the fetcher owns it.
export SECONDBOX_RUNNER_EXECUTION_IMAGE_CACHE_ROOT=/var/lib/secondbox-execution-images
export SECONDBOX_RUNNER_IMAGE_FETCHER_SOCKET=/run/secondbox-image-fetcher/fetcher.sock
export SECONDBOX_RUNNER_EXECUTION_IMAGE_PUBLIC_KEY=/etc/secondbox/execution-image-public.pem
export SECONDBOX_RUNNER_EXECUTION_IMAGE_PUBLIC_KEY_SHA256=OPERATOR_PUBLISHER_KEY_DER_SHA256
```

## Client-selected execution images

Every gVisor Runner requires the execution image settings above and a running image fetcher; there
is no fixed-assets-only mode. Run the fetcher as a separate unprivileged process (UID 10002 in the
reference pod) from the same release image, with `secondbox-image-fetcher` as its entrypoint and
exactly the inputs [deployment operations](deployment.md) documents for Firecracker: the cache root,
a host-private socket directory shared with the Runner, the per-Tenant registry configuration
directory, the publisher public key and fingerprint, the registry allowlist, and the three byte
limits. The fetcher receives no Workspace mounts, host devices, or privileges, and the Runner never
receives registry credentials.

At startup the Runner verifies the publisher key against its pinned fingerprint and proves that the
cache filesystem can reflink into an unnamed file; either failure stops the Runner. Readiness then
advertises `client-selected-image`, which admits the Runner to `images:prepare` targets and
selected-image placement.

A selected-image start verifies the cached signed bundle, reflinks its `rootfs.ext4` into an
unnamed file, and mounts that clone read-only as the sandbox root inside the mount supervisor's
private namespace. The supervisor creates only absent gVisor mount targets in the clone. An image
whose root symlinks one of those targets, such as `/etc/resolv.conf`, fails the start. The signed
kernel and `shared.img` are verified but not attached. The image's guest agent is ignored: the
materialization's agent must speak the image's guest protocol generation and every mandatory guest
feature the image signs.

Create `/etc/secondbox-runner/egress-contexts.json` as a root-owned, read-only file (or the reference pod's ConfigMap) before starting the Runner:

```json
{
  "schemaVersion": "secondbox.runner-egress-contexts/v1",
  "contexts": [
    {
      "name": "secondstack-staging",
      "gateways": [
        {
          "logicalName": "agent-gateway.secondbox.internal",
          "address": "10.210.2.2"
        }
      ]
    }
  ]
}
```

DNS resolution is IPv4-only end to end. The required
`SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM` accepts a literal IPv4 address and port
(`10.0.0.53:53`) - never a hostname - and Sandbox egress carries no IPv6 route, so IPv6-only
destinations are unreachable regardless of policy. All five `SECONDBOX_RUNNER_NETWORK_POLICY_*`
values above, together with `SECONDBOX_RUNNER_EGRESS_CONTEXT_CONFIG`, are the same explicit generic contract used by Firecracker. The absolute configuration path names a root-owned, non-writable, non-symlink strict `secondbox.runner-egress-contexts/v1` JSON file. Pin and TTL bounds, Runner addresses, management CIDRs, and the selected context's logical Runner gateways all reach the shared compiler; missing or malformed values keep the runner from starting. The reference pod projects the file from `secondbox-gvisor-runner-egress-contexts` as a read-only ConfigMap. Put only context names and logical-name-to-local-IP mappings in it, never certificates or upstream proxy addresses.

`SECONDBOX_GVISOR_NETWORK_PROFILE` separates runners sharing one host network namespace: each
profile selects its own DNS proxy address, link-local slot space, and veth and namespace names.
It must be set explicitly - there is no default - and a single runner per host states `0`. Valid profiles are `0`-`15`, every
runner in the same host network namespace **must** use a unique profile, and each profile bounds
the runner at 63 concurrent Instances (its link-local slot space). A reused profile is not
detected: startup reconciliation sweeps the profile's networks and cgroups and would tear down
another live runner's Instances.

Also set every required runner protocol address, RunnerPool ID, runner identity, mTLS
certificate, private key, CA, enabled feature, and evidence setting. These are deployment
authority and have no application defaults. The runner requires root for loop attachment, mount
and network namespaces, and nftables; the control plane must never run with that authority.

## End-to-end host bring-up

The complete host path, in order; every digest is computed from the operator's reviewed assets,
never copied from a document.

1. Compute and pin the identities from the build directory:

   ```sh
   cd runner
   go run ./cmd/secondbox-prepare-gvisor-flat-root /opt/secondbox-gvisor/rootfs
   go run ./cmd/secondbox-flat-root-digest /opt/secondbox-gvisor/rootfs
   sha256sum /opt/secondbox-gvisor/bin/runsc /opt/secondbox-gvisor/bin/secondbox-guest-agent
   go run ./cmd/secondbox-materialization-digest \
     /etc/secondbox/gvisor-linux-amd64.materialization.json
   ```

   Record the flat-root digest and launch-artifact digests in the reviewed materialization
   manifest, then pin the digest the repository tool prints in
   `SECONDBOX_GVISOR_MATERIALIZATION_DIGEST`. The tool computes the canonical digest exactly as
   runner startup does — over the decoded manifest, independent of the file's key order — so a
   reordered but equivalent manifest never produces a pin startup rejects.

2. Apply the reviewed public resources (the dedicated pool and Profile derived from
   `runner/deploy/gvisor-linux-amd64.resources.json`):

   ```sh
   secondbox resources check --file /absolute/path/to/reviewed-gvisor.resources.json
   secondbox resources apply --file /absolute/path/to/reviewed-gvisor.resources.json
   ```

3. Issue the Runner identity. On a guided single-host deployment, declare the Runner in
   `secondbox.toml` and run
   `secondbox-deploy runner-init <manifest> <runner-id> <handoff-directory>`; note that
   `runner-init` renders a Firecracker-shaped environment, so keep only the identity files
   (certificate, key, CA) and the enrollment credential from the handoff and compose the gVisor
   environment from this document. Install the identity under an operator-owned root such as
   `/opt/secondbox-runner-identity`.

4. Export the complete environment and start the runner as a root systemd service whose unit
   states exactly these variables — this is the full set the gVisor composition consumes, and
   `runner/deploy/gvisor-runner-pod.yaml` states the same set for the pod placement:

   | Variable | Value source |
   | --- | --- |
   | `SECONDBOX_COMPUTE_BACKEND` | Literal `gvisor`. |
   | `SECONDBOX_RUNNER_ID` / `SECONDBOX_RUNNER_POOL_ID` | The declared Runner identity (matching its certificate SPIFFE ID) and the dedicated pool name. |
   | `SECONDBOX_RUNNER_SOFTWARE_VERSION` | The exact source identity of the deployed runner build. |
   | `SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS` / `..._SERVER_NAME` / `..._CA` | The control plane's Runner gRPC endpoint, its certificate name, and the CA file. |
   | `SECONDBOX_RUNNER_CREDENTIAL` / `..._CLIENT_CERTIFICATE` / `..._CLIENT_KEY` | The enrollment credential and installed identity files. |
   | `SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS` / `..._ADVERTISED_ADDRESS` | The data-plane listener and its reachable advertised address. |
   | `SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS` / `..._WORKSPACE_CREATES` | Admission concurrency bounds. |
   | `SECONDBOX_RUNNER_LOG_DIR` / `SECONDBOX_RUNNER_LOG_PATH` | Operator-owned log directory and JSONL file. |
   | `SECONDBOX_RUNNER_WORKSPACE_ROOT` | The reflink-capable WorkspaceStore root. |
   | `SECONDBOX_RUNNER_NETWORK_POLICY_*` plus `SECONDBOX_RUNNER_EGRESS_CONTEXT_CONFIG` | Explicit generic enforcement bounds, protected Runner and management destinations, the strict context-indexed logical-gateway file, and the IPv4 DNS upstream. |
   | `SECONDBOX_GVISOR_*` (all values from the environment block above) | The backend block, including capacity maxima, the materialization pin, the runtime directory, and the network profile. |
   | `SECONDBOX_RUNNER_EXECUTION_IMAGE_CACHE_ROOT` / `SECONDBOX_RUNNER_IMAGE_FETCHER_SOCKET` / `SECONDBOX_RUNNER_EXECUTION_IMAGE_PUBLIC_KEY` / `..._SHA256` | The reflink-capable image cache shared with the fetcher, the fetcher's socket, and the independently pinned publisher key. |

   Instance capacity and per-Instance ceilings come only from the `SECONDBOX_GVISOR_MAXIMUM_*`
   values; the `SECONDBOX_RUNNER_SANDBOX_*`, storage-pressure, file-transfer, and remaining
   concurrency variables are Firecracker-only and ignored by this backend. Readiness is
   observable:

   ```sh
   # On the runner host, with the same environment the service runs under.
   # This proves authenticated mTLS/protocol connectivity to the control
   # plane only - it can succeed while gVisor composition or host
   # prerequisites are broken:
   secondbox-runner -healthcheck
   # From an operator workstation; the POOL column names the gVisor pool and
   # STATE is the authoritative backend readiness signal:
   secondbox runners list
   ```

   The Runner reaches `ready` state only after its materialization revalidation, loop and
   cgroup reconciliation, host network plumbing, network-policy enforcement, a live
   loop-device allocation, and the `runsc` boot probe all pass, and the degradable checks
   re-prove themselves on every readiness pass.

5. Tear down without losing Workspaces: stop the systemd unit (the backend fences and flushes
   every Instance), leave `SECONDBOX_RUNNER_WORKSPACE_ROOT` untouched, and decommission the
   Runner through the control plane only when its Workspaces have been relocated or are no
   longer needed. The WorkspaceStore root is the durable authority; never delete it as part of
   a runner restart or upgrade.

   Upgrades have no in-place path either: every Sandbox pins its exact Profile revision and
   backend materialization, so replacing `bin/runsc`, the guest agent, or `rootfs/` under a
   live materialization is unsupported and would strand the Sandboxes pinned to it. Deploy a
   new build directory beside the old one under a new materialization digest, keep the old
   assets until every Sandbox pinned to them has been deleted or relocated, and treat the
   forward-only database migrations as unrollbackable without the pre-upgrade backup.

   Relocation is the only path off a gVisor runner: RunnerPools seal to one backend kind, so
   no Sandbox can switch to a Firecracker pool in place, and a gVisor Workspace relocates only
   to another runner with matching gVisor materialization and free capacity. Provision that
   destination capacity before decommissioning the final gVisor runner, or its durable
   Sandboxes remain stranded until an eligible runner enrolls.

   Startup also modifies host-wide networking state that per-Instance teardown deliberately
   leaves in place; remove it only when decommissioning the host as a runner entirely:

   ```sh
   # The runner DNS interface and its per-profile listener address:
   ip link delete sbxgv-dns
   # The marked-traffic admission rule inserted into Docker's extension chain
   # (present only on hosts where Docker manages ip filter DOCKER-USER):
   nft -a list chain ip filter DOCKER-USER   # find the "ct mark 0x53425801 counter accept" handle
   nft delete rule ip filter DOCKER-USER handle <handle>
   ```

   IPv4 forwarding (`net.ipv4.ip_forward=1`) is enabled but not disabled automatically:
   other services commonly rely on it, so review the host's own policy before reverting it.

## Kubernetes pod install path

The gVisor runner is also qualified as a privileged, node-pinned pod on a Kubernetes node
without KVM. The qualified surface is the reference manifest at
`runner/deploy/gvisor-runner-pod.yaml`; anything broader — Deployments, operators, charts —
remains operator-authored and unqualified.

- Every release publishes the runner image as `ghcr.io/secondstack-ai/secondbox/runner-gvisor:vVERSION`
  and the backend assets as `ghcr.io/secondstack-ai/secondbox/gvisor-artifacts:vVERSION`: the
  prepared flat root under `/secondbox-runner-gvisor/rootfs`, `runsc` and the guest agent under
  `/secondbox-runner-gvisor/bin`, and `materialization.json`. The same materialization is the
  release file `secondbox-VERSION-gvisor-materialization.json`, and the artifact manifest records
  both image digests, the materialization digest, and the flat-root digest. Pin the pod to the
  runner image digest the artifact manifest names, and extract the artifacts image onto the node
  with ownership, modes, timestamps, and extended attributes preserved (for example
  `tar --xattrs --acls --numeric-owner`), since the flat-root digest covers them. The build
  contract above remains the way to assemble the same directory from source for qualification.
- Dedicate a tainted node pool: one runner pod per node, tolerating the pool taint, with a
  node-local reflink-capable volume for the WorkspaceStore.
- Set the pod's resource requests and limits equal to the node's declared sandbox budget plus
  runner overhead. Per-sandbox cgroups nest inside the pod's slice, so the pod budget caps the
  sum of all sandboxes.
- Provide the per-runner identity (mTLS keypair, CA, and runner credential) as a Secret; the
  flat root and materialization manifest arrive on the node through the operator's reviewed
  artifact flow.
- The reference pod runs the unprivileged `image-fetcher` container from the same
  `runner-gvisor` image beside the runner. They share a pod-local socket `emptyDir` and a node-local
  execution image cache on a reflink-capable filesystem. The fetcher alone mounts the per-Tenant
  registry Secret (`tenants.json`, token files, and optional `certificates/<host>/ca.crt`); the pod's
  `fsGroup` keeps those files group-readable only, as the fetcher requires. Both containers mount the
  publisher public key.
- Materialize the released assets on each runner node from the `gvisor-artifacts` image the
  artifact manifest pins (`gvisor.imageReference`). The image holds one directory,
  `/secondbox-runner-gvisor`, whose contents become the node directory the reference pod mounts
  at `/opt/secondbox-gvisor` (`/var/lib/secondbox-gvisor-materialized` in the reference
  manifest): `rootfs/` is `SECONDBOX_GVISOR_FLAT_ROOT_PATH`, `materialization.json` is
  `SECONDBOX_GVISOR_MATERIALIZATION_PATH`, and `bin/` holds the launch artifacts plus the two
  verifier binaries built at the release's version. Extract the image layers with ownership,
  modes, timestamps, and extended attributes preserved, then verify before enrolling:

  ```sh
  # OCI layout of the pinned image (skopeo copy docker://... oci:image, or an equivalent pull)
  manifest="image/blobs/sha256/$(jq -r '.manifests[0].digest' image/index.json | cut -d: -f2)"
  mkdir -p /var/lib/secondbox-gvisor-materialized
  for layer in $(jq -r '.layers[].digest' "$manifest" | cut -d: -f2); do
    tar --xattrs --xattrs-include='*' --acls --numeric-owner -xzf "image/blobs/sha256/$layer" \
      -C /var/lib/secondbox-gvisor-materialized --strip-components=1 secondbox-runner-gvisor
  done
  cd /var/lib/secondbox-gvisor-materialized
  sha256sum -c SHA256SUMS
  bin/secondbox-materialization-digest materialization.json   # must equal gvisor.materializationDigest
  bin/secondbox-flat-root-digest "$PWD/rootfs"                # must equal gvisor.flatRootDigest
  ```

  Never edit the published materialization: it is immutable and digest-bound. The runner
  image's `/usr/local/bin/runsc` and `/usr/local/bin/secondbox-guest-agent` are the same
  bytes as `bin/runsc` and `bin/secondbox-guest-agent` in the artifacts image; compare either
  against the materialization's `launchArtifacts` hashes and refuse to deploy on any mismatch.
  Set `SECONDBOX_GVISOR_MATERIALIZATION_DIGEST` in the pod to the manifest's
  `gvisor.materializationDigest`.
- The data plane is proxied through the control plane by default in clusters, and the
  reference manifest publishes no port. The only qualified direct-transport option is adding
  a `ports` entry to the runner container - `ports: [{containerPort: 9500, hostPort: 9500}]`
  - and changing `SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS` from the loopback default
  to a node address routable by data-plane clients; omit both to stay proxied-only.

## Qualification before enrollment

For qualification on the release host, set `QUALIFY_GVISOR_HOST_BUILD_ROOT` to an
absolute, dedicated cache path and `SECONDBOX_RUNNER_WORKSPACE_ROOT` to an existing
reflink-capable workspace root. `scripts/qualify-gvisor.sh --host --preflight`
accepts an absent cache; `--host` prepares it automatically before scenarios.
The cache path must have no symlink components or commas, and its existing
ancestor must be writable. Docker with Buildx and permission to run containers
with bind mounts are required. No host path or authority is selected implicitly.

Preparation uses `runner/deploy/gvisor-artifact-transport.Dockerfile`: its pinned
source image and runsc fetcher, the guest agent built from this checkout, and the
same materialization and flat-root digests recorded by release artifact manifests.
A container without host devices or privileged mode preserves numeric owners,
modes, timestamps, and extended attributes when extracting. Before publishing or
reusing a build, it verifies checksums, the runsc pin, and both digests against the
builder's original metadata. A mismatch fails without replacing that build.

The cache is owned by the invoking user with mode 0700 and an ownership marker.
Existing manually assembled directories are refused; configure a fresh dedicated
path instead. Preparation holds a per-cache lock and atomically publishes each
verified build under its image identity. Concurrent shards share those immutable
inputs; source changes produce another build without replacing an active reader's
assets. BuildKit caches unchanged compilation and downloads. Completed build
directories and Docker build images are retained; operators may remove them only
after all users of that cache have finished. The local manual-build and remote
nightly VM paths below remain separate qualification entry points.

Run the backend qualification suites and the full scenario driver on the target host class — a
real Linux x86-64 host without `/dev/kvm`. Both drivers run as root (network namespaces, nft
tables, and loop devices are created and destroyed), need Docker with Compose for the scenario
control plane, and consume a local gVisor build directory produced by the image build procedure
above: an absolute, non-symlink path whose `bin/runsc` and `bin/secondbox-guest-agent` match the
reviewed pins, with the materialization manifest derived from it.

```sh
export SECONDBOX_GVISOR_BUILD=/absolute/path/to/build
export SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM=/absolute/path/on/reflink-fs
just test-gvisor "$SECONDBOX_GVISOR_BUILD"

export SECONDBOX_GVISOR_LINUX_BUILD="$SECONDBOX_GVISOR_BUILD"
export SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1
export SECONDBOX_RUNNER_WORKSPACE_ROOT=/absolute/path/on/reflink-fs/scenario-workspaces
# The host scenario launches a retrievable signed execution image.
export SECONDBOX_SCENARIO_EXECUTION_IMAGE=registry.example/secondbox/agent@sha256:<digest>
export SECONDBOX_SCENARIO_IMAGE_REGISTRY_CONFIG=/absolute/path/to/tenant-registry-config
export SECONDBOX_SCENARIO_EXECUTION_IMAGE_PUBLIC_KEY=/absolute/path/to/publisher-public.pem
just test-scenario-gvisor
```

`scripts/qualify-gvisor.sh --host` supplies the release publisher key from
`SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY`, because its selected image is the release-signed microVM
artifact the Firecracker scenario also launches. The pod placement runs the fetcher sidecar but
skips the selected-image scenario: the qualification node has no capacity for a signed image.

For the pod placement, qualify the mechanisms and the identical scenario suite on the target
node class: a no-KVM Kubernetes node, as root, with node-local `kubectl` (both wrappers default
to `k3s kubectl`; override with `SECONDBOX_GVISOR_POD_KUBECTL` / `SECONDBOX_SCENARIO_POD_KUBECTL`).
The mechanism check additionally needs the qualification image imported into the node's
container runtime, the same node-local build directory, a compiled `internal/gvisor` test binary
(`go test -c ./internal/gvisor` from `runner/`), and a node-local reflink-capable directory:

```sh
export SECONDBOX_GVISOR_POD_IMAGE=<imported image reference>
export SECONDBOX_GVISOR_POD_BUILD=/absolute/path/to/build
export SECONDBOX_GVISOR_POD_TEST_BINARY=/absolute/path/to/gvisor.test
export SECONDBOX_GVISOR_POD_REFLINK=/absolute/path/on/reflink-fs
just test-gvisor-pod
just test-scenario-gvisor-pod
```

Pod qualification requires a node without `/dev/kvm`. The host scenario driver
also runs on a KVM-capable release host; it still selects gVisor and systrap,
without using KVM. The wrappers prepare and validate the flat root, verify the
pinned `runsc`, and derive materialization identity from the build directory.
Scenario coverage follows the selected release or nightly tier; see
[scenario qualification](scenario-qualification.md).
