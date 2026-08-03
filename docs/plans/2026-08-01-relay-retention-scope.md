---
title: Relay Frame Retention Scoped to Replay
date: 2026-08-01
status: implemented
owner: SecondStack
provenance: Unmeasured relay cost recorded in the direct Port and relay wakeup plans
---

# Plan: Relay Frame Retention Scoped to Replay

## Outcome

Bound the lifetime of durable relay frames by what a session can actually replay,
and measure the relay's durable cost, which two implemented plans record as
reasoned rather than observed.

The relay carries PTY, Exec, File, and ungranted Port. Every frame in either
direction becomes a row in `secondbox.data_plane_frames`
(`migrations/postgres/0001_secondbox.sql:521`) holding `payload bytea`,
`payload_hash`, `delivery_count`, and claim state, indexed twice — once uniquely
on `(session_id, direction, sequence)` and once on
`(direction, state, priority, created_at, id)`. `data_plane_sessions` separately
carries `stdout_bytes`, `stderr_bytes`, and `content_bytes` as `bytea` on the
session row.

Admission initializes every session's `retain_until` to
`created_at + relay.retention` in `internal/runnercontrol/postgres_relay.go:301`,
from `SECONDBOX_DATA_PLANE_RETENTION_SECONDS`, but that is not one uniform clock
after admission. Normal Exec, File, and Terminal completion replaces it with
`completed_at + relay.retention` at `postgres_relay.go:1217`. Runner loss and
generation fencing use `GREATEST(retain_until, completed_at)`, while the Port
terminal paths currently leave the admission value unchanged. One column
therefore owns several transition-specific session-retention rules, and every
one of them holds a session's frames for exactly as long as it holds the session
row — whether or not that session can ever replay them.

A session frequently cannot. `internal/runnercontrol/postgres_relay_terminal.go:213`
cancels a Terminal session outright when its client disconnects unless the
session is detachable with a non-zero detach window:

```go
if !session.Detachable || session.TerminalDetachSeconds == 0 {
        // enqueue cancellation
```

`--detachable` is opt-in. A non-detachable Terminal session is therefore
terminated at disconnect and can never be reattached, yet its frames occupy rows
and index entries for the full retention window. The frames are paying for a
capability that session structurally cannot use.

Both prior plans flag the cost as unquantified.
`docs/plans/2026-07-31-relay-data-plane-wakeups.md` records under **Not
measured** that notification volume "under a large File transfer and a saturated
relay Port session was reasoned about and pinned by the migration scope tests,
but not observed against a live workload", and that the inbound rule notifies
once per frame by design.

Success for this pass is:

- the durable cost of a relay session measured against a live workload — rows,
  bytes, index growth, and notification count for an interactive PTY, a large
  File transfer, and a saturated relay Port session;
- frame retention separated from session/result/idempotency retention, so
  materialised results and admission replay remain available for their existing
  horizon after redundant frames are pruned;
- frames pruned only after no live delivery or replay consumer remains, with the
  configured session-retention horizon as the safe fallback;
- no change to transport, delivery, ordering, credit, replay, detach, fencing, or
  notification rules;
- streaming Exec reconnect, Terminal detach/replay, Port delivery, and admission
  idempotency behavior unchanged and proven unchanged.

## Fixed design

### What is not touched

Frames stay the delivery mechanism. The caller-facing read at
`postgres_relay_terminal.go:442` selects `sequence, payload` for
`sequence > afterSequence`, and that same cursor read serves both live delivery
and post-detach replay. Persistence cannot be removed from the write path
without replacing delivery, which is out of scope and was decided against twice.

The transport split is settled. `docs/plans/2026-07-31-direct-port-data-plane.md`
rejected moving PTY, Exec, or File to the direct transport, and
`docs/plans/2026-07-31-relay-data-plane-wakeups.md` removed the poll latency that
was the strongest argument for doing so. This plan does not reopen either
decision. It changes how long a frame is kept, not how it travels.

### Frame lifetime and session lifetime become separate

