# Deployment and runtime operations

SecondBox deploys an unprivileged `secondboxd` control plane backed by PostgreSQL and an S3-compatible Artifact store. Privileged Firecracker Runners are separate processes on qualified Linux hosts, establish outbound mTLS connections to the control plane, and own their durable local Workspace filesystems.

## Release flow

A SecondBox release is a Git tag on a reviewed commit whose CI run passed. The portable gate is:

```sh
just test-non-kvm
```

The repository does not maintain a separate release-candidate, qualification-record, evidence-assembly, or publication controller. Firecracker validation remains a distinct qualified-host action:

```sh
just test-firecracker
```

`just build-artifacts` builds the control-plane and Runner binaries and checksums. Runtime microVM assets remain immutable, signed inputs verified by Runners; a passing source release does not claim that an arbitrary production host or asset bundle passed KVM validation.

## Bootstrap

Generate a deployment-specific private environment file:

```sh
install -d -m 700 .tmp/secondbox-deploy
just deploy-bootstrap .tmp/secondbox-deploy/environment
just deploy-validate .tmp/secondbox-deploy/environment
```

Bootstrap copies `deploy/environment.example` only when the target is absent. It generates independent PostgreSQL, RustFS, HTTP platform-token, and pre-shared Runner credentials, plus a deployment-local Runner CA and server certificate. Secret files use mode `0600`; the PKI directory uses `0700`; no secret value is printed. A symbolic link, non-regular target, or partial pre-existing PKI directory fails explicitly.

The control plane receives the Runner CA certificate, not its private key. Operators retain the CA private key and issue Runner certificates out of band:

```sh
export SECONDBOX_RUNNER_ID=secondbox-runner-1
export SECONDBOX_RUNNER_CA_CERTIFICATE=/secure/deployment/runner-pki/runner-ca.crt
export SECONDBOX_RUNNER_CA_PRIVATE_KEY=/secure/deployment/runner-pki/runner-ca.key
export SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS=825
deploy/bin/bootstrap-runner-trust.sh /var/lib/secondbox/runner-identity
```

Each Runner connection requires TLS 1.3, a CA-signed certificate whose URI identifies the Runner, and `SECONDBOX_RUNNER_CREDENTIAL`. The HTTP API instead accepts `SECONDBOX_PLATFORM_TOKEN` for operators and the credentials declared by `SECONDBOX_APPLICATION_AUTHORITIES_JSON` for scoped applications; none of these authorities are interchangeable. Replacing the platform, Runner, or application credentials requires a coordinated restart of its consumers so old authenticated connections do not remain live.

Create RunnerPools through the platform-token HTTP API before starting Runners. The CLI always supplies trusted ownership values, either as the explicit flags shown here or from the environment or stored configuration described in [SDK, CLI, and Flue quick starts](sdk-cli-and-flue.md):

```sh
secondbox \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref secondbox \
  --subject-ref secondbox-admin \
  runner-pools create \
  --body /secure/runner-pool.json
```

## Development Compose

Build and start the unprivileged control plane with loopback-only PostgreSQL and RustFS:

```sh
docker build --tag secondbox-control-plane:development .
just deploy-development-prepare .tmp/secondbox-deploy/environment
docker compose \
  --env-file .tmp/secondbox-deploy/environment \
  --file deploy/compose.yml \
  up -d control-plane
```

Preparation validates the complete inventory, starts the development dependencies, and creates the explicitly configured bucket. It is safe to repeat and does not start the control plane.

The control-plane container runs as UID/GID 65532, drops Linux capabilities, sets `no-new-privileges`, uses a read-only root filesystem and bounded `/tmp`, and has no KVM, TUN/TAP, host-cgroup, host-path, or container-engine access. Its only writable persistent mount is the JSON log volume.

The opt-in `same-host-runner` profile is privileged and mounts `/dev/kvm`, `/dev/net/tun`, host cgroups, issued identity, signed assets, and one dedicated state root. It is packaging for a Linux/amd64 Runner, not evidence that the host passed Firecracker validation.

## Production boundary

Production inventory is entirely explicit. It includes:

- a digest-pinned control-plane image and any explicitly deployed dependency images;
- TLS-verified external PostgreSQL;
- an HTTPS S3-compatible Artifact endpoint, existing bucket, and deployment-specific credentials;
- an HTTPS public base URL behind a reverse proxy that preserves `X-Request-ID`;
- the platform token, explicit application-authority JSON, pre-shared Runner credential, Runner CA certificate, and server keypair;
- explicit bind addresses, ports, timeouts, log path, protocol window, enabled Runner features, object limits, and per-subject quota limits;
- an operator-supplied signed-asset catalog.

`deploy/bin/validate-environment.sh` rejects missing values, duplicate keys, placeholders, weak file permissions, mutable production image references, plaintext production object-store URLs, disabled PostgreSQL TLS, reused cross-boundary credentials, invalid certificates, and invalid protocol ranges.

PostgreSQL owns desired state, ownership refs, immutable home assignments, generations, Leases, profile revisions, audit, and reconciliation. The S3-compatible store owns application Artifacts and immutable execution assets only. Each home Runner's `SECONDBOX_RUNNER_WORKSPACE_ROOT` owns its durable Workspace images, local Snapshots, manifests, and receipts; it is not a cache and cannot be reconstructed by the control plane.

## Migrations and replacement

Every `secondboxd` validates and applies the embedded ordered migration lineage under a PostgreSQL advisory lock before opening listeners. Missing, reordered, altered, duplicate, or ahead migration records fail startup. Cross-resource references remain logical strings; the schema deliberately contains no foreign keys or CHECK constraints.

Use coordinated replacement unless the exact deployment has independently proven mixed-version operation:

1. complete and verify a coordinated PostgreSQL/Artifact backup and quiescent backups of every affected Runner identity plus workspace root;
2. stop admission and old control-plane replicas;
3. start the new replicas and require readiness;
4. reopen traffic.

`scripts/backup.sh` preserves a shared database publication fence and verifies reachable Artifact objects. SecondBox provides no managed restore script for Runner-local Workspaces. Operators must recover each stable Runner identity and its workspace root as one consistent unit; see [backup and recovery](backup-and-restore.md).

## Startup checks

Before every start:

```sh
just deploy-config .tmp/secondbox-deploy/environment
curl --fail --silent --show-error http://127.0.0.1:8080/healthz
curl --fail --silent --show-error http://127.0.0.1:8080/readyz
curl --fail --silent --show-error http://127.0.0.1:8080/metrics
```

`/healthz` proves the process answers, `/readyz` proves PostgreSQL connectivity, and `/metrics` exports fixed-cardinality Sandbox and Operation state counts without tenant or resource identifiers.

See [observability and diagnostics](observability-and-diagnostics.md), [backup and restore](backup-and-restore.md), [Kubernetes boundary](kubernetes-boundary.md), and [Firecracker runtime](firecracker-runtime.md).
