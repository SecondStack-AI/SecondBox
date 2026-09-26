# Deployment and runtime operations

## Release upgrade boundary

v0.18.0 retains the v0.17.0 signed Firecracker bundle, Runner protocol generation 5, and the migration baseline, so a v0.17.0 deployment updates in place. It is breaking for gVisor Runners: each now requires the image fetcher, a reflink-capable execution image cache, registry configuration, and the publisher key before it starts. See the [v0.18.0 release notes](../releases/v0.18.0.md).

v0.17.0 shipped a new signed Firecracker bundle. Its runtime and toolchain component digests changed, so the guided updater refuses an in-place update from earlier releases. Retire Sandboxes, retain a coordinated backup, then reinstall with a fresh database and separate Runner storage root; recreate resources against the new signed asset catalog. A new database cannot recover another deployment's Workspaces. Keep the old database, storage, signed assets, and credentials together for rollback. See the [v0.17.0 release notes](../releases/v0.17.0.md).

Historically, v0.16.0 allowed a v0.15.0 deployment to update in place because its bundle and migration baseline were unchanged. v0.15.0 accepted the exact v0.14.0 migration baseline and applied forward migrations. Other checksum mismatches remain errors; do not reset migration records to bypass them. See each target release's notes for its boundary.

Each Runner selects one compute backend explicitly. RunnerPool backend homogeneity is control-plane-private and is sealed by the first healthy registration; operators cannot mutate or reset it. Profiles and public resources continue to name only the RunnerPool.

Firecracker is the backend this guide deploys. The experimental Microsandbox backend has no supported deployment path on Linux: it is exercised through the repository's KVM qualification suites (`just test-microsandbox-linux`, `just test-scenario-microsandbox-linux`, both requiring a pinned local Microsandbox build, `/dev/kvm`, and a reflink-capable qualification filesystem), and its only documented operator procedure is the native macOS guide at [`microsandbox-macos.md`](microsandbox-macos.md), which also records the backend's environment contract and known limitations. The [gVisor runtime](gvisor-runtime.md) covers hosts without KVM, deployed from the released `runner-gvisor` and `gvisor-artifacts` images rather than by this guide.

SecondBox deploys one unprivileged control plane and separately managed privileged Runners. Operators describe the deployment in one strict, versioned `secondbox.toml`; `secondbox-deploy` compiles that manifest into the process environments consumed by Compose, `secondboxd`, and remote Runner service managers. The generated environment is transport, not operator input.

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

Create development tenancy only as an observable post-start step. For a recorded
guided installation, `secondbox-deploy bootstrap-tenancy <operation-directory>`
is the maintained form. It accepts `--tenant-ref` and `--subject-ref` (defaults
`local` and `local-operator`), `--check` for prerequisite verification, and
`--application` to return one new application bearer token. It reads only the
recorded platform-token path, journals non-secret evidence, and revokes its
transient controller. Existing Tenant and Subject refs remain unchanged.
Guided installation offers this stage by default; production initialization
and `deploy-development-up` do not bootstrap tenancy.

The shell helper remains for the non-guided path. It follows the platform login,
Tenant and controller creation, controller login, Subject and application-authority
creation, application login, and authenticated Sandbox-list sequence:

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

1. `deployment`: mode, public ingress, TLS termination, host publication, and image references;
2. `database`: bundled or external PostgreSQL and the authority required by that choice;
3. `[[runners]]`: immutable Runner IDs, same-host or remote placement, pool, capacity, host integration, networking, and execution assets;
4. `runner_trust`: enrollment credential, CA, server identity, and certificate policy;
5. `applications`: the platform-token secret reference;
6. `standard_resources`: verified release manifest, explicit standard bundles, typed RunnerPool inventory, and apply readiness bound;
7. `policy` and `overrides`: data-plane retention, enabled Runner features, and intentionally selected tuning overrides.

Unknown keys, duplicate keys, unsupported schema versions, ambiguous bundled/external fields, incomplete authority, mutable production images, invalid cross-field relationships, and invalid cryptographic trust material fail with a `SecondBox deployment manifest` error. The decoder does not interpolate `${ENV}`, include files, or merge ambient environment variables.

