---
title: Event-Driven Relay Data-Plane Wakeups
date: 2026-07-31
status: implemented
owner: SecondStack
provenance: Named follow-up recorded in migration 0004 and the direct Port data-plane plan
---

# Plan: Event-Driven Relay Data-Plane Wakeups

## Outcome

Remove the polling phase from the relay data plane while preserving PostgreSQL
as durable authority. The configured `SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS`
and `SECONDBOX_RUNNER_COMMAND_POLL_INTERVAL_MILLISECONDS` remain mandatory
recovery bounds; transactional notifications become latency hints only. This is
the work the direct Port data-plane plan named and deferred when it recorded that
"event-driven wakeups for the relay transports remain separate, unstarted work."

The relay carries every data-plane kind the direct Port transport does not: PTY,
Exec, File, and Port for callers without the direct grant. Both directions of a
relay round trip are poll-gated today:

- outbound, control plane to Runner: `pumpOutboundFrames`
  (`internal/runnercontrol/server.go:328`) already subscribes to
  `worknotify.KindRunnerCommand` by Runner ID and already drains relay frames on
  the same pass through `sendClaimedRelayFrame`. Nothing publishes when a relay
  frame commits, so it falls through to the ticker at `server.go:335`;
- inbound, Runner to caller: `internal/api/terminal_http.go:188`,
  `internal/api/exec_stream_http.go:152`, and `internal/api/port_tunnels.go:138`
  poll with no subscription at all. `internal/api` and `internal/service` import
  no wakeup source.

At the deployed 250 ms this is roughly 250 ms mean and 500 ms worst case added
per round trip, applied twice. An interactive PTY pays it on every keystroke.

Success for this pass is:

- interactive PTY echo bounded by commit-to-delivery rather than by either poll
  interval, with no reduction of the explicit recovery intervals;
- no change to relay durability, ordering, credit, replay, detach, or fencing;
- notification volume proportional to work bursts rather than to frames, proven
  by test rather than asserted;
- correctness across control-plane restart, Runner reconnect, missed and
  coalesced notifications, generation fencing, and detach and replay.

## Fixed design

An additive migration emits one compact notification when a durable frame
transitions a session from having no pending work to having some. One dedicated
PostgreSQL connection per control-plane process already listens on
`secondbox_work` and feeds the bounded coalescing hub in `internal/worknotify`;
this plan adds publishers and one consumer, not a mechanism.

### The two directions have different state, so they get different rules

Migration `0004` declined a per-frame trigger for a recorded reason: `NOTIFY`
has no server-side filtering, so every payload reaches every listening replica,
and a per-frame trigger would fire once per relay Port message, per Exec and PTY
stdin chunk, and per File chunk, charging each insert a session lookup.

That objection applies unevenly, because the two directions are not symmetric in
the schema. Outbound frames are inserted `pending` and claimed by the Runner
pump, so the durable rows record whether the consumer is behind. Inbound frames
are inserted already `delivered` — they are the record of what the Runner sent,
and the caller reads them by cursor. No durable row records how far a caller has
read, so PostgreSQL cannot know whether an inbound consumer is behind.

### Outbound direction

Outbound answers the objection. Consumers already drain until the authoritative
query reports no work, so a notification delivered while a consumer is already
draining changes nothing. The outbound trigger therefore fires only when the
inserted frame is the only pending outbound frame for its session:

- a burst of stdin or File-upload frames produces one notification, not one per
  frame;
- the `data_plane_sessions` lookup that resolves the Runner ID runs only on that
  transition;
- two concurrent inserts in separate transactions cannot observe each other, so
  both may notify. A duplicate is harmless because the hub coalesces and the
  consumer drains.

It publishes the existing `worknotify.KindRunnerCommand` keyed by the owning
Runner ID. No new kind is introduced, because `sendNextOutboundFrame` already
claims commands and relay frames in one pass; waking that pump is exactly the
required effect. The Runner stream already subscribes and already drains once on
connection, so the existing race between a frame committed before subscription
and the subscription itself stays closed.

### Inbound direction

Inbound notifies per frame, deliberately. Collapsing would require a durable
consumer cursor, and adding one would put a write on the guest-output path to
save a notification — the wrong trade. The cost is also lower than the general
objection assumes: `session_id` is already on the row, so there is no lookup,
and every inbound frame is output that an attached caller is actively waiting
for. These are not speculative wakeups; they are the frames the poll interval is
currently delaying.

The residual waste is the fan-out to replicas that hold no subscriber for that
session, which is bounded by replica count rather than by frame volume. That
bound is reasoned rather than observed; see the Result section.

It publishes the new `worknotify.KindDataPlaneSession` keyed by the frame's
`session_id`.

`ControlPlaneService` gains a wakeup source alongside its existing
`DataPlanePollInterval`, exposed to the API as a subscription accessor in the
same shape as `DataPlanePollInterval()`. Each of the three caller-facing loops
subscribes before its first list and then selects on the wakeup channel
alongside its existing ticker. Subscribing before the first authoritative read
is required, not stylistic: it closes the same pre-subscription race the runner
stream already handles.

The relay remains the only authority. A notification carries no state, grants no
authority, and only causes an existing query to run earlier than the ticker
would have run it.

### Failure behaviour

The listener is already a required control-plane component whose failure stops
the process explicitly. Notification loss, listener restart, and PostgreSQL
failover degrade to the existing poll intervals, which is why those intervals
remain mandatory rather than becoming tunable optimisations.

## Non-goals

- Moving any relay payload byte out of PostgreSQL. That is the direct Port
  transport's concern and is deliberately not generalised here.
- The Port backpressure retry at `internal/api/port_tunnels.go:211`. It waits on
  credit accounting rather than on frame arrival, so it needs a different signal
  and a separate decision; it keeps its interval.
