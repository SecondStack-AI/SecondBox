# Profiles and authorization

Profile resources state integer `vcpuCount`, guest memory, Workspace capacity, concurrency, duration, and output bounds. They make no universal CPU-share or guest PID-enforcement promise; those controls remain backend-specific. Profiles select a homogeneous RunnerPool but contain no backend kind.

Profiles are server-owned policy. Application clients select an authorized
Profile name and may request Sandbox size within its policy, Tenant/Subject
quota, and Runner admission. Image, network, ports, execution bounds, and
startup mode remain owned by the immutable Profile revision.

`resources` supplies defaults for vCPU, memory, and Workspace capacity. An
absent `resourceCeiling` makes those values the size ceiling as well. A present
object must explicitly contain `vcpuCount`, `memoryBytes`, and `workspaceBytes`;
each is an integer at least its matching default or `null`, meaning the
Profile imposes no bound on that axis. Quota and Runner admission still apply.
Standard bundle lineages are unchanged and omit this object. The operator
[flexible example](../../examples/resources/durable-coding-flexible.json)
leaves CPU and memory unbounded and caps Workspace capacity at 256 GiB.

A requested `workspaceBytes` rounds up to a power of two before checking its
ceiling, except that a request at or below a bounded ceiling resolves to the
ceiling itself when rounding would exceed it, so rounding never refuses a
request that fits; omitted axes retain the Profile default unchanged. vCPU
remains a whole integer within schema minimums. Memory and Workspace
requests, Profile defaults, and finite ceilings must use whole MiB (multiples
of 1,048,576 bytes). Unaligned requests return `invalid_request` with a field
detail before disk rounding, durable intent, or quota allocation. The Sandbox
pins and reports the resolved allocation; quota, home selection, Workspace commands,
and Instance assignments use those pinned values. A finite ceiling refusal
returns `resources_exceed_profile`, with unbounded axes omitted from its
`ceiling` details. Runner startup prewarms power-of-two ext4 templates from
its supported 64 MiB creation floor through its configured maximum Workspace
capacity, plus the exact maximum; existing template validation remains exact.
The public schema's 1 MiB minimum is lower than the runner's supported
creation floor; requests below that floor still cannot create a Workspace.

A `snapshot_resume` revision rejects `resourceCeiling` at publication because
resume identity includes compute size. Creation accepts only the exact
Profile defaults (omitted or explicitly equal, without disk rounding).
Any changed axis returns `resources_fixed_by_profile` with the fixed size.

## HTTP authorities

The deployment-wide `SECONDBOX_PLATFORM_TOKEN` is the operator authority. It creates and manages Tenants, tenant-controller authorities, Profiles, RunnerPools, and Runners, and it may call application routes while asserting any bounded opaque `X-SecondBox-Tenant-Ref` and `X-SecondBox-Subject-Ref` values. SecondBox stores and compares both references on every owned read and write. Operators keep this credential out of application services.

A persisted tenant-controller authority has one server-generated bearer credential and is fixed to one Tenant. It manages that Tenant's Subjects, application authorities, and usage projection through the management routes. It cannot administer another Tenant, Profiles, RunnerPools, or Runners, and it cannot call ordinary Sandbox routes. The controller credential is returned only after successful creation or rotation; PostgreSQL retains only its public lookup identifier and one-way verifier.

A persisted application authority has one server-generated bearer credential, a unique ID, one fixed tenant reference, one fixed subject reference, one or more exact Sandbox operation scopes, and one or more Profile grants. An application request must present the bound references exactly. It cannot call platform or tenant management, Profile mutation, Runner administration, deployment usage, or aggregate timing routes; it can read only granted Profiles and can create Sandboxes only from them. Owned resource queries remain restricted to its bound tenant and subject. A Profile grant is a capability boundary, because a revision carries network egress, attributed execution, approved ports, and resources: every data-plane request that names a Sandbox (`sandbox:exec`, `sandbox:files`, and `sandbox:ports` routes) also requires that Sandbox's Profile to be granted, and fails with `authorization_denied` otherwise. This covers a Sandbox the platform or another authority placed in the same subject on another Profile. Reads, listing, Leases, and lifecycle remain subject-scoped, so an authority can still see, stop, and delete every Sandbox in its subject. Application credential creation and rotation also return the bearer token once and persist only its lookup identifier and verifier.

Supported application scopes are `sandbox:read`, `sandbox:lifecycle`, `sandbox:exec`, `sandbox:files`, `sandbox:ports`, and `sandbox:ports:direct`. Unknown routes and missing scopes fail closed.