Validate and inspect without rendering:

```sh
secondbox-deploy validate /secure/secondbox/secondbox.toml
secondbox-deploy inspect /secure/secondbox/secondbox.toml
```

`inspect` prints all resolved non-secret values, positive help for the data-plane retention policy, and all 17 available tuning overrides with their compiled defaults. Secret values and secret-revealing paths are redacted.

### Secret references

Secret-bearing manifest fields name files instead of containing secret material. Relative references resolve from the manifest directory, never the caller's working directory. References must name exact regular non-symbolic-link files. A secret file contains one value: resolution removes at most one terminal LF and rejects CR, NUL, or any additional line break without trimming other bytes.

Runner host paths are different: they are typed absolute values interpreted on the declared Runner host. Rendering a remote Runner handoff never opens or validates those paths on the control-plane host.

### Authority, policy, tuning, and compiled facts

Required deployment authority has no default. This includes identities, the platform and Runner credentials, public endpoints, host paths, signed-asset catalog source, verified artifact manifest, explicit standard-bundle selection, typed RunnerPool inventory, and data-plane retention. Tenant aggregate ceilings and Subject quotas are explicit persisted management resources created after startup; they are the sole quota source for admission. Runtime and toolchain digests are resolved from the verified artifact manifest rather than copied into policy fields.

The deployment compiler supplies the packaged container listeners (`0.0.0.0:8080` for the API and `0.0.0.0:9443` for Runners) and mounted catalog path (`/etc/secondbox/signed-assets.json`). Operators choose the host bind addresses, published ports, and catalog source. Generated process configuration still states every value explicitly.

Existing manifests must remove the retired `policy.default_subject_max_*`, `deployment.listen_address`, `deployment.runner_listen_address`, and `deployment.signed_asset_catalog_path` keys. Standard bundles now share one RunnerPool inventory declaration by name: remove its `bundle` key and consolidate identical `[[standard_resources.runner_pools]]` entries into one. Conflicting inventory declarations require an explicit operator choice; the compiler rejects duplicate names. The strict decoder identifies retired keys without disclosing their values.

`policy.data_plane_retention_seconds` participates in each data-plane session's result and idempotency deadline. The retained session row contains bounded one-shot results, terminal outcome, admission replay, and accounting, but no streaming payload bytes.

`policy.runner_enabled_features` must include `client-selected-image` because generation 5 requires an image on every create and start.
Enable other Runner features only when their backend evidence and application grants are ready.

The data-plane and Runner command polling cadences are optional tuning overrides, each defaulting to 250 milliseconds. The data-plane cadence also drives session/accounting sweeps and polling on proxied streams; it does not set retention deadlines. Runner command polling provides fallback delivery alongside work notifications. To migrate an existing manifest, remove `data_plane_poll_interval_milliseconds` and `runner_command_poll_interval_milliseconds` from `[policy]`. Preserve any intentional non-default values under `[overrides]`; the old policy keys are rejected.

The `[overrides]` table contains code-owned tuning. Every field is optional. When absent, `secondboxd` uses the reviewed value shown by `inspect`; when present, the exact value is rendered and passes the same validation and cross-field checks as before. Invalid overrides fail rather than falling back. Compose uses value-less pass-through mappings so an absent override remains unset instead of becoming an empty string.

`http_timeout_seconds` bounds request reads, response writes, idle keep-alive connections, and graceful server shutdown. The response write deadline starts only when an ordinary response begins, so it does not expire while a handler waits for an operation-specific result. Buffered Exec, Sandbox wait, proxied File operations, and upgraded streaming connections already have operation-specific bounds. A server-wide response deadline cannot be compatible with every valid Profile because Profile execution deadlines are explicit and may exceed this process tuning value.

Production ingress is a separate deadline boundary. Configure reverse proxies and load balancers so request and response timeouts exceed the longest admitted Profile operation and the 60-second Sandbox wait bound, including practical transport overhead. Upgraded Exec, Terminal, and Port connections must remain open for their full session lifetime with compatible idle timeouts. A shorter ingress deadline can still terminate a valid request even when `secondboxd` accepts it.

