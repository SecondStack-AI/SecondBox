# Deployment and runtime operations

SecondBox deploys one unprivileged control plane and separately managed privileged Firecracker Runners. Operators describe the deployment in one strict, versioned `secondbox.toml`; `secondbox-deploy` compiles that manifest into the process environments consumed by Compose, `secondboxd`, and remote Runner service managers. The generated environment is transport, not operator input.

## One-command development control plane

From a clean checkout:

```sh
just deploy-development-up .tmp/secondbox-development
```

The command creates the directory only when it is absent, writes a mode-`0600` manifest and mode-`0700` secret directory, generates independent local authorities and Runner PKI, builds the control-plane image, renders and validates the environment, starts loopback-only PostgreSQL and object storage, creates the configured bucket, starts the control plane, and requires `/readyz`. It refuses an existing directory without `secondbox.toml` and never rewrites an existing manifest, secret, identity, workspace, or execution asset.

Development initialization alone is available as:

```sh
just deploy-init-development .tmp/secondbox-development
just deploy-config .tmp/secondbox-development/secondbox.toml
```

The reviewed development topology intentionally starts no privileged Runner. Runner enrollment and host qualification remain separate operations on a qualified Linux host.

## The deployment manifest

[`deploy/secondbox.example.toml`](../../deploy/secondbox.example.toml) documents `schema_version = 1` and every accepted field. The manifest has eight decision groups:

1. `deployment`: mode, public ingress, TLS termination, process bind addresses, and image references;
2. `database`: bundled or external PostgreSQL and the authority required by that choice;
3. `object_store`: bundled or external S3-compatible storage, addressing, bucket, region, temporary path, and credentials;
4. `[[runners]]`: immutable Runner IDs, same-host or remote placement, pool, capacity, host integration, networking, and execution assets;
5. `runner_trust`: enrollment credential, CA, server identity, and certificate policy;
6. `applications`: platform and application authorities;
7. `policy`: the nine subject quota limits and relay retention;
8. `policy` and `overrides`: contested recovery/rollout settings and intentionally selected tuning overrides.

Unknown keys, duplicate keys, unsupported schema versions, ambiguous bundled/external fields, incomplete authority, mutable production images, invalid cross-field relationships, and invalid cryptographic trust material fail with a `SecondBox deployment manifest` error. The decoder does not interpolate `${ENV}`, include files, or merge ambient environment variables.

Validate and inspect without rendering:

```sh
go run ./cmd/secondbox-deploy validate /secure/secondbox/secondbox.toml
go run ./cmd/secondbox-deploy inspect /secure/secondbox/secondbox.toml
```

`inspect` prints all resolved non-secret values, positive help for the relay-retention policy, and all 19 available tuning overrides with their compiled defaults. Secret values and secret-revealing paths are redacted.

### Secret references

Secret-bearing manifest fields name files instead of containing secret material. Relative references resolve from the manifest directory, never the caller's working directory. References must name exact regular non-symbolic-link files. A secret file contains one value: resolution removes at most one terminal LF and rejects CR, NUL, or any additional line break without trimming other bytes.

Runner host paths are different: they are typed absolute values interpreted on the declared Runner host. Rendering a remote Runner handoff never opens or validates those paths on the control-plane host.

### Authority, policy, tuning, and compiled facts

Required deployment authority has no default. This includes identities, credentials, endpoints, process and storage paths, signed-asset catalog and bundle digests, object-store addressing mode, the nine subject quota limits, and relay retention.

`policy.data_plane_retention_seconds` participates in each relay session's transition-specific session/result/idempotency deadline. That stored deadline also bounds replayable relay frames as a maximum safety fallback. Frames are delivery and replay state, not the materialised result: after no live delivery or replay consumer remains, they may be removed before the session deadline while buffered results, terminal outcome, admission replay, and compact sequence/hash evidence remain available. Raising this value lengthens the fallback; it does not require every non-replayable payload frame to remain for that whole interval.

The manifest also requires three contested rollout/recovery decisions: relay poll interval, Runner command poll interval, and enabled Runner features. They remain operator policy until a separate decision reclassifies them.

The `[overrides]` table contains code-owned tuning. Every field is optional. When absent, `secondboxd` uses the reviewed value shown by `inspect`; when present, the exact value is rendered and passes the same validation and cross-field checks as before. Invalid overrides fail rather than falling back. Compose uses value-less pass-through mappings so an absent override remains unset instead of becoming an empty string.

Runner protocol minimum and maximum are not configuration. Both binaries compile the one supported protocol window, and generated-protocol verification rejects drift between the two modules.

## Production initialization

Create the protected skeleton:

```sh
just deploy-init-production /secure/secondbox-deployment
```

An incomplete production initialization is intentionally unusable and reports every unresolved decision group in one error. Before validation, production operators must supply:

- digest-pinned control-plane and Runner images, public HTTPS ingress, and external TLS termination;
- bundled or external database authority, with `sslmode=verify-full` for an external database;
- bundled object storage at its explicit private Compose endpoint, or external object-store authority at an HTTPS endpoint;
- zero or more explicit immutable Runner declarations and their placement;
- an operator-supplied signed-asset catalog, verified bundle digests, Runner CA, and server keypair;
- independent platform, application, and Runner enrollment authorities;
- all nine subject quota limits;
- retention, contested recovery/rollout policy, and any intentional tuning overrides.

Automation can materialize a complete create-only target non-interactively after generating and reviewing the same typed input:

```sh
go run ./cmd/secondbox-deploy init --mode production \
  --input /automation/complete-production.toml \
  /secure/secondbox-deployment
```

No generated development authority is accepted as a production default. Any dependency image selected in production is immutable by digest.

## Rendering and Compose

Render explicitly when handing the environment to another tool:

```sh
go run ./cmd/secondbox-deploy render \
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

The deployment command supplies the project name, env file, and exact overlay list explicitly. It removes ambient `SECONDBOX_*` and `COMPOSE_*` variables and retains only Docker client connectivity variables. The base [`deploy/compose.yml`](../../deploy/compose.yml) contains the control plane and shared resources; `compose.development.yml` adds the reviewed local database and object-store pair; production selects independent bundled-database and bundled-object-store overlays only when requested; `compose.same-host-runner.yml` adds only the privileged Runner. Inactive services are never hidden behind profiles that still interpolate missing values.

The control-plane container runs as UID/GID 65532 with a read-only root, dropped capabilities, `no-new-privileges`, and no KVM, TUN/TAP, host-cgroup, workspace, or container-engine access. A selected same-host Runner overlay is privileged and receives host devices and cgroups, but executes `secondbox-runner` directly as PID 1 and starts no private init system, login manager, or console process.

## Runner enrollment and handoff

Every `[[runners]]` entry is keyed by immutable `runner_id`. At most one may use `placement = "same-host"`; any number may use `placement = "remote"`.

Issue one declared identity and protected environment handoff:

```sh
go run ./cmd/secondbox-deploy runner-init \
  /secure/secondbox-deployment/secondbox.toml \
  runner-east-1 \
  /secure/handoffs/runner-east-1
```

The command signs a client certificate carrying `spiffe://secondbox/runner/<runner-id>`, writes the matching key, CA certificate, and canonical systemd environment, then atomically installs the directory. It refuses an undeclared ID, an existing target, a same-host target that differs from the declared identity directory, or mismatched CA evidence. Copying and activating a remote handoff on its Runner host is an explicit operator action.

Create RunnerPools through the platform API before starting their Runners. A Profile that names an absent pool admits Sandboxes that cannot be placed. Built-in Profile bundle fields must be canonical `sha256:` digests found in the deployment's verified signed-asset catalog.

## One-shot migration from the legacy environment

The former editable environment interface is intentionally replaced. Migrate one already validated legacy environment without modifying it:

```sh
go run ./cmd/secondbox-deploy migrate \
  /secure/legacy/secondbox.env \
  /secure/secondbox-deployment
```

Migration requires a mode-`0600` regular source with exactly the historical 146 names. It rejects unknown, duplicate, missing, placeholder, or invalid values; extracts inline secrets into protected referenced files; writes the manifest once; pins the legacy tuning values as explicit overrides; and removes the obsolete protocol declarations. It refuses an existing target and cleans up only artifacts it created after a failure. Migration is not a runtime compatibility path: all later validation, rendering, inspection, and Compose commands consume only the manifest.

## Recovery and replacement

PostgreSQL owns desired state, immutable home assignments, generations, Leases, profiles, audit, and reconciliation. S3-compatible storage owns Artifacts and immutable execution assets. Each home Runner's reflink-capable workspace root owns its Workspaces and local Snapshots. A Sandbox never relocates, and neither PostgreSQL nor object storage can reconstruct a lost unbacked Runner workspace filesystem.

Before replacement, take and verify a coordinated PostgreSQL/Artifact backup and quiescent backups of every affected Runner identity plus workspace root. Restore each stable Runner identity and workspace root as one consistent unit. The generated environment can be reproduced from `secondbox.toml` and its referenced secret material; it is not backup authority.

Every `secondboxd` applies the embedded ordered migration lineage under a PostgreSQL advisory lock before opening listeners. Use coordinated replacement unless the exact deployment has independently proven mixed-version operation.

## Runtime checks

```sh
curl --fail --silent --show-error http://127.0.0.1:8080/healthz
curl --fail --silent --show-error http://127.0.0.1:8080/readyz
curl --fail --silent --show-error http://127.0.0.1:8080/metrics
```

`/healthz` proves the process answers, `/readyz` proves PostgreSQL connectivity, and `/metrics` exports fixed-cardinality state counts without tenant or resource identifiers.

See [backup and restore](backup-and-restore.md), [Firecracker runtime](firecracker-runtime.md), [multirunner qualification](multirunner-qualification.md), and [observability and diagnostics](observability-and-diagnostics.md).