`sandbox:ports:direct` grants no route of its own. It selects the direct Port transport for an authority that already holds `sandbox:ports`, and it is the only grant through which any caller learns a Runner data-plane address. It is denied by default and is never implied by `sandbox:ports`; an authority without it receives the proxied WebSocket endpoint. See [Networking and ports](networking-and-ports.md). Tokens must be unique and distinct from the platform token. The Runner channel remains separate and requires the pre-shared Runner credential plus a CA-signed mTLS identity.

### Extending grants

Tenant ceilings and application authority grants are add-only after creation, so an existing installation can adopt a new Profile or scope such as `sandbox:ports` without re-creating authorities or rotating credentials held by other systems.

- `POST /v1/tenants/{tenantRef}:extend-ceiling` (`extendTenantCeiling`) is a platform operation, authorized like `createTenant`. Its body `{"profileGrants": [...], "applicationScopes": [...]}` requires both lists; at least one must be non-empty.
- `POST /v1/application-authorities/{authorityId}:extend` (`extendApplicationAuthority`) is a tenant-controller operation, authorized like `createApplicationAuthority` and limited to the controller's Tenant. Its body `{"profileGrants": [...], "scopes": [...]}` follows the same rules. The authority keeps its bearer token and lookup identifier.

Entries are validated exactly as at creation: known scopes, valid Profile names, and no duplicates. The stored result is the sorted union of the current and requested entries; a Tenant ceiling holds at most 32 Profile grants. An application authority's resulting grants and scopes must remain subsets of its Tenant's current ceiling, or the request fails with `grant_escalation_denied` exactly as creation does. Revoked or expired authorities return `invalid_lifecycle_transition`. Because a ceiling extension never narrows the ceiling, existing authorities stay within it.

Both operations require `If-Match` with the current revision ETag (`"revision-N"`) and fail with `precondition_failed` when it is stale. They require an `Idempotency-Key`: an identical retry replays the stored response with `Idempotency-Replayed: true`, and reusing a key with a different body returns `idempotency_conflict`. A request whose entries are all already present succeeds and returns the resource unchanged, without a new revision or `updatedAt`. Every accepted request, including such a no-op, writes one `tenant.ceiling_extended` or `application_authority.extended` audit event with the requested entries; once the body and `If-Match` parse, entry validation, ceiling, lifecycle, revision, and idempotency-conflict failures write a denied event.

Narrowing is not supported. To remove a grant or scope from an application authority, revoke it and create a replacement. No operation narrows a Tenant ceiling. The CLI exposes these operations as `secondbox tenant extend-ceiling TENANT` and `secondbox application-authority extend AUTHORITY`, each taking repeatable `--profile-grant` and `--scope` flags plus `--revision` and `--idempotency-key`.

## Profile and revision

A Profile is a stable operator-chosen name with an enabled state and current revision. Creation and each revision operation produce an immutable ProfileRevision. Updating the Profile changes the revision selected by future Sandbox creation. Existing Sandboxes remain pinned. Only the attributed connection numeric default and ceiling follow the current head at each new Assignment, as described below.

Every ProfileRevision contains:

- RunnerPool selector and required architecture/capability set;
- immutable runtime and toolchain component-manifest digests bound by one signed execution-bundle manifest; the runtime component covers the kernel, rootfs, and guest agent, while the toolchain component covers the shared tool payload and its locked provenance;
- vCPU, memory, Workspace capacity, and concurrent-operation limits;
- the startup mode every Instance uses, explicitly `cold_boot` or `snapshot_resume`;
- exec deadline, buffered output, streaming window, transfer, PTY, and port-session bounds;
- drain grace, idle timeout, maximum Instance duration, lease duration, and desired create state;
- Snapshot count and retention;
- outbound network and DNS policy, including the required `network.requiresTenantEgressContext` Boolean;
- approved exposed ports, protocols, and session limits.

An optional `attributedExecution` block lets execs in ordinary generations of the Profile request attribution through a named installation gateway, and bounds each attributed exec's concurrent connections. It requires `network.requiresTenantEgressContext: true`; the gateway is a canonical logical name, never a caller-selected URL or host socket. This policy is separate from ordinary outbound destinations and does not change them.

Every Assignment of a permitting Profile carries the pinned gateway and the resolved connection limit, and requires a home Runner that advertises `per-exec-attribution`. Firecracker and gVisor advertise it only with configured attributed routing, and fail an Assignment whose pinned context has no route for the gateway. An attributed buffered or streaming exec supplies an application authorization reference and an absolute expiry within the Profile execution limit. It needs the same `sandbox:exec` scope and Profile grant as any exec; the binding is stored with the data-plane session and travels in the Runner exec request under the Sandbox's Tenant and Subject. Routing, window, and termination semantics are documented in [Networking and ports](networking-and-ports.md#attributed-execution).

