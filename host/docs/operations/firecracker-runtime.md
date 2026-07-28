# Firecracker Runtime Operations

This note covers the privileged Sandbox Host Firecracker adapter.
See `microvm-image-pipeline.md` for image construction.

## Version pins

Startup asserts that `SANDBOX_HOST_FIRECRACKER_PATH --version` matches the in-repo pin at
`internal/firecracker/firecracker.lock` (currently Firecracker `v1.16.1`). This is
deliberately stricter than a minimum version check because golden snapshots are
coupled to Firecracker's snapshot ABI.

The guest kernel source is pinned separately in
`scripts/microvm-image/kernel.lock` (currently Linux `6.12.94`). Build it with:

```sh
just build-microvm-kernel
```

Changing either pin requires rebuilding image artifacts, discarding archived
golden snapshots, restarting Sandbox Host, and running staging verification
before enabling new Instances.

## Artifact activation

Sandbox Host starts only after init has materialized the signed microVM bundle at `SANDBOX_HOST_MICROVM_ARTIFACTS_DIR`. Source deployments select `directory`; tagged source-less releases select the dedicated `ghcr-image` transport and carry the exact image tag and trusted public-key fingerprint in their staged `.env.template`. The init helper fails closed on a missing source, an unknown mode, an image-label mismatch, an invalid signature or checksum, unexpected files, or a different non-empty target.

The transport image is only a host delivery mechanism. Init pulls and extracts it through Docker, verifies it, and atomically publishes the host directory. Sandbox Host receives the resulting directory read-only. Agent Manager receives only the launcher socket and remains unprivileged.

For an init failure, inspect the output of `27_install_sandbox_host_microvm_artifacts.sh`, confirm the three configured values for source mode, selected directory or image, and `SANDBOX_HOST_MICROVM_PUBLIC_KEY_SHA256`, then inspect the target directory. Do not delete or replace a non-empty target until its signed manifest and the intended release identity have been independently reconciled.

## Jailer mode

Production and staging should leave `SANDBOX_HOST_MICROVM_ALLOW_UNJAILED=false`. In that
mode the manager stages each VM under:

```text
${SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR}/firecracker/${instance_id}/root
```

Sandbox Host derives a bounded opaque `instance_id` from the Agent and compartment identities so the jailed API and vsock paths remain below the Unix-socket path limit with the shipped chroot directory.

The staged jail contains non-secret boot artifacts only:

- `vmlinux`: copied from `SANDBOX_HOST_MICROVM_KERNEL_PATH`
- `rootfs.ext4`: linked from the per-instance rootfs clone
- `workspace.ext4`: linked from the persistent per-agent workspace image
- `shared.img`: copied from `SANDBOX_HOST_MICROVM_SHARED_IMAGE_PATH` when configured
- `firecracker.json`: chroot-local Firecracker config

Set `SANDBOX_HOST_MICROVM_JAILER_UID` and `SANDBOX_HOST_MICROVM_JAILER_GID` to the unprivileged host
identity that should own the jailed Firecracker process and staged files. The
default is the current process UID/GID, which is useful for local validation but
is not a production isolation boundary.

Jailer cgroup memory limiting is enabled through:

```env
SANDBOX_HOST_MICROVM_JAILER_CGROUP_VERSION=2
SANDBOX_HOST_MICROVM_JAILER_PARENT_CGROUP=sandbox-host
```

## CPU template

The default is `None` on x86. Static x86 Firecracker templates such as `T2A`
target specific older CPU models and can make CPUID and XSAVE state disagree on
newer hosts. On arm64, the default is `V1N1`.

Set `SANDBOX_HOST_MICROVM_CPU_TEMPLATE` only when deliberately targeting a matching host
fleet:

```env
SANDBOX_HOST_MICROVM_CPU_TEMPLATE=None
```

Sandbox Host also appends `noxsave` to the guest kernel args. Archived
snapshots created without the current CPU template or kernel args must be
discarded.

## Tap networking

Set `SANDBOX_HOST_MICROVM_BRIDGE_NAME` to attach every VM tap to a host bridge. When
`SANDBOX_HOST_MICROVM_BRIDGE_CIDR` is set, the manager creates the bridge if missing,
assigns that CIDR, and brings it up before attaching the tap.

Example:

```env
SANDBOX_HOST_MICROVM_BRIDGE_NAME=agfc0
SANDBOX_HOST_MICROVM_BRIDGE_CIDR=172.30.0.1/24
SANDBOX_HOST_MICROVM_GUEST_IP=172.30.0.10
```

The guest image must configure its interface to match `SANDBOX_HOST_MICROVM_GUEST_IP`.
The manager uses that IP for service/browser routing and egress proxy identity.

### Host bridge and firewall setup

Sandbox Host creates a bare bridge when one is missing, but it does not
manage host firewall/NAT prerequisites. Provision those once per host with
`scripts/microvm-host-network-setup.sh`:

