# Public API conventions

`contracts/openapi/v1/secondbox.openapi.json` is the canonical OpenAPI 3.1 contract for administrative and application HTTP surfaces. Generated Go, TypeScript, and Python transports derive from that file. Handwritten SDK helpers add polling, streaming, and lifecycle ergonomics without redefining wire types.

## Common HTTP rules

All paths are under `/v1`. JSON schemas close request objects with `additionalProperties: false`. Identifiers are opaque. Timestamps are UTC RFC 3339 with fractional seconds. Lists use `limit` plus an opaque `cursor`, return stable ordering and `nextCursor`, and never expose database offsets. A cursor is a canonical URL-safe token bound to the resource kind and exact ownership or filter scope; malformed, stale, cross-resource, and cross-scope cursors return `400 invalid_request`. Traversal uses immutable creation order with an opaque resource key as its tie-breaker, so inserts ahead of an existing cursor cannot duplicate already-returned resources.

Every response carries `X-Request-ID`; a valid client-supplied `X-Request-ID` is preserved, otherwise the server creates one. Mutating resources return `ETag`. Update and lifecycle requests use `If-Match`; a stale value returns `412 precondition_failed`. Data-plane admission also binds the current public `generation`. Internal fencing tokens never cross the public API.

`Idempotency-Key` is required for declared create and state-changing operations, including the canonical POST, PATCH, PUT, and DELETE mutations that expose it. Its scope is asserted tenant, subject, operation, and target. Repeating the same key and canonical payload returns the original durable result and sets `Idempotency-Replayed: true`; reusing it with a different canonical payload returns `409 idempotency_conflict`. The record and mutation commit in one PostgreSQL transaction. Records expire after the documented retention interval and outlive ordinary HTTP retries.

Streaming Exec and Terminal cancellation apply that key contract independently of session state. The cancellation frame, the transition to `closing`, and the response snapshot commit atomically. Repeating a key returns that exact snapshot even after the runner has acknowledged cancellation and the live session is `closed`. A different key is a new accepted request with `Idempotency-Replayed: false`, including when the session is already closing or closed; state idempotency is not request replay.

Errors use `application/problem+json` with stable `type`, `title`, `status`, `code`, `requestId`, `retryable`, and bounded structured `details`. Messages are diagnostic, not machine contracts. Authentication, authorization, quota, admission, generation, lease, guest, runner, infrastructure, and transport failures have distinct codes.

## Resource surface

The HTTP resources cover Profiles and revisions, RunnerPools, Runners, Sandboxes, Operations, Leases, exec sessions, terminal sessions, files, snapshots, artifacts, and port sessions. Runner projections never appear inside Sandbox responses.

Snapshot creation is a lifecycle-scoped, revision-guarded, idempotent reflink of the stopped Sandbox's current local Workspace image. Snapshot create, delete, and restore return durable Operations; list and get use read scope. Snapshot responses contain logical size, creation time, optional expiry, lifecycle state, and bounded metadata. They contain no Workspace-image checksum, home Runner, host path, provider reference, or storage key.

`POST /v1/sandboxes` accepts only:

```json
{
  "profile": "operator-defined-name",
  "metadata": {
    "client.example/purpose": "bounded string"
  }
}
```

Tenant and subject ownership come from the trusted request headers. A Sandbox request cannot override backend, image, resources, lifecycle, storage, network, timeouts, ports, runner pool, placement, generation, or Instance state.

## Lifecycle semantics

Create, start, drain, stop, Snapshot create/delete/restore, and Sandbox delete are idempotent asynchronous mutations that return `202` with a durable `Operation`. `GET /v1/operations/{id}` is the canonical polling surface. `wait` is a bounded long-poll for declared Sandbox states and never changes activity.

`get` and `list` return durable projections. `inspect` returns the latest generation-fenced guest heartbeat and active-session evidence persisted by the runner path; it does not renew activity or synthesize a fresh observation while no synchronous runner-effect broker exists. `ping` reports that same persisted guest liveness without touch. `touch` explicitly renews useful activity for the current generation and may carry a Lease. `drain` rejects new work immediately, waits only through the profile grace, and then allows stop to fence remaining work. `stop` removes compute without deleting the Sandbox or workspace. `delete` never occurs on connection loss.

