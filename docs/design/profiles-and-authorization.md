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
request that fits; omitted axes retain the Profile default unchanged. vCPU and memory
remain continuous integers within schema minimums. The Sandbox pins and
reports the resolved allocation; quota, home selection, Workspace commands,
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

A persisted application authority has one server-generated bearer credential, a unique ID, one fixed tenant reference, one fixed subject reference, one or more exact Sandbox operation scopes, and one or more Profile grants. An application request must present the bound references exactly. It cannot call platform or tenant management, Profile mutation, Runner administration, deployment usage, or aggregate timing routes; it can read only granted Profiles and can create Sandboxes only from them. Owned resource queries remain restricted to its bound tenant and subject. Application credential creation and rotation also return the bearer token once and persist only its lookup identifier and verifier.

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
- outbound network and DNS policy, including the required `network.requiresTenantEgressContext` Boolean;
- approved exposed ports, protocols, and session limits.

An optional `attributedExecution` block permits a single-command generation through a named installation gateway and bounds its concurrent connections. It requires `network.requiresTenantEgressContext: true`; the gateway is a canonical logical name, never a caller-selected URL or host socket. This policy is separate from ordinary outbound destinations.

An attributed start supplies an application authorization reference and an absolute expiry within the Profile execution limit. Admission requires stopped compute and an attributed-capable home Runner. The binding is stored with lifecycle intent and travels in the assignment under the Sandbox's Tenant and Subject. Firecracker and gVisor advertise this capability only with configured attributed routing. Qualification and release requirements are tracked in the [attributed execution plan](../plans/2026-09-10-attributed-command-execution.md).

The release-owned `agent-compartment` appends this permission to its existing ordinary policy, with the same logical Agent gateway and a limit of two simultaneous attributed connections per generation. This is the limit exercised by backend and public scenario qualification. Use the generated bundle identity: the attributed revision is 3 for baseline assets, 4 after a nonbaseline ordinary asset revision, and 2 for development. Earlier revisions remain immutable; existing Sandboxes keep their pinned revision. The isolated Profile retains `deny_all` networking and no attributed permission.

The assignment retains the execution binding and its one admitted exec session ID. Result cleanup cannot reopen that allowance, and changing lifecycle intent cannot remove the active assignment's restriction. Control-plane admission and the Runner permit bounded read-only files but refuse PTYs, ports, file writes, and another exec.

Completion, expiry, or connection loss retires the attributed Instance and clears its running intent. The next command needs a new explicit start and authorization binding. Only the Workspace persists across generations. A hard compute or Runner crash can lose guest writes that were not flushed before the crash.

SecondBox releases three explicitly selected standard Profile bundles:

- `agent-compartment` is bounded ephemeral compute for Flue-style agent turns. It starts immediately, has short idle and maximum-duration bounds, exposes no ports, and states `requiresTenantEgressContext: true`.
- `durable-coding` is a long-running coding workspace with larger inline CPU, memory, disk, process, operation, transfer, PTY-detach, Snapshot, and development-port bounds; it states `requiresTenantEgressContext: true`.
- `agent-compartment-isolated` retains the bounded command, file, workspace, cancellation, and lifecycle capabilities of `agent-compartment` while denying all outbound network and DNS access, exposing no ports, and stating `requiresTenantEgressContext: false`.

The declarative resource engine materializes standard bundles as ordinary immutable ProfileRevisions. Selection is explicit in `[standard_resources]`; the control plane has no built-in defaults, reserved-name behavior, or request-time reconciler. Each release declares the complete ordered lineage and canonical spec digest, validates an installed prefix, and appends only missing revisions. Existing Sandboxes retain the exact earlier revision they pinned. Operator-defined Profiles remain fully supported and follow the same immutable pinning rules.

Tenant ceilings and application grants select these release-owned Profile names directly; they do not create tenant-specific Profile copies. The requirement is explicit policy, not an inference from `agent-gateway.secondbox.internal`, `platform-gateway.secondbox.internal`, or any other domain. Operator-defined Profiles must also state the Boolean on every revision. Omission is invalid; the control plane supplies no default and does not decode an old missing field into either value.

