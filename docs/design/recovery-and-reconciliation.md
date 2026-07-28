# Recovery and reconciliation

PostgreSQL desired state is the control-plane authority. Reconcilers are restart-safe workers that claim bounded work transactionally and emit durable Operations and audit evidence. Process memory, HTTP connections, and a particular control-plane replica never own a Sandbox.

## Reconciliation model

Each mutable record has a revision. A reconciler reads state, computes one idempotent action, claims it with a compare-and-swap transaction, performs bounded external work, then commits evidence only if the claim and revision remain current. Duplicate, late, or reordered runner results are matched by operation, Assignment, generation, and fencing token.

Multiple replicas may scan the same Sandbox, but only one claim commits. A crashed worker leaves an expiring claim that another replica resumes from durable evidence. Retry classification is explicit: transient transport and dependency failures may retry within operation policy; admission, compatibility, fencing, integrity, and authorization failures are terminal until an operator or desired-state change addresses them.

Revision conflicts and lost reconciliation claims are expected concurrency outcomes, so the corresponding worker immediately retries from fresh PostgreSQL state. Other errors remain visible and stop the worker so process supervision can surface the failed dependency instead of silently spinning.

Sandbox lifecycle reconciliation uses the same model as Assignment recovery. Each pass consumes at most one due Sandbox and records one of the durable actions `materialize`, `start_instance`, `mark_ready`, `drain`, `checkpoint`, `stop_instance`, `finish_stop`, `delete`, `finish_delete`, `wait`, or `fail`. The decision inputs keep guest heartbeat, useful activity, lease authority, current checkpoint publication, and active materialization as separate evidence.

The control-plane process runs this lifecycle claim loop with explicitly configured poll and claim durations. Database-only transitions, including create-to-stopped and deletion of an already stopped Sandbox, complete their Operations in the loop. Runner-facing actions pass through the durable effect broker: scheduling atomically records Instance, Assignment, exclusive preparing materialization, capacity reservation, and Assignment command; readiness records Assignment, guest, and materialization evidence before the lifecycle worker marks the Sandbox ready. Stop and checkpoint commands retain generation- and assignment-fenced authority across restart. A `wait` pass preserves the last runner-facing action instead of implying that it ran.

Assignments carry a reconciliation owner, expiring claim, next-action time, retry counter and limit, operation deadline, failure class, and revision. A dedicated Assignment worker runs beside the Sandbox lifecycle worker, marks heartbeat-expired Runners offline, and claims due Assignment rows with `FOR UPDATE SKIP LOCKED`. PostgreSQL serialization failures are retried only within the explicit scheduler retry bound supplied by the caller.

An Assignment that produces no terminal result by its operation deadline is fenced with the exact Assignment, Instance, Sandbox generation, and token authority before its startup Operation fails. A delivered Fence that produces no terminal result is expired and replaced by a distinct command with a renewed deadline only within the persisted retry bound. Exhaustion records terminal Assignment, Instance, Sandbox, command, and Operation failure evidence without releasing the unproved active materialization or advancing the generation. A late valid FenceResult can still establish release proof, but late readiness cannot revive fenced startup authority.

## Control-plane and dependency restart

After a control-plane restart, reconcilers rebuild no authority from memory. They inspect nonterminal Operations, current runner connections, assignment leases, object publication state, and desired versus observed Sandbox state.

A PostgreSQL outage rejects mutations and pauses reconciliation. The system does not continue from stale caches. An object-store outage prevents checkpoint publication, restore, artifact transfer, and any stop policy that requires a durable checkpoint. It never records a checkpoint as committed without verified reachable bytes.

Checkpointing remains an effect-producing reconciliation state until publication or terminal failure, even when a concurrent intent changes the desired state. If a queued command reaches its effect deadline without a matching terminal, one transaction expires the old command, increments the persisted retry count, installs a new command ID and deadline, and requeues delivery. Reaching the configured retry limit records `checkpoint_retry_exhausted`, moves the effect to `runner_failed`, and fails the old command. Stop and start intents then enter the explicit failed state; delete intent continues through fenced Instance stop and durable deletion because a failed optional checkpoint cannot strand resource removal. Reconnect and replica replacement therefore do not depend on an in-memory timer.

