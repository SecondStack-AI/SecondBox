# Firecracker Runtime Operations

This note covers the host-side pieces used by the Firecracker runtime.
See `docs/operations/microvm-image-pipeline.md` for image construction.

## Version pins

Startup asserts that `AG_FIRECRACKER_PATH --version` matches the in-repo pin at
`internal/microvm/firecracker.lock` (currently Firecracker `v1.16.1`). This is
deliberately stricter than a minimum version check because golden snapshots are
coupled to Firecracker's snapshot ABI.

The guest kernel source is pinned separately in
`scripts/microvm-image/kernel.lock` (currently Linux `6.12.94`). Build it with:

```sh
just build-microvm-kernel
```

Changing either pin is a runtime migration: rebuild image artifacts, discard
archived golden snapshots, restart runtimes, and run staging verification before
rollout.

## Jailer mode

Production and staging should leave `AG_MICROVM_ALLOW_UNJAILED=false`. In that
mode the manager stages each VM under:

```text
${AG_MICROVM_JAILER_CHROOT_BASE_DIR}/firecracker/${instance_id}/root
```

The staged jail contains non-secret boot artifacts only:

- `vmlinux`: copied from `AG_MICROVM_KERNEL_PATH`
- `rootfs.ext4`: linked from the per-instance rootfs clone
- `workspace.ext4`: linked from the persistent per-agent workspace image
- `shared.img`: copied from `AG_MICROVM_SHARED_IMAGE_PATH` when configured
- `firecracker.json`: chroot-local Firecracker config

Set `AG_MICROVM_JAILER_UID` and `AG_MICROVM_JAILER_GID` to the unprivileged host
identity that should own the jailed Firecracker process and staged files. The
default is the current process UID/GID, which is useful for local validation but
is not a production isolation boundary.

Jailer cgroup memory limiting is enabled through:

```env
AG_MICROVM_JAILER_CGROUP_VERSION=2
AG_MICROVM_JAILER_PARENT_CGROUP=agentcy
```

## CPU template

The default is `None` on x86. Static x86 Firecracker templates such as `T2A`
target specific older CPU models and can make CPUID and XSAVE state disagree on
newer hosts. On arm64, the default is `V1N1`.

Set `AG_MICROVM_CPU_TEMPLATE` only when deliberately targeting a matching host
fleet:

```env
AG_MICROVM_CPU_TEMPLATE=None
```

The manager also appends `noxsave` to the guest kernel args by default. Archived
snapshots created without the current CPU template or kernel args must be
discarded.

## Tap networking

Set `AG_MICROVM_BRIDGE_NAME` to attach every VM tap to a host bridge. When
`AG_MICROVM_BRIDGE_CIDR` is set, the manager creates the bridge if missing,
assigns that CIDR, and brings it up before attaching the tap.

Example:

```env
AG_MICROVM_BRIDGE_NAME=agfc0
AG_MICROVM_BRIDGE_CIDR=172.30.0.1/24
AG_MICROVM_GUEST_IP=172.30.0.10
```

The guest image must configure its interface to match `AG_MICROVM_GUEST_IP`.
The manager uses that IP for service/browser routing and egress proxy identity.

### Host bridge and firewall setup

The manager will auto-create a bare bridge when one is missing, but it does not
manage host firewall/NAT prerequisites. Provision those once per host with
`scripts/microvm-host-network-setup.sh`:

```sh
sudo env AG_MICROVM_BRIDGE_NAME=agfc0 AG_MICROVM_BRIDGE_CIDR=172.30.0.1/24 \
  scripts/microvm-host-network-setup.sh apply
# tear down with the same variables:
sudo env AG_MICROVM_BRIDGE_NAME=agfc0 scripts/microvm-host-network-setup.sh remove
```

It owns the host bridge, the guest subnet (`AG_MICROVM_GUEST_CIDR`), and optional
direct outbound NAT (`AG_MICROVM_ENABLE_DIRECT_NAT` / `AG_MICROVM_EGRESS_IFACE`);
the manager still owns per-VM tap creation and the per-instance transparent HTTP
redirect. Run `scripts/microvm-host-network-setup.sh --help` for the full
variable list.

The privileged launcher probes this posture on health checks, tap admission,
and immediately before every VM launch. It fails closed when the bridge is not
up with the configured CIDR, IPv4 forwarding is disabled, the guest-to-host or
guest-forward default-deny hooks are missing, or the IPv6 drop hooks have
drifted. The error lists stable invariant names such as
`guest_forward_default_deny`; it does not include firewall command output.

To diagnose a rejected launch, inspect the unit and compare the live rules to
the provisioning contract:

```sh
systemctl status agentcy-microvm-network.service
ip -o link show dev "${AG_MICROVM_BRIDGE_NAME:-agfc0}"
sysctl net.ipv4.ip_forward
sudo iptables-save | grep AGENTCY_
sudo ip6tables-save | grep AGENTCY_
```

Recover by reapplying the idempotent host policy with the same `AG_*` values as
the service, then restart the launcher only after the script succeeds:

```sh
sudo -E scripts/microvm-host-network-setup.sh apply
sudo systemctl restart agentcy-vmlauncher.service
```

Do not bypass the probe or start a VM while an invariant remains missing.

Hosted CI runs `scripts/test/microvm-network-namespace-test.sh` as a blocking
job. It applies the real host policy inside isolated Linux network namespaces
and proves the platform/proxy allow paths, transparent HTTP redirect, and
default-deny path without requiring KVM. That check does not replace the
owner-operated, self-hosted Firecracker isolation job: the latter remains the
required end-to-end proof for jailer, tap, generated-image, and vsock behavior
on a real KVM host.

## Transparent HTTP egress

When proxy egress is enabled for an agent and
`AG_EGRESS_PROXY_TRANSPARENT_HTTP_PORT` is set, the Firecracker manager registers
an iptables NAT rule for that instance:

```text
source AG_MICROVM_GUEST_IP tcp/80 -> local transparent proxy port
```

HTTPS still uses the explicit proxy environment injected through the runtime
secret bundle. The transparent route is removed when the VM is stopped or
removed.

## Golden snapshots

`internal/microvm.Manager.CreateGoldenSnapshot` is the operator-facing primitive
for baking a warmed VM into a golden snapshot. It:

- pauses the VM through Firecracker's API;
- writes a full `vmstate.snap` plus `memory.snap`;
- resumes the VM after the snapshot call;
- writes `manifest.json` with the kernel/rootfs/shared artifact paths and
  SHA-256 hashes plus the VM memory/vCPU shape.

The manifest is compatibility metadata, not a restore policy. A snapshot must be
discarded and recreated when the Firecracker version pin, guest-kernel pin, CPU
template, rootfs/shared image, or machine shape changes.

Golden-snapshot restore is intentionally not exposed by the manager. The former
restore method launched Firecracker directly and bypassed the root-owned,
peer-credentialed privileged launcher. Snapshot creation and artifact
verification remain available for diagnostics and future launcher-backed
restore work; do not treat these artifacts as a production restore pool.

Snapshot invalidation procedure:

1. Stop agent runtimes so snapshot creation cannot race the artifact swap.
2. Remove saved golden snapshots under the configured data/run snapshot
   directory for the affected agents.
3. Rebuild and verify microVM artifacts (`just build-microvm-images-std`,
   `just verify-microvm-staging`).
4. Restart `agentcy`; create a new diagnostic snapshot explicitly if one is
   needed after verification.

Guest-kernel patch cadence: review the `6.12.y` stable line monthly and after
any Linux guest CVE relevant to KVM, virtio, ext4, networking, or namespaces.
Patch by updating `scripts/microvm-image/kernel.lock`, rebuilding artifacts, and
following the invalidation procedure above.

## Workspace storage

The default workspace backend remains a sparse ext4 file:

```env
AG_MICROVM_WORKSPACE_BACKEND=ext4
AG_MICROVM_WORKSPACE_SIZE_MIB=8192
```

Production hosts can instead use a pre-created device-mapper thin pool:

```env
AG_MICROVM_WORKSPACE_BACKEND=dm-thin
AG_MICROVM_THIN_POOL_DEVICE=/dev/mapper/agentcy-thinpool
```

In dm-thin mode the manager provisions one thin device per agent workspace,
formats it as ext4 on first use, and attaches the block device directly to
Firecracker. `CreateWorkspaceThinSnapshot` freezes the guest workspace over
vsock, creates a host-side dm-thin snapshot device, and thaws the guest in a
defer path so backup readers can inspect a consistent point-in-time block view.

## Required host privileges

In SecondStack these privileges belong exclusively to the `agent-sandbox-host` artifact. Agent Service remains an unprivileged control plane and reaches the launcher through its Unix socket. See [SecondStack Agent Manager Artifacts](secondstack-artifacts.md) for the rendered-Compose validation command and the exact forbidden control-plane mounts/capabilities.

A staging or production host needs KVM plus enough privilege for:

- `jailer` chroot/cgroup setup
- tap creation and bridge attachment
- iptables NAT rule insertion
- device-mapper thin-pool administration when `AG_MICROVM_WORKSPACE_BACKEND=dm-thin`
- ownership changes for jailed boot artifacts when `AG_MICROVM_JAILER_UID/GID`
  differ from the manager process user

Run the staging gate on the target host before cutover:

```sh
AG_MICROVM_ALLOW_UNJAILED=false \
just verify-microvm-staging
```

The privileged gate runs
`TestSmokeJailedTapAndTransparentRouteGeneratedImage`, which boots a generated
image under jailer, creates the configured bridge/tap, installs a transparent
HTTP iptables route, and waits for the in-guest control heartbeat over vsock.
It must run on a genuinely privileged host. A root user inside an unprivileged
user/network namespace is not enough because Firecracker's jailer must create
`/dev/net/tun` inside the jail with `mknod`, which is denied in that environment.

For non-root configuration and artifact checks only:

```sh
just verify-microvm-staging --static
```