Runner protocol minimum and maximum are not configuration. Both binaries compile the one supported protocol window, and generated-protocol verification rejects drift between the two modules.

## Tenant-aware egress topology

The client-selected-image release uses Runner protocol generation 5 exactly. Earlier control planes and Runners cannot join a generation-5 deployment. Upgrade the control plane and every Runner as one coordinated replacement. A mixed fleet, legacy assignment, or global-gateway fallback is unsupported. The first cold start also includes registry download, extraction, and signature verification. Size assignment and ingress deadlines for that work; warm starts use the verified Runner-local cache.

One SecondBox Tenant represents one SecondStack installation and has at most one nullable operator-selected egress-context name. Names are opaque and match `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`, so they contain 1 through 63 lowercase ASCII letters, digits, or hyphens and begin and end with an alphanumeric character. They are not DNS names or network coordinates. Do not put SecondStack hostnames, proxy endpoints, certificates, network ranges, Tenant references, or secrets in a context name or in control-plane configuration.

A Runner can support several names. Each maps to its own Runner-local logical-gateway-to-address table. One separately deployed `agent-runner-gateway` process serves one context and authenticates to exactly one installation's Agent Egress Proxy. The SecondBox deployment owns only the context-indexed Runner-local mappings; gateway certificates, proxy addresses, interception CAs, policy databases, and credentials remain in that installation's gateway/proxy deployment. Gateway and proxy protocols do not change.

Logical mappings authorize network policy and never create guest DNS. Firecracker and gVisor Runners publish each resolved mapping to the guest execution environment as `SECONDBOX_RUNNER_GATEWAYS`, so the consuming application reads the address there instead of configuring a Runner-host address of its own, and its command wrapper still chooses the proxy variables. The same IP may appear more than once within or across contexts; operators select distinct addresses where isolation requires them. A context pins the mapping indirection rather than an address list or digest.

A gateway entry can also declare `attributed_socket`, rendered as `attributedSocket` in the Runner JSON. It names a canonical absolute Unix socket path, up to 107 bytes, for attributed execution forwarding. Each entry needs an `address`, an `attributed_socket`, or both. Socket-only entries do not authorize ordinary IP egress. The Runner resolves sockets only within the assignment's pinned context; another context or the ordinary address is never substituted. Firecracker and gVisor advertise attributed capability when this routing is configured; advertisement does not probe gateway availability. An unavailable socket causes forwarding to fail.

Deploy an attribution-aware gateway on the Runner host and mount its socket directory into the Runner at the declared path. Restrict the directory and socket to the operator-selected Runner UID; the standard privileged Runner uses UID 0. The gateway must authenticate each Unix peer before reading the `SBXATTR1` preface; `pkg/egressattribution.ReadRunnerExecutionAttribution` performs both checks with an explicit expected UID and read deadline. Do not accept attribution on an unauthenticated TCP listener or derive identity from guest headers. Socket permissions, mounts and gateway process lifetime belong to the operator's gateway deployment.

Runner context configuration is static for one connection. To replace or remove a mapping, drain the Runner, stop every active Sandbox using that context, update the reviewed configuration, and restart. A stopped Sandbox pinned to a removed context remains durable but cannot start until the mapping returns or the Sandbox is retired. There is no dynamic gateway health discovery, live remapping, default context, cross-context retry, or automatic reassignment.

State the mapping in the Runner declaration; never edit the generated JSON or environment transport. The path below is for remote placement; omit `egress_context_config_path` for same-host placement:

```toml
egress_context_config_path = "/etc/secondbox/egress-contexts.json"

[[runners.egress_contexts]]
name = "secondstack-staging"

[[runners.egress_contexts.gateways]]
logical_name = "agent-gateway.secondbox.internal"
address = "10.210.2.2"

[[runners.egress_contexts]]
name = "secondstack-development"

[[runners.egress_contexts.gateways]]
logical_name = "agent-gateway.secondbox.internal"
address = "10.210.3.2"
```