`retain_until` remains the session horizon. It continues to bound materialised
results, terminal outcome, admission idempotency replay, and the session row
itself. This pass does not shorten it or normalize its existing transition-
specific behavior.

This changes the meaning of the operator-owned retention setting for frames and
must be documented as such. `SECONDBOX_DATA_PLANE_RETENTION_SECONDS` participates
in computing the transition-specific `retain_until` deadline; it does not promise
one uniform interval after completion. That stored deadline remains authoritative
for session/result/idempotency cleanup and is the maximum safety fallback for
frames that still have a delivery or replay consumer. The setting no longer
promises that every redundant relay frame remains stored until that deadline.
Configuration, service-boundary, operational, forensic, and security text must
distinguish those semantics; the setting's value, authority, and required status
do not change.

A migration adds `frames_retain_until timestamptz NOT NULL`, initialized from
`retain_until`, plus nullable `frame_cleanup_completed_at`. The former is the
earliest time at which the session's frames may be deleted; the latter makes
early cleanup idempotent without repeatedly selecting a session whose eligible
frames are already gone.
Until a per-kind rule deliberately shortens the frame horizon, every transaction
that extends or otherwise changes `retain_until` updates `frames_retain_until` to
the same value. A long-running replayable session therefore receives the normal
`completed_at + retention` window rather than retaining its stale admission-time
horizon. The migration also adds a partial sweep index on
`(frames_retain_until, state, id) WHERE frame_cleanup_completed_at IS NULL`.

Deleting outbound frames must not change the session descriptor returned by an
idempotent admission replay. `dataPlaneSessionSelect` currently derives
`next_client_sequence` from the retained outbound frames. Add a durable session
projection for that value, initialize and update it in the same transactions
that append outbound frames, use it as the authority for allocating every later
outbound sequence, and make the descriptor read it before any frame can be
pruned. This is a producer sequence projection, not a consumer cursor, and does
not add a write to the guest-output path.

Every transaction that inserts a frame for an existing session already must
participate in session serialization. Make that requirement explicit for cleanup:
if `frame_cleanup_completed_at` is non-null, a permitted post-terminal insert
atomically clears it and derives a new per-kind frame horizon before inserting
with the durable sequence projection. A concurrent final session delete holds
the same session lock and either observes the reopened cleanup or wins first and
causes the insert to return not-found. Exact inbound terminal duplicates and an
already-projected Port acknowledgement retry use their compact projections and
do not reopen cleanup because they insert no frame. The first acknowledgement of
a Port byte frame may append credit; that transaction uses the producer sequence
projection and reopens cleanup like every other permitted insert. This covers the
terminal-state discarded frame path in `AppendExecClientFrame` and prevents a
late frame from stranding a cleanup-complete session.

Relay Port acknowledgement idempotency currently lives in each frame's
`consumed_at`. Add a compact acknowledged-sequence projection to `port_sessions`,
updated in the existing public acknowledgement transaction. An exact or older
acknowledgement consults that projection before looking up a frame, so deleting
an acknowledged terminal frame cannot turn a harmless retry into not-found. This
compacts an existing Port consumer cursor; it does not add a guest-output write.

Runner terminal replay also needs compact evidence after cleanup. The Runner
retains completed-operation tombstones and may re-emit the exact terminal frame
when it receives a duplicate command. `PersistInboundFrame` currently proves
that duplicate by reading the retained frame's `payload_hash`. Add terminal
inbound sequence and payload-hash projections to `data_plane_sessions`, written
atomically with every accepted terminal transition. When the frame row is gone,
the duplicate path uses those projections to accept the exact terminal replay
and reject a changed payload or sequence. This preserves duplicate conformance
without retaining the terminal payload.

The rule stated once: a frame remains durable while any current Runner delivery,
public delivery, or contractually supported replay can still read it. When the
last such consumer is gone, the terminal-state transaction or the transaction
that removes that consumer may move `frames_retain_until` earlier. It may never
move later than the session's `retain_until`, which remains the safe fallback.

