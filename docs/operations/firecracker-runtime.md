# Firecracker Runner Operations

This runbook covers the standalone privileged SecondBox Runner and its Firecracker backend. Image construction and signing are described in `microvm-image-pipeline.md`.

## Runtime topology

`secondbox-runner` is the privileged composition root. It opens one outbound, mutually authenticated gRPC stream to the control plane, negotiates the supported `secondbox.runner.v1` protocol window, publishes verified readiness and capacity, and accepts only profile-resolved assignments with generation and fencing identity.

The runner launches Firecracker directly through `jailer`; no auxiliary local process participates in compute composition. The runner owns jail roots, cgroups, network namespaces and TAP devices, vsock communication, workspace images, process reaping, assignment fencing, and termination evidence.

The in-guest service is reached over Firecracker vsock. Assignment identity and runtime credentials are delivered after the guest is ready; host process environment is not inherited by the guest.

## Explicit configuration

Runner configuration has no defaults. Every setting consumed by the service must be explicitly present and non-empty.

Control stream identity and mTLS:

```text
SECONDBOX_RUNNER_ID
SECONDBOX_RUNNER_POOL_ID
SECONDBOX_RUNNER_SOFTWARE_VERSION
SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS
SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME
SECONDBOX_RUNNER_CLIENT_CERTIFICATE
SECONDBOX_RUNNER_CLIENT_KEY
SECONDBOX_RUNNER_CONTROL_PLANE_CA
```

Firecracker, isolation, artifacts, and storage:

```text
SECONDBOX_RUNNER_LOG_PATH
SECONDBOX_RUNNER_FIRECRACKER_PATH
SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH
SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT
SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_START
SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_COUNT
SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_ALLOW_BELOW_1000
SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID
SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION
SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT
SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH
SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH
SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH
SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS
SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT
SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT
SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL
SECONDBOX_RUNNER_FIRECRACKER_CPU_TEMPLATE
SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR
SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR
SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED
SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY
SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256
SECONDBOX_RUNNER_WORKSPACE_ROOT
SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT
SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT
SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT
SECONDBOX_RUNNER_SANDBOX_MAX_VCPUS
SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB
SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB
SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB
SECONDBOX_RUNNER_SANDBOX_GUEST_IP
SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME
SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR
SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX
SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX
SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL
SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS
SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES
SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL
SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES
SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS
SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS
```

Production and qualification hosts set `SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED=false`. The jailer UID range supplies one unprivileged identity per live Instance; its count must cover `SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL`. The start must be at least 1000 unless `SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_ALLOW_BELOW_1000=true` explicitly acknowledges intentional use of the host's system-UID space. UID 0 and ranges that exceed unsigned 32-bit UIDs are rejected. Same-host preflight also rejects UIDs assigned to host accounts. The GID remains one positive shared group identity. The runner remains root so it can create jail roots, cgroups, TAP devices, and the required jailed file bindings.

The qualified kernel arguments include `quiet loglevel=1` and
`i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd`. Firecracker has no PS/2
controller, so omitting the i8042 arguments makes Linux wait for legacy device
probes during every Sandbox boot. Verbose kernel output also serializes boot
messages through Firecracker's emulated UART. The deployment validator rejects
kernel arguments that omit these latency-critical flags.

`SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL` bounds resident Firecracker Instances. `SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS` independently bounds transient assignment-start work and must not exceed the resident limit. `SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL` advertises runner-wide data-plane operation capacity; scheduling reserves each active Sandbox's immutable Profile limit against it.

`SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES` bounds formatting of
independent generation-one Workspace images. Other Workspace mutations remain
ordered behind outstanding creates so snapshot, restore, delete, reconciliation,
and assignment attachment cannot cross a create barrier.

`SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS` binds the caller-facing Port
transport and is a bind specification, so a wildcard host and an ephemeral port
are both valid. `SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS` is the
`host:port` the ingress tier dials and must name a reachable host and a fixed
port. The deployment decides how the bound socket is reachable; the runner never
infers one setting from the other. The advertised value is administrative
capacity evidence of the same class as advertised capacity and carries no
Sandbox identity. It reaches a caller only through a PortSession created by an
application authority holding the exact `sandbox:ports:direct` scope. An
unavailable listener makes the runner unready and fences its active instances,
matching the network-policy listener rule.

The guest HTTP bootstrap/control surface and the canonical bidirectional gRPC data plane use distinct, explicitly configured vsock ports. `SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL` is a positive Go duration no greater than 60 seconds. The runner does not report an assignment ready until the canonical stream has negotiated the exact assignment Instance, Sandbox generation, random connection nonce, signed guest build and manifest identities, protocol generation, and mandatory features. `/workspace` and `/runtime-private` are signed image ABI mount points: init mounts the persistent workspace at the former and a RAM-only secret tmpfs at the latter.

