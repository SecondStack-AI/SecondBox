# Recovery and reconciliation

PostgreSQL desired state is the control-plane authority. Reconcilers are
restart-safe workers that claim bounded work transactionally and emit durable
Operations and audit evidence. Process memory, HTTP connections, and a
particular control-plane replica never own a Sandbox. The home Runner's local
WorkspaceStore is the authority for Workspace image state.

## Reconciliation model

Each mutable record has a revision. A reconciler reads state, computes one
idempotent action, claims it with a compare-and-swap transaction, performs
bounded external work, then commits evidence only if the claim and revision
remain current. Duplicate, late, or reordered Runner results are matched by
effect or operation ID, Assignment, generation, and fencing token.

Multiple replicas may scan the same Sandbox, but only one claim commits. A
crashed worker leaves an expiring claim that another replica resumes from
durable database state and Runner receipts. Retry classification is explicit:
transient transport and dependency failures may retry within operation policy;
admission, compatibility, fencing, local-integrity, and authorization failures
are terminal until an operator or desired-state change addresses them.

One durable Workspace mutation slot serializes start, stop, Snapshot create and
delete, restore, relocation, and Sandbox deletion. Transactions lock Sandbox, Workspace,
then Snapshot when present. A pending start retains the slot until ready or
terminal failure. Every stop-producing path adopts a stable stop effect ID and
retains the slot through compute detach, local generation advance, and the
matching PostgreSQL generation commit.

Runner-facing mutations pass through durable effects. Each local-storage command
has a stable effect ID, expected generation, and current authoritative home Runner. The
Runner persists a receipt before acknowledging the result. A control-plane crash
before database commit therefore replays the same command and converges on the
same receipt instead of repeating or guessing the filesystem mutation.

Assignments carry a reconciliation owner, expiring claim, next-action time,
retry counter and limit, operation deadline, failure class, and revision. A
dedicated Assignment worker runs beside Sandbox lifecycle reconciliation, marks
heartbeat-expired Runners offline, and claims due rows with
`FOR UPDATE SKIP LOCKED`. PostgreSQL serialization failures retry only within
the caller's explicit bound.

## Control-plane and dependency restart

After a control-plane restart, reconcilers rebuild no authority from memory.
They inspect nonterminal Operations, Workspace mutation slots, current Runner
connections, Assignment leases, desired and observed Sandbox state, and durable
local-workspace results.

A PostgreSQL outage rejects mutations and pauses reconciliation. The system does
not continue from stale caches. Ordinary Sandbox start, stop, Snapshot, and
restore depend on PostgreSQL plus the owning Runner's local state.

If a Runner has completed a local mutation but PostgreSQL did not commit it, the
replayed receipt drives recovery:

- Workspace creation remains pending until the create receipt is recorded.
- Ordinary stop remains pending after local generation advance until
  `finish_stop` commits that exact generation.
- Snapshot creation and deletion remain pending until their local receipts
  finalize the Snapshot row.
- Restore records requested, staged, swapped, database-committed, and finalized
  phases. A swapped receipt commits the new generation before rollback cleanup.
- Workspace deletion remains pending until the home Runner proves all local
  images, Snapshots, staging, rollback, and receipt state has been removed.
- Workspace relocation restarts export while the original source remains sealed
  until a checksum-verified target receipt commits the home change. Before that
  commit, terminal transfer failure queues source unseal; after it, reconnect
  recovery replays source deletion.

Start stays blocked whenever PostgreSQL and the current-image manifest disagree
about generation.

Natural post-ready Instance termination is an observation, not release proof. A
current fenced terminal event records its bounded reason and makes the Sandbox
due. The retained Assignment must still be stopped and its Workspace attachment
released before local generation advance and `finish_stop`.

## Runner reconnect and loss

A reconnecting Runner reports its stable identity, new connection ID, active
Assignments, current Workspace inventory, generation manifests, and
unacknowledged local-operation receipts. The control plane accepts only evidence
matching the Workspace's current home and database authority. Stale
compute receives fence-and-cleanup commands. Missing or conflicting local data
becomes an explicit operator-visible failure; it never triggers empty Workspace
creation.

Heartbeat expiry marks the Runner unavailable and makes its Sandboxes
unavailable. It does not prove compute stopped, advance a generation, or
authorize another Runner. No scheduler or automatic recovery path relocates the
Workspace or restores it from object storage. Explicit relocation still
requires the source Runner to be connected and its local image intact.

Recovery requires the same trusted Runner identity and WorkspaceStore to return.
Machine-level recovery must restore the stable Runner identity and
`SECONDBOX_RUNNER_WORKSPACE_ROOT` as one consistent unit. Rebinding those files
to a new Runner ID is outside the v1 contract.

## Lifecycle races

Concurrent requests serialize through the Workspace mutation slot and Sandbox
revision:

- concurrent starts converge on one Instance;
- Snapshot create and restore require a stopped Sandbox and conflict with a
  pending start or stop;
- Snapshot delete may run while compute is active only when no restore references
  it;
- start waits while stop or restore owns the slot;
- delete dominates new start, Snapshot, restore, touch, and data-plane admission,
  but remains pending while the home Runner is unavailable.

Runner-side Workspace locking independently enforces one writer. It is not
weakened by a control-plane race or replica failure.

Lease expiry removes authority for that generation but does not delete a
Sandbox. Before counting useful activity, lifecycle claiming transactionally
expires due Leases and closes sessions bound to released, expired, or fenced
Leases, so abandoned session rows cannot suppress reclamation.

## Snapshot retention

Expired local Snapshots are selected from PostgreSQL and deleted through the
same idempotent home-Runner effect as explicit deletion. Snapshot rows are
removed only after a durable local receipt.

## Operational evidence

Every nonterminal state has a next action, deadline, and observable reason.
Operators can inspect desired state, current mutation, last durable receipt,
retry classification, home Runner health, local-inventory consistency, and
correlation IDs without accessing Workspace content. Reconciliation metrics use
fixed-cardinality state and reason classes.

See [Domain and lifecycle](domain-lifecycle.md), [Runner protocol](runner-protocol.md),
and [Workspace durability](workspace-durability.md).