A Tenant has at most one nullable operator-controlled egress-context name. The name is an opaque 1-to-63-character lowercase ASCII label matching `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`; it is not a hostname, network, gateway identity, or mapping digest. A Sandbox created from a requiring Profile pins the Tenant's current name. Later Tenant changes apply only to new Sandboxes. A non-requiring Profile pins and sends no context even when its Tenant has one.

Ordinary stop always flushes and detaches compute, advances the local Workspace manifest generation, and preserves every committed Workspace write without creating a Snapshot or transferring Workspace bytes off the Runner. A later start resolves that same current image on the current home Runner; it never adopts a newer Profile head. Operator relocation preserves the pinned ProfileRevision and validates its compatibility requirements against the target.

## Creation and compatibility

`POST /v1/sandboxes` contains only `profile` and bounded string metadata. The client supplies `Idempotency-Key` as a header. Resource, backend, image, lifecycle, storage, network, timeout, port, and placement fields are rejected as unknown properties.

Creation fails before allocating durable intent when the profile is absent, disabled, requires an egress context that the authenticated Tenant lacks, or has no RunnerPool capable of its immutable requirements. Successful creation persists the exact ProfileRevision ID, the immutable Tenant-context pin when required, and a resolved compatibility summary. Later Tenant or Runner availability changes do not rewrite that selection.

Profiles may be disabled to stop future creation. Disablement does not mutate pinned Sandboxes. A profile revision and its referenced assets cannot be deleted while reachable from a Sandbox or retention record.

## Startup mode

`startup.mode` is required on every ProfileRevision and has no application default. `cold_boot` starts a Sandbox by booting its guest. `snapshot_resume` starts it by resuming a prepared, identity-neutral guest, and it never falls back to `cold_boot`: an Instance that cannot be resumed fails, it does not boot.

The mode is a placement requirement, not a hint. A `snapshot_resume` ProfileRevision admits only onto Runners advertising the provider-neutral `snapshot-resume` capability, at initial home placement and at every later Instance assignment onto that same home Runner. A Runner advertises the capability only when it is configured with a resume template cache root, requires the jailer, and already holds a template built from the exact signed execution bundle it verified. A pool whose operator-declared capabilities omit `snapshot-resume` refuses a `snapshot_resume` Profile non-retryably with `startup_mode_unsupported`, because no Runner in it will ever be admissible; a declared pool with no currently advertising Runner refuses retryably, because a Runner may materialize the template.

Revisions recorded before the field existed are stamped `cold_boot` by migration `0015_profile_startup_mode`. That is a statement of the behavior they already had, not a default invented for them, and it keeps an upgraded database converging on exactly the spec a fresh one writes.

## Tenant-egress release boundary

The tenant-egress release is not an in-place Profile or Sandbox upgrade from v0.7.2. Applications quiesce, every v0.7.2 Sandbox is retired, and the deployment is replaced before new Profiles and Sandboxes are created. The new release adds no historical decoder for a missing `requiresTenantEgressContext`, legacy assignment form, or Sandbox migration operation. A coordinated v0.7.2 backup is retained only for complete rollback.

## Quotas

Tenant aggregate quota and Subject quota both cover total Sandboxes, active Instances, vCPU, memory, Snapshots, exposed-port sessions, and concurrent data-plane operations. Tenant quota additionally limits active Subjects and application authorities. Each chargeable admission reserves against the Tenant and Subject in one transaction and stable lock order; release and cleanup update both levels together. Workspace and Snapshot filesystem allocation is governed by Runner storage-pressure admission rather than charged as uniquely retained bytes. Profile resource limits, including standard Profile limits, remain inline immutable execution policy rather than a third quota set. A concurrent race either commits one authorized reservation or returns a typed quota error; it never overcommits and repairs later.

Metrics use fixed-cardinality labels. Tenant refs, subject refs, Sandbox IDs, profile names, and workspace paths are audit fields rather than metric dimensions.

See [Domain and lifecycle](domain-lifecycle.md), [API conventions](api-conventions.md), and [Security](security.md).
Customer-shared tenant, delegated authority, aggregate quota, subject cleanup, and network-isolation behavior is defined in [customer-shared tenancy](customer-shared-tenancy.md).
