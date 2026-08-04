# Direct data plane

Status: complete. This design defines the data-plane transport described by [Runner protocol](runner-protocol.md).

## Problem

The former PostgreSQL frame transport stored every data-plane message and repeatedly rewrote accumulated output bytes. For `N` frames of size `S`, accumulation required approximately `N²S/2` I/O in addition to the duplicate frame payload. Per-frame polling and notifications added latency and broadcast work across control-plane replicas. A session byte limit bounded the final size but did not change the quadratic path.

The finished transport removes payload bytes from PostgreSQL. PostgreSQL retains admission authority, bounded one-shot results, session lifecycle, accounting, idempotency, and terminal outcomes.

## Invariants

Only bytes leave PostgreSQL. Authority remains in PostgreSQL.

| Property | Preserve | Mechanism |
| --- | --- | --- |
| Admission: authorization, scopes, Profile grants, quota | Yes | Unchanged PostgreSQL transaction |
| Generation and Lease fencing at admission | Yes | Unchanged PostgreSQL transaction |
| Idempotency-keyed session creation | Yes | Unchanged PostgreSQL transaction |
| Single-use credential spend | Yes | Existing direct-Port consumption path |
| Session lifecycle and open/close evidence | Yes | One row per session, not per frame |
| Durable one-shot Exec outcome | Yes | One bounded completion row |
| Byte-level durable replay | No | Runner-owned PTY replay ring |
| Multi-replica routing without stickiness | Partly | Direct or in-memory proxied transport |

This is a transport change. It does not move an authority decision.

## Topology

Direct-only transport requires every API client to reach every runner data-plane listener. Existing direct Ports accept that requirement behind the separate direct data-plane scope. Applying the same requirement to Exec, PTY, and File would change deployments where runners are private and only establish outbound control connections.

Two transports carry data-plane bytes:

1. **Direct.** The caller connects to the runner data-plane listener. The control plane is absent from the byte path after admission.
2. **Proxied.** The caller connects to the control plane, which forwards bytes in memory over the runner control connection. The proxy persists no payload and applies the same credit windows.

A Profile selects the default transport. A deployment may forbid either transport.

Both transports are supported. The constraint being satisfied is that no data-plane payload is ever stored in PostgreSQL; a control-plane byte path that forwards in memory and persists nothing meets it. Runner reachability is therefore an optimization a deployment may choose, not a requirement the product imposes.

## Wire protocol

The existing bounded direct-Port credential and verdict exchange becomes the common data-plane handshake. `pkg/portdirect` and its independent runner mirror remain fixed by `contracts/portdirect/v1/vectors.json`.

- Magic is `SBXDP1`. There is no `SBXPORT1` compatibility path.
- A credential frame carries one session kind: `port`, `exec`, `pty`, or `file`.
- A valid kind without an implemented transport receives a typed unsupported-kind verdict.
- Credential and verdict details retain their existing bounds.
- Local credential rejection retains constant-time comparison.

After admission:

- `port` carries raw bytes in both directions.
- `exec`, `pty`, and `file` carry length-prefixed typed messages. These messages distinguish stdout and stderr, resize, exit status, and EOF. Existing guest-protocol shapes are reused where applicable.

The credential remains single-use. The runner spends it through the authenticated control connection, and the control plane remains the admission authority.

## Transport security

The caller-facing listener serves TLS 1.3 with the runner certificate used for its outbound control connection. That certificate carries `spiffe://secondbox/runner/<id>`.

Admission returns the endpoint and the certificate SPKI SHA-256. The caller pins that value before sending the credential frame. SPKI pinning is required instead of CA hostname validation because runner endpoints are routinely IP addresses; hostname validation would impose a runner naming scheme that no other product contract requires. Missing TLS material is a startup failure. Plaintext operation is not supported.

## PTY detach

The runner owns a bounded in-memory replay ring for each terminal session. Its size is the Profile stream window. A reattaching client supplies its last acknowledged sequence and the runner replays later entries.

Sequence numbering, gap rejection, and credit windows remain unchanged. The ring does not survive runner restart. The microVM also dies with that runner, so a terminal cannot usefully reattach after the same failure.

## One-shot Exec output

`POST /v1/sandboxes/{id}/exec` continues to return the existing `ExecOutcome`, including bounded `stdoutBase64` and `stderrBase64`.

The runner buffers output within `maximumOutputBytes` and sends one completion message over the control connection. The control plane persists one outcome row. It does not persist per-frame output for this path.

## Finished system

The `SBXDP1` handshake, TLS 1.3 listener, SPKI pinning, and session-kind discriminator apply to Port, Exec, PTY, and File sessions. Exec and File support direct and in-memory proxied streaming, while one-shot operations persist one bounded completion result. PTY uses the same transports with a Runner-owned replay ring for detach and reattach. Port callers use either the in-memory proxy or the separately authorized direct listener. Admission and cancellation commands contain no payload bytes.

The PostgreSQL frame implementation, its wakeups and retention sweep, its configuration bounds, and its conformance harness are absent. The schema retains no per-frame table or accumulated payload columns.

## What must not change

- Admission, authorization, quota, generation, and Lease fencing.
- The public OpenAPI contract except for fields required by a transport endpoint and certificate SPKI pin. Provider and runner vocabulary does not enter public schemas.
- The typed terminal-outcome union. Direct transport does not synthesize exit codes; `infrastructure_failed` remains distinct from `exited`.
- Evidence remains structurally payload-free.

## Documentation debt

The direct-Port transport invalidated unconditional statements that runner addresses are never disclosed. Keep these documents aligned with the scoped exception:

- `docs/design/api-conventions.md`
- `docs/design/api-reference-comparison.md`
- `docs/design/service-boundaries.md`

The rule is: the control plane never discloses a runner address except to an authority holding the direct data-plane scope, for one admitted session, with a pinned certificate.
