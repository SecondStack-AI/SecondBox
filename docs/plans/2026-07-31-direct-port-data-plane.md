---
title: Direct Port Data Plane
date: 2026-07-31
status: implemented
owner: SecondStack
provenance: SSH and VS Code latency evidence from the SecondStack ssh-piper ingress
---

# Plan: Direct Port Data Plane

## Outcome

Remove PostgreSQL from the Port byte path without weakening Port admission
authority. Port sessions carry SSH and VS Code Remote-SSH traffic, whose
handshake round trips and sustained throughput the durable frame relay cannot
serve. Admission stays PostgreSQL-authoritative and fenced; only the transport
between the caller and the Runner changes.

The baseline is the configured `SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS`
applied twice per round trip. `internal/api/port_tunnels.go:138` polls for
outbound events at that interval, and `internal/api/port_tunnels.go:211` sleeps a
further full interval on each backpressure retry. At the deployed 250 ms this is
roughly 250 ms mean and 500 ms worst-case added latency per round trip. An SSH
connection completes several round trips before its first prompt, so connection
setup alone costs seconds.

Success for this pass is:

- SSH connect and interactive echo bounded by ingress network round trip and one
  Runner bridge hop rather than by any poll interval;
- no change to PortSession admission, Lease binding, generation fencing, or
  single-use credential semantics;
- no Port payload byte persisted in PostgreSQL;
- VS Code Remote-SSH usable against a durable-coding Sandbox;
- unadmitted inbound traffic toward every TAP still denied;
- the relay transport retained and unchanged for callers without the direct
  grant.

## Fixed design

Admission is unchanged. A trusted caller still requests a session for one named
approved port on a ready Sandbox generation with the current Lease. The control
plane still transactionally binds tenant, subject, pinned ProfileRevision,
Lease, assignment fence, generation, named port, protocol, duration, and
subject/Profile/port-session limits. The single-use credential still exists and
is still consumed exactly once against PostgreSQL.

What changes is the endpoint the control plane returns and the leg that carries
bytes.

### Guest-facing half is untouched

The Runner already reaches an approved guest port through a dedicated
guest-protocol stream that dials `127.0.0.1:<guest-port>` inside the guest. That
stream runs over vsock. The guest TAP, bridge, and per-assignment nftables table
are not in the Port path, so this plan requires no network-policy change. Only
the Runner's caller-facing half is replaced: a live TCP socket instead of relay
frames claimed from PostgreSQL.

### Runner data-plane listener

The Runner gains one data-plane listener:

- bound to the explicitly configured `SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS`;
- advertised at registration as `SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS`,
  which is administrative capacity evidence of the same class as existing
  advertised capacity and carries no Sandbox identity;
- required for readiness. An unavailable listener makes the Runner unready and
  fences active instances, matching the existing network-policy listener rule.

### Connection admission

1. The caller connects and presents the single-use credential as the first
   framed message, before any payload byte.
2. The Runner rejects locally on any mismatch against the assignment-bound
   session state it already holds: session ID, generation, fencing token, Lease,
   named port, and deadline. Comparison is constant time.
3. The Runner consumes the credential through its existing outbound control
   connection before forwarding any byte. PostgreSQL remains the single
   consumption authority; a replayed or already-consumed credential fails there.
   This costs one control-plane round trip per TCP connection and none per byte.
4. On success the Runner opens the guest-protocol port stream exactly as today
   and copies bytes bidirectionally with no persistence.

Local rejection before the round trip keeps an unauthenticated peer from forcing
control-plane work.

### Flow control

TCP flow control governs the caller-to-Runner leg. The existing guest-protocol
credit window is retained on the Runner-to-guest leg, where backpressure must
still reach the guest process. The relay credit protocol does not apply to a
direct connection.

### Fencing and teardown

Generation fence, Lease expiry, session deadline, operator drain, Instance
termination, and loss of the Runner's control-plane connection each close every
live socket for the affected session. Connection loss cancelling admitted work
matches existing Runner behaviour. The Runner returns bounded proof of closure.

### Evidence

Port evidence is emitted at admitted open and at close, using the existing
fixed-shape Runner evidence record. Per-frame durability is not retained. This
is an explicit reduction from the current relay and is the property this plan
trades away.

### Authorization for the direct endpoint

A caller receives a direct endpoint only when its application authority holds a
new exact operation scope. Callers without it receive today's relay WebSocket
endpoint unchanged. This preserves the public-surface property that an ordinary
caller never learns a Runner address, confines the advertised address to an
explicitly granted trusted ingress, and makes rollout a per-authority grant
rather than a deployment-wide switch.

## Transport split