The per-kind rules are explicit:

- buffered Exec (`operation='exec'`) and File materialise their complete public
  results onto `data_plane_sessions` in the same transaction that records their
  terminal frame. Their frame horizon becomes that completion time.
- streaming Exec (`operation='exec-stream'`) keeps its current frame horizon.
  The public contract permits reconnect after completion and replays stdout,
  stderr, and terminal outcome from sequence zero on every connection; the
  materialised byte columns do not replace that ordered stream.
- Terminal keeps frames while live and while an attachment can read them. On a
  terminal-state transition with no attachment, its frame horizon becomes the
  completion time. With an attachment, the existing horizon remains until
  `DetachTerminalAttachment` clears that exact attachment after successful
  terminal delivery or a disconnected socket. A non-detachable disconnect must
  clear its attachment in the same transaction that enqueues cancellation, so
  the later terminal acknowledgement cannot retain frames for a dead reader.
  A crash before attachment cleanup conservatively keeps the existing horizon.
  No detachable session is pruned while reattachment is still permitted, and no
  active attachment loses the final output or terminal frame.
- a relay Port keeps frames while open. After terminalization it keeps them until
  the terminal event is acknowledged through the existing `consumed_at` cursor,
  the PortSession expires and no caller can consume another event, or the
  unchanged session `retain_until` arrives, whichever comes first. Port maximum
  duration and data-plane retention are independent settings, so the plan does
  not promise delivery beyond today's session-retention bound. The acknowledgement
  and expiry terminalization paths lower the frame horizon transactionally. A
  direct Port has no public relay payload consumer, so its Open and terminal
  control frames become prunable once the terminal projection commits.

Every normal and forced terminalization path applies these rules: inbound
Exec/File/PTY terminal frames, inbound Port terminal frames, explicit and
deadline cancellation completion, Runner loss, generation fencing, Port close,
and Port expiry. A shared domain-specific helper or SQL fragment may centralize
the calculation, but it must not obscure the per-kind policy.

### The existing sweep becomes two-phase

`internal/runnercontrol/postgres_relay_cancellation.go:148` already owns bounded
retention cleanup and remains the only worker and schedule. This pass establishes
one lock order for every path that needs more than one of these objects:
`data_plane_sessions`, then `port_sessions` when present, then
`activity_sessions` when present, then `data_plane_frames`. The activity
projection is included because `failDisconnectedRunnerDataPlaneSessions`
(`internal/runnercontrol/postgres.go:195`) writes it first today, ahead of both
`port_sessions` and the session rows it derives from.
Change `MarkOutboundFrameDelivered` to resolve the frame's
session, lock that session, and only then lock/update the exact claimed frame.
Port acknowledgement already follows session, Port projection, then frame. The
claim query may remain frame-only because it never acquires a session lock and
only admits non-terminal sessions.

Reorder mixed terminalization paths to the same rule. In particular,
`failDisconnectedRunnerDataPlaneSessions` currently updates `port_sessions`
before `data_plane_sessions`. It must first lock/materialise the affected
data-plane session IDs, terminalize those session rows, and only then update the
matching Port and activity projections from that fixed set. Audit the remaining
Runner-loss, generation-fence, explicit-close, and expiry paths for the same
order; none may acquire a Port or frame lock and then wait for its session.

The sweep becomes two bounded phases in the same invocation. Phase one is
steps 1 to 4 and removes frames in one transaction; phase two is step 5 and
removes frame-empty sessions in a separate transaction.

Phase one — remove frames:

1. select and lock at most `limit` terminal session candidates whose
   `frames_retain_until <= now` and `frame_cleanup_completed_at IS NULL`, ordered
   with `FOR UPDATE ... SKIP LOCKED`;
