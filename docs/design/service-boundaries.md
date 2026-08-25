# Service boundaries

SecondBox is a self-hostable network service for durable isolated Sandboxes. The public durable resource is a `Sandbox`; Firecracker compute is a replaceable `Instance` attached to one fenced Sandbox generation.

## Components

The unprivileged control plane serves the HTTP API, validates the platform token and trusted ownership assertions, resolves immutable profile revisions, persists desired state, schedules work, and reconciles failures. PostgreSQL is its authority for ownership refs, desired state, generations, home assignments, leases, idempotency, audit, and operation state. It never stores Workspace or Snapshot images.

The runner is a separately deployed privileged Go process on a qualified Linux host. It establishes an outbound mutually authenticated connection to the control plane, advertises verified capacity, and owns Firecracker, KVM, jailer, cgroups, network namespaces, TUN/TAP devices, its reflink-capable WorkspaceStore, and process cleanup. A runner accepts only fully resolved assignments and local-workspace commands addressed to its authenticated stable identity. It does not resolve profiles, authenticate HTTP callers, or choose ownership policy.

The guest agent runs inside each released Firecracker image. It performs bounded command, filesystem, PTY, activity, and port operations for the runner over the independently versioned guest protocol. It has no control-plane database credentials.

## Trust and network boundaries

The control plane runs without KVM, TUN, host cgroups, host paths, container-engine access, or added Linux capabilities. It may run in Compose, Kubernetes, or another ordinary container platform. Kubernetes deployment manifests and Kubernetes-native sandbox backends are outside the v1 supported surface.

Runners dial the control plane. Direct data-plane clients also require reachability to the admitted runner listener; proxied clients do not. Runner identity comes from the CA-signed client certificate and matching deployment-wide Runner credential, not from a claimed message field. The HTTP platform token and Runner credential are separate authorities and are never interchangeable.

The public API uses provider-neutral terms. Responses never contain Firecracker configuration, KVM state, runner identities, host paths, backend references, fencing tokens, or database row shapes. The control plane never discloses a runner address except to an authority holding the direct data-plane scope, for one admitted session, with the expected certificate SPKI SHA-256 pin. Runner administration uses the same trusted platform-token boundary as every other HTTP route.

## Ownership

- A trusted upstream system asserts an opaque tenant and subject for each request. SecondBox does not resolve those values or enforce the upstream relationship between them.
- The asserted `(tenant_ref, subject_ref)` owns Sandboxes, workspaces, snapshots, leases, operations, idempotency records, audit events, and quota usage.
- An operator owns platform-token distribution, profiles, profile revisions, runner pools, Runner certificates, and platform-wide retention and trust configuration. Data-plane retention supplies session, result, and idempotency deadlines.
- The control plane owns desired state and assignment authority.
- A Sandbox's current authoritative home runner owns its durable Workspace, local Snapshots, receipts, and current fenced compute process. Only the stopped-Sandbox relocation Operation may change that home.
- Application code owns Sandbox creation, reuse, stop, and deletion. Framework adapters do not own provider lifetime.

SecondBox does not own end-user identity, authorization, billing, an LLM runtime, application secrets, Git hosting, an IDE, or an Agent framework. The trusted caller maps users and workloads onto scoped Sandboxes. A bug or compromise in that caller can assert another subject; this is an accepted trust-boundary risk, not a protection SecondBox claims to provide.

## Supported v1 shape

Firecracker is the only compute backend. A single provider-neutral compute port and conformance suite preserve a clean internal seam, but v1 contains no placeholder adapters, capability claims, or fallback execution. The supported deployment is a Compose control plane with one or more same-host or remote Linux Firecracker runners.

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