Startup verifies that the Firecracker binary exactly matches `runner/internal/firecracker/firecracker.lock`. Runtime qualification depends on that exact version. The guest kernel source is separately pinned in `runner/scripts/microvm-image/kernel.lock`.

## Signed artifact readiness

The kernel, rootfs, shared image, manifest, signature, checksums, kernel provenance, rootfs source manifest, rootfs contract, package locks, and license inventories form one signed artifact directory. The runner requires an independently provisioned RSA public key and its lowercase DER SHA-256 fingerprint.

Before registration, readiness verifies the signature, checksums, manifest paths and digests, signed architecture and guest-protocol range, rootfs contract, captured file identities, read-write KVM access, cgroup controllers, the configured bridge address, workspace storage, and cleanup health. An assignment is admitted only when its artifact ID, manifest digest, signature key ID, architecture, and guest protocol generation select that verified local artifact. Artifact mutation after startup fails health checks and assignment launch.

Build and verification inputs are explicit. Inspect the current required inputs with:

```sh
runner/scripts/microvm-image/build.sh --help
runner/scripts/microvm-image/build-kernel.sh --help
runner/scripts/microvm-image/verify.sh --help
```

`verify.sh` requires the artifact directory, trusted public key, and trusted public-key fingerprint as positional arguments. Missing, unsigned, or incorrectly signed manifests fail.

## Jailer and cgroups

Each assignment receives a bounded opaque Firecracker instance ID and a jail rooted at:

```text
${SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT}/firecracker/${instance_id}/root
```

The jail contains only its kernel, rootfs clone, attached Workspace image, optional shared image, Firecracker configuration, API socket, and `guest.vsock`. Unix socket paths are checked before staging. The Runner resolves the private Workspace attachment inside the process and never copies or reformats it during start.

The runner sets the configured cgroup version and parent on every jailed process. The host memory cgroup includes the guest allocation plus 10% or 256 MiB, whichever is larger. The CPU cgroup uses a 100 ms period and the exact Profile CPU millis plus 10% or 100 millis, whichever is larger, so bounded Firecracker emulation and I/O work does not consume the entire guest allowance. The PID cgroup uses the Profile process limit plus 32 jailer/VMM worker slots and two slots per vCPU. It also enforces the profile-resolved vCPU, memory, disk, and guest process limits in the Firecracker configuration and guest boot contract.

Before launch, the Runner reserves a unique UID and persists it as root-owned per-Instance run state. Process launch, jailed artifact ownership, the Workspace hard link, and TAP ownership all use that exact UID. Normal teardown releases it only after the process and network cleanup complete. Startup orphan sweep reads the persisted UID before adopting and stopping a surviving process; missing, malformed, out-of-range, or conflicting evidence fails startup closed. Stale state for an already-dead process is removed without reserving its former UID.

When the control plane has accepted readiness, the runner snapshots the exact `oom_kill` counter from the Instance cgroup: `memory.events.local` for cgroup v2 or `memory.oom_control` for cgroup v1. After an unrequested Firecracker disappearance, an increased counter is the only evidence classified as `resource_exhaustion`. A generic process exit, exit code, missing counter, malformed counter, counter regression, or unavailable cgroup is never treated as OOM.

Natural `guest_shutdown` requires the exact successful-exit message emitted by the pinned Firecracker 1.16.1 process log after readiness and no Runner stop, fence, or cleanup has begun. Log parsing validates the structured line and exact message; substring matches do not qualify. Any other post-ready disappearance is `internal_failure`. The successful-exit marker must be requalified before changing `runner/internal/firecracker/firecracker.lock`. Unit tests qualify parsers and classification without KVM; the privileged Firecracker lane remains the boundary for proving the real jailer cgroup layout and process log on `/dev/kvm`.

## Host bridge policy

`runner/scripts/microvm-host-network-setup.sh` creates the standalone bridge and installs the base firewall policy. It requires:

```text
SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME
SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR
SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR
SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX
SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR
SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE
```

Apply and remove with the exact same environment:

```sh
sudo --preserve-env=SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME,SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR,SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR,SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX,SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR,SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE \
  runner/scripts/microvm-host-network-setup.sh apply

sudo --preserve-env=SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME,SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR,SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR,SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX,SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR,SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE \
  runner/scripts/microvm-host-network-setup.sh remove
```

