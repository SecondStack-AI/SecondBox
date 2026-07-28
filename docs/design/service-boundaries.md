# Service boundaries

SecondBox is a self-hostable network service for durable isolated Sandboxes. The public durable resource is a `Sandbox`; Firecracker compute is a replaceable `Instance` attached to one fenced Sandbox generation.

## Components

The unprivileged control plane serves the public and administrative HTTP API, authenticates application service accounts, resolves immutable profile revisions, persists desired state, schedules work, and reconciles failures. PostgreSQL is its authority for identity, desired state, generations, assignments, leases, idempotency, audit, and operation state. S3-compatible object storage is its authority for immutable workspace checkpoints, snapshots, artifacts, and released execution assets.

The runner is a separately deployed privileged Go process on a qualified Linux host. It establishes an outbound mutually authenticated connection to the control plane, advertises verified capacity, and owns Firecracker, KVM, jailer, cgroups, network namespaces, TUN/TAP devices, runner-local workspace materializations, and process cleanup. A runner accepts only fully resolved assignments. It does not resolve profiles, authenticate application API keys, or choose tenant policy.

The guest agent runs inside each released Firecracker image. It performs bounded command, filesystem, PTY, activity, and port operations for the runner over the independently versioned guest protocol. It has no control-plane database or object-store credentials.

## Trust and network boundaries

The control plane runs without KVM, TUN, host cgroups, host paths, container-engine access, or added Linux capabilities. It may run in Compose, Kubernetes, or another ordinary container platform. Kubernetes deployment manifests and Kubernetes-native sandbox backends are outside the v1 supported surface.

Runners dial the control plane; the control plane does not require inbound access to runner hosts. Runner identity comes from the authenticated runner credential, not from a claimed message field. Application API keys and runner credentials are separate authorities and are never interchangeable.

The public API uses provider-neutral terms. Application responses never contain Firecracker configuration, KVM state, runner addresses or identities, host paths, backend references, fencing tokens, database row shapes, or object-store keys. Administrative runner projections expose operational identity and capacity only to runner-administration scopes.

## Ownership

- A `Project` owns application service accounts, API keys, Sandboxes, workspaces, snapshots, artifacts, and quota usage.
- An operator owns profiles, profile revisions, runner pools, runner enrollment, and platform-wide retention and trust configuration.
- The control plane owns desired state and assignment authority.
- A runner owns the active materialization and compute process for its current fenced assignment.
- Object storage owns portable immutable bytes; runner-local storage is an active cache.
- Application code owns Sandbox creation, reuse, stop, and deletion. Framework adapters do not own provider lifetime.

SecondBox does not own end-user identity, billing, an LLM runtime, application secrets, Git hosting, an IDE, or an Agent framework. Applications map their users and workloads onto project-scoped Sandboxes without asserting arbitrary tenant identities.

## Supported v1 shape

Firecracker is the only compute backend. A single provider-neutral compute port and conformance suite preserve a clean internal seam, but v1 contains no placeholder adapters, capability claims, or fallback execution. The supported deployment is a Compose control plane with one or more same-host or remote Linux Firecracker runners.

See [Domain and lifecycle](domain-lifecycle.md), [Profiles and authorization](profiles-and-authorization.md), [Runner protocol](runner-protocol.md), [Guest-agent protocol](guest-agent-protocol.md), and [Security](security.md).
