# Backup and restore boundary

SecondBox provides a database-derived recovery bundle and an isolated restore-drill harness. The backup script holds a PostgreSQL publication fence across all control-plane replicas while it captures database and object authority. The repository-owned portable drill restores into a new database and object namespace, starts a fresh control-plane verifier, enrolls a new mTLS Runner authority, and proves that the production scheduler and checkpoint sender stream the expected bytes into that Runner before the restored Sandbox becomes ready.

A truthful recovery point must coordinate PostgreSQL desired state with immutable object-store checkpoints, stop admission, quiesce workspace mutation, fence assignments, and prove that every retained Sandbox or Snapshot points to a reachable checkpoint. A restore drill must load those authorities into isolated targets, enroll a fresh Runner credential, restore a stopped Sandbox, and reject a stale generation before reporting success.

## Database-derived backup

Before running `scripts/backup.sh`, the deployment operator must checkpoint or stop every running Sandbox. The script requires:

- `SECONDBOX_BACKUP_DATABASE_URL`;
- `SECONDBOX_BACKUP_DIR`;
- `SECONDBOX_BACKUP_RECOVERY_POINT_ID`;
- `SECONDBOX_BACKUP_OBJECT_EXPORT`, an exact provider export of the database-rooted objects.

The script opens one long-lived PostgreSQL transaction and acquires `SHARE` locks on every table in the `secondbox` schema in deterministic name order. Those locks wait for existing writers and then block new application and background writes from every replica while allowing reads and `pg_dump`. With the fence held, the script rejects active Sandboxes, Assignments, lifecycle effects, object publications, data-plane sessions, and Instances. It also rejects dangling retained checkpoint references. From the same database state it builds a canonical object manifest containing every current retained Sandbox checkpoint, every unexpired published Snapshot checkpoint, and every published Artifact. Runner credential serials are recorded so a restore verifier cannot claim an old credential is fresh.

The provider export must contain exactly the manifest paths. Missing files, extra files, symbolic links, unsafe paths, size differences, and SHA-256 differences all fail the backup before publication. The resulting `secondbox-backup/v2` archive contains:

- a custom-format dump of the `secondbox` PostgreSQL schema;
- the object-state archive;
- the database state and database-derived publication-fence, quiescence, fencing, and reachability documents;
- payload checksums and a portable archive checksum that remains valid after the bundle and sidecar move together.

The script queries the authoritative state again after capturing PostgreSQL and object data. A changed root set, quiescence projection, or credential baseline fails the capture. It releases the table-lock transaction only after this comparison. The recorded WAL position identifies the database recovery point; cluster-wide WAL may advance for unrelated databases and is not used as the publication-fence proof.

## Isolated restore drill

`scripts/restore-drill.sh` requires:

- `SECONDBOX_RESTORE_DATABASE_URL`, used only as the maintenance database for a newly created throwaway database;
- `SECONDBOX_RESTORE_BUNDLE`;
- `SECONDBOX_RESTORE_STAGE_DIR`;
- `SECONDBOX_RESTORE_OBJECT_TARGET`, which must not exist;
- `SECONDBOX_RESTORE_CONTROL_PLANE_URL` and `SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN`, bound to a control-plane process using the isolated restored database and objects;
- `SECONDBOX_RESTORE_FRESH_RUNNER_RESULT`, a new identity-result path;
- `SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND`, an executable live-drill harness.
- `SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS`, the bounded verifier startup and termination interval.

The drill verifies the relocatable archive checksum and every internal payload checksum, restores the PostgreSQL schema, extracts the object archive safely, and verifies every restored file against the database-derived object manifest. It then recomputes the reachable object roots from the restored database at the recovery-point timestamp. Missing, extra, or changed restored database roots fail before any Runner verifier executes.

The supervised verifier receives the isolated database URL, object target, object manifest, control-plane URL, and recovery-point identity. It must start or connect the isolated control plane, enroll and connect a fresh Runner, resume a stopped Sandbox from a manifest checkpoint, and atomically publish a `secondbox-fresh-runner-identity/v1` result. `SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS` bounds both verifier startup and termination. The restore script keeps the verifier alive while it performs its independent database and HTTP checks, then terminates and reaps it. A verifier that ignores termination is killed and fails the drill. The result contains identities rather than claimed outcomes:

- the new Runner and credential serial;
- the Sandbox, Workspace, Assignment, materialization, checkpoint, checksum, and authoritative generation used for restore;

The drill rejects a credential serial present in the backup baseline and rejects a checkpoint outside the recovery manifest. It queries the restored database to prove the credential belongs to the Runner, the Assignment is ready on that Runner and generation, the workspace materialization is ready from the named checkpoint, and the Sandbox still points to the matching published checkpoint.

After the database proof passes, the drill calls the restored control plane itself. A current-generation `pingSandbox` request must return the restored Sandbox and generation. The drill then submits the preceding generation to the same endpoint and accepts only the public `409 generation_fenced` problem response. The verifier result cannot assert or bypass either outcome.

`just test-backup-restore` is the repository-owned portable verifier. It creates an isolated source database, captures a real recovery bundle, restores into a newly created database and object directory, starts a fresh control-plane subprocess, issues a Runner credential absent from the backup, negotiates the checkpoint feature over a TLS 1.3 mTLS RunnerControl stream, and drives the production scheduler, lifecycle reconciler, and checkpoint restore sender. The verifier hashes the bytes received on that stream before it records the ready Assignment and materialization. The script then proves current-generation ping and stale-generation rejection through the restored HTTP API.

The portable verifier uses a read-only filesystem adapter for the already restored object namespace and a protocol-level Runner peer. It does not launch Firecracker or claim KVM, jailer, guest filesystem, or real-host recovery qualification. A deployment may supply its privileged Runner verifier through the same supervised command contract; real Firecracker evidence belongs to the qualified-host release gate.

## Qualification boundary

PostgreSQL `pg_dump` alone is control-plane metadata, and a RustFS volume or object export alone is object data. Copying Runner-local state while a VM may write it is neither crash-consistent nor portable.

The portable recovery drill demonstrates all of the following:

- the packaged backup script acquires its shared database publication fence and rejects non-quiescent authority;
- the database snapshot and immutable object manifest are captured while that fence remains held;
- restoration occurs into isolated PostgreSQL and object-store namespaces;
- a newly enrolled Runner restores the expected checkpoint bytes;
- stale Runner evidence is rejected through a real generation-fenced surface;
- a fresh mTLS Runner protocol authority completes the identity handoff and all script-owned positive and negative checks.

This qualifies the coordinated database/object backup and fresh-authority restore mechanics. It does not qualify a privileged Firecracker boot from the restored image; run the dedicated Linux/KVM release gate for that separate claim.

See [workspace durability](../design/workspace-durability.md) and [recovery and reconciliation](../design/recovery-and-reconciliation.md).
