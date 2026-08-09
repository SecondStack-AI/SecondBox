# Threat model

SecondBox treats guest workloads, network peers, mutable infrastructure, and stale distributed actors as hostile or fallible. The trusted upstream caller and operators are inside the platform boundary. Availability against an operator with database, platform-token, or Runner-host control is out of scope.

## Trust boundaries

The HTTP boundary validates one deployment-wide platform token and accepts caller-asserted tenant and subject references. The caller is responsible for authenticating end users and authorizing those assertions. A caller bug or compromise can cross subjects; this is an explicit accepted risk.

PostgreSQL is the desired-state and ownership authority. Each home Runner's local filesystem is the durable-byte authority for its Workspaces and Snapshots. Signed immutable execution assets are release inputs verified and cached by each Runner. The outbound Runner gRPC boundary uses a separate pre-shared credential plus CA-signed mTLS identity. A privileged Runner host contains an additional Firecracker/jailer boundary around each untrusted guest.

The control-plane image has no reason to access KVM, TUN/TAP, host cgroups, host paths, the container-engine socket, Runner private keys, or guest files. A Runner has no reason to receive the HTTP platform token or database credentials.

## Threats and required controls

| Threat | Security property | Required control and evidence |
| --- | --- | --- |
| Stolen platform token | Bounded incident response | Secret storage, transport TLS, bounded request inputs, audit correlation, token replacement, and control-plane restart |
| Incorrect ownership assertion | Visible accepted risk | Upstream authorization; exact tenant/subject scoping in every owned query; no claim that SecondBox independently verifies the assertion |
| Request replay | Single intended mutation | Tenant/subject/operation-scoped idempotency keys, request fingerprints, transactional results, and bounded retention |
| Stale Runner or duplicate assignment | Single generation authority | Monotonic generations, opaque fencing tokens, durable assignment state, authenticated Runner identity, and rejection before mutation |
| Compromised Runner | Bounded blast radius | Explicit trust pools, credential replacement, drain and fencing, operator recovery of stable identity plus Workspace root, and no platform/database credentials |
| Malicious guest escape or host probing | Host and peer isolation | Signed immutable assets, Firecracker/jailer, cgroups, minimal devices, TAP/firewall policy, narrow vsock protocol, and bounded paths/payloads |
| Execution-asset substitution | Execution integrity | Trusted signing keys, SHA-256 manifests, staged verification, and atomic local publication |
| Control-plane compromise | No direct compute-host access | Non-root container, dropped capabilities, read-only filesystem, no host devices or sockets, and distinct Runner authority |
| Home-Runner disk loss | Explicit durability boundary | Reflink-store readiness, no empty or automatic cross-Runner fallback, source-required operator relocation, and consistent external backup of stable Runner identity plus Workspace root |
| Resource exhaustion | Shared-service availability | Explicit per-subject quotas, request deadlines, payload/output limits, admission capacity, and backpressure |
| Unauthenticated peer reaching a Runner data-plane listener | No admission without PostgreSQL | Framed single-use credential before any payload byte, bounded handshake time and message size, constant-time local rejection against assignment-bound session state, and credential consumption through the authenticated control connection |
| Leaked Runner data-plane address | Confined ingress surface | Exact `sandbox:ports:direct` operation scope denied by default, per-authority grant, no Runner address in any other response or in Port evidence, and an advertised value that carries no Sandbox identity |
| Direct Port connection outliving its authority | Single generation authority | Generation fence, Lease expiry, session deadline, operator drain, Instance termination, and control-connection loss each close every live socket for the affected session, with bounded proof of closure |

## Credential lifecycle

The platform token, PostgreSQL credential, pre-shared Runner credential, Runner mTLS keys, artifact-signing keys, and guest runtime credentials are separate secrets. The deployment bootstrap creates the Compose development credentials and Runner PKI. Operators issue Runner certificates out of band; the Runner CA private key is not mounted into the control plane.

Replacing the platform token requires coordinated caller and control-plane rollout. Replacing the Runner credential requires restarting the control plane and every trusted Runner so no old authenticated stream remains. A CA compromise additionally requires replacing the CA, server credential, and Runner certificates.

## Detection and response

Security-sensitive mutations produce audit rows. HTTP responses and asynchronous Operations carry request identifiers. Metrics avoid ownership labels. Data-plane session rows retain terminal state, correlation, admission replay, and Port acknowledgement state until session cleanup. Direct and proxied transports persist no payload frames, so reconstructing Port bytes is outside the forensic boundary. An incident preserves audit and bounded logs, replaces affected credentials, fences affected Runner assignments, and recovers affected Sandboxes only by restoring the same stable Runner identity and its Workspace root from a trusted consistent backup. Without that backup, those Sandboxes and local Snapshots are lost.

See [security model](security.md), [service boundaries](service-boundaries.md), [profiles and authorization](profiles-and-authorization.md), [networking and ports](networking-and-ports.md), and [deployment operations](../operations/deployment.md).
