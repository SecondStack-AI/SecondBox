# Deployment and runtime operations

SecondBox deploys one unprivileged control plane and separately managed privileged Firecracker Runners. Operators describe the deployment in one strict, versioned `secondbox.toml`; `secondbox-deploy` compiles that manifest into the process environments consumed by Compose, `secondboxd`, and remote Runner service managers. The generated environment is transport, not operator input.

The standalone binary distribution includes `secondbox-deploy`; the commands below assume it is on `PATH`. The `Justfile` continues to use `go run ./cmd/secondbox-deploy` as a source-checkout developer path.

For one qualified Linux amd64 host, the [guided single-host installer](guided-single-host-install.md) is the shortest release-backed path. It creates a complete loopback development manifest with one same-host Runner from an accepted plan. The manual development, production, and remote-Runner procedures below remain separate; the guided installer does not add defaults to them or change the meaning of `init --mode development`.

## One-command development control plane

From a clean checkout:

```sh
just deploy-development-up .tmp/secondbox-development
```

The command creates the directory only when it is absent, writes a mode-`0600` manifest and mode-`0700` secret directory, generates a platform token and Runner PKI, explicitly selects the three reviewed development standard bundles, builds the control-plane image, renders and validates the environment, starts loopback-only PostgreSQL and the control plane, requires `/readyz`, and applies the selected resources. It refuses an existing directory without `secondbox.toml` and never rewrites an existing manifest, secret, identity, workspace, or execution asset. It does not create a Tenant, Subject, tenant controller, or application authority.

Development initialization alone is available as:

```sh
just deploy-init-development .tmp/secondbox-development
just deploy-config .tmp/secondbox-development/secondbox.toml
```

The reviewed development topology intentionally starts no privileged Runner. Runner enrollment and host qualification remain separate operations on a qualified Linux host.

Create development tenancy only as an observable post-start step. The repository helper follows the same platform login, Tenant and controller creation, controller login, Subject and application-authority creation, application login, and authenticated Sandbox-list sequence as the qualified scenario harness:

```sh
scripts/bootstrap-development-tenancy.sh \
  "$(realpath ./secondbox)" \
  http://127.0.0.1:8080 \
  "$(realpath .tmp/secondbox-development/secrets/platform-token)" \
  development development agent-compartment-isolated
```

The command prints the two one-time bearer tokens in one JSON response. Capture it in a protected secret store; neither token can be read back from SecondBox.

## The deployment manifest

[`deploy/secondbox.example.toml`](../../deploy/secondbox.example.toml) documents `schema_version = 1` and every accepted field. The manifest has seven decision groups:

1. `deployment`: mode, public ingress, TLS termination, process bind addresses, and image references;
2. `database`: bundled or external PostgreSQL and the authority required by that choice;
3. `[[runners]]`: immutable Runner IDs, same-host or remote placement, pool, capacity, host integration, networking, and execution assets;
4. `runner_trust`: enrollment credential, CA, server identity, and certificate policy;
5. `applications`: the platform-token secret reference;
6. `standard_resources`: verified release manifest, explicit standard bundles, typed RunnerPool inventory, and apply readiness bound;
7. `policy` and `overrides`: subject quota limits, data-plane retention, contested recovery/rollout settings, and intentionally selected tuning overrides.

Unknown keys, duplicate keys, unsupported schema versions, ambiguous bundled/external fields, incomplete authority, mutable production images, invalid cross-field relationships, and invalid cryptographic trust material fail with a `SecondBox deployment manifest` error. The decoder does not interpolate `${ENV}`, include files, or merge ambient environment variables.

Validate and inspect without rendering:

```sh
secondbox-deploy validate /secure/secondbox/secondbox.toml
secondbox-deploy inspect /secure/secondbox/secondbox.toml
```

`inspect` prints all resolved non-secret values, positive help for the data-plane retention policy, and all 18 available tuning overrides with their compiled defaults. Secret values and secret-revealing paths are redacted.

### Secret references

Secret-bearing manifest fields name files instead of containing secret material. Relative references resolve from the manifest directory, never the caller's working directory. References must name exact regular non-symbolic-link files. A secret file contains one value: resolution removes at most one terminal LF and rejects CR, NUL, or any additional line break without trimming other bytes.