The renderer writes one strict `secondbox.runner-egress-contexts/v1` document per Runner beneath the generated environment's `.runners` directory. For same-host Compose, a one-shot root init service copies that file into a named volume and the Runner mounts it read-only at `/run/secondbox-runner-config/egress-contexts.json`. A remote `runner-init` handoff contains `runner.env` plus `egress-contexts.json`; install both together, mount the JSON read-only at the declared absolute path, and use the supplied systemd unit's `ReadOnlyPaths` fence. The qualified gVisor pod uses a read-only ConfigMap projection. Every path is explicit, and startup rejects a missing, unsafe, malformed, empty, or stale file.

After the control plane and Runners are connected, run the platform-authorized, read-only placement preflight:

```sh
secondbox diagnostics egress-contexts
```

The JSON lists each active Tenant grant whose current Profile head requires a context, the connected compatible Runner IDs, all Runner advertisements, and grouped nonterminal assignments by context, Runner, and state. `ready` is false when a required Tenant context is absent or no connected ready Runner in the Profile's pool advertises it. The check does not change Tenants, Runner configuration, assignments, or lifecycle. Before replacing or removing a mapping, use `activeAssignments` to find what must be stopped, then drain and restart the affected Runner.

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
- retention, enabled Runner features, and any intentional tuning overrides.

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

For same-host placement, set `identity_host_directory`, `artifact_host_directory`, and `state_host_directory` to explicit host paths. The compiler supplies all seventeen fixed container paths: identity and egress configuration, workspace root, Runner logs, Firecracker and jailer executables, jail root, kernel/rootfs/shared assets, runtime and Firecracker logs, snapshot-template cache, signing-key file, network state, and nft executable. Remove `workspace_host_directory` from existing manifests and omit the path fields marked remote-only in the Runner template from same-host declarations. Nonempty values are rejected instead of silently ignored. The signing-key fingerprint stays explicit; only its packaged file location is derived. The existing `state_host_directory/workspaces` directory remains authoritative and must exist on the qualified storage filesystem; resolution never creates or relocates it. Remote declarations still require explicit paths. Existing custom state or asset layouts must be reconciled with the documented packaged paths before adopting this schema; compilation does not move their files.

Packaged deployments always use the Firecracker jailer for both placements. Remove `firecracker_allow_unjailed` from existing Runner declarations; the compiler emits the fixed `false` runtime value.

### Runner declaration scaffold

Generate the complete inert declaration on stdout, or create one separate file without replacing an existing target:

```sh
secondbox-deploy runner-template
secondbox-deploy runner-template --output /secure/secondbox/runner-east-1.toml
```

Replace `runners = []` in the deployment manifest with the completed block.
Required values are invalid placeholders, and validation cannot accept the scaffold before the operator supplies them.
Set all three execution-image limits to positive byte counts, and keep the cache limit at least as large as the expanded-image limit.
Each preparation reserves `2 * execution_image_max_download_bytes + execution_image_max_expanded_bytes` because the archive, extraction staging, and published cache can overlap.
This conservative reservation also applies to cached images and must fit below the storage-pressure denial threshold with existing filesystem usage and Workspace reservations.
The guided install values reserve 48 GiB for staging from a 16 GiB download limit and a 16 GiB expanded limit.
Existing manifests must add these fields before a generation-5 Runner starts.
They must also set `[deployment].execution_image_public_key` to the publisher's PEM public key and `execution_image_public_key_sha256` to its SHA-256 DER fingerprint.
Calculate the fingerprint with `openssl pkey -pubin -in PUBLIC_KEY -outform DER | sha256sum`.
Use that same publisher fingerprint for each Runner's execution-image trust, and make the public key readable by the fetcher's UID 10002.
This publisher may differ from the historical release asset publisher.
Leave the three remote-only paths empty for same-host placement.

