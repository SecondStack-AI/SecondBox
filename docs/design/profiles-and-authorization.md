# Profiles and authorization

Profiles are server-owned policy. Application clients select an authorized profile name; they do not assemble compute policy in a Sandbox request.

## Trusted-caller model

Every HTTP request authenticates with the deployment-wide `SECONDBOX_PLATFORM_TOKEN` and carries `X-SecondBox-Tenant-Ref` plus `X-SecondBox-Subject-Ref`. Both references are bounded opaque strings. SecondBox stores and compares them on every owned read and write, but does not verify that the caller is entitled to assert them.

The upstream platform is therefore the authorization boundary. A bad assertion can cross subjects; SecondBox deliberately accepts that risk to avoid duplicating the upstream identity graph. The Runner channel remains separate and requires the pre-shared Runner credential plus a CA-signed mTLS identity.

## Profile and revision

A Profile is a stable operator-chosen name with an enabled state and current revision. Creation and each revision operation produce an immutable ProfileRevision. Updating the Profile only changes the revision selected by future Sandbox creation. Existing Sandboxes remain pinned; there is no silent migration.

Every ProfileRevision contains:

- RunnerPool selector and required architecture/capability set;
- immutable runtime and toolchain component-manifest digests bound by one signed execution-bundle manifest; the runtime component covers the kernel, rootfs, and guest agent, while the toolchain component covers the shared tool payload and its locked provenance;
- vCPU, memory, workspace disk, process, and concurrent-operation limits;
- exec deadline, buffered output, streaming window, transfer, PTY, and port-session bounds;
- drain grace, idle timeout, maximum Instance duration, lease duration, and desired create state;
- Snapshot count and retention plus Artifact count, byte, and retention policy;
- outbound network and DNS policy;
- approved exposed ports, protocols, and session limits.

SecondBox ships two versioned built-in Profiles:

- `agent-compartment` is bounded ephemeral compute for Flue-style agent turns. It starts immediately, has short idle and maximum-duration bounds, and exposes no ports.
- `coding-environment` is a long-running coding workspace with larger inline CPU, memory, disk, process, operation, transfer, PTY-detach, Snapshot, and development-port bounds.

Built-ins are materialized as ordinary immutable ProfileRevisions with deterministic version IDs when first resolved. Their names are reserved: operators cannot create, revise, or disable them. A later SecondBox release may advance a built-in head, but existing Sandboxes retain the exact earlier revision they pinned. Operator-defined Profiles remain fully supported and follow the same immutable pinning rules. There is no missing-profile fallback and no other profile name triggers application-specific behavior.

Ordinary stop always flushes and detaches compute, advances the local Workspace manifest generation, and preserves every committed Workspace write without creating a Snapshot or contacting object storage. A later start resolves that same current image on the immutable home Runner; it never adopts a newer Profile head.

## Creation and compatibility

`POST /v1/sandboxes` contains only `profile` and bounded string metadata. The client supplies `Idempotency-Key` as a header. Resource, backend, image, lifecycle, storage, network, timeout, port, and placement fields are rejected as unknown properties.

Creation fails before allocating durable intent when the profile is absent, disabled, or has no RunnerPool capable of its immutable requirements. Successful creation persists the exact ProfileRevision ID and a resolved compatibility summary. Later runner availability changes do not rewrite that selection.

Profiles may be disabled to stop future creation. Disablement does not mutate pinned Sandboxes. A profile revision and its referenced assets cannot be deleted while reachable from a Sandbox or retention record.

## Quotas

`subject_quotas` is the only persisted quota set. It covers total Sandboxes, active Instances, vCPU, memory, Artifact bytes, Snapshots, Artifacts, exposed-port sessions, and concurrent data-plane operations for the asserted tenant and subject. Workspace and Snapshot filesystem allocation is governed by Runner storage-pressure admission rather than charged as uniquely retained bytes. Profile resource limits, including the built-ins' limits, remain inline immutable execution policy rather than a second quota table. Admission and quota reservation are transactional. A concurrent race either commits one authorized reservation or returns a typed quota error; it never overcommits and repairs later.

Metrics use fixed-cardinality labels. Tenant refs, subject refs, Sandbox IDs, profile names, workspace paths, and artifact names are audit fields rather than metric dimensions.

See [Domain and lifecycle](domain-lifecycle.md), [API conventions](api-conventions.md), and [Security](security.md).