Runner host paths are different: they are typed absolute values interpreted on the declared Runner host. Rendering a remote Runner handoff never opens or validates those paths on the control-plane host.

### Authority, policy, tuning, and compiled facts

Required deployment authority has no default. This includes identities, the platform and Runner credentials, endpoints, process and storage paths, signed-asset catalog, verified artifact manifest, explicit standard-bundle selection, typed RunnerPool inventory, the seven deployment fallback Subject quota limits, and data-plane retention. Tenant aggregate ceilings and explicit Subject quotas are persisted management resources created after startup; they are not deployment-manifest fields. Runtime and toolchain digests are resolved from the verified artifact manifest rather than copied into policy fields.

`policy.data_plane_retention_seconds` participates in each data-plane session's result and idempotency deadline. The retained session row contains bounded one-shot results, terminal outcome, admission replay, and accounting, but no streaming payload bytes.

The manifest also requires three contested rollout/recovery decisions: data-plane session sweep interval, Runner command poll interval, and enabled Runner features. They remain operator policy until a separate decision reclassifies them.

The `[overrides]` table contains code-owned tuning. Every field is optional. When absent, `secondboxd` uses the reviewed value shown by `inspect`; when present, the exact value is rendered and passes the same validation and cross-field checks as before. Invalid overrides fail rather than falling back. Compose uses value-less pass-through mappings so an absent override remains unset instead of becoming an empty string.

`http_timeout_seconds` bounds request reads, response writes, idle keep-alive connections, and graceful server shutdown. The response write deadline starts only when an ordinary response begins, so it does not expire while a handler waits for an operation-specific result. Buffered Exec, Sandbox wait, proxied File operations, and upgraded streaming connections already have operation-specific bounds. A server-wide response deadline cannot be compatible with every valid Profile because Profile execution deadlines are explicit and may exceed this process tuning value.

Production ingress is a separate deadline boundary. Configure reverse proxies and load balancers so request and response timeouts exceed the longest admitted Profile operation and the 60-second Sandbox wait bound, including practical transport overhead. Upgraded Exec, Terminal, and Port connections must remain open for their full session lifetime with compatible idle timeouts. A shorter ingress deadline can still terminate a valid request even when `secondboxd` accepts it.

Runner protocol minimum and maximum are not configuration. Both binaries compile the one supported protocol window, and generated-protocol verification rejects drift between the two modules.

## Production initialization

Create the protected skeleton:

```sh
just deploy-init-production /secure/secondbox-deployment
```

An incomplete production initialization is intentionally unusable and reports every unresolved decision group in one error. Before validation, production operators must supply:

- digest-pinned control-plane and Runner images, public HTTPS ingress, and external TLS termination;
- bundled or external database authority, with `sslmode=verify-full` for an external database;
- zero or more explicit immutable Runner declarations and their placement;
- an operator-supplied signed-asset catalog, verified release artifact manifest, explicit standard-bundle and RunnerPool inventory selection, Runner CA, and server keypair;
- independent platform and Runner enrollment authorities;
- all seven subject quota limits;
- retention, contested recovery/rollout policy, and any intentional tuning overrides.

Automation can materialize a complete create-only target non-interactively after generating and reviewing the same typed input:

```sh
secondbox-deploy init --mode production \
  --input /automation/complete-production.toml \
  /secure/secondbox-deployment
```

Production initialization materializes only the explicitly supplied platform authority. It creates no implicit Tenant, Subject, tenant controller, or application authority. After startup, use the authenticated management CLI sequence documented in [SDK, CLI, and Flue integration](sdk-cli-and-flue.md). No generated development authority is accepted as a production default. Any dependency image selected in production is immutable by digest.

## Rendering and Compose

Render explicitly when handing the environment to another tool:

```sh
secondbox-deploy render \
  --output /secure/secondbox-deployment/.secondbox.generated.env \
  /secure/secondbox-deployment/secondbox.toml
```

