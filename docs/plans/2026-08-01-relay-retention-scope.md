---
title: Relay Frame Retention Scoped to Replay
date: 2026-08-01
status: proposed
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

Every session's frames are retained on one uniform clock.
`internal/runnercontrol/postgres_relay.go:301` sets
`retain_until = created_at + relay.retention` at session creation, from
`SECONDBOX_DATA_PLANE_RETENTION_SECONDS`, regardless of whether that session can
ever be replayed.

It frequently cannot. `internal/runnercontrol/postgres_relay_terminal.go:213`
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
- frames belonging to a session that cannot replay pruned at session completion
  rather than at the uniform retention horizon;
- no change to transport, delivery, ordering, credit, replay, detach, fencing, or
  notification rules;
- a detachable session's replay window unchanged and proven unchanged.

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

### Retention becomes a function of replay capability

`retain_until` is currently fixed at insert. It becomes derived at the point the
session reaches a terminal state, from a property the schema already records:

- a Terminal session that is not detachable, or whose `terminal_detach_seconds`
  is zero, is cancelled at client disconnect and cannot be reattached. Its frames
  become prunable once the session is terminal and its final state is recorded.
- a detachable Terminal session keeps the existing horizon, and while detached
  keeps frames at least until `detach_expires_at`. A detached session's replay
  window is the property this plan must not regress.
- Exec and File keep their frames until the session's terminal state and
  materialised output are committed on the `data_plane_sessions` row, then become
  prunable. The result of an Exec is read from the session row's `stdout_bytes`,
  `stderr_bytes`, and `content_bytes`, not by re-reading frames, so the frames
  are redundant once that materialisation has committed. Replay of a
  *reconnecting* Exec stream is preserved by keeping frames while the session is
  live.

The rule stated once: a frame is retained while its session can still be read
from frames, and pruned when it cannot. The existing uniform horizon remains the
outer bound for everything, so this narrows retention and never extends it.

### Pruning stays in the existing sweep, unchanged

`internal/runnercontrol/postgres_relay_cancellation.go:148` already sweeps both
tables in one transaction, and its predicate is already exactly right:

```sql
SELECT id FROM secondbox.data_plane_sessions
WHERE state IN ('completed','failed','cancelled','expired') AND retain_until<=$1
ORDER BY retain_until,id
FOR UPDATE SKIP LOCKED
```

It is already terminal-state gated, already ordered by `retain_until`, and
already deletes frames before sessions. The only lever this plan needs is the
value of `retain_until`, so the sweep requires no change at all — no new worker,
no new schedule, no new predicate, and no new configuration. This is the reason
the change is narrow: the mechanism exists and is correct; only the horizon it
reads is wrong.

### Measurement precedes the change

Task 1 measures before Task 3 changes anything, because the prior plans'
reasoning about volume is explicitly unvalidated and this plan should not add a
second layer of reasoning on top of it. If measurement shows the durable cost is
already negligible at deployed volumes, Tasks 3 onward are not justified and the
plan stops at recorded evidence.

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
- Changing `SECONDBOX_DATA_PLANE_RETENTION_SECONDS` as a value. It remains the
  outer bound; see [Configuration Surface Split](2026-08-01-configuration-surface.md)
  for where the value lives.
- Removing `stdout_bytes`, `stderr_bytes`, or `content_bytes` from
  `data_plane_sessions`. Those are the materialised result and are the reason
  frames become redundant.

## Validation Commands

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- focused relay retention, detach, and replay tests
- `just test-scenario` with real KVM, TUN/TAP, and Btrfs Workspaces
- `just test-stress` for sustained-output volume

## Tasks

### Task 1: Measure the durable cost

Record, against a live workload rather than a fixture: row count, payload bytes,
table and index growth, and notification count for an interactive PTY session, a
large File transfer, and a saturated relay Port session. This closes the gap both
prior plans record as unmeasured, and the figures belong in the Result section
regardless of whether the rest of this plan proceeds.

### Task 2: Pin current retention behaviour with a test

Assert the present uniform `retain_until` for each session kind and for both the
detachable and non-detachable Terminal paths, so Task 3 changes behaviour
visibly. Include a detached session's replay after reattach, which is the
property most at risk.

### Task 3: Derive retention from replay capability

Set `retain_until` at terminal state from the session's replay capability, per the
rule above. Keep the uniform horizon as the outer bound so retention only ever
narrows. Prove that a non-detachable Terminal session's frames are prunable once
terminal, that a detachable session's are not before `detach_expires_at`, and
that an Exec result remains readable from the session row after its frames are
pruned.

### Task 4: Prove replay and detach are unchanged

Cover reattach within the detach window, reattach after expiry, control-plane
restart mid-session, Runner reconnect, generation fencing, and cancellation, each
against both the detachable and non-detachable paths.

### Task 5: Re-measure and record

Repeat Task 1 against the change and record both figures side by side, following
the precedent set by the direct Port and control-plane wakeup plans.

### Task 6: Revise the affected documents

Update `docs/design/runner-protocol.md` to state that relay frame retention is
bounded by replay capability, and correct the **Not measured** section of
`docs/plans/2026-07-31-relay-data-plane-wakeups.md` to point at the figures from
Task 1.

## Alternatives rejected

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
- A durable consumer cursor so retention can follow reads. The relay wakeup plan
  already rejected adding one, on the grounds that it puts a write on the
  guest-output path; that objection applies unchanged here.
