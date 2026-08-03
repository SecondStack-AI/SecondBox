# Runner decommissioning

Decommission a Runner only after every Sandbox homed there is stopped and its
Workspace has either been relocated or deliberately deleted. Runner drain alone
does not move data. Keep the source Runner connected, its stable identity
available, and its Workspace root mounted until every relocation Operation has
completed successfully.

## Procedure

1. Put the source Runner into drain and wait for its active Assignments to
   release. Stop each remaining Sandbox through the ordinary lifecycle API and
   wait for the stop Operation to succeed.
2. List retained Snapshots for each Sandbox. Relocation refuses any Snapshot
   whose state is not `deleted`; export or otherwise preserve required content,
   then delete those Snapshots and wait for their Operations to complete.
3. Choose a named target Runner, or choose the Sandbox's pinned RunnerPool. The
   target must be healthy, connected, non-draining, reflink-capable, compatible
   with the pinned ProfileRevision, and have sufficient CPU, memory, disk,
   Instance, and operation capacity.
4. Read the Sandbox and retain its current `ETag`. Submit the relocation with a
   unique `Idempotency-Key` and that `ETag`:

   ```http
   POST /v1/sandboxes/{sandboxId}:relocate
   If-Match: "{revision}"
   Idempotency-Key: {unique-key}
   Content-Type: application/json

   {"targetRunnerId":"{runnerId}"}
   ```

   To let SecondBox select within the pinned pool, send
   `{"runnerPool":"{poolName}"}` instead. Exactly one selector is required.
5. Poll the returned Operation. Do not shut down or remove the source Workspace
   root while it is pending or running. Success means the target durably
   imported and checksum-verified the image, PostgreSQL changed the home, and
   the source durably deleted its local Workspace state.
6. Start the Sandbox on its target home if qualification requires it. After all
   source-homed Sandboxes have succeeded or been deleted, verify that no
   Workspace row names the source Runner. The host may then be removed.

## Failure handling

Before the home-assignment transaction, the source image remains the only home
and stays sealed against writers. A terminal transfer failure queues a durable
unseal and fails the Operation only after the original source is writable
again. A control-plane or Runner reconnect restarts transfer from the sealed
source; image chunks are not resumed from PostgreSQL or object storage.

After the target's checksum-verified receipt commits the home assignment, the
source stays sealed and only source deletion remains. Reconnect recovery replays
that deletion. Do not manually copy, rename, or remove WorkspaceStore files to
unstick either phase. Inspect the Operation, Workspace mutation, Runner
connections, and relocation command receipts, restore the affected Runner
connection, and let reconciliation continue.

Relocation cannot recover a lost source disk. If the source Workspace root is
already unavailable, restore the Runner identity and Workspace root together
from an operator-managed consistent backup before retrying.