Stopping uses the same durable effect discipline. A missing FenceResult causes a distinct bounded Fence retry with a renewed command deadline. Exhaustion records `stop_retry_exhausted`, fails the final command, and moves the Sandbox to an explicit terminal failure instead of leaving `stopping` nonterminal forever.

Natural post-ready Instance termination is an observation, not a release transition. A current fenced `InstanceTerminal` atomically stores its immutable reason and digest, marks guest liveness stopped, and makes the Sandbox due. A running Sandbox then drains and enters the existing stop effect. No replacement is materialized until a matching FenceResult proves the retained Assignment and mutable materialization released, `finish_stop` advances the generation, and lifecycle reconciliation starts the new Instance. Duplicate evidence is harmless; changed or stale evidence fails.

## Runner reconnect and loss

A reconnecting Runner reports its stable identity, new connection ID, active Assignments, Instances, operation states, and local materialization evidence. The control plane accepts only entries matching current database authority. Unknown or stale entries receive fence-and-cleanup commands.

Heartbeat expiry makes the Runner unavailable and marks its Assignments uncertain. It does not prove compute stopped and never authorizes reassignment. Recovery follows one of two safe paths:

1. the same trusted Runner reconnects with evidence that still matches current Assignment authority; or
2. that Runner returns verifiable stop and materialization-release evidence for the exact fence, after which the control plane advances authority and restores only the last committed checkpoint on another compatible Runner.

The replacement never attaches the old mutable image. V1 makes no claim that writes after the last committed checkpoint survive Runner loss.

An uncertain Assignment transitions to fencing before replacement. A successful FenceResult must match Runner, Assignment, Instance, Sandbox, generation, opaque fencing token, and durable operation correlation, and it must include a termination-evidence digest. Only then does one PostgreSQL transaction release the Assignment, expire its remaining commands, mark the Instance `stopped` with lost guest liveness and stable `runner_lost` reason, fence old-generation Leases, activity, exec/file/port data-plane sessions, and open PortSessions, terminate any still-active materialization, advance Sandbox and Workspace generation together, preserve the current checkpoint, create the durably correlated replacement start Operation, clear the current Instance, and wake lifecycle reconciliation. A restarted control-plane worker resumes this transaction from persisted fence proof. Late Assignment results, Lease use, and data-plane frames for the released generation are rejected.

## Lifecycle races

Concurrent start requests converge on one current generation and Instance. Stop during start changes desired state and drains or fences the startup operation. Start during stop waits for stop and checkpoint policy to finish, then creates a new generation. Delete dominates start, touch, and new data-plane admission.

Drain records its admission barrier before waiting for in-flight work. New work is rejected after that barrier. Drain-grace expiry durably queues current-Assignment cancellation frames for admitted operations. Checkpointing waits for their fenced terminal evidence so guest writes cannot race the immutable workspace image.

Lease expiry removes authority for that generation but does not delete a Sandbox. Before counting useful activity, lifecycle claiming transactionally expires due Leases and closes sessions bound to released, expired, or fenced Leases, so abandoned session rows cannot suppress reclamation. Profile policy decides whether active authority contributes to drain. Ping and lifecycle polling never delay idle reconciliation; explicit touch and currently authorized active sessions do.

## Checkpoint and garbage-collection recovery

Checkpoint staging objects are not reachable until checksum verification and atomic PostgreSQL publication. Reconciliation resumes or deletes abandoned staging uploads. A database record that references missing or corrupt object bytes enters an integrity-failed state and blocks restore; it does not materialize an empty workspace.

Garbage collection marks candidates transactionally, rechecks reachability after its grace, deletes immutable objects idempotently, and records completion. Current Workspace checkpoints, retained Snapshots, Artifacts, active downloads, restore Operations, and backup manifests are roots.

## Operational evidence

Every nonterminal state has a next action, deadline, and observable reason. Operators can inspect desired state, last successful evidence, retry classification, Runner health, checkpoint reachability, and correlation IDs without accessing workspace content. Reconciliation metrics use fixed-cardinality state and reason classes.

See [Domain and lifecycle](domain-lifecycle.md), [Runner protocol](runner-protocol.md), [Workspace durability](workspace-durability.md), and [Compatibility policy](compatibility-policy.md).
