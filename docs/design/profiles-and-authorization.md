# Profiles and authorization

Profiles are server-owned policy. Application clients select an authorized profile name; they do not assemble compute policy in a Sandbox request.

## HTTP authorities

The deployment-wide `SECONDBOX_PLATFORM_TOKEN` is the operator authority. It may call administrative and application routes while asserting any bounded opaque `X-SecondBox-Tenant-Ref` and `X-SecondBox-Subject-Ref` values. SecondBox stores and compares both references on every owned read and write. Operators keep this credential out of application services.

`SECONDBOX_APPLICATION_AUTHORITIES_JSON` explicitly provisions zero or more application authorities. Each entry has a unique ID and token, one fixed tenant reference, one fixed subject reference, one or more exact Sandbox operation scopes, and one or more Profile grants. An application request must present the bound references exactly. It cannot call Profile mutation, Runner administration, or aggregate timing routes; it can read only granted Profiles and can create Sandboxes only from them. Owned resource queries remain restricted to its bound tenant and subject.

Supported application scopes are `sandbox:read`, `sandbox:lifecycle`, `sandbox:exec`, `sandbox:files`, `sandbox:ports`, and `sandbox:ports:direct`. Unknown routes and missing scopes fail closed.

`sandbox:ports:direct` grants no route of its own. It selects the direct Port transport for an authority that already holds `sandbox:ports`, and it is the only grant through which any caller learns a Runner data-plane address. It is denied by default and is never implied by `sandbox:ports`; an authority without it receives the proxied WebSocket endpoint. See [Networking and ports](networking-and-ports.md). Tokens must be unique and distinct from the platform token. The Runner channel remains separate and requires the pre-shared Runner credential plus a CA-signed mTLS identity.

## Profile and revision

A Profile is a stable operator-chosen name with an enabled state and current revision. Creation and each revision operation produce an immutable ProfileRevision. Updating the Profile only changes the revision selected by future Sandbox creation. Existing Sandboxes remain pinned; there is no silent migration.

Every ProfileRevision contains:

- RunnerPool selector and required architecture/capability set;
- immutable runtime and toolchain component-manifest digests bound by one signed execution-bundle manifest; the runtime component covers the kernel, rootfs, and guest agent, while the toolchain component covers the shared tool payload and its locked provenance;
- vCPU, memory, workspace disk, process, and concurrent-operation limits;
- the startup mode every Instance uses, explicitly `cold_boot` or `snapshot_resume`;
- exec deadline, buffered output, streaming window, transfer, PTY, and port-session bounds;
- drain grace, idle timeout, maximum Instance duration, lease duration, and desired create state;
- Snapshot count and retention;
- outbound network and DNS policy;
- approved exposed ports, protocols, and session limits.

SecondBox releases two explicitly selected standard Profile bundles:

- `agent-compartment` is bounded ephemeral compute for Flue-style agent turns. It starts immediately, has short idle and maximum-duration bounds, and exposes no ports.
- `durable-coding` is a long-running coding workspace with larger inline CPU, memory, disk, process, operation, transfer, PTY-detach, Snapshot, and development-port bounds.

The declarative resource engine materializes standard bundles as ordinary immutable ProfileRevisions. Selection is explicit in `[standard_resources]`; the control plane has no built-in defaults, reserved-name behavior, or request-time reconciler. Each release declares the complete ordered lineage and canonical spec digest, validates an installed prefix, and appends only missing revisions. Existing Sandboxes retain the exact earlier revision they pinned. Operator-defined Profiles remain fully supported and follow the same immutable pinning rules.

Ordinary stop always flushes and detaches compute, advances the local Workspace manifest generation, and preserves every committed Workspace write without creating a Snapshot or transferring Workspace bytes off the Runner. A later start resolves that same current image on the current home Runner; it never adopts a newer Profile head. Operator relocation preserves the pinned ProfileRevision and validates its compatibility requirements against the target.

## Creation and compatibility

`POST /v1/sandboxes` contains only `profile` and bounded string metadata. The client supplies `Idempotency-Key` as a header. Resource, backend, image, lifecycle, storage, network, timeout, port, and placement fields are rejected as unknown properties.

Creation fails before allocating durable intent when the profile is absent, disabled, or has no RunnerPool capable of its immutable requirements. Successful creation persists the exact ProfileRevision ID and a resolved compatibility summary. Later runner availability changes do not rewrite that selection.

Profiles may be disabled to stop future creation. Disablement does not mutate pinned Sandboxes. A profile revision and its referenced assets cannot be deleted while reachable from a Sandbox or retention record.

## Startup mode

`startup.mode` is required on every ProfileRevision and has no application default. `cold_boot` starts a Sandbox by booting its guest. `snapshot_resume` starts it by resuming a prepared, identity-neutral guest, and it never falls back to `cold_boot`: an Instance that cannot be resumed fails, it does not boot.

The mode is a placement requirement, not a hint. A `snapshot_resume` ProfileRevision admits only onto Runners advertising the provider-neutral `snapshot-resume` capability, at initial home placement and at every later Instance assignment onto that same home Runner. A Runner advertises the capability only when it is configured with a resume template cache root, requires the jailer, and already holds a template built from the exact signed execution bundle it verified. A pool whose operator-declared capabilities omit `snapshot-resume` refuses a `snapshot_resume` Profile non-retryably with `startup_mode_unsupported`, because no Runner in it will ever be admissible; a declared pool with no currently advertising Runner refuses retryably, because a Runner may materialize the template.

Revisions recorded before the field existed are stamped `cold_boot` by migration `0015_profile_startup_mode`. That is a statement of the behavior they already had, not a default invented for them, and it keeps an upgraded database converging on exactly the spec a fresh one writes.

## Quotas

`subject_quotas` is the only persisted quota set. It covers total Sandboxes, active Instances, vCPU, memory, Snapshots, exposed-port sessions, and concurrent data-plane operations for the asserted tenant and subject. Workspace and Snapshot filesystem allocation is governed by Runner storage-pressure admission rather than charged as uniquely retained bytes. Profile resource limits, including standard Profile limits, remain inline immutable execution policy rather than a second quota table. Admission and quota reservation are transactional. A concurrent race either commits one authorized reservation or returns a typed quota error; it never overcommits and repairs later.

Metrics use fixed-cardinality labels. Tenant refs, subject refs, Sandbox IDs, profile names, and workspace paths are audit fields rather than metric dimensions.

See [Domain and lifecycle](domain-lifecycle.md), [API conventions](api-conventions.md), and [Security](security.md).
Customer-shared tenant, delegated authority, aggregate quota, subject cleanup, and network-isolation behavior is defined in [customer-shared tenancy](customer-shared-tenancy.md).
