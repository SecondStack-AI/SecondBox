# Deployment and runtime operations

The supported packaged runtime in this repository is the unprivileged `secondboxd` HTTP and Runner gRPC control plane backed by PostgreSQL and an S3-compatible immutable-object store. `secondboxd` composes checkpoint upload, verified restore streaming, Artifact publication/download, retention, and garbage collection through one shared object-store client. `deploy/compose.yml` provisions PostgreSQL 18.4 and a loopback-only RustFS service for development; that development dependency is not by itself a backup or production-durability qualification.

Compose includes an operator-enabled `same-host-runner` profile. It is separated from the control-plane container and requires explicit KVM, TUN, cgroup, signed-artifact, state, network, and issued mTLS identity inputs. Starting that profile does not establish KVM qualification; its Compose label records that boundary.

## Release inputs

Production releases publish these artifacts from one reviewed source revision:

- a digest-pinned control-plane image built by the root `Dockerfile`;
- Linux `secondbox`, `secondboxd`, `secondbox-runner-identity`, `secondbox-runner`, `secondbox-guest-agent`, and `secondbox-artifact-evidence` binaries with `SHA256SUMS`;
- the two Runner systemd units under `runner/deploy`;
- a digest-pinned GHCR Firecracker artifact transport image built from `runner/deploy/microvm-artifact-transport.Dockerfile` and a signed microVM bundle containing the kernel, rootfs, shared image, manifests, checksums, signature, provenance, package locks, and license inventories;
- canonical OpenAPI, Runner, and guest protocol descriptors plus `release/current-compatibility.json`.

`just build-artifacts` cross-compiles the six standalone binaries explicitly for Linux amd64 and writes their checksums. `scripts/package-release-artifacts.sh` verifies that every binary is an ELF64 little-endian x86-64 executable, creates a deterministic runtime archive, preserves executable mode on deployment scripts, includes the Runner trust bootstrap, and hashes every packaged contract, migration, deployment file, notice, document, and binary from one commit. Its manifest records both file modes and hashes, and packaging fails if copied binary or Runner deployment bytes differ from their source artifacts. `scripts/package-release-sdk-artifacts.sh` creates the Go source archive and versioned npm package from that same clean commit. `scripts/package-release-guest-assets.sh` verifies and packages an independently trusted signed guest bundle whose kernel and rootfs provenance identify the commit. The subject manifest requires the GHCR transport image by immutable digest and binds it to that exact signed guest bundle digest. The image's SLSA statement must repeat the guest digest as a resolved dependency. The evidence scripts bind SBOM, vulnerability, dependency-age, license, checksum, signature, and provenance records to every exact digest. None of these scripts publishes, pushes, tags, or creates a release. A release is not production-qualified merely because a local artifact was built.

## Bootstrap

Generate a deployment-specific private environment file:

```sh
install -d -m 700 .tmp/secondbox-deploy
just deploy-bootstrap .tmp/secondbox-deploy/environment
just deploy-validate .tmp/secondbox-deploy/environment
```

Bootstrap copies `deploy/environment.example` when the target does not exist, generates independent PostgreSQL, RustFS, bootstrap-operator, API-key hashing, and runner-enrollment hash credentials, and creates a deployment-local Runner CA and server certificate. In development mode it also replaces the catalog placeholder with a deployment-local catalog containing one clearly development-only trust record, which is sufficient for control-plane startup but is not Runner or release qualification. The environment and private keys use mode `0600`; the catalog uses `0644` so the unprivileged control-plane container can read its non-secret trust metadata; the PKI directory uses `0700`; no secret value is printed. Repeating the command does not change an already bootstrapped file. A symbolic link, non-regular target, or pre-existing untracked PKI directory is rejected.

Production bootstrap never generates asset trust inventory. Set `SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH` to an existing operator-supplied release catalog before bootstrapping a production environment. The validator and control plane fail when that file is absent, and no deployment command creates a production bucket.

Set `SECONDBOX_RUNNER_SERVER_NAME` in the template copy before the first bootstrap when Runners will use a remote DNS name or IPv4 address. Bootstrap validates that value and places exactly that identity in the server certificate SAN. It does not add a hidden loopback or Compose alias. Runners set the same value as `SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME`.

