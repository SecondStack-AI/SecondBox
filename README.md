# Sandbox Service

Sandbox Service is the durable control plane for isolated compute Environments. An Environment and its workspace survive Instance stop, loss, replacement, and service restart. Instances are generation-fenced replaceable compute.

The service supports three built-in lifecycle policies:

- `agent-compartment`: retained workspace, disposable compute, idle shutdown, and bounded retention.
- `chat-thread`: retained thread workspace with Chat-specific idle shutdown and retention.
- `coding-environment`: explicit start and stop, retained workspace, long retention, and compute that may remain active without a wake owner.

The production compute adapter calls a privileged Sandbox Host through a provider-neutral internal contract. The service never receives Firecracker, KVM, container, host-path, or network implementation details.

Required environment variables are documented by `internal/config.Config`. They intentionally have no application defaults.

The authenticated `/metrics` endpoint exports fixed-cardinality lifecycle, Instance failure, retained-workspace, and artifact usage metrics. Tenant identifiers, provider details, backend references, and Sandbox Host credentials are never metric dimensions.

## Operations

The Sandbox Service owns backup and recovery of its `sandbox` PostgreSQL schema together with opaque Sandbox Host state. A valid recovery point requires both parts from the same quiesced window.

```sh
SANDBOX_BACKUP_DATABASE_URL=postgresql://... \
SANDBOX_HOST_STATE_DIR=/var/lib/secondstack/sandbox-host \
SANDBOX_BACKUP_DIR=/var/backups/secondstack/sandbox \
scripts/backup.sh

SANDBOX_RESTORE_DATABASE_URL=postgresql://... \
SANDBOX_RESTORE_BUNDLE=/var/backups/secondstack/sandbox/sandbox-backup-....tar \
SANDBOX_RESTORE_STAGE_DIR=/var/tmp \
scripts/restore-drill.sh
```

The backup command hashes the database dump and Host state archive, writes a versioned manifest, then extracts and verifies the finished bundle. The restore drill verifies both checksum layers, restores into a throwaway database, confirms the `sandbox` schema, and extracts Host state only into a temporary drill directory.

Operational logs are JSON Lines at `SANDBOX_SERVICE_LOG_PATH`. The Sandbox Host writes its separate provider log at `SANDBOX_HOST_LOG_PATH`. Restart the dependency chain with `./force_restart.sh sandbox-host`; the service graph recreates the Host network sidecar, waits for Sandbox Service health, and then recreates Agent and Chat consumers.

Correlate Environment, Instance, generation, lease, workspace-version, and artifact IDs with the originating wake or trace ID. Sandbox Service owns lifecycle state; Sandbox Host logs are execution evidence, not a second control plane. See [Agent platform service boundaries](../../docs/design/agent-platform-service-boundaries.md).

```sh
SANDBOX_SERVICE_TEST_DATABASE_URL=postgresql://.../sandbox_service_test \
just test
just verify-generated

cd host
just test
just verify-microvm-images /absolute/path/to/signed/artifacts
```

`just test` resets the `sandbox` schema in `SANDBOX_SERVICE_TEST_DATABASE_URL` for PostgreSQL EnvironmentStore conformance. The database must be disposable and its name must contain `test` or `conformance`; the suite refuses canonical database names.

`host/` is a separate Go module and image boundary. It owns the `sandbox-host` and `sandbox-guest-agent` commands, the Firecracker adapter, signed microVM image pipeline, privileged host scripts, and deployment unit. Provider operations and image construction are documented in [Firecracker runtime operations](host/docs/operations/firecracker-runtime.md), and the unsupported Kubernetes boundary is documented in [Kubernetes Sandbox Host](host/docs/operations/kubernetes-sandbox-host.md).
