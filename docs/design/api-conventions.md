# Public API conventions

`contracts/openapi/v1/secondbox.openapi.json` is the canonical OpenAPI 3.1 contract for administrative and application HTTP surfaces. The Go and TypeScript operation tables and wire types are generated from that file. Handwritten HTTP mechanics and SDK composition helpers add polling, streaming, and lifecycle ergonomics without redefining wire types.

## Common HTTP rules

All paths are under `/v1`. JSON schemas close request objects with `additionalProperties: false`. Identifiers are opaque. Timestamps are UTC RFC 3339 with fractional seconds. Lists use `limit` plus an opaque `cursor`, return stable ordering and `nextCursor`, and never expose database offsets. A cursor is a canonical URL-safe token bound to the resource kind and exact ownership or filter scope; malformed, stale, cross-resource, and cross-scope cursors return `400 invalid_request`. Traversal uses immutable creation order with an opaque resource key as its tie-breaker, so inserts ahead of an existing cursor cannot duplicate already-returned resources.

Every response carries `X-Request-ID`; a valid client-supplied `X-Request-ID` is preserved, otherwise the server creates one. Mutating resources return `ETag`. Update and lifecycle requests use `If-Match`; a stale value returns `412 precondition_failed`. Data-plane admission also binds the current public `generation`. Internal fencing tokens never cross the public API.

`Idempotency-Key` is required for declared create and state-changing operations, including the canonical POST, PATCH, PUT, and DELETE mutations that expose it. Its scope is asserted tenant, subject, operation, and target. Repeating the same key and canonical payload returns the original durable result and sets `Idempotency-Replayed: true`; reusing it with a different canonical payload returns `409 idempotency_conflict`. The record and mutation commit in one PostgreSQL transaction. Records expire after the documented retention interval and outlive ordinary HTTP retries.

Streaming Exec and Terminal cancellation apply that key contract independently of session state. The payload-free cancellation command, the transition to `closing`, and the response snapshot commit atomically. Repeating a key returns that exact snapshot even after the runner has acknowledged cancellation and the live session is `closed`. A different key is a new accepted request with `Idempotency-Replayed: false`, including when the session is already closing or closed; state idempotency is not request replay.

Errors use `application/problem+json` with stable `type`, `title`, `status`, `code`, `requestId`, `retryable`, and bounded structured `details`. Messages are diagnostic, not machine contracts. Authentication, authorization, quota, admission, generation, lease, guest, runner, infrastructure, and transport failures have distinct codes.

## Resource surface

The HTTP resources cover Tenants, Subjects, controller and application authorities, policy and usage, Profiles and revisions, RunnerPools, Runners, Sandboxes, Operations, Leases, exec sessions, terminal sessions, files, Snapshots, and port sessions. Runner projections never appear inside Sandbox responses.

Snapshot creation is a lifecycle-scoped, revision-guarded, idempotent reflink of the stopped Sandbox's current local Workspace image. Snapshot create, delete, and restore return durable Operations; list and get use read scope. Snapshot responses contain logical size, creation time, optional expiry, lifecycle state, and bounded metadata. They contain no Workspace-image checksum, home Runner, host path, provider reference, or storage key.

`POST /v1/sandboxes` requires `profile` and `metadata`:

```json
{
  "profile": "operator-defined-name",
  "metadata": {
    "client.example/purpose": "bounded string"
  }
}
```

Creation also accepts optional `resources` (`vcpuCount`, `memoryBytes`, and
`workspaceBytes`) within the Profile ceilings, and `sourceSnapshotId` to clone
a ready Snapshot into a new Sandbox on the same home Runner. Clone admission
requires the source disk capacity and compatible asset identity. See
[Profiles and authorization](profiles-and-authorization.md) for sizing rules.

Tenant and Subject ownership come from the authenticated request context.
The request cannot select a backend, image, network, port policy, runner pool,
placement, generation, or Instance state. Effective lifecycle comes from the
pinned Profile and any applicable delegated Subject policy.

