# SecondBox

SecondBox is a self-hostable network service for durable, isolated development sandboxes. The current unprivileged Go control plane exposes the v1 HTTP resource API and mTLS Runner control endpoint, and stores desired state in PostgreSQL. The repository also contains the versioned runner protocol, credential authority, scheduler, reconciliation logic, and a separately built Firecracker runner.

A `Sandbox` is the durable public resource and its running `Instance` is replaceable compute fenced to one Sandbox generation. `secondboxd` composes verified S3-compatible checkpoint publication and restore, immutable named Snapshots of committed stopped-state disk, and immutable application Artifact storage. Runner identity administration is available as a separate CLI, and Compose contains an optional same-host Runner profile, but production RunnerPool provisioning, KVM qualification, and remote multi-runner qualification remain separate requirements.

SecondBox v1 implements Firecracker only. It does not ship built-in profiles: operators explicitly create profiles that fix image, toolchain, resource, lifecycle, storage, networking, execution, and runner-pool policy before clients can create Sandboxes.

## Repository layout

- `cmd/secondboxd` — unprivileged control plane
- `cmd/secondbox` — administrative and application CLI
- `contracts` — canonical public, runner, and guest-agent protocols
- `internal` — control-plane domain, API, scheduling, reconciliation, and persistence
- `migrations/postgres` — SecondBox database migration lineage
- `runner` — privileged Firecracker runner and guest agent
- `sdk` — generated transports and handwritten client helpers
- `deploy` — Compose, systemd, and deployment examples
- `docs/design` — current architecture and compatibility contracts
- `docs/operations` — installation, qualification, backup, and diagnostics

## Validation

The non-KVM gate runs from the repository root:

```sh
just test-non-kvm
```

CI runs the same gate through `just test-clean-clone`. That command refuses a dirty source tree and executes the complete portable matrix from an independently cloned commit with isolated Go and npm caches.

Firecracker and multi-runner qualification require dedicated Linux hosts that satisfy the prerequisites in [qualification gates](docs/operations/qualification.md):

```sh
just test-firecracker
just test-multirunner
```

## Development deployment

The Compose deployment starts the unprivileged control plane and offers loopback-only PostgreSQL and RustFS S3-compatible dependencies under the `development` profile. It never embeds a shared credential; bootstrap creates a private environment file with unique generated secrets.

```sh
install -d -m 700 .tmp/secondbox-deploy
just deploy-bootstrap .tmp/secondbox-deploy/environment
just deploy-validate .tmp/secondbox-deploy/environment
docker build --tag secondbox-control-plane:development .
docker compose --env-file .tmp/secondbox-deploy/environment --file deploy/compose.yml --profile development up -d postgres object-store
docker compose --env-file .tmp/secondbox-deploy/environment --file deploy/compose.yml up -d control-plane
```

Read [deployment and runtime operations](docs/operations/deployment.md) before exposing the API or using external PostgreSQL. The supplied RustFS service is a loopback-only development implementation of the object-store dependency consumed by checkpoint and Artifact operations. The coordinated backup command and isolated restore drill prove portable PostgreSQL/object-store recovery and fresh-Runner checkpoint materialization; they do not replace provider durability or packaged KVM and multi-runner qualification.

The implementation plan is tracked in [SecondBox standalone service](docs/plans/2026-07-28-secondbox-standalone-service.md).

## License

SecondBox source is licensed under the MIT License. Third-party components and execution assets retain their own licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
