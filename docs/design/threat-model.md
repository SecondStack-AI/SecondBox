# Threat model

SecondBox treats application clients, guest workloads, network peers, mutable infrastructure, and stale distributed actors as hostile or fallible. The objective is to preserve Project isolation, generation authority, durable state integrity, host integrity, credential separation, and useful forensic evidence. Availability against an operator with database or host control is outside the platform boundary.

## Trust boundaries

The public HTTP boundary separates application credentials from the unprivileged control plane. PostgreSQL is a separate authority boundary reached with a deployment-specific credential. The configured S3-compatible store is a separate durable-byte boundary reached only by the control plane. The outbound Runner gRPC boundary uses Runner-only mTLS credentials. A privileged Runner host contains an additional Firecracker/jailer boundary around each untrusted guest.

The control-plane image has no reason to access KVM, TUN/TAP, host cgroups, host paths, the container-engine socket, Runner private keys, or guest files. A Runner has no reason to receive bootstrap-operator tokens, application API keys, database credentials, or global object-store credentials. Breaking either separation is a deployment failure.

## Threats and required controls

| Threat | Security property | Required control and evidence |
| --- | --- | --- |
| Stolen application key | Project-confined authority | Opaque high-entropy material, HMAC-backed hashes, scopes, profile grants, expiry, revocation, last-use evidence, and ownership checks before resource disclosure |
| Cross-Project enumeration | Tenant confidentiality | Authentication derives one Project; not-found and authorization behavior do not disclose another Project's identifiers |
| Request replay | Single intended mutation | Project/principal/operation-scoped idempotency keys, request fingerprints, transactional results, and bounded retention |
| Stale Runner or duplicate assignment | Single generation authority | Monotonic generations, opaque fencing tokens, durable assignment state, authenticated Runner identity, and rejection before mutation |
| Compromised Runner | Bounded blast radius | Explicit trust pools, revocable mTLS identity, drain and fencing, per-assignment object authority, fresh-host recovery, and no application/database credentials |
| Malicious guest escape or host probing | Host and peer isolation | Signed immutable artifacts, exact Firecracker/jailer version, cgroups, minimal devices, TAP/firewall policy, narrow vsock protocol, bounded paths and payloads |
| Artifact substitution | Execution integrity | Trusted signing-key fingerprint, signature, SHA-256 manifest, safe relative paths, captured file identity, architecture and protocol compatibility checks |
| Control-plane compromise | No direct compute-host access | Non-root container, all capabilities dropped, read-only filesystem, no host devices or sockets, distinct Runner client credentials, least-privilege database/object roles |
| Database theft | Credential and tenant confidentiality | TLS, restricted network path, encrypted provider storage, non-plaintext credential hashes, private backups, and separate audit-reader role |
| Object-store tampering | Durable-byte integrity | Immutable keys, content hashes, atomic metadata publication, scoped credentials, TLS, retention, verified downloads, and fenced restore streaming; production provider and recovery qualification remain mandatory |
| Log or diagnostic exfiltration | Secret minimization | Structured bounded logs, no secret/workspace collection, private temporary files, output size limits, checksums, and operator review |
| Resource exhaustion | Shared-service availability | Explicit project/profile quotas, request deadlines, payload/output limits, fixed-cardinality metrics, admission capacity, and backpressure |
| Supply-chain compromise | Reproducible trusted release | Pinned dependencies and bases, signed bundles, checksums, provenance, SBOM/license inventory, vulnerability review, and digest-pinned production images |

## Credential lifecycle

Bootstrap-operator, API-key hash, PostgreSQL, object-store, Runner enrollment, Runner mTLS, artifact-signing, and guest runtime credentials are independent secrets. None may be blank, committed, shared across deployments, or reused across trust boundaries.

The deployment bootstrap generates only the credentials consumed by the Compose development stack, including the Runner CA, Runner server certificate, and enrollment-token hash secret. Runner enrollment tokens are single-use and expiring; Runner certificates are revocable and TLS 1.3-only. Operators create explicit RunnerPools through the authenticated administrative API before the separate identity CLI performs enrollment, issuance, rotation, or revocation. Bootstrap does not invent a schedulable pool or pre-enroll a privileged host.

The current bootstrap operator and API-key HMAC secret have no online rotation workflow. Changing them makes durable credential verification fail. This is an accepted release blocker for a production credential-rotation claim, not a reason to retain a second shared secret or fallback hash.

The control plane currently mounts the Runner CA private key because enrollment, rotation, and handshake revocation share one credential authority. A control-plane compromise can therefore issue Runner certificates even though it cannot directly access KVM or runner hosts. Protecting CA issuance in a separate service or hardware-backed signer is future hardening; current incident response rotates the Runner CA, server credential, and every Runner certificate together.

## Deployment risks

The development profile is safe only on one developer host: all ports bind to loopback and RustFS uses a generated deployment credential. Production requires an external TLS proxy, TLS-verified PostgreSQL, an HTTPS S3-compatible endpoint, digest-pinned images, and operator-managed secret storage. Compose environment variables can be visible to host administrators and container-engine operators; those principals are already inside the deployment trust boundary.

The S3 endpoint, bucket, and credential in deployment inventory are active `secondboxd` authorities for checkpoints and Artifacts. Runner gRPC and authenticated RunnerPool administration are active, while the optional same-host Runner is packaging rather than KVM qualification. Operators must not manually insert pool state or treat an uncommitted Runner-local workspace as a durable checkpoint.

## Detection and response

Security-sensitive mutations produce transactional audit rows. HTTP responses return request identifiers, and operations carry identifiers for asynchronous state. Metrics avoid tenant labels. The bounded support collector excludes environment, database, object, workspace, and credential data.

An incident procedure preserves audit and bounded logs, revokes affected application keys, fences affected Runner assignments, revokes Runner credentials, removes compromised hosts from their pool, and restores only from a verified checkpoint on a fresh host. The process contains those revocation, fencing, publication, and restore primitives; an exact production incident workflow remains subject to multi-runner and recovery qualification.

See [security model](security.md), [service boundaries](service-boundaries.md), [profiles and authorization](profiles-and-authorization.md), [networking and ports](networking-and-ports.md), and [deployment operations](../operations/deployment.md).