## Lifecycle semantics

Create, start, drain, stop, Snapshot create/delete/restore, and Sandbox delete are idempotent asynchronous mutations that return `202` with a durable `Operation`. `GET /v1/operations/{id}` is the canonical polling surface. `wait` is a bounded long-poll for declared Sandbox states and never changes activity.

`DELETE /v1/sandboxes/{sandboxId}` can return `409 workspace_mutation_conflict`
while a stop owns the Workspace mutation slot, including a stop caused by idle
timeout or maximum duration. The request is rejected: no delete Operation or
idempotency result is recorded, and the stop continues unchanged. The conflict
lasts through compute detach and the local generation receipt until PostgreSQL
commits the stopped generation on the successful path. A receipt alone is not
completion; terminal stop failure requires inspecting the failure and recovery
state rather than assuming deletion can finish.

For this case, wait for stop progress, re-read the Sandbox, and retry DELETE with
its current `ETag` in `If-Match` and the same `Idempotency-Key`. Bound retries and
backoff by the caller's deadline; runner unavailability need not clear within
that deadline. A stale `If-Match` returns `412 precondition_failed` before the
mutation-slot check and also requires a fresh read. The shared
`workspace_mutation_conflict` problem currently carries `retryable: false`;
this explicit read-and-retry procedure applies to an outstanding stop, and does
not imply every use of that error code will resolve automatically. Once DELETE
returns `202`, poll its Operation; an identical accepted request replays that
Operation even after the Sandbox revision advances.

Start accepts an optional `attributedExecution` body member with `authorizationRef` and `expiresAt`. Omission requests ordinary execution; explicit null or malformed attribution is invalid. The binding participates in idempotency, so reusing a key with a different command reference conflicts. Profile policy supplies routing and bounds, and an unsupported home Runner refuses admission. The Go SDK accepts `StartSandboxRequest` before `LifecycleOptions`; the TypeScript start options include the same optional member.

`get` and `list` return durable projections. `inspect` returns the latest generation-fenced guest heartbeat and active-session evidence persisted by the runner path; it does not renew activity or synthesize a fresh observation while no synchronous runner-effect broker exists. `ping` reports that same persisted guest liveness without touch. `touch` explicitly renews useful activity for the current generation and may carry a Lease. `drain` rejects new work immediately, waits only through the profile grace, and then allows stop to fence remaining work. `stop` removes compute without deleting the Sandbox or workspace. `delete` never occurs on connection loss.

## Leases

A Lease is explicit, bounded, subject-scoped authority for one Sandbox generation. Acquire, renew, release, and inspect are separate calls. A Lease cannot be renewed after expiry, drain, or generation change. A stale Lease produces `lease_fenced`, not an authentication error. Profiles determine whether the absence or expiry of Leases contributes to stopping compute.

## Execution

Buffered execution uses a discriminated request:

- `shell` carries one shell string interpreted by the released guest shell;
- `argv` carries a non-empty executable and exact argument array.

Both forms accept bounded cwd, environment, stdin, deadline, and output limits. Environment augments the profile-defined guest environment and cannot replace protected variables. Non-zero guest exit is an `exited` result.

Terminal outcomes are a closed union: `exited`, `spawn_failed`, `deadline_exceeded`, `cancelled`, `output_exhausted`, and `infrastructure_failed`. Spawn failure further distinguishes executable not found, permission denied, invalid cwd, and malformed executable. Service outcomes are never synthetic exit codes.

Streaming exec creates a durable, generation-fenced session and returns an
opaque WebSocket URL using `secondbox.exec.v1`. Attachment repeats API
authentication and the Sandbox generation. Stdin frames carry canonical base64
data and an explicit `endOfInput` boolean. A true value closes stdin after those
bytes; an empty payload is valid only for EOF, and later stdin is rejected.
Input, credit, cancellation, output, and the terminal outcome flow through a
bounded live stream. PostgreSQL retains session lifecycle, accounting, and
terminal state, not payload frames. Exec output has no durable reconnect replay.
Disconnect cancels the command; ordinary execution leaves the Sandbox intact,
while an attributed generation retires its Instance. If the output limit is
reached after bytes were emitted, those bytes precede `output_exhausted`.