The latest release-owned `agent-compartment` revision explicitly sets 128 simultaneous attributed TCP connections per exec and an `attributedExecutionCeiling.maximumConnections` of 4096. Historical revisions, including the two-connection revisions, remain immutable. Existing Sandboxes pinned to a permitting revision adopt the current numeric grant on their next Assignment; their pinned gateway and other authority do not change. The isolated Profile retains `deny_all` networking and no attributed permission. See [Configurable limits](configurable-limits.md#attributed-connection-policy) for delegation and fallback semantics.

Attribution never claims or retires a generation. Sequential and concurrent attributed execs, ordinary execs, PTYs, Ports, and file operations share one Instance, and each attributed exec has its own window and identity. While a window is open, any process in the Instance can use it; the owner accepts this residual risk because the guest never holds real credentials. See [Threat model](threat-model.md).

SecondBox releases three explicitly selected standard Profile bundles:

- `agent-compartment` starts compute for Flue-style agent turns, defaults to 60-second idle shutdown and unlimited maximum runtime, exposes no ports, and states `requiresTenantEgressContext: true`.
- `durable-coding` is a long-running coding workspace with larger inline CPU, memory, disk, operation, transfer, PTY-detach, Snapshot, and development-port bounds; it states `requiresTenantEgressContext: true`.
- `agent-compartment-isolated` uses the same Agent lifecycle defaults and bounded command, file, workspace, and cancellation capabilities while denying all outbound network and DNS access, exposing no ports, and stating `requiresTenantEgressContext: false`.

Both Agent bundles allow delegated finite or unlimited idle and runtime selections through
explicit null `lifecycleCeiling` dimensions. These settings affect new Sandboxes only;
existing Sandboxes keep their pinned policy. Unlimited runtime does not remove command
deadlines, cancellation, ownership checks, or idle shutdown.

The declarative resource engine materializes standard bundles as ordinary immutable ProfileRevisions. Selection is explicit in `[standard_resources]`; the control plane has no built-in defaults, reserved-name behavior, or request-time reconciler. Each release declares the complete ordered lineage and canonical spec digest, validates an installed prefix, and appends only missing revisions. Existing Sandboxes retain the exact earlier revision they pinned. Operator-defined Profiles remain fully supported and follow the same immutable pinning rules.

Tenant ceilings and application grants select these release-owned Profile names directly; they do not create tenant-specific Profile copies. The requirement is explicit policy, not an inference from `agent-gateway.secondbox.internal`, `platform-gateway.secondbox.internal`, or any other domain. Operator-defined Profiles must also state the Boolean on every revision. Omission is invalid; the control plane supplies no default and does not decode an old missing field into either value.

A Tenant has at most one nullable operator-controlled egress-context name. The name is an opaque 1-to-63-character lowercase ASCII label matching `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`; it is not a hostname, network, gateway identity, or mapping digest. A Sandbox created from a requiring Profile pins the Tenant's current name. Later Tenant changes apply only to new Sandboxes. A non-requiring Profile pins and sends no context even when its Tenant has one.

Ordinary stop always flushes and detaches compute, advances the local Workspace manifest generation, and preserves every committed Workspace write without creating a Snapshot or transferring Workspace bytes off the Runner. A later start resolves that same current image on the current home Runner; it never adopts a newer Profile head. Operator relocation preserves the pinned ProfileRevision and validates its compatibility requirements against the target.

## Creation and compatibility

`POST /v1/sandboxes` accepts `profile`, bounded string `metadata`, an optional
`sourceSnapshotId`, and optional `resources` axes (`vcpuCount`, `memoryBytes`,
`workspaceBytes`). The client supplies the `Idempotency-Key` header. Each
omitted axis uses the pinned ProfileRevision's default. Without
`resourceCeiling`, defaults also bound requests; with it, each explicit integer
bounds its axis and `null` removes the Profile bound. Tenant/Subject quota and
Runner admission still apply.

Memory and Workspace requests, Profile defaults, and finite ceilings must be
whole MiB. Invalid alignment returns `invalid_request` with a field detail
before allocation. Requested disk rounds up to a power of two, using the finite
ceiling instead if the original request fits but rounding would exceed it;
omitted disk keeps its exact default. The Sandbox pins and reports the resolved
immutable allocation. Quota accounting, Workspace capacity, placement, and
later Instance assignments read those pinned values. Requests beyond a finite
bound return `resources_exceed_profile`; `snapshot_resume` accepts only exact
Profile defaults without rounding and returns `resources_fixed_by_profile`
for a different size. Backend, image, lifecycle, storage, network, timeout,
port, and placement fields remain rejected as unknown properties.

Creation fails before allocating durable intent when the profile is absent, disabled, requires an egress context that the authenticated Tenant lacks, or has no RunnerPool capable of its immutable requirements. Successful creation persists the exact ProfileRevision ID, the immutable Tenant-context pin when required, and a resolved compatibility summary. Later Tenant or Runner availability changes do not rewrite that selection.

### Profile lifecycle

A Profile has two states, `enabled` and `disabled`, and three mutations:

- `POST /v1/profiles` creates it `enabled` with revision 1.
- `POST /v1/profiles/{name}:revise` appends an immutable revision under `If-Match` and makes it current. It does not check state: a disabled Profile can be revised and stays disabled.
- `POST /v1/profiles/{name}:disable` sets `disabled` under `If-Match`. Disabling an already disabled Profile changes nothing.

There is no enable, re-enable, or delete operation, so disabling is terminal and every Profile and ProfileRevision is retained. A disabled Profile refuses Sandbox creation, including from a Snapshot, with `409 profile_unavailable`. It also refuses Subject sandbox-policy selection, is skipped by image preparation, and makes `standard_resources` apply fail for that name rather than re-enable it. Existing Sandboxes keep their pinned revision: start, stop, restore, Snapshots, Leases, and data-plane requests do not consult Profile state.

## Startup mode

`startup.mode` is required on every ProfileRevision and has no application default. `cold_boot` starts a Sandbox by booting its guest. `snapshot_resume` starts it by resuming a prepared, identity-neutral guest, and it never falls back to `cold_boot`: an Instance that cannot be resumed fails, it does not boot.

Client-selected images currently support only `cold_boot` Firecracker assignments.
For `snapshot_resume` Profiles, omit the image on create and start to boot the Runner's installed execution bundle.

The mode is a placement requirement, not a hint. A `snapshot_resume` ProfileRevision admits only onto Runners advertising the provider-neutral `snapshot-resume` capability, at initial home placement and at every later Instance assignment onto that same home Runner. A Runner advertises the capability only when it is configured with a resume template cache root, requires the jailer, and already holds a template built from the exact signed execution bundle it verified. A pool whose operator-declared capabilities omit `snapshot-resume` refuses a `snapshot_resume` Profile non-retryably with `startup_mode_unsupported`, because no Runner in it will ever be admissible; a declared pool with no currently advertising Runner refuses retryably, because a Runner may materialize the template.

Revisions recorded before the field existed are stamped `cold_boot` by migration `0015_profile_startup_mode`. That is a statement of the behavior they already had, not a default invented for them, and it keeps an upgraded database converging on exactly the spec a fresh one writes.

## Tenant-egress release boundary

The tenant-egress release is not an in-place Profile or Sandbox upgrade from v0.7.2. Applications quiesce, every v0.7.2 Sandbox is retired, and the deployment is replaced before new Profiles and Sandboxes are created. The new release adds no historical decoder for a missing `requiresTenantEgressContext`, legacy assignment form, or Sandbox migration operation. A coordinated v0.7.2 backup is retained only for complete rollback.

## Quotas

Tenant aggregate quota and Subject quota both cover total Sandboxes, active Instances, vCPU, memory, Snapshots, exposed-port sessions, and concurrent data-plane operations. Tenant quota additionally limits active Subjects and application authorities. Each chargeable admission reserves against the Tenant and Subject in one transaction and stable lock order; release and cleanup update both levels together. Workspace and Snapshot filesystem allocation is governed by Runner storage-pressure admission rather than charged as uniquely retained bytes. Profile resource limits, including standard Profile limits, remain inline immutable execution policy rather than a third quota set. A concurrent race either commits one authorized reservation or returns a typed quota error; it never overcommits and repairs later.

Metrics use fixed-cardinality labels. Tenant refs, subject refs, Sandbox IDs, profile names, and workspace paths are audit fields rather than metric dimensions.

See [Domain and lifecycle](domain-lifecycle.md), [API conventions](api-conventions.md), and [Security](security.md).
Customer-shared tenant, delegated authority, aggregate quota, subject cleanup, and network-isolation behavior is defined in [customer-shared tenancy](customer-shared-tenancy.md).
