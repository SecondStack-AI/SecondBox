# Security model

SecondBox assumes application clients and guest workloads may be malicious. It separates application tenancy, unprivileged orchestration, privileged execution, and durable bytes so compromise of one boundary does not silently grant every other authority.

## Assets and principals

Protected assets include API and runner credentials, profile and assignment authority, workspace and artifact content, released execution assets, audit evidence, control-plane availability, Runner host integrity, and other Projects' existence and data.

Runner operation evidence is structurally payload-free. Assignment, Exec, File, Port, checkpoint, restore, fence, network-failure, and teardown terminals use one fixed record correlated by request, operation, Sandbox, Instance, generation, Assignment, optional Lease, and Runner IDs. The evidence types cannot represent fencing tokens, credentials, commands, environment values, byte streams, file paths/content, checksums, network destinations, or workspace data. Definitive terminal paths fail hard if their local evidence cannot be recorded. Correlation IDs belong in audit evidence rather than metric labels.

Principals are bootstrap and ongoing operators, project-scoped ServiceAccounts, control-plane replicas, enrolled Runners, guest agents tied to one Instance generation, and external object-store/PostgreSQL services. End users are application concepts and are not SecondBox principals.

## Application tenancy

Authentication derives exactly one Project. Authorization checks scope and Project ownership before lookup results are disclosed, preventing cross-Project enumeration as well as mutation. Metadata is bounded and treated as untrusted display data. API key hashes, prefixes, rotation, revocation, expiry, and last-use evidence are stored separately from plaintext.

Idempotency, quota reservation, generation checks, and optimistic concurrency are transactional. A stale Lease, stream, Instance, or Assignment cannot mutate a newer generation. Audit records capture denied and accepted security-sensitive operations without storing secrets or workspace content.

## Control-plane compromise

The control plane has database and object-store authority and can schedule Runners, so compromise is severe. Its process remains unprivileged and has no KVM, TUN, host cgroups, host paths, container-engine socket, or Runner shell access. Runner mTLS credentials and application API keys are distinct. Short-lived credentials, rotation, least-privilege database/object-store roles, immutable release artifacts, and fixed outbound dependencies limit persistence and lateral movement.

## Runner compromise

A Runner is privileged on its host and can observe active guest memory and its local workspace cache. Runner pools are therefore explicit trust and placement boundaries. A Runner receives only assignments for its pool, resolved policy, and the minimum object access needed for those assignments. It does not receive application API keys or global object-store credentials.

Control-plane fencing prevents a compromised or stale Runner from committing authoritative state for a newer generation. It cannot erase the need for host remediation: a compromised Runner is drained, its credential revoked, its assignments fenced, and affected Sandboxes restored from the last trusted durable checkpoint on fresh hosts.

## Malicious guest

Firecracker, jailer, cgroups, namespaces, minimal devices, signed images, and a narrow guest protocol form defense in depth. Guest paths are resolved beneath descriptor-pinned workspace roots. Resource and output bounds apply at admission and execution. Guest traffic follows [Networking and ports](networking-and-ports.md).

The guest receives no database, object-store, runner enrollment, application API-key, or general provider secret. V1 does not inject model or application credentials. Guest output, filenames, log text, and protocol errors are untrusted and bounded before logging or returning to clients.

## Supply chain

Profiles reference immutable signed kernel, rootfs, guest-agent, and toolchain bundles. Runners verify signature, trusted-key fingerprint, checksum manifest, architecture, Firecracker compatibility, and guest-protocol metadata before advertising or using an asset. Mutable tags, unsigned local substitution, and arbitrary runtime pulls do not satisfy assignments.

Release artifacts include checksums, signatures, SBOMs, provenance, license notices, vulnerability results, and dependency-age evidence. Trust-anchor rotation is explicit and audited; overlapping trust is bounded and old keys are revocable.

## Availability and recovery

All work is deadline- and size-bounded. Per-Project and per-profile quotas protect shared control-plane and Runner capacity. Backpressure prevents slow clients from creating unbounded output buffers. Database, object-store, Runner, and guest failures produce explicit state rather than fallback execution or empty-data success.

Security incidents use correlation across request, operation, Project, Sandbox, Instance, generation, Assignment, Lease, and Runner. Support bundles are bounded, redact credentials and workspace content, and require an administrative scope.

See [Service boundaries](service-boundaries.md), [Profiles and authorization](profiles-and-authorization.md), [Runner protocol](runner-protocol.md), and [Recovery and reconciliation](recovery-and-reconciliation.md).