2. within those locked sessions, select at most `limit` ordered frames. Never
   select an unexpired `claimed` frame. At or after claim expiry, cleanup and
   `MarkOutboundFrameDelivered` contend for the session lock: if marking wins,
   the frame becomes delivered and cleanup may delete it; if cleanup wins, it
   revokes/deletes the expired claim and the later mark returns
   `ErrRelayDeliveryClaim`. This session lock is the explicit expiry
   linearization boundary;
3. bound payload work with cumulative stored `payload_bytes` and an explicit byte
   budget passed to `SweepDataPlane`. That budget is a compiled constant beside
   the sweep, not an operator setting and not a reused data-plane bound: it
   answers "how much payload may one cleanup transaction touch", which is a
   property of the sweep, whereas
   `SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES` answers "how large may one
   session grow". Binding cleanup batching to a session-size limit would make an
   operator's session sizing silently retune cleanup transaction width. The
   constant follows the Category C pattern established by
   `docs/plans/2026-08-01-configuration-surface.md`, so it may later gain a
   validated optional override without becoming required. Always admit one first
   row for progress if a historical row exceeds the budget; such a transaction is
   bounded by that row's stored `payload_bytes`, not by today's
   `SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES`;
4. delete the selected frames. Set `frame_cleanup_completed_at=now` only for at
   most `limit` locked candidates with no remaining frame, then commit.

Phase two — remove frame-empty sessions:

5. in a separate transaction on every invocation, select at most `limit`
   terminal sessions whose `retain_until <= now`, whose frame cleanup is
   complete, and for which no frame exists. Delete only those session rows. The
   session phase no longer performs a residual bulk frame delete.

The unit of the new data batch is frames, and both session candidate sets are
bounded. One maximum-size session cannot starve session retention, while an
active delivery claim prevents both early frame deletion and final session
deletion. Materialised outcomes and idempotency records remain readable until
the transition-specific session deadline even when their frames have already
been removed. `SweepDataPlane` returns progress when it deletes or revokes any
frame, finalizes any frame cleanup, or removes any due session, so a session
larger than one batch is drained immediately rather than waiting a poll interval
between batches.

### Measurement precedes the change

Task 1 builds the reproducible harness and measures before the retention migration
changes anything, because the prior plans' reasoning about volume is explicitly
unvalidated. If the measured durable cost is negligible at deployed volumes, the
schema and behavior tasks stop. Publishing the evidence and correcting the prior
plan remain unconditional, so an early stop cannot leave **Not measured** behind.

### Configuration-surface prerequisite — satisfied

The operator-facing part of Task 7 depends on Tasks 3, 7, and 8 of
`docs/plans/2026-08-01-configuration-surface.md`, which create the deployment-
manifest schema and help, compiler, generated `deploy/secondbox.example.toml`,
redacted `secondbox-deploy inspect` output, and operator documentation.

Those tasks landed on `main` through PR #5, introduced at `1a12dc1` and merged
through `f781e7c`. `deploy/environment.example` is removed,
`deploy/secondbox.example.toml` carries `data_plane_retention_seconds`, and
`secondbox-deploy inspect` exists. Task 7 therefore closes against real files and
commands, and this plan does not absorb any manifest or documentation deliverable
from the configuration-surface work. Confirm those artifacts still exist before
starting Task 7 rather than assuming this note stayed true.

## Non-goals

- Moving any relay payload byte out of PostgreSQL. That is the direct Port
  transport's concern and is deliberately not generalised, per that plan's own
  non-goals.
- Moving PTY, Exec, or File to the direct transport. Rejected in both prior
  plans, with reasons that this plan does not dispute.
- Changing the inbound or outbound notification rules, or the migration `0005`
  trigger scope.
- Changing credit, ordering, replay semantics, detach semantics, or fencing.
- Reducing or repurposing either configured poll interval.
- Changing `SECONDBOX_DATA_PLANE_RETENTION_SECONDS` as a value, required setting,
  or operator-owned authority. It continues to feed the transition-specific
  session deadline and maximum frame fallback; its narrower frame semantics are
  an explicit outcome of this plan. See
  [Configuration Surface Split](2026-08-01-configuration-surface.md) for where
  the value lives.