Rendering strictly resolves the manifest, invokes the production `secondboxd` environment loader as a postcondition, and atomically replaces the target with a mode-`0600` file carrying a do-not-edit header. Manual changes are overwritten on the next deployment command. Remote Runner declarations also produce isolated systemd `EnvironmentFile` handoffs beside the generated environment.

Supported Compose workflows always take the manifest:

```sh
just deploy-config /secure/secondbox-deployment/secondbox.toml
just deploy-up /secure/secondbox-deployment/secondbox.toml
just deploy-down /secure/secondbox-deployment/secondbox.toml
```

The deployment command supplies the project name, env file, and exact overlay list explicitly. It removes ambient `SECONDBOX_*` and `COMPOSE_*` variables and retains only Docker client connectivity variables, so `COMPOSE_PROJECT_NAME` never reaches Docker; the project name comes from the manifest alone.

The base [`deploy/compose.yml`](../../deploy/compose.yml) contains the control plane and shared resources; `compose.explicit-network.yml` applies a manifest-reviewed backend CIDR when one is stated; `compose.development.yml` adds the reviewed local database; production selects the bundled-database overlay only when requested; `compose.same-host-runner.yml` adds only the privileged Runner. Inactive services are never hidden behind profiles that still interpolate missing values.

`deployment.compose_project_name` names the Compose project. It is optional and defaults to `secondbox`, so a manifest written before the key existed keeps deploying exactly where it always did. Compose derives every container, volume, and network name from it, which makes it the isolation boundary between deployments: two deployments that share one Docker daemon must state different project names, or the second `up` binds the first's volumes and recreates its containers instead of failing. The name must start with a lowercase letter or digit and use at most 63 lowercase letters, digits, underscores, or hyphens.

`deployment.compose_backend_cidr` optionally assigns the Compose backend an explicit RFC1918 IPv4 `/24`. For a same-host Runner, it must not overlap either `sandbox_guest_cidr` or the network containing `sandbox_bridge_cidr`. A manual selection must also avoid every prefix and host route reported by `ip -j -4 route show table all` and every IPv4 `IPAM.Config[].Subnet` reported by `docker network inspect` for the local daemon. The guided installer performs those all-table route and Docker-IPAM checks and always states this field; omitting it preserves the behavior of older hand-authored manifests, including Docker's automatic network allocation.

The control-plane container runs as UID/GID 65532 with a read-only root, dropped capabilities, `no-new-privileges`, and no KVM, TUN/TAP, host-cgroup, workspace, or container-engine access. A selected same-host Runner overlay is privileged and receives host devices and cgroups, but executes `secondbox-runner` directly as PID 1 and starts no private init system, login manager, or console process.

## Runner enrollment and handoff

Every `[[runners]]` entry is keyed by immutable `runner_id`. At most one may use `placement = "same-host"`; any number may use `placement = "remote"`.

### Runner declaration scaffold

Generate the complete inert declaration on stdout, or create one separate file without replacing an existing target:

```sh
secondbox-deploy runner-template
secondbox-deploy runner-template --output /secure/secondbox/runner-east-1.toml
```

Replace `runners = []` in the deployment manifest with the completed block. Every emitted value is an invalid placeholder; validation cannot accept the scaffold before the operator replaces every value.