This plan fixes the transport for Port only. The durable relay remains correct
for the other data-plane kinds, and the split is deliberate:

| kind | transport | reason |
|---|---|---|
| Port | direct | handshake round trips and sustained throughput dominate |
| PTY | durable relay | no guest listener exists to route to; works under deny-all with no guest network surface; detach and replay depend on durable frames |
| Exec | durable relay | buffered and latency-insensitive; replay is the point |
| File | durable relay | large transfers are unaffected by poll latency; integrity and replay are the point |

PTY is not moved. A PTY opens through `ExecOpen` with `allocate_pty` over the
guest-agent protocol and has no guest TCP endpoint, so a direct transport would
require the Runner to terminate and bridge it regardless. Its value is that it
needs no guest listener, no declared Profile port, and no guest network surface,
which is what makes it usable on a locked-down Profile. Interactive PTY latency
is addressed by event-driven relay wakeups, which bring it below the threshold
where a further transport change is perceptible, and moving it would forfeit
`--detachable` replay for no observable gain.

## Non-goals

- UDP and port ranges. Both require kernel-path forwarding and a flow-lifetime
  model that has no analogue in the current connection-scoped session semantics.
- Kernel-path forwarding with per-flow nftables admission and conntrack fence
  teardown.
- Multi-host Runner routing. This plan assumes the ingress tier can reach the
  advertised Runner data-plane address.
- Public unauthenticated port exposure.
- Moving PTY, Exec, or File off the durable relay.
- Returning a Runner address to any caller without the explicit grant.
- Removing the ingress tier in favour of client-to-guest connectivity.

## Validation Commands

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- focused Port admission, credential-consumption, and fencing tests
- `just test-scenario` with real KVM, TUN/TAP, and Btrfs Workspaces
- SSH and VS Code Remote-SSH qualification against a durable-coding Sandbox

## Tasks

### Task 1: Add the Runner data-plane listener and readiness contract — complete

Bind the configured listen address, advertise the configured address at
registration and heartbeat, and fail readiness when the listener is
unavailable. Prove that an unavailable listener fences active instances and that
the advertised value carries no Sandbox identity.

### Task 2: Add the direct-endpoint operation scope — complete

Extend application-authority configuration with the exact scope, deny it by
default, and prove that a caller without it receives the relay endpoint and
never observes a Runner address.

### Task 3: Return a direct endpoint for granted callers — complete

Resolve the home Runner's advertised address at PortSession creation and return
it with the existing single-use credential. Keep the relay endpoint shape
byte-identical for ungranted callers.

### Task 4: Implement Runner connection admission — complete

Parse the leading credential frame, reject locally on any binding mismatch in
constant time, consume the credential through the control connection before
forwarding, and bound handshake time and message size. Prove replay, expired
Lease, superseded generation, wrong named port, and post-deadline rejection.

### Task 5: Bridge the admitted connection to the existing guest port stream — complete

Copy bidirectionally with no persistence, retain the guest-protocol credit
window on the guest leg, and close both legs deterministically on either side's
termination.

### Task 6: Implement fencing and teardown for live connections — complete

Close live sockets on generation fence, Lease expiry, deadline, drain, Instance
termination, and control-connection loss, and return bounded closure proof.
Prove no admitted connection survives a fence.

### Task 7: Emit open and close Port evidence — complete

Emit the existing fixed-shape record at admitted open and at close. Prove the
record contains no payload bytes, credential, fencing token, or Runner address.

### Task 8: Revise the affected design documents — complete

Update `docs/design/networking-and-ports.md` to describe both transports and to
narrow the unsupported list to UDP, port ranges, public unauthenticated sharing,
and ungranted direct access. Update `docs/design/threat-model.md` and
`docs/design/security.md` for the reduced Port evidence granularity and the new
scope. These properties are load-bearing and must change deliberately rather
than by implication.

### Task 9: Qualify SSH and VS Code Remote-SSH — latency qualified, SSH blocked

Measure connect time and interactive echo against the relay baseline, and
qualify VS Code Remote-SSH server install and steady-state operation.

## Alternatives rejected

- Kernel-path forwarding now. It requires per-flow nftables admission and
  conntrack-based teardown, and fencing correctness for a live kernel-forwarded
  flow is unproven. It is the right transport for UDP and belongs with that
  work.
- Shorter data-plane poll intervals. They create constant database load, retain
  a phase-dependent tail, and turn a recovery setting into the normal delivery
  path.
- Moving PTY, Exec, or File to the direct transport. Each depends on durable
  replay, and PTY has no guest endpoint to route to.
- Removing the ingress tier for client-to-guest connectivity. It requires a
  distinct public address per Sandbox and makes the guest sshd the only
  authentication boundary.