- Shortening result, session-row, or admission-idempotency retention.
- Removing `stdout_bytes`, `stderr_bytes`, or `content_bytes` from
  `data_plane_sessions`. Those are the materialised result and are the reason
  buffered Exec and File frames become redundant.
- Adding a streaming Exec consumer cursor or changing its replay-from-zero
  contract.

## Validation Commands

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- `just measure-relay-retention` on the qualified scenario host
- focused relay retention, detach, and replay tests
- `just test-scenario` with real KVM, TUN/TAP, and Btrfs Workspaces
- `just test-stress` for sustained-output volume

## Tasks

### Task 1: Build the harness and measure the durable cost

Add a dedicated `tests/scenario/relay_retention` harness and a
`just measure-relay-retention` recipe. Do not overload the current stress suite:
it has buffered Exec, streaming Exec, File, and Snapshot workloads, but no PTY or
relay Port workload and no PostgreSQL or notification instrumentation.

Run each measurement against a fresh migrated scenario database with fixed,
checked-in parameters for:

- an interactive PTY transcript with a fixed input/output byte count;
- a fixed-size large File upload and download;
- a forced-relay PortSession driven at a fixed frame size, concurrency, byte
  total, and duration. Assert that it did not negotiate the direct transport.

The harness starts its PostgreSQL listener before admission and uses an otherwise
idle dedicated Runner. It accounts for the two notification directions
separately: inbound `data_plane_session` notifications are keyed by measured
session ID, while outbound `runner_command` notifications are keyed by that
Runner ID. It records one machine-readable result containing the workload
parameters, PostgreSQL version, frame rows by kind and direction,
`sum(payload_bytes)`, table bytes, each index's bytes, and directional received
notification counts.

Allocated relation size does not fall merely because PostgreSQL deletes rows.
Record logical live rows and payload bytes immediately after the sweep, plus heap
and index allocation before workload and after workload. Then, only in the
disposable measurement database, run `VACUUM (FULL, ANALYZE)` on
`secondbox.data_plane_frames` and record the compacted heap and index sizes as
physical reclaimability evidence. Repeat fixed workload/sweep/vacuum cycles to
show whether allocation plateaus or continues growing. The fresh database and
fixed cycle count make comparisons repeatable. Keep the raw JSON result with the
human-readable Result table.

Record the baseline figures in this plan regardless of whether the behavior
tasks proceed.

### Task 2: Pin current retention and replay behaviour

Assert the actual baseline for every session kind and terminalization path:

- admission initializes `retain_until` from `created_at`;
- normal Exec/File/Terminal completion moves it to `completed_at + retention`;
- Runner loss and generation fencing apply their existing `GREATEST` rule;
- Port completion retains the admission value;
- `exec-stream` reconnect after completion/cancellation replays ordered output and
  outcome from sequence zero;
- Terminal reattach within the detach window replays from sequence zero, and an
  attached Terminal receives output and outcome committed immediately before the
  handler's authoritative read;
- relay Port terminal delivery remains readable until acknowledged, expired, or
  today's session-retention bound, including a short-retention/long-Port case;
- replaying an admission while its frames remain returns the exact same terminal
  result and `nextClientSequence`.

These tests distinguish session retention from frame retention so Task 3 changes
only the latter.

### Task 3: Add the separate frame lifecycle

Add `frames_retain_until`, `frame_cleanup_completed_at`, the partial sweep index,
the durable client-sequence projection, and the compact Port
acknowledged-sequence projection in additive migration `0007`, with migration
scope tests. Add the terminal inbound sequence and payload-hash projections used
for exact Runner terminal duplicates after cleanup. Backfill existing rows with
`frames_retain_until=retain_until` and derive each applicable projection from
their retained frames before making the required columns non-null.

Extend the deterministic migration tests to prove that inbound inserts retain
their per-frame `data_plane_session` notification, outbound inserts retain the
idle-to-pending `runner_command` rule, and frame deletion plus session horizon or
projection updates emit neither notification. Keep the documented allowance for
duplicate outbound hints from concurrent insert transactions.