<!-- runner-template-output:start -->
```toml
# Identity and placement
[[runners]]
# Immutable opaque Runner ID; start with an ASCII letter or digit, then use at most 127 letters, digits, dots, underscores, colons, or hyphens.
runner_id = ''
# Runner location; must be same-host or remote, with at most one same-host Runner.
placement = ''
# RunnerPool name; required and must match the selected standard-resource inventory that admits this Runner.
pool_id = ''
# Runner software version reported to the control plane; required.
software_version = ''
# Authenticated control-plane Runner endpoint; required.
control_plane_address = ''
# TLS server name for the control-plane Runner endpoint; required.
control_plane_server_name = ''
# Runner identity directory; absolute on the Runner, and /run/secondbox-runner-identity for same-host placement.
identity_directory = ''
# Identity directory on the Runner host; absolute when set and required for same-host placement.
identity_host_directory = '<replace-with-absolute-runner-host-path>'

# Artifact trust
# Execution-asset directory on the Runner host; absolute when set and required for same-host placement.
artifact_host_directory = '<replace-with-absolute-runner-host-path>'
# Provisioned signed-artifact public key; an absolute Runner-host path within /opt/secondbox-artifacts for same-host placement.
artifact_public_key = ''
# Provisioned signed-artifact key fingerprint; exactly 64 lowercase hexadecimal characters and not all zeroes.
artifact_public_key_sha256 = '0000000000000000000000000000000000000000000000000000000000000000'

# Runner storage
# Dedicated reflink-capable Runner storage root on the host; absolute when set and required for same-host placement. Compose binds this root once at /var/lib/secondbox-runner so its state and workspaces children retain one mount identity.
state_host_directory = '<replace-with-absolute-runner-host-path>'
# Runner JSON log path; an absolute Runner-host path within /var/lib/secondbox-runner/state for same-host placement.
log_path = ''
# Runner log directory; required and absolute, and within /var/lib/secondbox-runner/state for same-host placement.
log_directory = ''

# Workspace persistence
# Reflink-capable workspace directory on the Runner host; for same-host placement this must be the workspaces child of state_host_directory.
workspace_host_directory = '<replace-with-absolute-runner-host-path>'
# Workspace root seen by the Runner; /var/lib/secondbox-runner/workspaces for same-host placement.
workspace_root = ''
# Storage-pressure recovery threshold; positive and lower than warning and admission-deny thresholds.
storage_pressure_recovery_percent = 0
# Storage-pressure warning threshold; positive and between recovery and admission-deny thresholds.
storage_pressure_warning_percent = 0
# Storage-pressure admission-deny threshold; positive, above warning, and below 100.
storage_pressure_admission_deny_percent = 0

# Firecracker
# Firecracker executable; an absolute Runner-host path.
firecracker_path = ''
# Firecracker jailer executable; an absolute Runner-host path.
firecracker_jailer_path = ''
# Firecracker jail root; absolute, below the Unix-socket path limit, and within /var/lib/secondbox-runner but outside its workspaces child for same-host placement.
firecracker_jail_root = ''
# First per-Instance jailer user ID; must be at least 1000 unless the explicit lower-bound acknowledgement is true, and the range must not include UID 0.
firecracker_jailer_uid_start = 0
# Number of distinct jailer user IDs; positive and at least max_concurrent_global.
firecracker_jailer_uid_count = 0
# Explicit acknowledgement for a jailer UID range starting below 1000; required, so replace this string with a Boolean.
firecracker_jailer_uid_allow_below_1000 = '<replace-with-boolean>'
# Jailer group ID; must be positive.
firecracker_jailer_gid = 0
# Host cgroup version used by the jailer; must be positive.
firecracker_cgroup_version = 0
# Host cgroup parent used by the jailer; required.
firecracker_cgroup_parent = ''
# Guest kernel; absolute and within /opt/secondbox-artifacts for same-host placement.
firecracker_kernel_path = ''
# Guest root filesystem; absolute and within /opt/secondbox-artifacts for same-host placement.
firecracker_rootfs_path = ''
# Shared guest image; absolute and within /opt/secondbox-artifacts for same-host placement.
firecracker_shared_image_path = ''
# Kernel arguments; must include console=ttyS0, reboot=k, panic=1, pci=off, root=/dev/vda, rw, quiet, loglevel=1, i8042.noaux, i8042.nomux, i8042.nopnp, i8042.dumbkbd, and init=/init.
firecracker_kernel_args = ''
# Firecracker CPU template; required.
firecracker_cpu_template = ''
# Firecracker runtime directory; absolute and within /var/lib/secondbox-runner/state for same-host placement.
firecracker_run_directory = ''
# Firecracker log directory; absolute and within /var/lib/secondbox-runner/state for same-host placement.
firecracker_log_directory = ''
# Packaged Runner jail policy; must be false.
firecracker_allow_unjailed = true

# Snapshot-resume startup
# Runner-local resume template cache; absolute and within /var/lib/secondbox-runner/state for same-host placement. The Runner advertises snapshot-resume capacity only when this cache already holds a template built from the signed bundle the Runner verified, so a Profile whose startup mode is snapshot_resume never places onto a Runner that cannot resume it. Keep it on the same filesystem as firecracker_jail_root: the golden memory file is hard-linked into each jail so every resumed Instance shares one inode and one page cache.
snapshot_template_cache_root = ''

# Sandbox networking
# Guest IP address assigned to Sandboxes; must be an IP address.
sandbox_guest_ip = ''
# Host bridge name used by Sandboxes; required.
sandbox_bridge_name = ''
# Host bridge network; must be a CIDR.
sandbox_bridge_cidr = ''
# Guest address range; must be a CIDR.
sandbox_guest_cidr = ''
# Prefix for per-Sandbox TAP interfaces; required.
sandbox_tap_prefix = ''
# Persisted network state; absolute and within /var/lib/secondbox-runner/state for same-host placement.
sandbox_network_state_directory = ''
# Bridge cleanup policy; required, so replace this string with an explicit Boolean.
sandbox_delete_bridge = '<replace-with-boolean>'
# nft executable; an absolute Runner-host path.
network_policy_nft_path = ''
# Maximum pinned DNS answers; must be positive.
network_policy_max_dns_pins = 0
# Maximum DNS TTL; must be a positive Go duration.
network_policy_max_dns_ttl = ''
# Runner-local addresses; a comma-separated list of IP addresses.
network_policy_runner_addresses = ''
# Management networks; a comma-separated list of CIDRs.
network_policy_management_cidrs = ''
# Logical gateways; unique domain=IP pairs or none, including every gateway required by selected standard bundles.
network_policy_runner_gateways = ''
# Upstream DNS resolver; must be an IP:port with a nonzero port.
network_policy_dns_upstream = ''

# Capacity
# Maximum vCPUs per Sandbox; must be positive.
sandbox_max_vcpus = 0
# Maximum memory per Sandbox in MiB; must be positive.
sandbox_max_memory_mib = 0
# Maximum disk per Sandbox in MiB; must be positive.
sandbox_max_disk_mib = 0
# Aggregate Sandbox memory budget in MiB; must be positive.
sandbox_memory_budget_mib = 0
# Maximum concurrent commands per Sandbox; must be positive.
max_concurrent_per_sandbox = 0
# Maximum concurrent commands across the Runner; must be positive.
max_concurrent_global = 0
# Maximum concurrent starts; positive and no greater than max_concurrent_global.
max_concurrent_starts = 0
# Maximum concurrent Workspace creations; must be positive.
max_concurrent_workspace_creates = 0
# Maximum concurrent operations across the Runner; must be positive.
max_concurrent_operations_global = 0
# Maximum bytes per file transfer; must be positive.
file_transfer_max_bytes = 0

# Data plane
# Guest-control vsock port; a positive integer through 65535 distinct from the guest-protocol port.
guest_control_vsock_port = 0
# Guest-protocol vsock port; a positive integer through 65535 distinct from the guest-control port.
guest_protocol_vsock_port = 0
# Guest heartbeat cadence; a Go duration from 1ms through 1m.
guest_heartbeat_interval = ''
# Runner data-plane listener; a host:port with a port from 0 through 65535.
data_plane_listen_address = ''
# Reachable Runner data-plane endpoint; host:port with an explicit host and a port from 1 through 65535.
data_plane_advertised_address = ''
```
<!-- runner-template-output:end -->

