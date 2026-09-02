# Security model

Execution-asset identity in the shared runner protocol is provider-neutral. Firecracker admission still requires its independently configured local trust anchor and signed bundle verification. Microsandbox and gVisor admission require an exact locally revalidated materialization whose source OCI manifest and flat root are both content-addressed; the gVisor materialization additionally pins the `runsc` and guest-agent launch artifacts by digest. No backend may resolve mutable tags or implicitly pull during assignment start.

The gVisor backend is a distinct isolation class: sandboxes run on the gVisor sentry (a user-space kernel on the systrap platform) rather than a hardware virtual machine, so its guest-kernel attack surface is the sentry's syscall implementation instead of KVM. Workspace bytes keep host-mount provenance — the runner loop-attaches the same raw ext4 Workspace image inside a supervisor-private mount namespace and serves it to the sandbox through the gofer, so Workspace content never changes identity between backends. Where the runner runs as a privileged Kubernetes pod, the pod is trusted infrastructure exactly like a privileged runner host: `privileged: true` is the qualification boundary, sandboxes nest their cgroups inside the pod budget, and the sentry remains the isolation boundary for guest workloads. gVisor advertises no `snapshot_resume` capability.

SecondBox assumes guest workloads and network peers may be malicious. Its control plane is also a trusted subsystem. The platform token is deployment-wide, tenant-controller authorities are fixed to one Tenant, and application authorities are fixed to one Tenant, one Subject, exact Sandbox scopes, and explicit Profile grants. SecondBox does not authenticate end users or duplicate an upstream identity graph.

## Principals and authority

The HTTP API accepts the deployment-wide `SECONDBOX_PLATFORM_TOKEN` as its operator authority. Operators use it to establish Tenant ceilings, delegate tenant-controller authorities, mutate Profiles and RunnerPools, administer Runners, and inspect deployment-wide usage and timing. Platform-token application requests may also supply bounded opaque `X-SecondBox-Tenant-Ref` and `X-SecondBox-Subject-Ref` values. PostgreSQL queries scope owned resources, idempotency, quota, and audit records to both values.

Tenant-controller and application authorities use independent server-generated bearer credentials. PostgreSQL stores a public lookup identifier and one-way verifier, never recoverable token material, and resolves current state and expiry on every newly admitted request. A controller is fixed to one Tenant and has one code-owned management grant. Each application authority is fixed to tenant and subject references, exact Sandbox operation scopes, and an allowlist of Profile names. Header mismatch, administrative access, an ungranted Profile, or a missing scope is denied before the route handler. Revocation and expiry take effect without a process restart. Compromise of an application credential is subject-scoped, compromise of a controller is tenant-scoped, and compromise of the platform token remains deployment-wide compromise.

Runner authority is separate. A Runner establishes an outbound TLS 1.3 connection, presents a CA-signed client identity, and proves the deployment-wide pre-shared Runner credential. The control plane compares the certificate identity, configured Runner identity, and protocol identity. HTTP tokens are never accepted on this channel, and Runner credentials are never accepted by the HTTP API.

Browser-facing PTY and port-tunnel connections do not rely on caller-supplied tenancy. They use single-use, session-bound, expiring HMAC capability tokens carried in the WebSocket subprotocol. Generation, Lease, Assignment, and attachment checks still apply at admission.

One Port operation scope is a capability rather than an operation permission. `sandbox:ports:direct` grants the direct Port transport, whose endpoint names the home Runner's advertised data-plane address. It is denied by default, is never implied by `sandbox:ports`, and is the only way any caller learns a Runner address; every other authority receives the proxied WebSocket endpoint. The same single-use capability token authenticates either transport, and on the direct transport the home Runner rejects a mismatch locally in constant time before spending the token against PostgreSQL, which remains the single consumption authority.

## Control-plane boundary

The control plane has database authority and can schedule Runners, so compromise is severe. It remains unprivileged and has no KVM, TUN/TAP, host cgroups, host paths, container-engine socket, Runner private keys, or Runner shell access. The Runner CA private key stays outside the control-plane deployment.

Idempotency, two-level Tenant and Subject quota reservation, generation checks, ownership checks, and optimistic concurrency are transactional. A stale Lease, stream, Instance, Assignment, or reconciliation claim cannot mutate a newer generation. Subject closure and expiry create one restart-safe cleanup Operation whose Runner workspace removal must be acknowledged. Audit records capture security-sensitive mutations and denials without storing credentials or workspace content.

## Runner and guest boundary

A Runner is privileged on its host and can observe active guest memory and every Workspace homed there. Runner pools are explicit trust and placement boundaries. A Runner receives only fully resolved assignments and logical local-workspace commands for its stable identity. It does not receive the platform token or PostgreSQL credentials.

Firecracker, jailer, cgroups, namespaces, minimal devices, signed images, and a narrow guest protocol provide defense in depth. Guest paths resolve beneath descriptor-pinned workspace roots. Resource, deadline, payload, transfer, and output bounds apply at admission and execution. Guest output, filenames, log text, and protocol errors are untrusted and bounded.

Control-plane fencing prevents a stale Runner from committing authoritative state for a newer generation. It cannot remediate a compromised or lost home Runner. Recovery requires a trusted consistent backup of that Runner's stable identity and Workspace root; the control plane never automatically relocates or reconstructs its Sandboxes on a fresh Runner. Operator relocation requires the intact source Runner to seal and stream the stopped Workspace.

## Durable bytes and recovery

Workspace durability is local to one authoritative home Runner at a time. The WorkspaceStore
uses reflink-only cloning, atomic manifests, fsync, exclusive writer locks, and
durable operation receipts. Except for the bounded operator-initiated stopped-Sandbox relocation stream, the runner protocol never transports image bytes or
paths. Loss of an unbacked home-Runner filesystem loses its Sandboxes and local
Snapshots; PostgreSQL recovery alone is insufficient.

All work is deadline- and size-bounded. Tenant aggregate and Subject quotas protect shared control-plane and Runner capacity. Backpressure prevents slow clients from creating unbounded output buffers. Database, Runner, and guest failures produce explicit state rather than fallback execution or empty-data success. On a direct Port connection, backpressure is TCP flow control on the caller leg and the retained guest-protocol credit window on the guest leg, so no unbounded buffer exists on either.

Port evidence is transport independent. Both transports keep payload-free session accounting, terminal state, correlation, acknowledgement state, and admission replay until the session deadline. Both emit fixed-shape Runner evidence at admitted open and close, neither persists per-frame payloads, and payload reconstruction is outside the forensic boundary. No Port evidence record can contain a payload byte, a credential, a fencing token, or a Runner address.

See [Service boundaries](service-boundaries.md), [Profiles and authorization](profiles-and-authorization.md), [Runner protocol](runner-protocol.md), and [Recovery and reconciliation](recovery-and-reconciliation.md).