Initialize the frame horizon at admission and update the sequence projection in
every outbound-frame transaction, using it rather than retained rows to allocate
the next sequence. Refresh the frame horizon whenever the session horizon changes
unless that kind is being shortened by policy. Make every permitted frame insert
lock the session and atomically reopen completed frame cleanup. Apply the
per-kind frame rules above in every normal and forced terminalization path.
Extend Terminal attachment cleanup and relay Port acknowledgement/expiry so
removal of the last public consumer lowers the horizon transactionally. Reorder
Runner-loss projection updates and audit every mixed session/Port/frame path for
the canonical lock order. Do not modify `retain_until` beyond its current
behavior.

### Task 4: Replace cleanup with the bounded two-phase sweep

Implement the canonical session-before-frame order, delivery-claim
linearization, bounded frame phase, and frame-empty session phase. Prove that:

- buffered Exec and File frames disappear after completion while their outcomes
  and admission replay remain readable until `retain_until`;
- an exact Runner terminal retransmission after frame cleanup is accepted from
  the compact sequence/hash evidence, while a changed duplicate is rejected;
- the stored client sequence and idempotent descriptor are unchanged by frame
  deletion;
- normal completion after the admission-time frame horizon gives streaming Exec
  and an attached Terminal the full `completed_at + retention` fallback;
- a Terminal with no attachment is prunable after terminalization, while an
  attached Terminal retains its final frames until attachment cleanup;
- a control-plane crash before Terminal cleanup falls back to the existing
  horizon rather than deleting frames early;
- streaming Exec frames remain replayable for the full current horizon;
- relay Port frames remain until terminal acknowledgement, expiry, or the
  unchanged session-retention bound, while direct-Port control frames become
  prunable at terminal projection;
- an exact relay Port acknowledgement remains idempotent after its terminal frame
  is pruned, using the compact acknowledged-sequence projection;
- a claimed outbound frame terminalized between transport send and delivery mark
  is not pruned while its claim is active. At expiry, test both serialized
  outcomes: marking wins and succeeds, or cleanup wins and the later mark receives
  `ErrRelayDeliveryClaim`, without a deadlock;
- more than `limit` eligible frames are removed across immediate sweeper loops,
  while frame rows, session candidates, and frame-cleanup finalization remain
  bounded per transaction and contribute to the progress return;
- cumulative stored `payload_bytes` stops a batch at the compiled byte budget,
  and one first row already larger than that budget — stored before the budget
  existed, or under a larger frame bound — is still removed alone so cleanup
  makes bounded, observable progress rather than stalling on it forever;
- both cleanup-before-append and append-before-cleanup interleavings preserve the
  durable outbound sequence, reopen cleanup when necessary, and cannot strand a
  post-terminal discarded Exec frame;
- exercise `CloseConnection`/`failDisconnectedRunnerDataPlaneSessions` racing a
  relay Port byte acknowledgement in both serialization orders. If acknowledgement
  wins, its consumed-frame and acknowledged-sequence updates plus credit-frame
  append commit before disconnect terminalization. If disconnect wins, the later
  acknowledgement still advances the compact acknowledged sequence, allocates
  the late credit frame from the durable producer sequence, preserves that
  frame's existing pending-state behavior, and reopens frame cleanup. In both
  orders assert the final Port/session projections, no deadlock, and eventual
  removal of the late credit frame by the bounded sweep;
- a due session row is deleted only after bounded cleanup has removed every
  frame; the session phase never performs a residual bulk frame delete.

### Task 5: Prove replay, delivery, and fencing are unchanged

Cover streaming Exec reconnect before and after terminal outcome, Terminal
reattach within and after the detach window, attached final-frame delivery,
control-plane restart mid-session and between terminal commit and public read,
Runner reconnect, generation fencing, deadline and explicit cancellation, Port
close and expiry, exact admission replay, Runner terminal retransmission after
cleanup, terminalization racing an in-flight outbound delivery mark, post-cleanup
frame insertion, and Runner disconnect racing Port acknowledgement. Exercise
detachable and non-detachable Terminal paths and relay and direct Port paths.

