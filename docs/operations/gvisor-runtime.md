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
   printf 'sha256:%s\n' "$(jq --compact-output --join-output . \
     /etc/secondbox/gvisor-linux-amd64.materialization.json | sha256sum | awk '{print $1}')"
   ```

   Record the flat-root digest and launch-artifact digests in the reviewed materialization
   manifest, then pin the canonical manifest digest in
   `SECONDBOX_GVISOR_MATERIALIZATION_DIGEST`.

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

4. Export the complete environment from this document plus the runner protocol settings
   (control-plane address and server name, credential, identity paths, pool ID, data-plane
   addresses, log paths, concurrency and storage-pressure bounds), then start the runner as a
   root systemd service whose unit states that exact environment. Readiness is observable:

   ```sh
   curl -fsS http://127.0.0.1:8080/metrics | grep runner
   secondbox runners list --pool <gvisor-pool>
   ```

   The Runner appears `ready` only after its materialization revalidation, loop and cgroup
   reconciliation, host network plumbing, and the `runsc` boot probe all pass.

5. Tear down without losing Workspaces: stop the systemd unit (the backend fences and flushes
   every Instance), leave `SECONDBOX_RUNNER_WORKSPACE_ROOT` untouched, and decommission the
   Runner through the control plane only when its Workspaces have been relocated or are no
   longer needed. The WorkspaceStore root is the durable authority; never delete it as part of
   a runner restart or upgrade.

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
real Linux host without `/dev/kvm`:

```sh
export SECONDBOX_GVISOR_BUILD=/absolute/path/to/build
just test-gvisor "$SECONDBOX_GVISOR_BUILD"

export SECONDBOX_GVISOR_LINUX_BUILD=/absolute/path/to/build
just test-scenario-gvisor
```

For the pod placement, qualify the mechanisms and the identical scenario suite on the target
node class (a no-KVM Kubernetes node with local kubectl):

```sh
just test-gvisor-pod
just test-scenario-gvisor-pod
```

The scenario wrapper refuses hosts that expose `/dev/kvm`, verifies the `runsc` binary against
the reviewed pin, derives the materialization from the build directory, and runs the complete
control-plane scenario suite against the gVisor runner container. Passing this document's checks
does not remove the experimental label.
