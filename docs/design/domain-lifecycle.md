# Domain and lifecycle

A Sandbox is durable intent plus one retained workspace lineage. An Instance is replaceable compute for exactly one Sandbox generation. API connections, data-plane streams, Leases, Instances, and Sandboxes have independent lifetimes.

## Records and ownership

All identifiers are server-generated opaque strings. Timestamps are UTC RFC 3339 values. Mutable administrative and lifecycle records carry a monotonically increasing revision used to produce an HTTP `ETag`. Cross-record relationships are logical identifiers rather than physical database constraints.

| Record | Owner and lifecycle |
| --- | --- |
| `Profile` | Operator-owned stable name and mutable head. It is enabled or disabled and points to its current immutable ProfileRevision. There is no implicit profile. |
| `ProfileRevision` | Immutable resolved policy, resources, execution assets, and runner-pool selector. A Sandbox pins one revision for its lifetime. |
| `Sandbox` | Belongs to one asserted tenant/subject pair and one ProfileRevision. It owns a Workspace, desired and observed state, current generation, lifecycle timestamps, bounded client metadata, and optional current Instance. |
| `Instance` | Belongs to one Sandbox generation. It records state, Assignment, start/ready/stop timestamps, and one stable termination reason. It contains no public backend or host location. |
| `Assignment` | Internal authority joining one Sandbox generation to one Runner. It contains the fencing token, capability snapshot, resolved execution assets, state, and proof of release. |
| `Lease` | Subject-scoped, bounded authority for useful activity against one Sandbox generation. It expires, is released, or is fenced; it never outlives its generation. |
| `Workspace` | Belongs to one Sandbox and has one authoritative home Runner. It records logical capacity, current generation, readiness/deletion state, one durable mutation slot, and opaque local receipt evidence without a host path. Its home changes only through an operator-initiated stopped-Sandbox relocation. |
| `Snapshot` | Subject-owned immutable local reflink of one stopped Sandbox Workspace. It records logical size, lifecycle state, creation time, optional expiration, and bounded metadata without an image digest or storage reference. |
| `RunnerPool` | Operator-owned placement and trust boundary. It declares allowed architectures, capabilities, capacity policy, and enrolled Runners. |
| `Runner` | Operator-provisioned execution identity. It belongs to one pool and records pre-shared credential state, advertised/verified capabilities, capacity, protocol versions, health, and drain state. |

An `Operation` is the public asynchronous observation record for lifecycle mutations. It identifies the requested action, target Sandbox, status, request correlation, timestamps, and typed terminal result. It does not grant data-plane access.

## Sandbox states

The durable observed states are `creating`, `stopped`, `starting`, `ready`, `draining`, `stopping`, `failed`, `deleting`, and `deleted`. Desired state is `running`, `stopped`, or `deleted`. Snapshot and restore Operations have their own asynchronous lifecycle. Reconcilers converge observed state on desired state; API handlers do not claim completion before durable evidence exists.

Lifecycle mutations require an `Idempotency-Key`; revision-sensitive Sandbox mutations also require `If-Match`. Snapshot delete deliberately has no independent revision because Snapshot has no ETag. PostgreSQL serializes equal keys, replays the original Operation for an equal canonical request, rejects changed payloads, and rejects stale revisions.

Creation resolves and pins the ProfileRevision, selects one compatible healthy home Runner, persists that home, and remains asynchronous until the Runner returns a durable Workspace-create receipt. A profile determines whether creation also requests a running Instance. Starting always targets the current exact home, resolves the current local image at the expected generation, and makes the Instance ready only after Runner and guest negotiation succeed.

The pinned ProfileRevision's `startup.mode` selects which start path the home Runner takes, and it is never a fallback. A `cold_boot` revision boots a guest kernel and init for every Instance. A `snapshot_resume` revision resumes an identity-neutral post-boot memory snapshot instead: the guest receives its Sandbox identity, Workspace, and network identity in one atomic bind after the resume, rather than from boot arguments. A `snapshot_resume` Sandbox is placeable only onto a Runner advertising the provider-neutral `snapshot-resume` capability, and a Runner that cannot resume refuses the start with a typed retryable unavailability rather than cold booting it. Both modes hold the same generation fence, the same exclusive Workspace writer lock, the same fail-closed host network policy, and the same readiness contract.

`POST /v1/sandboxes/{sandboxId}:relocate` is an asynchronous, revision-sensitive
operator mutation. It refuses a running Sandbox and a Workspace with any
non-deleted Snapshot. The caller names a target Runner or requests compatible
selection within the pinned ProfileRevision's pool. Admission validates the
same pool, architecture, capability, health, drain, storage, and capacity
requirements as initial placement before the source image is sealed or any byte
moves. Source seal, bounded transfer, checksum-verified target import, atomic
home change, and source deletion are separate receipt-backed phases under the
one Workspace mutation slot.