### Task 6: Re-measure the implemented change

If Tasks 3–5 proceed, repeat Task 1 with the same checked-in parameters and fresh
database procedure. Record baseline and changed figures side by side, including
logical post-sweep rows/bytes and compacted physical sizes. Report inbound and
outbound live notification counts separately against their documented trigger
rules; do not require exact before/after equality because concurrent outbound
inserts may legally produce duplicate hints. The deterministic migration tests,
not a timing-sensitive live count, prove that retention cleanup adds no
notifications and changes no trigger scope.

### Task 7: Publish evidence and revise affected documents unconditionally

Always add a Result section to this plan with Task 1's figures and decision. If
the behavior change proceeds, also include Task 6's comparison; otherwise record
that the measured cost did not justify the migration.

Publishing Task 1 evidence and correcting the prior wakeup plan remain
unconditional and do not wait for the configuration-surface work. If the
retention behavior proceeds, satisfy the configuration-surface prerequisite
before completing the operator-facing retention deliverables or shipping the
behavior; do not merely name future artifacts.

Update `docs/plans/2026-07-31-relay-data-plane-wakeups.md` so **Not measured**
links to the recorded evidence. If the behavior change proceeds, update
`docs/design/runner-protocol.md` to describe separate frame and session horizons,
the per-kind replay rules, compact duplicate evidence, and the two-phase sweep;
update `docs/design/api-conventions.md` so Terminal `nextClientSequence` names
the durable session projection rather than the retained outbox as its sole source
of truth; and update `docs/design/service-boundaries.md` plus
`docs/plans/2026-08-01-configuration-surface.md` to distinguish operator-owned
transition-specific session deadlines from the maximum frame fallback. Add the
same positive operator-facing explanation to `docs/operations/deployment.md`,
the deployment-manifest schema and retention-field help, the generated
`deploy/secondbox.example.toml`, and redacted `secondbox-deploy inspect` output.
Do not update `deploy/environment.example`: the configuration-surface plan
replaces it as an operator input. Update
`docs/design/security.md`, `docs/design/threat-model.md`, and
`docs/design/networking-and-ports.md` to state which materialised evidence
survives until session cleanup, which payload frames may be removed earlier, and
the resulting forensic/replay boundary. If the behavior change does not proceed,
leave those design semantics unchanged.

## Result

The measured relay cost justified the separate frame lifecycle, so Tasks 3–6
proceeded. The reproducible harness ran the same three-cycle workload against a
fresh PostgreSQL 18.4 database on a qualified KVM/TUN/Btrfs host: a 512-byte PTY
input with 4,096 output bytes, a 1 MiB File upload and download, and a 1 MiB
forced-relay Port exchange in 16 KiB public frames. The baseline used `main` at
`f781e7c` with the measurement-only harness overlaid; the changed run used this
implementation. The raw reports are
[baseline](evidence/2026-08-01-relay-retention-baseline.json) and
[changed](evidence/2026-08-01-relay-retention-changed.json).

Each baseline cycle left all 290 new frame rows and about 4.32 MB of encoded
payload live after the sweep. The changed sweep left no frame from the measured
sessions. `VACUUM (FULL, ANALYZE)` makes the physical consequence visible:

| Cycle | Baseline post-sweep rows / payload | Changed post-sweep rows / payload | Baseline compacted heap / indexes | Changed compacted heap / indexes |
| --- | ---: | ---: | ---: | ---: |
| 1 | 290 / 4,323,982 B | 0 / 0 B | 262,144 / 131,072 B | 0 / 24,576 B |
| 2 | 290 / 4,323,983 B | 0 / 0 B | 516,096 / 196,608 B | 0 / 24,576 B |
| 3 | 290 / 4,323,982 B | 0 / 0 B | 770,048 / 278,528 B | 0 / 24,576 B |

