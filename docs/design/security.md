# Security model

SecondBox assumes guest workloads and network peers may be malicious. Its control plane is also a trusted subsystem: callers holding the platform token may assert any tenant and subject reference. SecondBox preserves strict row scoping for those assertions, but it does not claim to authenticate end users or defend one subject from a compromised trusted caller.

## Principals and authority

The HTTP API accepts one deployment-wide `SECONDBOX_PLATFORM_TOKEN`. Every request also supplies bounded opaque `X-SecondBox-Tenant-Ref` and `X-SecondBox-Subject-Ref` values. PostgreSQL queries scope owned resources, idempotency, quota, and audit records to both values. The upstream platform must authorize those assertions before calling SecondBox.

Runner authority is separate. A Runner establishes an outbound TLS 1.3 connection, presents a CA-signed client identity, and proves the deployment-wide pre-shared Runner credential. The control plane compares the certificate identity, configured Runner identity, and protocol identity. HTTP tokens are never accepted on this channel, and Runner credentials are never accepted by the HTTP API.

Browser-facing PTY and port-tunnel connections do not rely on caller-supplied tenancy. They use single-use, session-bound, expiring HMAC capability tokens carried in the WebSocket subprotocol. Generation, Lease, Assignment, and attachment checks still apply at admission.

## Control-plane boundary

The control plane has database and object-store authority and can schedule Runners, so compromise is severe. It remains unprivileged and has no KVM, TUN/TAP, host cgroups, host paths, container-engine socket, Runner private keys, or Runner shell access. The Runner CA private key stays outside the control-plane deployment.

Idempotency, subject quota reservation, generation checks, ownership checks, and optimistic concurrency are transactional. A stale Lease, stream, Instance, Assignment, or reconciliation claim cannot mutate a newer generation. Audit records capture security-sensitive mutations without storing credentials or workspace content.

## Runner and guest boundary

A Runner is privileged on its host and can observe active guest memory and every Workspace homed there. Runner pools are explicit trust and placement boundaries. A Runner receives only fully resolved assignments and logical local-workspace commands for its stable identity. It does not receive the platform token, PostgreSQL credentials, or global object-store credentials.

Firecracker, jailer, cgroups, namespaces, minimal devices, signed images, and a narrow guest protocol provide defense in depth. Guest paths resolve beneath descriptor-pinned workspace roots. Resource, deadline, payload, transfer, and output bounds apply at admission and execution. Guest output, filenames, log text, and protocol errors are untrusted and bounded.

Control-plane fencing prevents a stale Runner from committing authoritative state for a newer generation. It cannot remediate a compromised or lost home Runner. Recovery requires a trusted consistent backup of that Runner's stable identity and Workspace root; the control plane never relocates or reconstructs its Sandboxes on a fresh Runner.

## Durable bytes and recovery

Artifact publication uses immutable keys, declared size and SHA-256 evidence, verified object reads, atomic metadata publication, retention, and two-phase garbage collection. Uploads spool and hash before durable admission; downloads are fully integrity-verified before response bytes are exposed. Missing or corrupt reachable bytes fail explicitly.

Workspace durability is local to one immutable home Runner. The WorkspaceStore
uses reflink-only cloning, atomic manifests, fsync, exclusive writer locks, and
durable operation receipts. The runner protocol never transports image bytes or
paths. Loss of an unbacked home-Runner filesystem loses its Sandboxes and local
Snapshots; PostgreSQL or S3 recovery alone is insufficient.

All work is deadline- and size-bounded. Per-subject quotas protect shared control-plane and Runner capacity. Backpressure prevents slow clients from creating unbounded output buffers. Database, object-store, Runner, and guest failures produce explicit state rather than fallback execution or empty-data success.

See [Service boundaries](service-boundaries.md), [Profiles and authorization](profiles-and-authorization.md), [Runner protocol](runner-protocol.md), and [Recovery and reconciliation](recovery-and-reconciliation.md).