No literal shared or blank credential exists in the Compose file or example. Keep the generated file outside source control and back it up as secret material. The control plane creates the bootstrap operator exactly once under a PostgreSQL advisory lock. Every later startup verifies that the configured bootstrap credential hashes to the durable credential; accidentally replacing either the bootstrap token or `SECONDBOX_API_KEY_HASH_SECRET` makes startup fail.

Online rotation of the bootstrap token or API-key hashing secret is not implemented. Do not edit either value in place. PostgreSQL and object-store credentials may be rotated with the provider's atomic credential procedure, followed by updating the private environment and restarting the consumers. A release must add a transactional rehash/rotation workflow before it can claim control-plane credential rotation.

`secondboxd` serves Runner control with TLS 1.3 and per-connection certificate revocation verification. `secondbox-runner-identity` provides create-enrollment, redeem, rotate, and revoke operations against the same PostgreSQL authority and CA. Enrollment tokens are single-use and expiring; certificate outputs are create-only mode `0600`.

Create the RunnerPool through the authenticated administrative API before issuing enrollment. The RunnerPool declares accepted architectures, capabilities, operator capacity policy, and whether new enrollment is accepted. `readyRunnerCount` is runner-reported evidence and cannot be set by the operator.

```sh
cat > /tmp/secondbox-runner-pool.json <<'JSON'
{
  "name": "qualified-amd64",
  "state": "ready",
  "architectures": ["amd64"],
  "capabilities": ["checkpoint", "firecracker"],
  "capacityPolicy": {"maxInstances": 16}
}
JSON
secondbox \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_BOOTSTRAP_ADMIN_TOKEN" \
  runner-pools create \
  --body /tmp/secondbox-runner-pool.json
```

RunnerPool and Runner list/get commands return administrative projections without credential or host-path material. Runner enrollment remains a separate local operator action because it holds the Runner CA signing authority.

After creating the pool, use `secondbox-runner-identity create-enrollment` to mint a bounded single-use token, then create the runner key and redeem the token into a private host directory:

```sh
export SECONDBOX_RUNNER_ID=secondbox-runner-1
export SECONDBOX_RUNNER_CA_CERTIFICATE=/secure/deployment/runner-pki/runner-ca.crt
export SECONDBOX_RUNNER_IDENTITY_BINARY=/secure/release/bin/secondbox-runner-identity
export SECONDBOX_RUNNER_ENROLLMENT_TOKEN='one-time token from create-enrollment'
deploy/bin/bootstrap-runner-trust.sh /var/lib/secondbox/runner-identity
unset SECONDBOX_RUNNER_ENROLLMENT_TOKEN
```

The command stages a new RSA key and CSR, redeems the single-use token, verifies the returned certificate against the configured CA, and publishes the directory only after the key and certificate match. Rerunning against a complete identity verifies it without requiring enrollment authority and leaves all files unchanged. A partial, mismatched, or differently rooted identity fails rather than being replaced.

The packaged Runner unit reads `/run/secondbox-runner/runner.env` without compiled or unit-file defaults. That private file must forward `SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT` and `SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT` as distinct integers from 1 through 65535, and `SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL` as a Go duration from 1 millisecond through 60 seconds. The example inventory uses `1024`, `1025`, and `5s`; operators must keep the same explicit values in the Runner environment selected for a release qualification.

The same environment must identify the nftables executable, DNS pin count and TTL bounds, every Runner address, every management CIDR, and the controlled DNS upstream through the six `SECONDBOX_RUNNER_NETWORK_POLICY_*` settings. `deploy/compose.yml` forwards each value into the privileged Runner and supplies no Compose default. The addresses and upstream in `deploy/environment.example` describe only its development bridge inventory; an operator must replace them with the complete addresses and networks of the qualified host. Missing or malformed pin bounds fail deployment validation, and the Runner independently fails readiness when nftables or the DNS listener is unavailable.