For a remote Runner, map the manifest values to `SECONDBOX_RUNNER_EXECUTION_IMAGE_CACHE_ROOT`, `SECONDBOX_RUNNER_EXECUTION_IMAGE_REGISTRIES`, `SECONDBOX_RUNNER_EXECUTION_IMAGE_CERTIFICATES`, `SECONDBOX_RUNNER_EXECUTION_IMAGE_PUBLIC_KEY`, `SECONDBOX_RUNNER_EXECUTION_IMAGE_PUBLIC_KEY_SHA256`, `SECONDBOX_RUNNER_EXECUTION_IMAGE_MAX_DOWNLOAD_BYTES`, `SECONDBOX_RUNNER_EXECUTION_IMAGE_MAX_EXPANDED_BYTES`, and `SECONDBOX_RUNNER_EXECUTION_IMAGE_MAX_CACHE_BYTES`.
`SECONDBOX_RUNNER_EXECUTION_IMAGE_CACHE_ROOT` must be on the same filesystem as `SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR`, because a start stages the selected rootfs into the run directory by reflink; the Runner refuses to start otherwise.
Run `secondbox-image-fetcher` as a separate unprivileged user with Skopeo, access to the image cache, and a private Unix socket at `SECONDBOX_RUNNER_IMAGE_FETCHER_SOCKET`.
It must have no access to Workspaces, host devices, or the Docker socket.
Configure its explicit `SECONDBOX_IMAGE_FETCHER_*` environment as shown in `deploy/compose.same-host-runner.yml`.
The same-host package provisions this separate service and its mounts.
A gVisor Runner uses the same fetcher, cache, socket, and publisher-key settings, but needs no run directory: its cache must itself support reflink; see [gVisor runtime](gvisor-runtime.md#client-selected-execution-images).

### Tenant registry access

The Runner host's `execution_image_registry_config_directory` contains `tenants.json`, private credential files, and a `certificates` directory for private registry CAs.
Paths in the JSON refer to the fetcher container, not to the host.
For example:

```json
{
  "application-tenant": {
    "registry": "registry.example.com",
    "repositories": ["agent-sandbox"],
    "authentication": {
      "mode": "token",
      "username": "sandbox-reader",
      "tokenFile": "/run/image-registry/pull-token"
    }
  }
}
```

Use `{"mode":"anonymous"}` for public access.
Use `{"mode":"docker_config","dockerConfigFile":"/run/image-registry/docker.json"}` for an exported Docker login file, including a JSON-key password.
Credential helpers are not supported; export the authentication entry itself.
The fetcher reads the files for each preparation, so credential rotation needs no application changes.
The same-host initialization service assigns this directory to UID 10002; keep credentials private and readable by that UID.
Neither lifecycle requests nor Agent Platform receive these credentials.

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
# Remote placement requires an absolute Runner identity directory. Leave empty for same-host placement; Compose supplies its identity mount.
identity_directory = ''
# Identity directory on the Runner host; absolute when set and required for same-host placement.
identity_host_directory = '<replace-with-absolute-runner-host-path>'

# Artifact trust
# Execution-asset directory on the Runner host; absolute when set and required for same-host placement.
artifact_host_directory = '<replace-with-absolute-runner-host-path>'
# Registry hosts allowed for client-selected execution images.
execution_image_registries = '<replace-with-comma-separated-registry-hosts>'
# Private directory containing tenants.json and exported registry credentials on the Runner host.
execution_image_registry_config_directory = '<replace-with-absolute-tenant-registry-config-directory>'
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package mounts the deployment's execution-image publisher key.
execution_image_public_key = ''
# Client-selected execution-image signing-key fingerprint; exactly 64 lowercase hexadecimal characters and not all zeroes.
execution_image_public_key_sha256 = '0000000000000000000000000000000000000000000000000000000000000000'
# Maximum downloaded OCI archive size per image.
execution_image_max_download_bytes = 0
# Maximum expanded signed bundle size per image.
execution_image_max_expanded_bytes = 0
# Maximum retained expanded image cache size.
execution_image_max_cache_bytes = 0
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /opt/secondbox-artifacts/signing.pub.
artifact_public_key = ''
# Provisioned signed-artifact key fingerprint; exactly 64 lowercase hexadecimal characters and not all zeroes.
artifact_public_key_sha256 = '0000000000000000000000000000000000000000000000000000000000000000'

# Runner storage
# Dedicated reflink-capable Runner storage root on the host; absolute when set and required for same-host placement. Compose binds this root once at /var/lib/secondbox-runner so its state and workspaces children retain one mount identity.
state_host_directory = '<replace-with-absolute-runner-host-path>'
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /var/lib/secondbox-runner/state/logs/runner.jsonl.
log_path = ''
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /var/lib/secondbox-runner/state/logs.
log_directory = ''

# Workspace persistence
# Remote placement requires an absolute Workspace root. Leave empty for same-host placement; Compose uses the existing workspaces child of state_host_directory.
workspace_root = ''
# Storage-pressure recovery threshold; positive and lower than warning and admission-deny thresholds.
storage_pressure_recovery_percent = 0
# Storage-pressure warning threshold; positive and between recovery and admission-deny thresholds.
storage_pressure_warning_percent = 0
# Storage-pressure admission-deny threshold; positive, above warning, and below 100.
storage_pressure_admission_deny_percent = 0

# Firecracker
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /usr/local/bin/firecracker.
firecracker_path = ''
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /usr/local/bin/jailer.
firecracker_jailer_path = ''
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /var/lib/secondbox-runner/jail.
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
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /opt/secondbox-artifacts/kernel.
firecracker_kernel_path = ''
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /opt/secondbox-artifacts/rootfs.ext4.
firecracker_rootfs_path = ''
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /opt/secondbox-artifacts/shared.img.
firecracker_shared_image_path = ''
# Kernel arguments; must include console=ttyS0, reboot=k, panic=1, pci=off, root=/dev/vda, rw, quiet, loglevel=1, i8042.noaux, i8042.nomux, i8042.nopnp, i8042.dumbkbd, and init=/init.
firecracker_kernel_args = ''
# Firecracker CPU template; required.
firecracker_cpu_template = ''
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /var/lib/secondbox-runner/state/run.
firecracker_run_directory = ''
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /var/lib/secondbox-runner/state/firecracker-logs.
firecracker_log_directory = ''

# Snapshot-resume startup
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /var/lib/secondbox-runner/state/snapshot-template-cache. Keep this cache on the same filesystem as firecracker_jail_root: golden memory files are hard-linked into each jail. Snapshot-resume capacity requires a template built from the verified signed bundle.
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
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /var/lib/secondbox-runner/state/network.
sandbox_network_state_directory = ''
# Bridge cleanup policy; required, so replace this string with an explicit Boolean.
sandbox_delete_bridge = '<replace-with-boolean>'
# Remote placement requires this absolute Runner-host path. Leave empty for same-host placement; the package uses /usr/sbin/nft.
network_policy_nft_path = ''
# Maximum pinned DNS answers; must be positive.
network_policy_max_dns_pins = 0
# Maximum DNS TTL; must be a positive Go duration.
network_policy_max_dns_ttl = ''
# Runner-local addresses; a comma-separated list of IP addresses.
network_policy_runner_addresses = ''
# Management networks; a comma-separated list of CIDRs.
network_policy_management_cidrs = ''
# Remote placement requires an absolute egress-context configuration path. Leave empty for same-host placement; Compose supplies its configuration mount.
egress_context_config_path = ''
# Context-indexed Runner-local mappings. Replace the empty list with one or more
# [[runners.egress_contexts]] tables, each containing a unique valid name, and
# [[runners.egress_contexts.gateways]] tables containing logical_name and
# address, attributed_socket, or both. The generator emits the strict JSON file.
egress_contexts = []
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

- Put `state_host_directory` on a dedicated non-root XFS or Btrfs filesystem with reflink support. For same-host placement, the compiler derives the existing `workspaces` child and its container mount at `/var/lib/secondbox-runner/workspaces`. Compose binds the common storage root once so Workspace images, jail state, run state, and snapshot templates retain one mount identity.
- Leave the filesystem target named by `identity_host_directory` absent before `runner-init`. The command validates the declaration without the same-host identity preflight, then creates that exact target; create the artifact and Runner storage host directories first, and run full manifest validation after enrollment.
- Set `pool_id` to the `name` of the selected `[[standard_resources.runner_pools]]` inventory that admits the Runner architecture and capabilities.
- For v0.7.2 only, `network_policy_runner_gateways` is the legacy single global logical-gateway map. It is not accepted as a generation-4 default or fallback. The tenant-aware deployment replaces it with explicit context-indexed Runner-local mappings; the mapping remains `logical-name=IP` authorization rather than guest-side name resolution. The Runner DNS proxy only forwards to its configured upstream, rejects answers resolving to protected addresses, and does not synthesize logical gateway names. Production qualification must prove each installation's injected gateway address and reachability.

Issue one declared identity and protected environment handoff:

```sh
secondbox-deploy runner-init \
  /secure/secondbox-deployment/secondbox.toml \
  runner-east-1 \
  /secure/handoffs/runner-east-1
```

The command signs a client certificate carrying `spiffe://secondbox/runner/<runner-id>`, writes the matching key, CA certificate, and canonical systemd environment, then atomically installs the directory. It refuses an undeclared ID, an existing target, a same-host target that differs from the declared identity directory, or mismatched CA evidence. Copying and activating a remote handoff on its Runner host is an explicit operator action.

Selected RunnerPools and standard Profile lineages are checked and applied after the control plane becomes ready. A repeated deployment is a no-op; an interrupted application resumes from the verified installed prefix. In the tenant-aware release, every Runner in a selected pool maps each required standard Profile logical gateway inside every advertised context that should accept that Profile. No mapping adds a DNS record. See [declarative resources](declarative-resources.md).

## Recovery and replacement

### Upgrade prerequisite: unique ready Snapshot names

Before upgrading across migration `0024_snapshot_name_index.sql`, inspect every
Sandbox's ready Snapshots on the **source release**. Migration 0024 refuses
control-plane startup if two ready Snapshots on one Sandbox share a name.
List them with `secondbox snapshots <sandbox>` or the source-compatible alias:

```sh
secondbox snapshots list --path sandboxId=sbx_SOURCE
```

Follow any `nextCursor` with `--query cursor=<cursor>` to inspect every page.
Compare names among `ready` entries within each Sandbox, choose the Snapshot
to retain, and delete each unwanted duplicate **by identifier** while the
source control plane is still running:

```sh
secondbox snapshots delete --path snapshotId=snp_UNWANTED \
  --header Idempotency-Key=cleanup-snp_UNWANTED
```

Repeat the listing until each ready name is unique, then take the pre-upgrade
backup and activate the target release. Snapshot deletion discards that retained
restore point; preserve the intended one. If migration 0024 already refused
startup, complete this cleanup using the source deployment before retrying the
upgrade; follow the backup/restore requirements below if other forward-only
migrations have already changed the database.

### Release boundaries and recovery

Replacing v0.7.2 with the tenant-aware release in place is unsupported. Quiesce every consuming application, retire every pre-v0.8.0 Sandbox, stop the old deployment, and remove its database, Runner state, and Workspaces through the documented recreation procedure before initializing the new release. Recreate Tenants, authorities, Profiles, Runner context mappings, and Sandboxes from the new contract. There is no historical Profile decoder for the required context policy, legacy assignment support, Workspace import path, or Sandbox migration operation.

The target release's `secondbox-deploy update --check` refuses every source through exactly v0.7.2 before downloading or staging target release inputs. Its error prints this recreation sequence, including the guided single-host `uninstall` and reviewed `uninstall --purge` steps. This refusal is not a migration assistant and never retires a Sandbox or deletes state on the operator's behalf.

Take one coordinated v0.7.2 backup of PostgreSQL, deployment state, every Runner identity, and every complete Workspace filesystem before retirement. That backup is for complete rollback only. Rollback stops the new deployment and restores the complete matching v0.7.2 database and Runner filesystems; never combine v0.7.2 global-gateway state with a generation-4 database or Runner.

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

RunnerPools declare placement inventory: name, state, architectures, and capabilities. Remove `capacityPolicy` from resource documents and API requests, remove it from `mutableFields`, and remove `--capacity` from standard-bundle CLI commands. In deployment manifests, remove `max_sandboxes`, `max_vcpu_count`, and `max_memory_bytes` from `[[standard_resources.runner_pools]]`. These pool settings were never enforced. Tenant and Subject quotas, Profile resource ceilings, and reported Runner capacity continue to govern admission. The forward migration drops only the unused pool metadata column; deploy the control plane and clients together because old clients and old control-plane binaries still expect that field.