PTY creation pins the current tenant, subject, Sandbox generation, ready Assignment fence, active Lease, and ProfileRevision policy into one stable Terminal session ID. The returned endpoint accepts only authenticated `secondbox.terminal.v1` WebSocket upgrades for the same generation, and PostgreSQL grants only one active attachment. Client text frames are exactly one canonical-base64 `terminal_input`, positive `credit`, bounded `resize`, or `cancel` with one gap-free sequence shared across reconnects. The descriptor's `nextClientSequence` comes from the durable session producer-sequence projection. The Runner retains output and the single terminal outcome in a per-session in-memory replay ring bounded by the pinned stream window; a reconnect supplies its last acknowledged output sequence and receives exactly the later frames. A cursor older than the ring fails explicitly. The ring does not survive a Runner restart, which also terminates the microVM and leaves no useful PTY to reattach. Output cannot exceed credit, the pinned outstanding window, or the pinned response limit.

A detachable disconnect starts the ProfileRevision's pinned `terminalDetachSeconds` interval; reconnect replaces the attachment identity without replacing the Terminal session or its runner operation. Expiry of that interval, release or expiry of the bound Lease, generation fencing, deadline, and explicit cancellation all enqueue a priority guest cancel and remain `closing` until the current-fence PTY terminal acknowledges process exit. A non-detachable disconnect takes the same cancellation path immediately. No Terminal action stops or deletes the Sandbox.

## Files and ports

Filesystem operations are binary-safe read/write, UTF-8 convenience read/write, stat, direct-child list, exists, mkdir, and remove. Paths are workspace-relative protocol strings; the guest resolves them beneath a descriptor-pinned root and rejects traversal or symlink replacement. Transfer bodies are bounded streams with checksums.

An exposed-port session names only a profile-approved guest port and protocol and requires the current generation and Lease. The control plane never discloses a runner address except to an authority holding the direct data-plane scope, for one admitted session, with the expected certificate SPKI SHA-256 pin. Proxied sessions return an expiring control-plane WebSocket endpoint. Both transports use a one-time endpoint credential; public payloads are binary bytes, and disconnect closes the session.

See [Domain and lifecycle](domain-lifecycle.md), [Networking and ports](networking-and-ports.md), and [API reference comparison](api-reference-comparison.md).

## Inventory filters and quota update observations

`GET /v1/sandboxes` accepts repeated `state` (up to nine values) and `id`
(up to 64 values) filters alongside Metadata containment. Values within a set
combine with OR; state, ID, and Metadata filters combine with AND. Results remain
Tenant/Subject-scoped and cursor-paginated. Cursors bind to the normalized filters;
reordering a set is allowed, changing it is rejected. Sandbox `lifecycle` and
`resources` already report the resolved settings; inventory consumers need not
fetch Profile history to reconstruct those values.

Subject quota updates return `quotaObservation` on the updated Subject, containing
limits, usage, and Tenant-constrained admission headroom from the mutation's
transaction. The Subject revision and observation timestamp identify this result.
Idempotent replay preserves the original observation; it does not refresh it.
Ordinary Subject reads omit this mutation observation. Both SDKs offer typed
Subject and application-authority management helpers with revision and idempotency
headers; the transport must be configured with tenant-controller authority.

A full Workspace returns HTTP 507 `workspace_full` for file mutations; the caller can
delete files and retry. A missing Workspace file returns `file_not_found`. A missing owned resource
returns `not_found`; an existing Sandbox without current compute returns
`execution_node_unavailable`, or `generation_fenced` if the request names an old
generation. Inactive Leases return `lease_fenced`. A file miss does not require a
follow-up Sandbox read to determine whether the file or its compute is absent.