The current negotiated Runner feature inventory contains Exec, File, PTY, evidence, checkpoint, and port-proxy support. The packaged Runner requires that exact set and fails its Hello negotiation if the control plane omits a mandatory feature. Port sessions terminate at the control plane URL in `SECONDBOX_PUBLIC_BASE_URL` and traverse the existing outbound Runner connection; they do not require or permit a public Runner listener or host-port publication.

## Development Compose

Build the control-plane image, then prepare the development dependencies and inventory before the API:

```sh
docker build --tag secondbox-control-plane:development .
just deploy-development-prepare .tmp/secondbox-deploy/environment
docker compose --env-file .tmp/secondbox-deploy/environment --file deploy/compose.yml up -d control-plane
```

`just deploy-development-prepare` calls `deploy/bin/prepare-development-inventory.sh`. It validates the complete environment, refuses every mode other than `development`, waits the explicitly configured number of seconds for PostgreSQL and RustFS, and runs the pinned `SECONDBOX_OBJECT_STORE_CLIENT_IMAGE` once to create exactly `SECONDBOX_OBJECT_STORE_BUCKET`. Bucket creation uses `--ignore-existing`, so repeating the command preserves existing objects and proves the bucket remains addressable. It does not start the control plane.

The development profile binds API, PostgreSQL, RustFS S3 API, and RustFS console ports to `127.0.0.1`. PostgreSQL applies `migrations/postgres/0001_secondbox.sql` when initializing a new empty volume. Its health probe uses TCP so the temporary Unix-socket-only initialization server cannot make preparation return before the final database server starts. Before opening any store or listener, `secondboxd` then validates and applies the same embedded ordered migration lineage under a PostgreSQL advisory lock. A raw Compose baseline with an empty migration ledger is adopted only when its complete catalog fingerprint exactly matches the frozen initial-v1 catalog; partial or extra schema objects fail startup. RustFS starts in single-node/single-disk development mode with generated root credentials. `secondboxd` consumes the prepared bucket but does not create provider inventory itself.

The control-plane container runs as UID/GID 65532, drops all Linux capabilities, sets `no-new-privileges`, uses a read-only root filesystem and bounded `/tmp`, and has no KVM, TUN/TAP, host cgroup, host path, or container-engine socket. Its only writable mount is the JSON log volume. A short-lived root initializer copies the host-owned mode `0600` Runner keys into a dedicated named volume, assigns them to UID 65532, and exits; the control plane mounts that volume read-only. The initializer receives only `CHOWN`, `DAC_OVERRIDE`, and `FOWNER`.

To render the opt-in runner configuration, first set `SECONDBOX_SAME_HOST_RUNNER_ENABLED=true`, provision every host directory and signed-artifact input listed in `deploy/environment.example`, and validate the environment. Start it separately:

```sh
docker compose --env-file .tmp/secondbox-deploy/environment --file deploy/compose.yml --profile same-host-runner up -d same-host-runner
```

The runner container is intentionally privileged, uses host networking and cgroups, and mounts `/dev/kvm`, `/dev/net/tun`, its issued identity, signed artifacts, and one dedicated state root. The profile is Linux/amd64 Firecracker packaging, not proof that the host, artifacts, bridge policy, cgroup delegation, cleanup, or a real assignment passed qualification. Run the dedicated qualified-host gate before representing it as ready capacity.

Stop the stack without destroying durable volumes:

```sh
docker compose --env-file .tmp/secondbox-deploy/environment --file deploy/compose.yml --profile development down
```

Deleting the PostgreSQL or object-store volumes is destructive and is not part of an ordinary stop or upgrade.

## Database migrations and control-plane replacement

Every `secondboxd` replica validates the migration ledger before constructing application stores. Recorded migrations must be an exact checksum-matching prefix of the embedded lineage. Missing earlier versions, duplicate or ahead versions, altered SQL checksums, a missing ledger in an existing schema, and an untracked initial catalog all fail startup. A fresh database applies the lineage transactionally while holding the migration lock. Concurrent replica starts serialize on that lock and revalidate the committed ledger.

The initial-v1 release-candidate compatibility drill freezes a minimal v1 client, the canonical API and protocol descriptors, the baseline ProfileRevision, the checkpoint compatibility map, and the migration checksum. It proves the frozen client across control-plane process replacement, immutable old and new ProfileRevision pins, verified checkpoint streaming, fresh migration, exact raw-baseline adoption, idempotent replay, and rejection of partial, extra, drifted, missing-prefix, and ahead database state.

