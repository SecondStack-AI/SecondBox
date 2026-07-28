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
SECONDBOX_RUNNER_STATE_DIR
SECONDBOX_RUNNER_LOG_PATH
SECONDBOX_RUNNER_FIRECRACKER_PATH
SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH
SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT
SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID
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
SECONDBOX_RUNNER_SANDBOX_WORKSPACE_DIR
SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_DIR
SECONDBOX_RUNNER_SANDBOX_STORAGE_BACKEND
SECONDBOX_RUNNER_SANDBOX_THIN_POOL_DEVICE
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
SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES
```

Production and qualification hosts set `SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED=false`. The jailer UID and GID identify the unprivileged process inside the jail and must be positive. The runner remains root so it can create jail roots, cgroups, TAP devices, and device-mapper resources.

The guest HTTP bootstrap/control surface and the canonical bidirectional gRPC data plane use distinct, explicitly configured vsock ports. `SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL` is a positive Go duration no greater than 60 seconds. The runner does not report an assignment ready until the canonical stream has negotiated the exact assignment Instance, Sandbox generation, random connection nonce, signed guest build and manifest identities, protocol generation, and mandatory features. `/workspace` and `/runtime-private` are signed image ABI mount points: init mounts the persistent workspace at the former and a RAM-only secret tmpfs at the latter.

Startup verifies that the Firecracker binary exactly matches `runner/internal/firecracker/firecracker.lock`. Snapshot compatibility depends on that exact version. The guest kernel source is separately pinned in `runner/scripts/microvm-image/kernel.lock`.

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

The jail contains only its kernel, rootfs clone, workspace image, optional shared image, Firecracker configuration, API socket, and `guest.vsock`. Unix socket paths are checked before staging. Cross-filesystem hard-link failures are fatal; configure the run, workspace, and jail locations on compatible filesystems.

The runner sets the configured cgroup version and parent on every jailed process. It also enforces the profile-resolved vCPU, memory, and disk limits in the Firecracker configuration and guest boot contract.

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

The base policy allows established traffic and otherwise denies guest-to-host input, forwarding, and IPv6. It does not install NAT, redirects, or service-port exceptions. Assignments currently inherit this default-deny connectivity policy.

`runner/scripts/microvm-network-namespace-test.sh` exercises the real bridge and firewall script in isolated Linux network namespaces and proves that an unassigned host port remains unreachable.

## Workspace storage and cleanup

The ext4 backend stores one persistent workspace image per sandbox, instance, and assigned generation. The `dm-thin` backend binds the generation into the device identity, requires the explicit `SECONDBOX_RUNNER_SANDBOX_THIN_POOL_DEVICE`, and creates devices with the `secondbox-ws-` prefix.

Runner storage must be isolated from the host root filesystem. `SECONDBOX_RUNNER_SANDBOX_WORKSPACE_DIR` and `SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_DIR` must be non-symbolic-link directories on separate dedicated filesystems with device identities different from `/` and each other. The dm-thin pressure probe samples and allocates only the exact configured pool; a failed `dmsetup` probe is fatal and never falls back to filesystem capacity. The restore spool has its own filesystem probe and reservations, so its capacity is never inferred from thin-pool capacity.

The same-host Compose profile mounts `SECONDBOX_RUNNER_WORKSPACE_HOST_DIR` and `SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_HOST_DIR` independently at those runtime paths. The deployment validator requires both host directories to exist and verifies that their filesystem devices differ from the host root and from each other.

The recovery, warning, and admission-denial percentages are required integers satisfying `0 < recovery < warning < admission deny < 100`. One storage-pressure controller combines measured consumption with atomic reservations for accepted-but-not-yet-materialized assignment disks. It emits bounded `storage_pressure` evidence on warning, admission denial, probe failure, and recovery. Warning does not reject work. Admission denial occurs before workspace progress or allocation, remains latched while utilization is above the recovery threshold, and makes readiness fail closed. Probe errors also fail readiness and admission rather than returning partial success.

Cleanup is explicit and generation-fenced. Failed and expired restores remove their exact partial spool file and release its reservation; a successfully materialized restore is consumed before the replacement assignment becomes ready. Fencing releases an assignment reservation only after the exact generation has been removed. Under admission pressure, cleanup may remove only released generation workspaces whose checkpoint stream completed before fencing. Active assignments, replacement generations, and uncheckpointed local durable roots are never cleanup candidates. A cleanup failure remains retryable, emits `storage_pressure_cleanup_failed`, and keeps admission closed. The next observation reopens admission and emits recovery only after measured and reserved utilization reaches the configured recovery threshold.

Natural Firecracker exit and explicit fencing both pass through the same idempotent cleanup path. The runner waits or polls for the jailed process, removes the assignment index, removes its TAP, releases the guest address only after TAP cleanup succeeds, and retains the address on cleanup failure. Generation and opaque fencing tokens must match exactly before a live assignment can be stopped.

Snapshot creation remains an operator diagnostic. Golden VM state is rebuildable and is not the source of truth for assignment generation or workspace state.

## Qualification

Static qualification requires all runtime inputs listed by `runner/scripts/microvm-stage-check.sh`:

```sh
runner/scripts/microvm-stage-check.sh --static
```

Privileged qualification uses the same explicit environment and runs as root:

```sh
sudo --preserve-env=SECONDBOX_RUNNER_FIRECRACKER_PATH,SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH,SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH,SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH,SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH,SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT,SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID,SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID,SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION,SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT,SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED,SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS,SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY,SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256,SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME,SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR,SECONDBOX_RUNNER_SANDBOX_GUEST_IP,SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX,SECONDBOX_RUNNER_SANDBOX_STORAGE_BACKEND,SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB,SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB,SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT,SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT,SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT,SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_DIR \
  runner/scripts/microvm-stage-check.sh
```

The privileged check requires real `/dev/kvm`, jailer, cgroup, bridge/TAP, signed artifact, generated guest, and vsock readiness. Missing evidence fails; qualification does not convert a requested capability failure into a skip.