Review these relationships before enrollment:

- Put `state_host_directory` on a dedicated non-root XFS or Btrfs filesystem with reflink support. For same-host placement, `workspace_host_directory` must be its `workspaces` child and `workspace_root` must be `/var/lib/secondbox-runner/workspaces`. Compose binds the common storage root once so Workspace images, jail state, run state, and snapshot templates retain one mount identity.
- Leave the filesystem target named by `identity_host_directory` absent before `runner-init`. The command validates the declaration without the same-host identity preflight, then creates that exact target; create the artifact and Runner storage host directories first, and run full manifest validation after enrollment.
- Set `pool_id` to the `name` of the selected `[[standard_resources.runner_pools]]` inventory that admits the Runner architecture and capabilities.
- Map every logical gateway required by the selected standard bundles in `network_policy_runner_gateways`. The mapping is Runner-local `domain=IP` authorization, not guest-side name resolution. The Runner DNS proxy only forwards to its configured upstream, rejects answers resolving to protected addresses, and does not synthesize the logical gateway domain. Production qualification must prove the deployment's own guest resolution and gateway reachability.

Issue one declared identity and protected environment handoff:

```sh
secondbox-deploy runner-init \
  /secure/secondbox-deployment/secondbox.toml \
  runner-east-1 \
  /secure/handoffs/runner-east-1
```

