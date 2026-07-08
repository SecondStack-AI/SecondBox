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
golden snapshots, restart runtimes, and run staging verification before rollout.

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

Snapshot restore is sensitive to the guest CPU/xstate feature set. The default
is `None` on x86, which keeps save/restore on the same physical CPU in host
passthrough mode. Static x86 Firecracker templates such as `T2A` target specific
older CPU models and can make CPUID and XSAVE state disagree on newer hosts.
On arm64, the default is `V1N1`.

Set `AG_MICROVM_CPU_TEMPLATE` only when deliberately restoring across a matching
host fleet:

```env
AG_MICROVM_CPU_TEMPLATE=None
```

The manager also appends `noxsave` to the guest kernel args by default. Existing
per-agent snapshots created without the current CPU template or kernel args are
invalidated; the next start cold boots once and saves a new compatible snapshot
on stop.

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

Snapshot invalidation procedure:

1. Stop agent runtimes so no snapshot restore races the artifact swap.
2. Remove saved golden snapshots under the configured data/run snapshot
   directory for the affected agents.
3. Rebuild and verify microVM artifacts (`just build-microvm-images-std`,
   `just verify-microvm-staging`).
4. Restart `agentcy`; the first warm start recreates snapshots from the new VMM,
   kernel, CPU template, and image set.

Guest-kernel patch cadence: review the `6.12.y` stable line monthly and after
any Linux guest CVE relevant to KVM, virtio, ext4, networking, or namespaces.
Patch by updating `scripts/microvm-image/kernel.lock`, rebuilding artifacts, and
following the invalidation procedure above.

Restores can use the default Firecracker `File` memory backend or an explicit
`Uffd` backend path for a future userfaultfd page-fault handler. The restore
pool keeps paused clones ready; acquisition resumes a clone and asks the guest
control agent to mix host-supplied entropy into `/dev/urandom` and set the guest
clock to host time.

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