The base policy allows established traffic, the runner DNS proxy, and
assignment-policy-approved connections carrying the runner's private
connection mark. Only marked IPv4 egress is masqueraded. All other
guest-to-host input, forwarding, and IPv6 remain denied; no redirect or
service-port exception is installed. Each bridge owns separate firewall
chains so applying or removing one Runner network cannot alter another
Runner's baseline on the same host network namespace.

`runner/scripts/microvm-network-namespace-test.sh` exercises the real bridge and firewall script in isolated Linux network namespaces and proves that an unassigned host port remains unreachable.

## Workspace storage and cleanup

`SECONDBOX_RUNNER_WORKSPACE_ROOT` is one required clean absolute path on a
dedicated reflink-capable filesystem. It contains versioned sparse raw ext4
Workspace images, atomic current-image manifests, immutable local Snapshot
reflinks, immutable capacity-keyed ext4 templates, staged and rollback restore
state, locks, and durable operation receipts. The directory must not be a
symbolic link or the filesystem root.

Runner startup creates the deterministic private layout, proves the Workspace
and Snapshot roots share one device, performs a real `FICLONE`, and verifies
copy-on-write mutation isolation. Any failure makes the Runner unready; there is
no byte-copy, alternate backend, or object-store fallback. `mke2fs` is required
to preformat the explicitly configured maximum-capacity template before Runner
registration, and `tune2fs` is required to assign each reflink child its stable
Workspace UUID before publication. Other exact capacities are formatted once
on first use and then reused. Existing templates must retain their exact size,
read-only mode, ext4 identity, and deterministic template UUID; invalid
template evidence makes startup or Workspace creation fail closed.

The same-host Compose profile mounts the explicitly configured
`SECONDBOX_RUNNER_WORKSPACE_HOST_DIR` at
`SECONDBOX_RUNNER_WORKSPACE_ROOT`. The deployment validator requires that host
directory to exist on a device distinct from the host root. Operators must back
up this directory together with the stable Runner identity; it is authoritative
durable state, not expendable cache.

The recovery, warning, and admission-denial percentages are required integers satisfying `0 < recovery < warning < admission deny < 100`. One storage-pressure controller combines measured consumption with atomic reservations for accepted-but-not-yet-materialized assignment disks. It emits bounded `storage_pressure` evidence on warning, admission denial, probe failure, and recovery. Warning does not reject work. Admission denial occurs before workspace progress or allocation, remains latched while utilization is above the recovery threshold, and makes readiness fail closed. Probe errors also fail readiness and admission rather than returning partial success.

Cleanup is explicit, operation-scoped, and generation-fenced. Restore abort may remove only pre-swap staged files. After a swap, the previous image and rollback manifest remain until PostgreSQL commits the matching generation and sends finalize. Snapshot and Workspace deletion validate exact logical paths and persist receipts before acknowledgement. Storage pressure never authorizes deletion of a Sandbox Workspace or Snapshot; admission remains closed until explicit deletion or operator capacity work returns utilization to the recovery threshold.

Natural Firecracker exit and explicit fencing both pass through the same idempotent cleanup path. The runner waits or polls for the jailed process, removes the assignment index, removes its TAP, releases the guest address only after TAP cleanup succeeds, and retains the address on cleanup failure. Generation and opaque fencing tokens must match exactly before a live assignment can be stopped.

Firecracker VM-state diagnostic snapshots, if used during qualification, are unrelated to public Sandbox Snapshots. Public Snapshots are immutable local reflinks of a stopped Sandbox Workspace and never contain Firecracker process state.

## Qualification

Static qualification requires all runtime inputs listed by `runner/scripts/microvm-stage-check.sh`:

```sh
runner/scripts/microvm-stage-check.sh --static
```

Privileged qualification uses the same explicit environment and runs as root:

```sh
sudo --preserve-env=SECONDBOX_RUNNER_FIRECRACKER_PATH,SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH,SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH,SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH,SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH,SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT,SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_START,SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_COUNT,SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_ALLOW_BELOW_1000,SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID,SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION,SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT,SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED,SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS,SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT,SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT,SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL,SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY,SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256,SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME,SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR,SECONDBOX_RUNNER_SANDBOX_GUEST_IP,SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX,SECONDBOX_RUNNER_WORKSPACE_ROOT,SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB,SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB,SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT,SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT,SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT \
  runner/scripts/microvm-stage-check.sh
```

The privileged check requires real `/dev/kvm`, jailer, cgroup, bridge/TAP, signed artifact, generated guest, and vsock readiness. Missing evidence fails; qualification does not convert a requested capability failure into a skip.
