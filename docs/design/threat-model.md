# Threat model

SecondBox treats guest workloads, network peers, mutable infrastructure, and stale distributed actors as hostile or fallible. The trusted upstream caller and operators are inside the platform boundary. Availability against an operator with database, object-store, platform-token, or Runner-host control is out of scope.

## Trust boundaries

The HTTP boundary validates one deployment-wide platform token and accepts caller-asserted tenant and subject references. The caller is responsible for authenticating end users and authorizing those assertions. A caller bug or compromise can cross subjects; this is an explicit accepted risk.

PostgreSQL is the desired-state and ownership authority. The S3-compatible store is the durable-byte authority for Artifacts and immutable execution assets. Each home Runner's local filesystem is the durable-byte authority for its Workspaces and Snapshots. The outbound Runner gRPC boundary uses a separate pre-shared credential plus CA-signed mTLS identity. A privileged Runner host contains an additional Firecracker/jailer boundary around each untrusted guest.

The control-plane image has no reason to access KVM, TUN/TAP, host cgroups, host paths, the container-engine socket, Runner private keys, or guest files. A Runner has no reason to receive the HTTP platform token, database credentials, or global object-store credentials.

## Threats and required controls

| Threat | Security property | Required control and evidence |
| --- | --- | --- |
| Stolen platform token | Bounded incident response | Secret storage, transport TLS, bounded request inputs, audit correlation, token replacement, and control-plane restart |
| Incorrect ownership assertion | Visible accepted risk | Upstream authorization; exact tenant/subject scoping in every owned query; no claim that SecondBox independently verifies the assertion |
| Request replay | Single intended mutation | Tenant/subject/operation-scoped idempotency keys, request fingerprints, transactional results, and bounded retention |
| Stale Runner or duplicate assignment | Single generation authority | Monotonic generations, opaque fencing tokens, durable assignment state, authenticated Runner identity, and rejection before mutation |
| Compromised Runner | Bounded blast radius | Explicit trust pools, credential replacement, drain and fencing, operator recovery of stable identity plus Workspace root, and no platform/database credentials |
| Malicious guest escape or host probing | Host and peer isolation | Signed immutable assets, Firecracker/jailer, cgroups, minimal devices, TAP/firewall policy, narrow vsock protocol, and bounded paths/payloads |
| Artifact substitution | Execution and data integrity | Trusted signing keys, SHA-256 manifests, immutable keys, staged verification, atomic publication, and verified downloads |
| Control-plane compromise | No direct compute-host access | Non-root container, dropped capabilities, read-only filesystem, no host devices or sockets, and distinct Runner authority |
| Object-store tampering | Artifact integrity | Content hashes, size evidence, retention, and verified Artifact reads |
| Home-Runner disk loss | Explicit durability boundary | Reflink-store readiness, immutable home placement, no empty or cross-Runner fallback, and consistent external backup of stable Runner identity plus Workspace root |
| Resource exhaustion | Shared-service availability | Explicit per-subject quotas, request deadlines, payload/output limits, admission capacity, and backpressure |

## Credential lifecycle

The platform token, PostgreSQL credential, object-store credential, pre-shared Runner credential, Runner mTLS keys, artifact-signing keys, and guest runtime credentials are separate secrets. The deployment bootstrap creates the Compose development credentials and Runner PKI. Operators issue Runner certificates out of band; the Runner CA private key is not mounted into the control plane.

Replacing the platform token requires coordinated caller and control-plane rollout. Replacing the Runner credential requires restarting the control plane and every trusted Runner so no old authenticated stream remains. A CA compromise additionally requires replacing the CA, server credential, and Runner certificates.

## Detection and response

Security-sensitive mutations produce audit rows. HTTP responses and asynchronous Operations carry request identifiers. Metrics avoid ownership labels. An incident preserves audit and bounded logs, replaces affected credentials, fences affected Runner assignments, and recovers affected Sandboxes only by restoring the same stable Runner identity and its Workspace root from a trusted consistent backup. Without that backup, those Sandboxes and local Snapshots are lost.

See [security model](security.md), [service boundaries](service-boundaries.md), [profiles and authorization](profiles-and-authorization.md), [networking and ports](networking-and-ports.md), and [deployment operations](../operations/deployment.md).
