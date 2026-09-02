# Service boundaries

SecondBox is a self-hostable network service for durable isolated Sandboxes. The public durable resource is a `Sandbox`; compute — a Firecracker microVM on KVM hosts, a gVisor sentry sandbox on hosts without KVM, or a Microsandbox microVM on the experimental backend — is a replaceable `Instance` attached to one fenced Sandbox generation.

## Components

The unprivileged control plane serves the HTTP API, resolves the deployment-wide platform token and persisted tenant-controller and application authorities, resolves immutable profile revisions, persists desired state, schedules work, and reconciles failures. PostgreSQL is its authority for Tenants, Subjects, non-recoverable credential verifiers, ownership refs, quota, desired state, generations, home assignments, leases, idempotency, audit, cleanup, and operation state. It never stores Workspace or Snapshot images.

The runner is a separately deployed privileged process on a qualified host. It selects exactly one private compute backend at startup, establishes an outbound mutually authenticated connection to the control plane, advertises only locally revalidated materializations, and owns backend composition, its reflink-capable WorkspaceStore, and process cleanup. Firecracker serves KVM hosts and gVisor serves hosts without KVM; the experimental Microsandbox backend is qualified Linux-first. A runner accepts only fully resolved assignments and local-workspace commands addressed to its authenticated stable identity. It does not resolve profiles, authenticate HTTP callers, or choose ownership policy.

The guest agent runs inside each Instance: baked into the released Firecracker image and reached over vsock, or injected as a bind-mounted binary and reached over gofer-served Unix sockets on the gVisor backend. It performs bounded command, filesystem, PTY, activity, and port operations for the runner over the independently versioned guest protocol, with the same negotiated identity guarantees on either transport. It has no control-plane database credentials.

## Trust and network boundaries

The control plane runs without KVM, TUN, host cgroups, host paths, container-engine access, or added Linux capabilities. It may run in Compose, Kubernetes, or another ordinary container platform. Kubernetes deployment manifests and Kubernetes-native sandbox backends are outside the v1 supported surface.

Runners dial the control plane. Direct data-plane clients also require reachability to the admitted runner listener; proxied clients do not. Runner identity comes from the CA-signed client certificate and matching deployment-wide Runner credential, not from a claimed message field. The HTTP platform token and Runner credential are separate authorities and are never interchangeable.

The public API uses provider-neutral terms. Responses never contain Firecracker configuration, KVM state, runner identities, host paths, backend references, fencing tokens, or database row shapes. The control plane never discloses a runner address except to an authority holding the direct data-plane scope, for one admitted session, with the expected certificate SPKI SHA-256 pin. Runner administration uses the platform-token boundary. Persisted controller and application credentials are rejected from it.

## Ownership

- The platform operator creates each Tenant and delegates one or more fixed tenant-controller authorities. A controller creates Subjects and application authorities inside only its bound Tenant. An application request repeats the exact opaque tenant and subject references bound to its persisted credential; SecondBox rejects mismatches before route handling.
- The asserted `(tenant_ref, subject_ref)` owns Sandboxes, workspaces, snapshots, leases, operations, idempotency records, audit events, and quota usage.
- An operator owns platform-token distribution, profiles, profile revisions, runner pools, Runner certificates, and platform-wide retention and trust configuration. Data-plane retention supplies session, result, and idempotency deadlines.
- The control plane owns desired state and assignment authority.
- A Sandbox's current authoritative home runner owns its durable Workspace, local Snapshots, receipts, and current fenced compute process. Only the stopped-Sandbox relocation Operation may change that home.
- Application code owns Sandbox creation, reuse, stop, and deletion until its Subject is closed. Subject closure and expiry converge through one durable cleanup Operation that revokes application admission and removes the Subject's resources. Framework adapters do not own provider lifetime.

SecondBox does not own end-user identity, billing, an LLM runtime, application secrets, Git hosting, an IDE, or an Agent framework. The tenant controller maps users and workloads onto Subjects and distributes their scoped application credentials. Compromise of one application credential is limited to its stored Tenant, Subject, scopes, and Profile grants; compromise of a tenant controller reaches its whole Tenant; compromise of the platform token reaches the deployment.

## Supported v1 shape

Firecracker and gVisor are the supported v1 backends. They and the experimental Microsandbox backend use the same provider-neutral compute port with explicit runner-wide selection and no per-assignment selection or fallback. RunnerPools are homogeneous and privately sealed to one backend kind; backend identity never enters public resources.

## Future smolvm adapter contract

No smolvm backend or conditional branch exists in v1. Its current high-level
`MachineSpec` owns creation of `storage.raw` and `overlay.raw`, while its guest
layout assumes the root and workspace devices are `/dev/vda` and `/dev/vdb`.
That is not compatible with SecondBox's WorkspaceStore authority.

A future smolvm adapter must change that boundary before it can satisfy the
compute conformance suite. The adapter must accept the mandatory externally
supplied raw ext4 Workspace attachment and its generation/fence, attach that
exact image through `krun_add_disk2`, and preserve the exclusive writer lock
until all machine and host-side users have stopped. The guest must discover and
mount the Workspace by filesystem label or UUID at `/workspace`, rather than
depending on a fixed device number. The adapter may not ask `MachineSpec` to
create or replace Workspace storage and may not add a copy, overlay, or
provider-specific fallback.

See [Domain and lifecycle](domain-lifecycle.md), [Profiles and authorization](profiles-and-authorization.md), [Customer-shared tenancy](customer-shared-tenancy.md), [Runner protocol](runner-protocol.md), [Guest-agent protocol](guest-agent-protocol.md), and [Security](security.md).