- Removing control-plane replica independence. The durable admission write stays
  on the critical path either way, so single-process operation would save one
  notification hop while forfeiting availability and rolling replacement.

## Result

Implemented. The transport split is live: Port sessions admitted for a granted
authority carry bytes on a direct socket to the home Runner, and every other
data-plane kind, including an ungranted Port caller, is unchanged.

### Measured against the relay baseline

`just test-scenario` on real KVM, TUN/TAP, and Btrfs Workspaces, with the
deployed 250 ms `SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS`, one guest TCP
listener, and both transports measured in the same run:

| metric | relay baseline | direct | ratio |
|---|---|---|---|
| interactive echo, mean | 276.0 ms | 0.45 ms | 614x |
| interactive echo, worst | 519.2 ms | 0.64 ms | 812x |
| 20 round-trip session | 5.527 s | 0.179 s | 31x |
| connect | 6.3 ms | 169.5 ms | 0.04x |

The relay's per-round-trip cost tracked the poll interval as predicted: roughly
263 ms mean at a 250 ms interval and 135 ms mean at 100 ms.

Direct connect does not track it. Across runs it measured 96 ms to 196 ms at a
250 ms interval and 102 ms at a 100 ms interval, so the ranges overlap and the
250 ms runs include values well below 100 ms. A poll-bound quantity cannot do
that. What connect actually pays is one control-plane consumption round trip plus
one guest-protocol stream setup, and its spread is host load rather than any
interval. The qualification therefore reports connect without gating on it: a
fixed threshold there would assert host speed instead of a transport property.
Echo and whole-session latency are gated, and both hold with two to three orders
of magnitude of margin.

Direct connect is higher than the relay's bare WebSocket upgrade because the
relay defers its cost to every round trip instead of charging it at connect. A
whole interactive session is the comparison that matters, and any session longer
than one round trip favours the direct transport.

### Deviations from the fixed design

Two things the plan did not specify were required to meet its own success
criteria, and both are narrow:

- A caller can present its credential before the admitting frame reaches the
  Runner, because the control plane commits admission and answers the caller in
  one response while the frame travels the durable command path. The Runner now
  waits a bounded time for the admitting frame before denying. An unknown
  credential is still denied, and the wait stays inside the handshake deadline.
- The admitting frame is on the caller's connect path, so its delivery was
  waking on the outbound poll interval and connect time was poll-bound at
  roughly 250 ms. A commit notification now wakes the runner command pump, which
  cut connect to roughly 100 ms and the interactive session from 2.7 s to 0.11 s.
  The durable frame remains the only authority and the existing poll remains the
  fallback.

  The notification fires on admitting one direct PortSession, not on inserting a
  data-plane frame. NOTIFY has no server-side filtering, so a per-frame trigger
  would broadcast to every listening replica once per relay Port message, per
  Exec and PTY stdin chunk, and per File chunk, and would charge each of those
  inserts a session lookup. This plan moves only the Port byte path, so it must
  not become an unmeasured wakeup change for the transports it leaves on the
  relay. A migration test pins that scope: it inserts relay Port, direct Port,
  and Exec sessions with three outbound frames each and requires exactly one
  notification, and it fails with nine against a per-frame trigger. Event-driven
  wakeups for the relay transports were taken up separately in
  [Event-Driven Relay Data-Plane Wakeups](2026-07-31-relay-data-plane-wakeups.md),
  which answers the per-frame objection above rather than reversing it.

### Not qualified: SSH and VS Code Remote-SSH

The source-built guest rootfs ships `openssh-client` and no `openssh-server`, so
a Sandbox on a source-mode image cannot terminate an SSH connection. Interactive
echo is the closest proxy available on that image and is what the table above
measures.

This bounds the measurement, not the transport. The image pipeline also builds
in `extend` and `prepared` modes, which preserve the base OCI image's filesystem
and therefore carry whatever SSH server that image ships; `Dockerfile.prepared`
removes `/etc/ssh/ssh_host_*` precisely so host identity is generated per
Workspace rather than baked into the image. A Sandbox on such an image
terminates SSH today, and the direct transport carries it with no further
change.

Qualification is therefore a question of which image the exercised Profile pins,
not a blocked capability. Adding `openssh-server` to the source-built rootfs
would change the image ABI, the Debian package lock, the license inventory, and
the rootfs contract, and would need an artifact rebuild and re-signing; that
remains a separate reviewed change and is not in this plan's task list. Until
either that lands or an `extend`/`prepared` image is qualified here, the direct
transport is proven for latency and correctness but not yet measured against the
SSH and VS Code Remote-SSH workloads that motivated it.