- Reducing, renaming, or repurposing either configured poll interval.
- Changing credit, ordering, replay, detach, or fencing semantics.
- Notifying on inbound frames for sessions no replica is serving. The key is the
  session, and an unserved session simply has no subscriber.

## Validation Commands

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- focused relay wakeup scope, coalescing, and fallback-poll tests
- `just test-scenario` with real KVM, TUN/TAP, and Btrfs Workspaces
- interactive PTY echo measurement against the polled baseline

## Tasks

### Task 1: Add the inbound wakeup kind

Add `KindDataPlaneSession` to `internal/worknotify` and accept it in
`decodePostgresPayload`, requiring a non-empty key as `KindRunnerCommand` does.

This task precedes the trigger that emits the kind, because the decoder's
`default` branch returns an error and the listener returns on a decode failure,
which stops the control plane. A replica that receives a kind it does not accept
does not drop a hint; it stops. Coordinated replacement already forbids the
mixed-version window that would expose this, so the requirement is not new — but
this migration makes it load-bearing rather than merely advisable, and
`docs/operations/deployment.md` must say so.

### Task 2: Add the relay wakeup migration

Add triggers on `secondbox.data_plane_frames` that publish `runner_command`
keyed by the resolved Runner ID when an inserted outbound frame is the sole
pending outbound frame for its session, and `data_plane_session` keyed by
`session_id` for every inserted inbound frame.

Prove both rules by test. A burst of outbound frames on one session must produce
one notification, and that test must fail against a per-frame outbound trigger.
An outbound frame inserted `discarded` must produce none. Inbound frames must
notify once each, which pins the asymmetry as intended rather than as an
oversight.

### Task 3: Plumb a wakeup source through the control-plane service

Add the source to `ControlPlaneConfig` and expose a subscription accessor beside
`DataPlanePollInterval()`. A service constructed without a source must keep
working on its poll interval, so the existing integration fixtures and any
deployment that has not enabled notifications stay correct.

### Task 4: Subscribe the three caller-facing loops

Subscribe before the first authoritative read and select on the wakeup alongside
the existing ticker in `terminal_http.go`, `exec_stream_http.go`, and
`port_tunnels.go`. Prove a frame committed before subscription is still
delivered, and that cancellation releases the subscription.

### Task 5: Prove fallback and fencing are unchanged

Cover missed and coalesced notifications, listener restart, control-plane
restart, Runner reconnect, generation fencing, and detach and replay, each
resolving on the existing poll interval when no notification arrives.

### Task 6: Measure interactive latency

Measure PTY echo against the polled baseline and record before and after figures
in the Result section, following the precedent set by the control-plane wakeups
plan. Record the observed notification volume for a large File transfer and a
saturated relay Port session so the scope decision above is evidenced rather
than asserted.

### Task 7: Revise the affected documents

Update `docs/design/runner-protocol.md` to describe relay delivery as
notification-driven with the poll interval as its recovery bound, matching how
the control-plane wakeup path is already described. State the coordinated-
replacement dependency from Task 1 in `docs/operations/deployment.md`. Correct
migration `0004`'s comment, which currently states this work is unstarted.

## Alternatives rejected

- A per-frame trigger. It makes notification volume proportional to frames,
  broadcasts each one to every replica, and charges every insert a session
  lookup. Migration `0004` recorded that objection; this plan answers it rather
  than reversing it.
- Scoping the trigger to interactive session kinds only. It would leave File and
  relay Port on the poll interval for no benefit once volume is proportional to
  bursts, and it would put a policy decision in a trigger predicate where it is
  invisible to the code that depends on it.
- Shorter poll intervals. They create constant database load, preserve a
  phase-dependent tail, and turn a recovery setting into the normal delivery
  path.
- A new outbound wakeup kind. The Runner command pump already claims relay
  frames on the same pass, so a second kind would add a subscription without
  changing what gets drained.
- Process-local callbacks. They fail when the writer and the serving replica
  differ, which is the same reason the control-plane wakeup plan rejected them.

## Result

Implemented. PostgreSQL remains authoritative and both configured poll intervals
remain recovery bounds.

### Scope, as pinned by test

The outbound rule collapses a burst. Three pending outbound frames on one
session produce one notification; the same test observes three against a
per-frame trigger, so the property is discriminating rather than incidental. A
frame inserted `discarded` produces none, and a session that has been drained
wakes again on its next frame, so the rule is a transition rather than a
one-shot.

The inbound rule notifies once per frame, asserted directly so the asymmetry
reads as intended rather than as an oversight.

### Latency

Measured through the full chain rather than by benchmark harness: trigger,
PostgreSQL listener, hub, and caller-facing loop. `TestPublicTerminalDeliversOnNotificationRatherThanPollInterval`
configures a 60-second data-plane poll interval, so polling cannot account for
the result.

| configuration | first Terminal output |
|---|---:|
| notifications enabled | 0.37 s (test wall clock, including fixture setup) |
| wakeup source removed | no delivery before the 20 s read deadline |

The delta is the whole result: with the poll interval as the only delivery path,
output does not arrive within twenty seconds; with notifications it arrives
immediately. A production p50/p95 PTY echo figure still wants the lifecycle
benchmark's treatment and is not claimed here.

### Not measured

This gap is now closed by the qualified live workload in
[Relay Frame Retention Scoped to Replay](2026-08-01-relay-retention-scope.md#result).
Across three fixed PTY, 1 MiB File upload/download, and saturated 1 MiB relay
Port cycles, both the baseline and changed runs observed 456 inbound
notifications. Outbound notifications were 207 and 210 respectively; the small
variation is allowed by concurrent idle-to-pending inserts. The raw reports are
linked from that plan's Result section.