Drain rejects new exec, filesystem transfer, PTY, port, and Lease admission. Already admitted operations receive the profile's bounded drain grace. Stop flushes and detaches compute, durably advances the local Workspace generation, then commits that exact generation in PostgreSQL. It does not create a Snapshot, hash an image, or contact object storage. Delete drains and stops, asks the home Runner to delete all local Workspace and Snapshot state, and only then tombstones the Sandbox. Deletion is never implied by a client disconnect or Flue harness close.

## Generation and assignment invariants

An active Sandbox has exactly one current generation and at most one writer Assignment. Every Instance, Lease, operation stream, and activity report is bound to that generation. The fencing token is internal and changes whenever assignment authority changes.

A completed stop is the terminal boundary for its Instance generation. After Runner release evidence proves compute detached, the Runner atomically advances the current-image manifest and persists a replayable receipt. The `finish_stop` transaction advances the Sandbox and Workspace generations to that exact value, fences remaining Leases and activity sessions from the old generation, and clears the current Instance. Replaying either side cannot advance the generation again.

Runner loss uses the Assignment reconciler rather than normal stop. Heartbeat expiry makes the Assignment uncertain without changing generation or scheduling a replacement. The Sandbox reports its home Runner unavailable and remains on that home. When the same stable Runner returns, exact inventory, receipt, Assignment, and fencing evidence determines whether reconciliation may resume or must surface missing/conflicting local data. Runner loss never initiates relocation automatically.

A stale control-plane replica may observe old state but cannot commit a mutation without the current database revision and Workspace mutation slot. A stale Runner or guest may send old evidence but cannot mutate a newer generation. No automatic recovery path relocates a Sandbox or creates an empty replacement.

## Activity and termination

`ping` observes guest health and never renews activity. `touch` explicitly records useful client activity. Active exec, filesystem transfer, PTY, and port sessions report useful activity and prevent idle reclamation. PTY attachment, input, resize, credit, and accepted output are useful activity; descriptor reads and reconnect polling are not. A session suppresses reclamation only while its bound Lease remains active and unexpired. Release, expiry, or fencing enqueues guest cancellation for active terminal work, and activity remains open until the current Assignment returns the terminal acknowledgement proving process exit. Read-only lifecycle polling, wait calls, metrics, and health probes do not.

Guest liveness and idleness are separate inputs. An Instance terminates with exactly one stable reason: `requested_drain`, `requested_stop`, `idle_timeout`, `maximum_duration`, `guest_shutdown`, `resource_exhaustion`, `guest_agent_lost`, `runner_lost`, `startup_failed`, `fenced`, or `internal_failure`.

After readiness, a Runner may report natural guest shutdown, cgroup-proved resource exhaustion, or an unclassified runtime disappearance. The event must match the current Assignment fence and full durable correlation. PostgreSQL stores one immutable event and applies its reason only when the Instance has no earlier reason. FenceResult preserves an existing Instance cause, otherwise propagates the Sandbox lifecycle cause, and uses `fenced` only when neither exists. `finish_stop` applies the same first-writer rule to the old generation before advancing it. A terminal observation stops the Instance and wakes lifecycle reconciliation, but it does not release the Assignment or Workspace attachment, change the generation, or authorize replacement. The ordinary stop path still requires exact release proof and a local generation receipt.

The lifecycle worker claims due Sandboxes with `FOR UPDATE SKIP LOCKED`, an owner, expiry, and revision. It loads the pinned ProfileRevision, current Instance, authoritative home Workspace, mutation state, and active useful-session count, computes one action, and commits it only while the claim is current. Runner-facing Workspace create, start, stop, Snapshot, restore, relocation, and delete actions remain durable instructions until matching generation-bound receipts advance the next transition.

The durable schedule is the sole authority over which Sandboxes hold reconciliation work. A Sandbox already holding its desired state with no deadline that could change the decision — stopped or failed while stopped is wanted, deleted while deleted is wanted — commits no next reconciliation deadline at all and leaves the claim scan until a lifecycle intent schedules it again. A decision that changes no field a caller can observe holds the public revision and updated timestamp where they are, so an `If-Match` precondition never loses a race to a transition that changed nothing.

See [API conventions](api-conventions.md), [Workspace durability](workspace-durability.md), and [Recovery and reconciliation](recovery-and-reconciliation.md).
