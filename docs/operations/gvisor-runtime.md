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

## Qualification before enrollment

Run the backend qualification suites and the full scenario driver on the target host class — a
real Linux host without `/dev/kvm`:

```sh
export SECONDBOX_GVISOR_BUILD=/absolute/path/to/build
just test-gvisor "$SECONDBOX_GVISOR_BUILD"

export SECONDBOX_GVISOR_LINUX_BUILD=/absolute/path/to/build
just test-scenario-gvisor
```

The scenario wrapper refuses hosts that expose `/dev/kvm`, verifies the `runsc` binary against
the reviewed pin, derives the materialization from the build directory, and runs the complete
control-plane scenario suite against the gVisor runner container. Passing this document's checks
does not remove the experimental label.
