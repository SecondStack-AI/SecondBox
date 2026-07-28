# Domain and lifecycle

A Sandbox is durable intent plus one retained workspace lineage. An Instance is replaceable compute for exactly one Sandbox generation. API connections, data-plane streams, Leases, Instances, and Sandboxes have independent lifetimes.

## Records and ownership

All identifiers are server-generated opaque strings. Timestamps are UTC RFC 3339 values. Mutable administrative and lifecycle records carry a monotonically increasing revision used to produce an HTTP `ETag`. Cross-record relationships are logical identifiers rather than physical database constraints.

| Record | Owner and lifecycle |
| --- | --- |
| `Project` | Operator-created application isolation boundary. It owns service accounts, Sandboxes, retained bytes, and quota accounting. Disablement rejects new application work without rewriting historical audit. |
| `ServiceAccount` | Belongs to one Project. It has a display name, state, granted profile names, and allowed scopes. It is not an end-user identity. |
| `APIKey` | Belongs to one ServiceAccount. Only a keyed hash and non-secret prefix are stored. It has scopes, creation/expiry/revocation timestamps, and last-use evidence. Plaintext is returned once at creation or rotation. |
| `Profile` | Operator-owned stable name and mutable head. It is enabled or disabled and points to its current immutable ProfileRevision. There is no implicit profile. |
| `ProfileRevision` | Immutable resolved policy, resources, artifacts, and runner-pool selector. A Sandbox pins one revision for its lifetime. |
| `Sandbox` | Belongs to one Project and one ProfileRevision. It owns a Workspace, desired and observed state, current generation, lifecycle timestamps, bounded client metadata, and optional current Instance. |
| `Instance` | Belongs to one Sandbox generation. It records state, Assignment, start/ready/stop timestamps, and one stable termination reason. It contains no public backend or host location. |
| `Assignment` | Internal authority joining one Sandbox generation to one Runner. It contains the fencing token, capability snapshot, resolved artifacts, state, and proof of release. |
| `Lease` | Project-scoped, bounded authority for useful activity against one Sandbox generation. It expires, is released, or is fenced; it never outlives its generation. |
| `Workspace` | Belongs to one Sandbox. It names the exclusive active materialization, last committed checkpoint, retained-byte evidence, and retention state. |
| `Snapshot` | Project-owned immutable durable disk-state record derived from a committed checkpoint. It has content hash, byte size, source generation, compatibility metadata, and retention timestamps. |
| `Artifact` | Project-owned immutable application exchange object with name, media type, size, checksum, source generation, and retention timestamps. It is separate from a workspace path. |
| `RunnerPool` | Operator-owned placement and trust boundary. It declares allowed architectures, capabilities, capacity policy, and enrolled Runners. |
| `Runner` | Operator-owned enrolled execution identity. It belongs to one pool and records credential state, advertised/verified capabilities, capacity, protocol versions, health, and drain state. |

An `Operation` is the public asynchronous observation record for lifecycle mutations. It identifies the requested action, target Sandbox, status, request correlation, timestamps, and typed terminal result. It does not grant data-plane access.

## Sandbox states

The durable observed states are `creating`, `stopped`, `starting`, `ready`, `draining`, `stopping`, `checkpointing`, `failed`, `deleting`, and `deleted`. Desired state is `running`, `stopped`, or `deleted`. Reconcilers converge observed state on desired state; API handlers do not claim completion before durable evidence exists.

Lifecycle mutations require both an Idempotency-Key and an If-Match revision. PostgreSQL serializes equal keys, replays the original Operation for an equal canonical request, rejects changed payloads, and rejects stale revisions. The requested lifecycle intent is stored separately from the reconciler's latest action so an explicit checkpoint remains distinguishable from a profile-driven stop checkpoint and retains its bounded request metadata.