The command signs a client certificate carrying `spiffe://secondbox/runner/<runner-id>`, writes the matching key, CA certificate, and canonical systemd environment, then atomically installs the directory. It refuses an undeclared ID, an existing target, a same-host target that differs from the declared identity directory, or mismatched CA evidence. Copying and activating a remote handoff on its Runner host is an explicit operator action.

Selected RunnerPools and standard Profile lineages are checked and applied after the control plane becomes ready. A repeated deployment is a no-op; an interrupted application resumes from the verified installed prefix. Each Runner in a selected pool maps the standard Profile's logical gateway in `network_policy_runner_gateways`; that mapping does not add a DNS record. See [declarative resources](declarative-resources.md).

## Recovery and replacement

Replacing v0.5.2 with v0.6.0 in place is unsupported. v0.6.0 is a clean-install
boundary: stop workloads, back up PostgreSQL and each Runner identity plus its
workspace root, preserve or explicitly migrate workload data outside the guided
installer, remove the v0.5.2 deployment through its recorded uninstall
procedure, and perform a complete v0.6.0 production initialization. The
installer does not import v0.5.2 authorities, manifests, receipts, Profiles, or
desired state. Workspace files are usable only when an operator has separately
preserved and migrated them under a reviewed v0.6.0 Runner home; PostgreSQL
alone cannot reconstruct them. Once v0.6.0 migrations or resources have been
created, rollback means restoring a complete, consistent v0.5.2 database and
Runner-filesystem backup. Running v0.5.2 binaries against v0.6.0 state is not a
rollback path.

PostgreSQL owns Tenants, Subjects, delegated authority verifiers, two-level quota, cleanup Operations, desired state, authoritative home assignments, generations, Leases, Profiles, audit, and reconciliation. Each home Runner's reflink-capable workspace root owns its Workspaces and local Snapshots. Ordinary lifecycle and recovery never relocate a Sandbox; only the operator-initiated stopped-Sandbox relocation Operation may change its home. PostgreSQL cannot reconstruct a lost unbacked Runner workspace filesystem.

Before replacement, take and verify a PostgreSQL backup and quiescent backups of every affected Runner identity plus workspace root. Restore each stable Runner identity and workspace root as one consistent unit. The generated environment can be reproduced from `secondbox.toml` and its referenced secret material; it is not backup authority.

Every `secondboxd` applies the embedded ordered migration lineage under a PostgreSQL advisory lock before opening listeners. Use coordinated replacement unless the exact deployment has independently proven mixed-version operation.

## Runtime checks

```sh
curl --fail --silent --show-error http://127.0.0.1:8080/healthz
curl --fail --silent --show-error http://127.0.0.1:8080/readyz
curl --fail --silent --show-error http://127.0.0.1:8080/metrics
```

`/healthz` proves the process answers, `/readyz` proves PostgreSQL connectivity, and `/metrics` exports fixed-cardinality state counts without tenant or resource identifiers.

See [backup and restore](backup-and-restore.md), [Firecracker runtime](firecracker-runtime.md), [multirunner qualification](multirunner-qualification.md), and [observability and diagnostics](observability-and-diagnostics.md).

For public releases, verify the published checksums and artifact manifest, then initialize with `secondbox-deploy init --mode production --input COMPLETE_MANIFEST --artifact-manifest URL DIRECTORY`. The artifact manifest supplies digest-pinned control-plane and Runner images, the Runner software version, and release-owned standard-resource identity. All deployment identity, credentials, storage, topology, host paths, gateways, capacity, retention, and independently held guest trust anchors remain explicit in `COMPLETE_MANIFEST`. See [release distribution](release-distribution.md).
