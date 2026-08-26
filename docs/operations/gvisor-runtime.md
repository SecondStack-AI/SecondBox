# Experimental gVisor runner on Linux hosts without KVM

The gVisor runner is experimental. It is a separate, operator-managed Runner deployment for
Linux x86_64 hosts that cannot expose `/dev/kvm`; it does not alter or replace the qualified
Linux Firecracker installer, container, systemd units, network setup, or standard Profiles.

## Host contract

Use a Linux x86_64 host with loop-device support (`/dev/loop-control`), `nftables`, `iproute2`,
`e2fsprogs`, and a Btrfs or XFS volume with reflink support for the complete WorkspaceStore root.
The backend requires no KVM device, no TUN/TAP device, and no hardware-virtualization CPU flags:
the sentry runs on its systrap platform. The runner needs the authority to create network
namespaces, veth pairs, loop attachments, and nftables tables.

Check a candidate host with the installer preflight in its gVisor mode:

```sh
secondbox-deploy install --check --backend gvisor
```

The gVisor mode requires loop-device, `nft`, and `ip` availability and reports absent KVM, TUN,
and virtualization as passing observations rather than blockers. The Firecracker preflight is
unchanged.

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
- `rootfs/` is the flattened extract of the reviewed source OCI image. Its digest must equal the
  materialization's `flatRootDigest`, computed by `runner/cmd/secondbox-flat-root-digest`.

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
export SECONDBOX_GVISOR_RUNSC_PATH=/opt/secondbox-gvisor/bin/runsc
export SECONDBOX_GVISOR_AGENT_PATH=/opt/secondbox-gvisor/bin/secondbox-guest-agent
export SECONDBOX_GVISOR_FLAT_ROOT_PATH=/opt/secondbox-gvisor/rootfs
export SECONDBOX_GVISOR_MATERIALIZATION_PATH=/etc/secondbox/gvisor-linux-amd64.materialization.json
export SECONDBOX_GVISOR_MATERIALIZATION_DIGEST=sha256:OPERATOR_CANONICAL_DIGEST
export SECONDBOX_GVISOR_RUNTIME_DIR=/run/secondbox-gvisor
export SECONDBOX_GVISOR_MAXIMUM_VCPUS=8
export SECONDBOX_GVISOR_MAXIMUM_MEMORY_BYTES=8589934592
export SECONDBOX_GVISOR_MAXIMUM_DISK_BYTES=34359738368
export SECONDBOX_GVISOR_MAXIMUM_INSTANCES=4
export SECONDBOX_GVISOR_MAXIMUM_OPERATIONS=32
export SECONDBOX_GVISOR_WORKSPACE_TEMPLATE_CAPACITY_BYTES=8589934592
```

`SECONDBOX_GVISOR_NETWORK_PROFILE` (default `0`) separates runners sharing one host network
namespace: each profile selects its own DNS proxy address, link-local slot space, and veth and
namespace names. A single runner per host keeps the default.

Also set every required runner protocol address, RunnerPool ID, runner identity, mTLS
certificate, private key, CA, enabled feature, and evidence setting. These are deployment
authority and have no application defaults. The runner requires root for loop attachment, mount
and network namespaces, and nftables; the control plane must never run with that authority.

## End-to-end host bring-up

The complete host path, in order; every digest is computed from the operator's reviewed assets,
never copied from a document.

1. Compute and pin the identities from the build directory:

   ```sh
   cd runner && go run ./cmd/secondbox-flat-root-digest /opt/secondbox-gvisor/rootfs
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
   | `SECONDBOX_GVISOR_*` (all values from the environment block above) | The backend block, including capacity maxima, the materialization pin, the runtime directory, the network profile, and the optional `SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM` resolver override. |

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

- Build and publish the gVisor runner image yourself; CI builds the image and records its
  digest as workflow metadata (`runner-gvisor-oci-metadata`) but publishes no registry copy.
  From the reviewed source commit:

  ```sh
  docker buildx create --name secondbox-oci --driver docker-container --use
  docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
    --build-arg RELEASE_VERSION=<version> --build-arg SOURCE_COMMIT=$(git rev-parse HEAD) \
    --file runner/Dockerfile.gvisor \
    --tag <registry>/secondbox/runner-gvisor:<version> --push .
  ```

  Record the pushed digest (`docker buildx imagetools inspect`) and pin the pod to that exact
  digest reference.
- Dedicate a tainted node pool: one runner pod per node, tolerating the pool taint, with a
  node-local reflink-capable volume for the WorkspaceStore.
- Set the pod's resource requests and limits equal to the node's declared sandbox budget plus
  runner overhead. Per-sandbox cgroups nest inside the pod's slice, so the pod budget caps the
  sum of all sandboxes.
- Provide the per-runner identity (mTLS keypair, CA, and runner credential) as a Secret; the
  flat root and materialization manifest arrive on the node through the operator's reviewed
  artifact flow.
- The data plane is proxied through the control plane by default in clusters. The only
  qualified direct-transport option is the manifest's hostPort entry, which exposes the
  runner's data-plane listener on its node; remove it to stay proxied-only.

## Qualification before enrollment

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
just test-scenario-gvisor
```

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

The scenario wrapper refuses hosts that expose `/dev/kvm`, verifies the `runsc` binary against
the reviewed pin, derives the materialization from the build directory, and runs the complete
control-plane scenario suite against the gVisor runner container. Passing this document's checks
does not remove the experimental label.