This is current release-candidate evidence, not adjacent released-version qualification. Until two released control-plane versions exist and the old binary is exercised before and after the new migration set, use a coordinated replacement: complete and verify the database/object backup, stop admission, stop old control-plane replicas, start the new replicas, and require readiness before reopening traffic. Do not claim an online mixed-version rolling upgrade from the current evidence.

## Production boundary

Set `SECONDBOX_DEPLOYMENT_MODE=production` and supply:

- digest-pinned control-plane, Runner, PostgreSQL, and object-store image references, even when optional services are not started;
- an external PostgreSQL URL with TLS verification enabled;
- an HTTPS public base URL behind an explicitly configured reverse proxy or ingress;
- an HTTPS S3-compatible endpoint and deployment-specific bucket metadata;
- an existing deployment-specific bucket and an operator-supplied signed-asset catalog;
- explicit HTTP and Runner gRPC bind addresses and published ports, timeouts, log path, protocol window, enabled Runner features, and every project/profile quota.

`deploy/bin/validate-environment.sh` rejects mutable production image references, plaintext production object-store URLs, `sslmode=disable`, non-external HTTP TLS termination, placeholders, missing values, duplicate keys, weak file permissions, reused trust-boundary credentials, invalid Runner certificates, and protocol ranges with minimum greater than maximum. The external HTTP proxy must preserve `X-Request-ID`, enforce request and response bounds, and use the readiness endpoint rather than liveness for traffic admission. Runner gRPC terminates its own mTLS and must not be exposed through a proxy that replaces client-certificate identity.

The PostgreSQL URL and S3-compatible endpoint, bucket, and region are active `secondboxd` production inventory; validation rejects disabled database TLS and plaintext object-store transport. Checkpoint and Artifact paths use immutable keys, size and SHA-256 verification, atomic metadata publication, and two-phase garbage collection. Production use still requires provider durability, credentials, retention, monitoring, KVM, multi-runner, and recovery qualification for the exact deployment.

## Ports, storage, pools, and capacity

The API container listens on `SECONDBOX_LISTEN_ADDR`; the supplied image health check expects container port 8080, so packaged deployments set `0.0.0.0:8080` and control exposure with `SECONDBOX_API_BIND_IP` and the external TLS proxy. Runner gRPC listens on `0.0.0.0:9443` in the container and is published through the explicit Runner bind address and port. PostgreSQL and RustFS ports are published only for the development profile and remain loopback-only.

PostgreSQL is the authority for operators, projects, profiles, desired state, assignments, operations, idempotency, Artifact metadata, and audit. The configured S3-compatible bucket is the authority for immutable checkpoint and Artifact bytes. The control-plane log volume is operational evidence, not durable domain state; Runner-local active workspaces remain replaceable caches of the last committed checkpoint.

Default project and profile quotas are explicit admission settings for newly created resources. They are not host-capacity discovery. Runner registration publishes pool, capability, capacity, protocol, and signed-artifact evidence through the gRPC stream. Creating a RunnerPool does not make it schedulable: profile admission still requires a registered ready Runner with compatible evidence, and the same-host profile is not KVM qualification.

## Startup validation

Before every start or upgrade:

```sh
just deploy-config .tmp/secondbox-deploy/environment
```

Then require:

```sh
curl --fail --silent --show-error http://127.0.0.1:8080/healthz
curl --fail --silent --show-error http://127.0.0.1:8080/readyz
curl --fail --silent --show-error http://127.0.0.1:8080/metrics
```

`/healthz` proves the HTTP process is responsive. `/readyz` performs a PostgreSQL readiness check. `/metrics` exports fixed-cardinality sandbox, operation, and API-key state counts without project or resource identifiers.

See [observability and diagnostics](observability-and-diagnostics.md), [backup and restore boundary](backup-and-restore.md), [Kubernetes boundary](kubernetes-boundary.md), [compatibility status](compatibility.md), [release evidence](release-evidence.md), and [qualification gates](qualification.md).
