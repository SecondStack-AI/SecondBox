# Direct data plane

This document describes the implemented transport. See also [Runner protocol](runner-protocol.md).

## Problem

The former PostgreSQL frame transport stored every data-plane message and repeatedly rewrote accumulated output bytes. For `N` frames of size `S`, accumulation required approximately `N²S/2` I/O in addition to the duplicate frame payload. Per-frame polling and notifications added latency and broadcast work across control-plane replicas. A session byte limit bounded the final size but did not change the quadratic path.

The finished transport removes payload bytes from PostgreSQL. PostgreSQL retains admission authority, bounded one-shot results, session lifecycle, accounting, idempotency, and terminal outcomes.

## Authority and transport

PostgreSQL owns authorization, Profile grants, quota, generation and Lease
fencing, idempotent admission, credential consumption, session lifecycle, and
bounded one-shot results. Streaming payloads are not stored there.

The public Exec, File, and PTY APIs terminate at the control plane. For their
Runner leg, the Profile's `execution.dataPlaneTransport` selects:

- `proxied`: bounded in-memory forwarding over the Runner's authenticated
  outbound control connection;
- `direct`: the control plane opens the Runner's TLS data-plane listener and
  authenticates the admitted session with its single-use credential.

A deployment may disable either Runner transport. Direct selection therefore
requires control-plane reachability to the Runner listener; it does not disclose
that listener to public Exec, File, or PTY clients.

Ports have a separate caller-facing choice: an application authority with
`sandbox:ports:direct` receives a Runner endpoint and connects directly.
Other callers receive the control-plane WebSocket proxy. Only direct Port
clients need public reachability to the Runner. See [Networking and ports](networking-and-ports.md).

## Wire protocol

The bounded credential and verdict exchange is the common data-plane handshake. `pkg/portdirect` and its independent runner mirror remain fixed by `contracts/portdirect/v1/vectors.json`.

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

The Runner data-plane listener serves TLS 1.3 with the runner certificate used for its outbound control connection. That certificate carries `spiffe://secondbox/runner/<id>`.

Admission binds the endpoint and certificate SPKI SHA-256. The connecting peer pins that value before sending the credential frame; only direct Port admission returns it to the public caller. SPKI pinning is required instead of CA hostname validation because runner endpoints are routinely IP addresses; hostname validation would impose a runner naming scheme that no other product contract requires. Missing TLS material is a startup failure. Plaintext operation is not supported.

## PTY detach

The runner owns a bounded in-memory replay ring for each terminal session. Its size is the Profile stream window. A reattaching client supplies its last acknowledged sequence and the runner replays later entries.

Sequence numbering, gap rejection, and credit windows remain unchanged. The ring does not survive runner restart. Runner restart tears down its Instances, so the terminal cannot reattach after that failure.

## One-shot Exec output

`POST /v1/sandboxes/{id}/exec` continues to return the existing `ExecOutcome`, including bounded `stdoutBase64` and `stderrBase64`.

The runner buffers output within `maximumOutputBytes` and sends one completion message over the control connection. The control plane persists one outcome row. It does not persist per-frame output for this path.

## Public contract

Transport selection preserves admission, authorization, quota, generation,
Lease fencing, and typed terminal outcomes. Provider details stay private,
with the scoped Runner-address disclosure for direct Ports described above.
Evidence remains payload-free.