## Leases

A Lease is explicit, bounded, subject-scoped authority for one Sandbox generation. Acquire, renew, release, and inspect are separate calls. A Lease cannot be renewed after expiry, drain, or generation change. A stale Lease produces `lease_fenced`, not an authentication error. Profiles determine whether the absence or expiry of Leases contributes to stopping compute.

## Execution

Buffered execution uses a discriminated request:

- `shell` carries one shell string interpreted by the released guest shell;
- `argv` carries a non-empty executable and exact argument array.

Both forms accept bounded cwd, environment, stdin, deadline, and output limits. Environment augments the profile-defined guest environment and cannot replace protected variables. Non-zero guest exit is an `exited` result.

Terminal outcomes are a closed union: `exited`, `spawn_failed`, `deadline_exceeded`, `cancelled`, `output_exhausted`, and `infrastructure_failed`. Spawn failure further distinguishes executable not found, permission denied, invalid cwd, and malformed executable. Service outcomes are never synthetic exit codes.

Streaming exec creates a durable, generation-fenced session and returns an opaque WebSocket URL using `secondbox.exec.v1`. The attach request repeats API authentication and the Sandbox generation. Every stdin frame carries canonical base64 data and an explicit `endOfInput` boolean. A true value closes process stdin after the frame's bytes; an empty payload is valid only for this EOF frame, and later stdin is rejected. Ordered stdin, output-credit, and cancel frames are durably relayed to the current Assignment; exact retransmission of one persisted client sequence is idempotent, while a changed duplicate or a gap fails. Stdout, stderr, and the single terminal outcome remain replayable across a client reconnect. No output is admitted beyond granted credit or the negotiated outstanding window. Disconnect requests guest cancellation without stopping or deleting the Sandbox. If the output limit is reached after bytes were emitted, those ordered partial bytes are delivered before `output_exhausted`.

PTY creation pins the current tenant, subject, Sandbox generation, ready Assignment fence, active Lease, and ProfileRevision policy into one stable Terminal session ID. The returned endpoint accepts only authenticated `secondbox.terminal.v1` WebSocket upgrades for the same generation, and PostgreSQL grants only one active attachment. Client text frames are exactly one canonical-base64 `terminal_input`, positive `credit`, bounded `resize`, or `cancel` with one gap-free sequence shared across reconnects. The descriptor's `nextClientSequence` is derived from the retained relay outbox so a reconnect resumes that sequence without another source of truth. Runner output and the single terminal outcome are retained in order and replay from server sequence zero on every attachment. Output cannot exceed credit, the pinned outstanding window, or the pinned response limit.

A detachable disconnect starts the ProfileRevision's pinned `terminalDetachSeconds` interval; reconnect replaces the attachment identity without replacing the Terminal session or its runner operation. Expiry of that interval, release or expiry of the bound Lease, generation fencing, deadline, and explicit cancellation all enqueue a priority guest cancel and remain `closing` until the current-fence PTY terminal acknowledges process exit. A non-detachable disconnect takes the same cancellation path immediately. No Terminal action stops or deletes the Sandbox.

## Files, artifacts, and ports

Filesystem operations are binary-safe read/write, UTF-8 convenience read/write, stat, direct-child list, exists, mkdir, and remove. Paths are workspace-relative protocol strings; the guest resolves them beneath a descriptor-pinned root and rejects traversal or symlink replacement. Transfer bodies are bounded streams with checksums.

Artifacts use separate upload, list, metadata, download, and delete resources. Their retention and authorization do not depend on a mutable workspace path.

An exposed-port session names only a profile-approved guest port and protocol and requires the current generation and Lease. It returns an expiring control-plane WebSocket endpoint, never a runner address or raw host port. The endpoint fragment is a one-time credential that the client passes as a WebSocket protocol alongside `secondbox.port.v1`; public frames are binary bytes, and disconnect closes the session.

See [Domain and lifecycle](domain-lifecycle.md), [Networking and ports](networking-and-ports.md), and [API reference comparison](api-reference-comparison.md).