The per-cycle row/payload columns cover that cycle's four measured sessions;
the relation-size columns are cumulative for the disposable database. Baseline
compacted heap allocation therefore grew on every identical cycle, while the
changed relation returned to an empty heap and minimum 8 KiB allocation for each
of its three indexes.

The two runs are comparable after the sweep but deliberately not before it. Each
report also records `framesAfterWorkload`, which is 290 rows and 4,323,982 B on
the baseline against 260 rows and 2,209,632 B on the changed run. That gap is the
change working rather than a lighter workload: buffered Exec and File take a
completion-time frame horizon, so their frames are already eligible and are
removed by a concurrent sweep before the post-workload sample is taken. The
workload parameters are identical and checked in, and both runs deliver the same
PTY, File, and Port bytes. Only the post-sweep columns above compare the same
state on both sides.

The baseline observed 456 inbound `data_plane_session` notifications and 207
outbound `runner_command` notifications; the changed run observed 456 and 210.
The equal inbound count matches the per-frame rule. The small outbound variation
is permitted because concurrent inserts may both observe the idle-to-pending
transition. Deterministic migration tests separately prove that horizon,
projection, frame-delete, and cleanup-finalization mutations emit no notification
and that the existing frame-insert triggers retain their scope.

The implementation retains materialised buffered Exec/File results, terminal
state, admission replay, producer sequence, exact terminal duplicate evidence,
and Port acknowledgement state on compact session projections. Focused
PostgreSQL tests prove those projections remain usable after frame cleanup, and
the qualified live run proves PTY, File, and relay Port delivery end to end.

## Alternatives rejected

- Reusing `retain_until` as the frame horizon. The existing sweep treats it as
  the session horizon and deletes `data_plane_sessions` with the frames. That
  removes materialised results and admission idempotency instead of narrowing
  only frame retention.
- Reusing `SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES` as the sweep's byte
  budget. It needs no new value, but it overloads a session-size limit as a
  cleanup batch width, so raising the permitted session size would silently
  widen every cleanup transaction. The two quantities have no reason to move
  together, and this plan's own non-goals keep operator-owned data-plane settings
  unchanged in meaning.
- Putting a retention timestamp on every frame. It permits finer-grained Port
  cleanup, but adds an indexed policy field and more updates to the hottest table.
  Session-level terminal batches capture the justified savings without that
  write and index cost.
- Limiting the change to buffered Exec and File. It is the smallest safe behavior
  change, but does not address the measured PTY and relay Port retention once
  their final public consumers are gone. The explicit per-kind horizon retains
  the same safety while covering those cases.
- Not persisting frames for non-detachable sessions at all. Frames are the
  delivery cursor, not only the replay log, so a live non-detachable caller reads
  from them. Removing the write requires replacing delivery.
- Shortening `SECONDBOX_DATA_PLANE_RETENTION_SECONDS` globally. It penalises the
  detachable sessions that the retention window exists for, and leaves the
  non-detachable majority on the same clock as the sessions that need it.
- Deleting frames inline at session completion. It puts a bulk delete on the
  completion path, where it competes with the terminal-state write; the sweep at
  `postgres_relay_cancellation.go:148` already owns this and is the reason
  `retain_until` exists.
- Changing the sweep predicate to test replay capability directly. It would put
  the policy in the sweep, where every future caller of the relay would have to
  know about it, instead of in the one place that already decides a session's
  retention.
- Moving payloads to the object store. It adds a network round trip to the
  delivery path, replaces row pressure with object pressure and a second
  lifecycle, and contradicts the rule that S3-compatible storage owns Artifacts
  and immutable execution assets only.
- Adding a new durable consumer cursor for Exec or Terminal so retention can
  follow every read. The relay wakeup plan already rejected that write on the
  guest-output path. This design uses Terminal attachment ownership and Port's
  existing `consumed_at` acknowledgement, while streaming Exec retains its
  current horizon.
