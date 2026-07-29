# SecondBox

SecondBox is a self-hostable network service for durable, isolated development sandboxes. The unprivileged Go control plane exposes the v1 HTTP resource API and mTLS Runner control endpoint, and stores desired state in PostgreSQL. The repository also contains the versioned runner protocol, scheduler, reconciliation logic, and a separately built Firecracker runner.

A `Sandbox` is the durable public resource and its running `Instance` is replaceable compute fenced to one Sandbox generation. Each Sandbox is pinned to one home Runner whose reflink-capable filesystem owns its durable Workspace and local immutable Snapshots. `secondboxd` stores desired state in PostgreSQL and immutable application Artifacts in S3-compatible storage; it never transports Workspace images. Compose contains an optional same-host Runner profile; production RunnerPool provisioning, runner-filesystem recovery, and Firecracker validation remain separate operator responsibilities.

SecondBox v1 implements Firecracker only. It ships immutable `agent-compartment` and `coding-environment` built-in profiles for its two core use cases. Operators may also create explicit profiles that fix image, toolchain, resource, lifecycle, storage, networking, execution, and runner-pool policy. Every Sandbox pins the resolved immutable profile revision at creation.

The HTTP API has one deployment-wide `SECONDBOX_PLATFORM_TOKEN`. A trusted upstream caller also supplies opaque `X-SecondBox-Tenant-Ref` and `X-SecondBox-Subject-Ref` headers; SecondBox scopes every owned row to both values but does not authenticate or resolve them. Runner connections use a separate pre-shared Runner credential plus a CA-signed mTLS identity.

## Repository layout

- `cmd/secondboxd` — unprivileged control plane
- `cmd/secondbox` — profile, runner, Sandbox, and data-plane CLI
- `contracts` — canonical public, runner, and guest-agent protocols
- `internal` — control-plane domain, API, scheduling, reconciliation, and persistence
- `migrations/postgres` — SecondBox database migration lineage
- `runner` — privileged Firecracker runner and guest agent
- `sdk` — thin handwritten Go, TypeScript, and Python clients
- `deploy` — Compose, systemd, and deployment examples
- `docs/design` — current architecture and compatibility contracts
- `docs/operations` — installation, backup, and diagnostics

## Validation

The non-KVM gate runs from the repository root:

```sh
just test-non-kvm
```

CI runs the same command as its portable smoke gate. A release is a Git tag on a commit whose CI run passed; SecondBox has no separate qualification, evidence-assembly, or publication controller.

Firecracker validation requires a dedicated Linux host with KVM and the configured test assets:

```sh
just test-firecracker
```

## Development deployment

The Compose deployment starts the unprivileged control plane and offers loopback-only PostgreSQL and RustFS S3-compatible dependencies under the `development` profile. It never embeds a shared credential; bootstrap creates a private environment file with unique generated secrets.

```sh
install -d -m 700 .tmp/secondbox-deploy
just deploy-bootstrap .tmp/secondbox-deploy/environment
just deploy-validate .tmp/secondbox-deploy/environment
docker build --tag secondbox-control-plane:development .
just deploy-development-prepare .tmp/secondbox-deploy/environment
docker compose --env-file .tmp/secondbox-deploy/environment --file deploy/compose.yml up -d control-plane
```

The preparation command is safe to repeat: it validates the bootstrapped development inventory, starts PostgreSQL and RustFS, and creates the explicitly configured bucket before the control plane starts. Read [deployment and runtime operations](docs/operations/deployment.md) before exposing the API or using external PostgreSQL. The supplied RustFS service is a loopback-only development implementation of the object-store dependency consumed by Artifact operations. The coordinated backup command captures PostgreSQL and reachable Artifact objects only. Operators must back up each Runner's stable identity and workspace root as one consistent recovery unit; see [backup and recovery](docs/operations/backup-and-restore.md).

## License

SecondBox source is licensed under the MIT License. Third-party components and execution assets retain their own licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