Creation resolves and pins the ProfileRevision, creates the Workspace, and records desired state. A profile determines whether creation also requests a running Instance. Starting selects a compatible runner, advances the generation when required, materializes the last committed checkpoint, and makes the Instance ready only after runner and guest negotiation succeed.

Drain rejects new exec, filesystem transfer, PTY, port, and lease admission. Already admitted operations receive the profile's bounded drain grace. Stop destroys compute after drain and commits a checkpoint when policy requires one. Delete drains, stops, applies retention policy, and eventually tombstones the Sandbox. Deletion is never implied by a client disconnect or Flue harness close.

## Generation and assignment invariants

An active Sandbox has exactly one current generation and at most one writer Assignment. Every Instance, Lease, operation stream, and activity report is bound to that generation. The fencing token is internal and changes whenever assignment authority changes.

A completed stop is the terminal boundary for its Instance generation. After Runner release evidence has removed the active materialization, the `finish_stop` transaction advances the Sandbox and Workspace generations together, fences remaining Leases and activity sessions from the old generation, and clears the current Instance. It preserves the last published checkpoint as the source root for a later start. Replaying the completed reconciliation cannot advance the generation again.

Runner loss uses the Assignment reconciler rather than the normal stop transition. Heartbeat expiry first makes the Assignment uncertain without changing generation or scheduling a replacement. Exact persisted FenceResult proof then permits one transaction to record the Instance and guest as lost with reason `runner_lost`, terminate all old-generation Lease, activity, data-plane, PortSession, Assignment, and materialization authority, advance Sandbox and Workspace generations, and wake the still-running lifecycle intent. The last published checkpoint remains unchanged.

A stale control-plane replica may observe old state but cannot commit a mutation without the current database revision. A stale runner or guest may send old evidence but cannot mutate a newer generation. Recovery either proves the old Instance stopped or advances the generation before materializing the last durable checkpoint elsewhere.

## Activity and termination

`ping` observes guest health and never renews activity. `touch` explicitly records useful client activity. Active exec, filesystem transfer, PTY, and port sessions report useful activity and prevent idle reclamation. PTY attachment, input, resize, credit, and accepted output are useful activity; descriptor reads and reconnect polling are not. A session suppresses reclamation only while its bound Lease remains active and unexpired. Release, expiry, or fencing enqueues guest cancellation for active terminal work, and activity remains open until the current Assignment returns the terminal acknowledgement proving process exit. Read-only lifecycle polling, wait calls, metrics, and health probes do not.

Guest liveness and idleness are separate inputs. An Instance terminates with exactly one stable reason: `requested_drain`, `requested_stop`, `idle_timeout`, `maximum_duration`, `guest_shutdown`, `resource_exhaustion`, `guest_agent_lost`, `runner_lost`, `startup_failed`, `fenced`, or `internal_failure`.

After readiness, a Runner may report natural guest shutdown, cgroup-proved resource exhaustion, or an unclassified runtime disappearance. The event must match the current Assignment fence and full durable correlation. PostgreSQL stores one immutable event and applies its reason only when the Instance has no earlier reason. FenceResult preserves an existing Instance cause, otherwise propagates the Sandbox lifecycle cause, and uses `fenced` only when neither exists. `finish_stop` applies the same first-writer rule to the old generation before advancing it. A terminal observation stops the Instance and wakes lifecycle reconciliation, but it does not release the Assignment or materialization, change the generation, or authorize replacement. The ordinary stop path still requires exact FenceResult release proof.

The lifecycle worker claims due Sandboxes with `FOR UPDATE SKIP LOCKED`, an owner, expiry, and revision. It loads the pinned ProfileRevision, current Instance and Workspace evidence, and active useful-session count, computes one action, and commits it only while the claim is current. Runner-facing materialize, start, stop, and checkpoint actions remain durable instructions until matching generation-bound evidence advances the next transition.

See [API conventions](api-conventions.md), [Workspace durability](workspace-durability.md), and [Recovery and reconciliation](recovery-and-reconciliation.md).