```sh
sudo env SANDBOX_HOST_MICROVM_BRIDGE_NAME=agfc0 SANDBOX_HOST_MICROVM_BRIDGE_CIDR=172.30.0.1/24 \
  scripts/microvm-host-network-setup.sh apply
# tear down with the same variables:
sudo env SANDBOX_HOST_MICROVM_BRIDGE_NAME=agfc0 scripts/microvm-host-network-setup.sh remove
```

It owns the host bridge, the guest subnet (`SANDBOX_HOST_MICROVM_GUEST_CIDR`), and optional
direct outbound NAT (`SANDBOX_HOST_MICROVM_ENABLE_DIRECT_NAT` / `SANDBOX_HOST_MICROVM_EGRESS_IFACE`);
Sandbox Host still owns per-VM tap creation and the per-instance transparent HTTP
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
systemctl status sandbox-host-guest-network.service
ip -o link show dev "${SANDBOX_HOST_MICROVM_BRIDGE_NAME:-agfc0}"
sysctl net.ipv4.ip_forward
sudo iptables-save | grep AGENT_MANAGER_
sudo ip6tables-save | grep AGENT_MANAGER_
```

Recover by reapplying the idempotent host policy with the same `AGENT_MANAGER_*` values as
the service, then restart the launcher only after the script succeeds:

```sh
sudo -E scripts/microvm-host-network-setup.sh apply
./force_restart.sh sandbox-host --rebuild
```

Do not bypass the probe or start a VM while an invariant remains missing.

Hosted CI runs `scripts/microvm-network-namespace-test.sh` as a blocking
job. It applies the real host policy inside isolated Linux network namespaces
and proves the platform/proxy allow paths, transparent HTTP redirect, and
default-deny path without requiring KVM. That check does not replace the
owner-operated, self-hosted Firecracker isolation job: the latter remains the
required end-to-end proof for jailer, tap, generated-image, and vsock behavior
on a real KVM host.

## Transparent HTTP egress

When proxy egress is enabled for an agent and
`SANDBOX_HOST_EGRESS_PROXY_TRANSPARENT_HTTP_PORT` is set, the Firecracker manager registers
an iptables NAT rule for that instance:

```text
source SANDBOX_HOST_MICROVM_GUEST_IP tcp/80 -> local transparent proxy port
```

HTTPS still uses the explicit proxy environment injected through the runtime
secret bundle. The transparent route is removed when the VM is stopped or
removed.

## Harness transient-unit secret transport

Agent Service registers the launcher-derived harness guest address with
Integration Service before invoking the privileged launcher. The resulting
short-lived source proof is one of the validated harness variables below and is
removed from Integration's proxy registry after the launcher returns. Host
harness bindings admit no connector IDs; their purpose is source attribution
for ordinary platform/model traffic, not credential injection.

The privileged launcher starts each host harness cell as a bounded transient systemd service. Scoped bearer tokens, wake JSON, and the other validated harness variables are written to the cell-bound file `${SANDBOX_HOST_LAUNCHER_STATE_DIR}/harness/<cell-id>.env`. The launcher creates that file exclusively as root with mode `0600`; systemd reads it through `EnvironmentFile=` immediately before execution. The unit has a constant description, and neither `systemd-run` arguments nor bubblewrap arguments contain environment assignments or values. A non-root helper in the Sandbox Host executable retains only fixed `HOME` and the validated variable names before replacing itself with bubblewrap, so systemd manager variables cannot widen the harness environment. Only allowlisted names appear in the helper arguments; bubblewrap inherits their values without rebuilding them with `--setenv`.

The launcher removes the environment file after every completed or failed start. The persisted harness state binds the exact environment-file path so launcher startup recovery also deletes it after a launcher or host crash. An existing file, path mismatch, malformed environment entry, duplicate variable, attempted `HOME` override, or mismatch between the legacy bubblewrap request assignment and the validated environment fails execution before systemd starts. Operators may inspect transient unit properties and the journal without exposing scoped tokens or wake payloads; the root-owned environment file itself must never be copied into diagnostic bundles.

## Golden snapshots

Sandbox Host's `internal/firecracker.Manager.CreateGoldenSnapshot` is the operator-facing primitive
for baking a warmed VM into a golden snapshot. It:

- pauses the VM through Firecracker's API;
- writes a full `vmstate.snap` plus `memory.snap`;
- resumes the VM after the snapshot call;
- writes `manifest.json` with the kernel/rootfs/shared artifact paths and
  SHA-256 hashes plus the VM memory/vCPU shape.

The manifest is compatibility metadata, not a restore policy. A snapshot must be
discarded and recreated when the Firecracker version pin, guest-kernel pin, CPU
template, rootfs/shared image, or machine shape changes.

Golden-snapshot restore is not exposed by Sandbox Service. Snapshot creation and
artifact verification are diagnostic operations; golden artifacts are not a
production restore pool.

Snapshot invalidation procedure:

1. Stop agent runtimes so snapshot creation cannot race the artifact swap.
2. Remove saved golden snapshots under the configured data/run snapshot
   directory for the affected agents.
3. Rebuild and verify microVM artifacts (`just build-microvm-images-std`,
   `just verify-microvm-staging`).
4. Restart `agent-manager`; create a new diagnostic snapshot explicitly if one is
   needed after verification.

Guest-kernel patch cadence: review the `6.12.y` stable line monthly and after
any Linux guest CVE relevant to KVM, virtio, ext4, networking, or namespaces.
Patch by updating `scripts/microvm-image/kernel.lock`, rebuilding artifacts, and
following the invalidation procedure above.

## Workspace storage

The default workspace backend remains a sparse ext4 file:

```env
SANDBOX_HOST_MICROVM_WORKSPACE_BACKEND=ext4
SANDBOX_HOST_MICROVM_WORKSPACE_SIZE_MIB=8192
```

Production hosts can instead use a pre-created device-mapper thin pool:

```env
SANDBOX_HOST_MICROVM_WORKSPACE_BACKEND=dm-thin
SANDBOX_HOST_MICROVM_THIN_POOL_DEVICE=/dev/mapper/sandbox-host-thinpool
```

In dm-thin mode the manager provisions one thin device per agent workspace,
formats it as ext4 on first use, and attaches the block device directly to
Firecracker. `CreateWorkspaceThinSnapshot` freezes the guest workspace over
vsock, creates a host-side dm-thin snapshot device, and thaws the guest in a
defer path so backup readers can inspect a consistent point-in-time block view.

### Logical versions and terminal checkpoints

Sandbox Service records a monotonic logical workspace version for every terminal turn, including failed and cancelled turns. The Firecracker Sandbox Host creates opaque checkpoints and content hashes at the service's generation-fenced requests. Terminal checkpointing stops disposable compute without emitting the owner-wide emergency revocation used by an operator-requested Environment stop, so the completing turn cannot cancel itself while its evidence is being persisted. Terminal handling rejects stale generations and active conflicting leases before a version can be committed.

Snapshot metadata and logical versions live only in the `sandbox` schema. Opaque checkpoint bytes and retained workspace images live only in Sandbox Host state. Ext4-image checkpoint creation uses a same-filesystem copy-on-write clone before hashing the immutable clone; it never expands an 8 GiB sparse workspace into an 8 GiB physical copy. A host filesystem without copy-on-write clone support fails the checkpoint explicitly. Exact fork materialization resolves an immutable service-owned logical version, verifies its snapshot evidence, and asks the Host to materialize it into the target generation. Missing, altered, or mismatched evidence fails closed.

The authenticated `GET /v1/workspace-usage` endpoint returns subject-scoped durable storage evidence. Quota is the sum of retained Environments' Resource Class disk limits, and usage is the sum of their current immutable checkpoint sizes. A running Environment may contain writes newer than its last terminal checkpoint; operations deliberately report durable recovery evidence instead of probing mutable Host files.

The Sandbox Service backup bundle captures the `sandbox` schema and matching Host state in one checksummed recovery point. Agent Service database backups do not own or reconstruct workspace versions.

Golden snapshots, `vmstate.snap`, and `memory.snap` are rebuildable acceleration artifacts only. Logical-version recovery never reads them: canonical Session state comes from Agent Service/Flue persistence, while workspace state comes from the generation base or verified terminal checkpoint. Deleting golden snapshots cannot change which committed workspace version is recovered.

## Required host privileges

In SecondStack these privileges belong exclusively to the `sandbox-host` artifact. Agent Service remains an unprivileged control plane and reaches the launcher through its Unix socket. See [SecondStack deployment artifacts](secondstack-artifacts.md) for the rendered-Compose validation command and the exact forbidden control-plane mounts/capabilities.

A staging or production host needs KVM plus enough privilege for:

- `jailer` chroot/cgroup setup
- tap creation and bridge attachment
- iptables NAT rule insertion
- device-mapper thin-pool administration when `SANDBOX_HOST_MICROVM_WORKSPACE_BACKEND=dm-thin`
- ownership changes for jailed boot artifacts when `SANDBOX_HOST_MICROVM_JAILER_UID/GID`
  differ from the manager process user

Run the staging gate on the target host before enabling Sandbox workloads:

```sh
SANDBOX_HOST_MICROVM_ALLOW_UNJAILED=false \
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

## Restored workspace guest-read qualification

SecondStack full recovery bundles require `AGENT_MANAGER_BACKUP_REFERENCE_GUEST_FILE_PATH` and `AGENT_MANAGER_BACKUP_REFERENCE_GUEST_FILE_SHA256` for the reference session compartment. Create the canary inside that compartment before quiescing the source deployment and record the SHA-256 of its exact bytes. The path is relative to `/workspace`; absolute paths, parent traversal, and backslashes are rejected.

Agent Service restore drills cover only the `agents` and `agent_flue` schemas plus Agent-owned durable files and recovery keys. Sandbox Service performs the independent Environment restore drill against its `sandbox` schema and Host-state bundle; Agent Service never reconstructs or inspects workspace images.

The restore host therefore needs the same Firecracker or privileged-launcher access, KVM access, signed artifact paths, pinned public key, and jailer configuration as the source Sandbox Host. A checkout with `/dev/kvm` but no Firecracker binary or signed artifact bundle can run the contract tests, but it cannot produce recovery qualification evidence.
