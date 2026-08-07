# Runner protocol

The runner protocol is the authenticated control-plane-to-runner boundary. Its compatibility and release cadence are independent from the public API and guest-agent protocol.

`contracts/runner/v1/runner.proto` is the canonical schema. Generated code and a committed descriptor set derive from it. Frozen binary fixtures cover every released message generation, and breaking-change validation compares the schema with the latest supported release.

## Connection and identity

A runner presents the deployment's pre-shared Runner credential over a mutually authenticated TLS 1.3 connection. Its CA-signed client certificate carries the stable Runner identity; a message cannot claim a different Runner. The HTTP platform token is never accepted on this channel.

The shared credential is configured explicitly on the control plane and every trusted Runner; PostgreSQL stores neither the credential nor a credential lifecycle. `secondbox-deploy runner-init` issues one create-only client identity for an immutable manifest-declared Runner ID, and its certificate carries `spiffe://secondbox/runner/<runner-id>`. The RunnerPool is reported during registration and must already exist in a registration-accepting state. The control-plane server certificate is configured separately.

The supported Runner protocol window is a compiled fact, not deployment configuration. Identical constants live beside both independently built generated protocol packages, the generation verifier rejects drift, and both implementations use those constants for negotiation. Supported peers select that window; adjacent versions are rejected as unsupported.

Unknown, empty, or mismatched shared credentials are rejected before protocol negotiation. CA verification and the certificate identity are also mandatory. Rotating the deployment-wide credential or Runner CA is an operator-coordinated replacement of the affected control-plane and Runner secret material; no database-backed enrollment, rotation, or revocation workflow exists.

The runner dials the control-plane endpoint and maintains a versioned bidirectional gRPC stream. This outbound connection supports runners behind ordinary host firewalls and NAT. The control plane does not call an unauthenticated runner HTTP port.

The first exchange negotiates a protocol version and connection ID. Unsupported versions fail before registration or mutation. Reconnect creates a new connection ID but preserves stable Runner identity. Duplicate or reordered messages are handled by operation ID and sequence number rather than connection lifetime.

The Runner owns an explicit reconnect loop with exponential delay bounded between 250 milliseconds and 30 seconds. A transient connect, send, receive, or control-plane restart error closes only the current transport session; it does not stop or fence Runner-owned Instances. The Runner retains active Assignment and correlation summaries in memory, repeats readiness registration on the new connection, and immediately sends a heartbeat that re-advertises those summaries before consuming more commands. A successful registration resets the delay. Terminal authentication, authorization, and protocol-negotiation failures stop the Runner, as does operator context cancellation; the process composition root shuts down the compute backend only after `Run` returns.

## Registration and capacity

Registration reports:

- stable Runner and RunnerPool identity established by the certificate;
- software and runner-protocol versions;
- architecture and kernel evidence;
- verified KVM, Firecracker, jailer, cgroup, networking, storage, cleanup, and caller-facing data-plane capabilities;
- whether the Runner can start a Sandbox by resuming a prepared guest;
- supported guest-protocol generations;
- verified immutable image and toolchain cache entries;
- allocatable and currently reserved vCPU, memory, disk, Instance, and operation capacity;
- the advertised caller-facing Port data-plane address.

The runner performs prerequisite and trust validation before advertising schedulable capacity. Instance capacity and concurrent data-plane operation capacity are independent bounds; Profile operation limits reserve the latter while a Sandbox Instance is active. Missing KVM, required network controls, trust anchors, storage health, cleanup capability, or a bound caller-facing data-plane listener make it unready.

Resume capacity is optional evidence rather than a prerequisite. A Runner reports it only when it is configured with a resume template cache root, requires the jailer, and already holds a template built from the exact signed execution bundle it verified; the control plane then records the provider-neutral `snapshot-resume` capability, which is the only way a `snapshot_resume` ProfileRevision is admitted. A Runner without it registers and schedules normally for `cold_boot` Profiles. Each Assignment carries the ProfileRevision's startup mode, and a Runner that cannot honour the stated mode fails the Assignment before creating any Workspace, TAP, or jail rather than substituting the other mode. The control plane treats claims as evidence for scheduling, not as authority to change a ProfileRevision.

Heartbeats carry monotonically increasing sequence numbers, connection identity, capacity, active assignment summaries, and drain state. A heartbeat is runner liveness; it is not Sandbox useful activity.

The advertised data-plane address is administrative capacity evidence of the same class as advertised capacity: a dialable `host:port` carrying no Sandbox identity. The control plane stores it and returns it only inside a direct PortSession endpoint issued to an application authority holding the exact `sandbox:ports:direct` scope.

Registration and heartbeats also carry the retained Sandbox startup sample count and p95 already maintained by the Runner. The count is bounded by the Runner's 256-sample window rather than being a process-lifetime counter. This provider-neutral timing evidence is administrative capacity information; it contains no Sandbox identifiers or backend references.

Connections and message envelopes are persisted. A reconnect supersedes the prior connection ID without changing the Runner identity. Duplicate message IDs are idempotent; a new message at an old or repeated sequence is rejected. The Runner allocates each connection-global control sequence while holding the same lock that sends its frame, so concurrent heartbeats and lifecycle results cannot reach the stream out of sequence. Per-operation Exec, File, PTY, and Port messages retain independent stream sequences. This ordering evidence remains effective when a runner reconnects to a different control-plane replica.

## Assignments and fencing

An assignment command contains the immutable ProfileRevision requirements, exactly one runtime component reference and one toolchain component reference, exact deny-all or allow-list network policy, Sandbox, Workspace, and Instance IDs, expected generation, opaque fencing token, operation deadline, and correlation IDs. The assignment is delivered only to the Workspace's authenticated home Runner and names no source image or host path. Each component reference carries its catalog-resolved artifact ID, component-manifest digest, signing-key ID, architecture, guest-protocol generation, and mandatory guest features. Network destinations carry one exact domain or CIDR plus protocol and port; Runner-local DNS pin bounds and protected management addresses remain local admission inputs rather than Profile-controlled exceptions. The assignment contains no HTTP platform token or end-user identity.

The released Firecracker artifact is one signed execution bundle because the toolchain is physically embedded in its rootfs/shared-image set. Its signed top-level manifest binds two distinct component manifests: runtime and toolchain. Profile `runtimeBundleDigest` and `toolchainBundleDigest` values name those component-manifest digests rather than aliasing the top-level manifest. Runner readiness advertises both exact component identities. Admission requires both assignment references to match the locally verified component descriptors and trusted key; a missing component, mutable tag, digest/key/architecture/generation/feature substitution, duplicate runtime reference, or arbitrary pull fails before VM mutation. Guest negotiation receives the two component digests independently.

The runner validates the full assignment and resolves the current local Workspace attachment before acknowledgement. It returns either an accepted capacity reservation or a typed startup rejection. Acceptance does not make the Instance ready. Progress events cover artifact verification, Workspace attachment, network setup, Firecracker launch, guest negotiation, and readiness.

Every runner message that can mutate compute state includes assignment ID, Sandbox generation, and fencing token. The control plane rejects evidence from a superseded assignment. A runner receiving an explicit fence command stops admitting work, terminates the old Instance, releases the Workspace attachment only after every host-side user has stopped, and returns bounded proof. It never continues work under a newer generation by inference.

`InstanceTerminal` reports a post-ready runtime observation under the retained Assignment fence and full request, operation, Sandbox, Instance, generation, Assignment, optional Lease, and Runner correlation. Its bounded reason is only `guest_shutdown`, `resource_exhaustion`, or `internal_failure`, and it carries an immutable evidence digest rather than logs or counters. Exact replay is idempotent; changed reason, digest, time, or correlation is rejected. The event records Instance state and wakes reconciliation only. It is not release proof and cannot clear Assignment or Workspace-attachment authority, advance a generation, or schedule replacement.

Local-workspace commands are logical, versioned, and idempotent. They cover Workspace create, inspect, generation advance, and delete; Snapshot create and delete; restore prepare, swap, and finalize; and inventory reconciliation. Every mutation carries Sandbox, Workspace, optional Snapshot, expected generation, fencing identity where applicable, and a stable effect/operation ID. The authenticated session supplies the home Runner identity. A Runner rejects a command for data it does not own.

Local-workspace results contain bounded typed status and opaque receipt evidence only. The control plane binds create and clone results to the durable Workspace mutation, effect kind, and exact source Snapshot before accepting the receipt. No ordinary lifecycle protocol message carries Workspace image bytes, host paths, object-store credentials, storage keys, or provider-specific attachment details. Protocol generation 2 adds the sole exception: an operator-authorized stopped-Sandbox relocation forwards bounded image chunks between its authenticated source and target Runner streams. The control plane applies a 64 KiB chunk bound and one MiB credit window and never persists those bytes. A Runner persists each seal, import, abort, or deletion receipt before acknowledgement and replays an unacknowledged result after reconnect until the control plane records it durably.

Initial Sandbox creation applies hard immutable ProfileRevision requirements, compatible RunnerPool, architecture, guest-protocol generation, provider-neutral capabilities, health, drain barrier, storage readiness, and free CPU, memory, disk, Instance, and operation capacity, then uses stable deterministic tie-breaking. A snapshot-seeded Sandbox applies the same capacity envelope to the Snapshot's home Runner because relocation refuses retained Snapshots. The selected stable Runner ID becomes the Workspace's authoritative home before Workspace creation is dispatched. Every later Instance assignment targets the current exact home; artifact-cache locality may inform only the initial choice and never permits automatic relocation. Assignment creation locks the Sandbox generation and home-runner capacity in one serializable PostgreSQL transaction. The durable Assignment records Runner, ProfileRevision, generation, fencing token, deadlines, and retry bounds without exposing backend or host-storage details publicly.

Relocation locks Sandbox then Workspace, verifies stopped state and an empty
retained-Snapshot set, reserves the Workspace mutation slot, and validates the
target before dispatch. The source export command atomically seals the current
manifest and persists a receipt before its stream opens. The target writes into
operation-scoped staging, honors sequential offsets and credit, validates size,
SHA-256, ext4 identity, and capacity, fsyncs, atomically publishes, and persists
an import receipt before returning success. Only that success permits the
PostgreSQL transaction that changes the single home assignment and queues
source deletion. A disconnect before the home transaction restarts export from
the sealed source or queues source unseal; a disconnect after it replays source
deletion. At no point do both Runners hold assignment authority for the image.

## Operations and streams

The connection multiplexes lifecycle commands, exec, filesystem transfer, PTY, port tunnels, logs, and evidence with globally unique operation and stream IDs. Each admitted Exec, File, and Port opening carries its durable per-operation correlation on the frame envelope: request, operation, Sandbox, Instance, generation, Assignment, optional Lease, and selected Runner IDs; the reserved PTY frame has the same correlation shape. The Runner rejects missing or inconsistent opening correlation, retains it for the operation lifetime, and returns it on output and terminal frames, including terminal replay after a connection interruption. Port ingress also compares every returned frame with the PostgreSQL-pinned session correlation before accepting credit, bytes, or terminal state. Each direction uses per-stream sequence numbers. Byte-producing streams use explicit credit frames; a sender cannot exceed granted credit. Control frames have bounded priority so cancellation and fencing cannot be starved by output.

Cancellation is an acknowledged guest-directed operation. Transport disconnect does not imply successful cancellation. On reconnect, the runner reports active operations and terminal results so the control plane can reconcile without replaying a completed mutation.

Terminal results distinguish guest outcomes from admission, fencing, guest-agent, runner, local-workspace, and infrastructure failures. Assignment results and rejections, Exec, File, and Port terminals, local-workspace effects, natural Instance termination, fencing, host network-policy failure, and instance teardown emit the same schema-versioned Runner evidence record. Its fixed shape carries only event/outcome classifications, observation time, and request, operation, Sandbox, Instance, generation, Assignment, Lease, and Runner identifiers. Lifecycle operations omit only Lease when no Lease exists. The record type and runner-protocol `Evidence` message has no fields for credentials, fencing tokens, commands, environment values, stdin/stdout/stderr, file paths or content, checksums, network destinations, or workspace data, so those values cannot be accidentally serialized as evidence. Definitive local completion or failure emits evidence before its terminal protocol frame, and evidence sink failure is surfaced rather than silently dropping required evidence.

Operation evidence is emitted as structured audit/log data, not metrics. Metrics retain only bounded dimensions such as terminal class and do not label by any correlation identifier.

The root data-plane boundary is PostgreSQL-authoritative admission followed by the `SBXDP1` direct or proxied transport. Admission locks the subject and Sandbox operation budgets, resolves the current ready Assignment, pins its generation and fencing token, validates an optional Lease, and atomically records the activity session. Idempotency is scoped by tenant, subject, operation, Sandbox, and key; a changed request hash conflicts. PostgreSQL stores lifecycle, accounting, terminal outcome, and bounded one-shot results, but never streaming payload bytes.

The direct transport connects the caller to the Runner's TLS listener after credential consumption. The proxied transport forwards bytes in memory between the caller and the Runner's existing authenticated outbound control connection. Both apply ordered operation sequences and credit windows without PostgreSQL payload storage. Runner and client disconnects fail the affected live stream explicitly; durable session state remains available for reconciliation.

A PortSession admitted for the direct transport receives a `PortDirectOpen`: it carries the guest port, protocol, named port, deadline, Lease, and the digest of the single-use credential, so the Runner holds the assignment-bound state needed to reject an unauthenticated peer locally in constant time. The Runner then spends the credential with one `PortDirectConsume`, answered inline on the same authenticated stream by a non-durable `PortDirectAdmission`, and only then bridges bytes on a live socket. The proxied transport performs the same PostgreSQL credential spend before upgrading the caller's WebSocket and forwarding bytes in memory. See [Networking and ports](networking-and-ports.md).

Deadlines and byte-limit violations move an operation to `cancelling`, enqueue a payload-free Runner cancellation command, and retain active-session authority until a current-fence terminal proves the guest work stopped. Draining rejects new admission but continues accepting ordered output and terminal proof for work admitted under the current Assignment. A generation fence terminalizes every remaining generation-bound data-plane session before replacement.

The session `retain_until` is authoritative for the session row, bounded buffered Exec/File result, terminal outcome, and admission replay. Streaming Exec, File, PTY, and Port payloads are never retained in PostgreSQL. PTY detach replay is a bounded Runner-owned in-memory ring and does not survive Runner loss.

Durable session projections preserve lifecycle and accounting cursors needed by public descriptors and admission replay. Cleanup uses the session row as the expiry linearization boundary and deletes only terminal sessions whose retention horizon has passed.

The Runner translates Exec, PTY, and File data-plane messages to its retained assignment-bound guest protocol session. Generation 1 supports buffered and live-streaming shell and argv execution with binary stdin plus read, write, stat, direct-child list, exists, mkdir, and remove. Streaming stdin bytes and their explicit `end_of_input` signal, output credit, and cancellation are forwarded in sequence while the guest process is running. A PTY operation opens through `ExecOpen` with `allocate_pty`, then shares one incoming sequence across PTY input, resize, and credit plus Exec cancellation. The Runner emits only typed PTY output and terminal messages for that operation, including admission and fencing failures. PTY output is bounded and stays binary through the guest, Runner, selected transport, and public base64 envelope.

The operation sequence validator treats an exact retransmission of the same EOF sequence as idempotent, records stdin closure with that message, and rejects later stdin. Guest stdout and stderr flow back immediately when credit is available; unused credit remains available for later writes, so a slow client applies backpressure at the guest without accumulating unbounded proxy output. Bounded partial output is preserved before an `output_exhausted` terminal. Exec and PTY terminals preserve the public outcome evidence: exit code, signal, and elapsed time for successful commands; spawn-failure reason and message; elapsed deadline; exhausted byte limit; or typed infrastructure reason and retryability. Binary file content remains bytes end to end. File operations preserve the caller's recursive and force flags, while stat and list return complete kind, size, and modification-time metadata. Runner-protocol generation 1 creates new files with mode `0600`; the guest still verifies the declared size and checksum and commits atomically.

Completed operation state is retained only in a bounded 256-entry tombstone window per operation kind for immediate duplicate terminal handling. Total active plus retained state is capped at 1024 Exec and 1024 File operations; new admission receives a typed capacity failure when the bound is exhausted. A connection loss cancels active guest work, while the durable session outcome determines reconciliation after reconnect.

## Draining and loss

Operator drain prevents new Sandbox homes, new Instance assignments, and new operation admission. Existing assignments follow their profile drain bounds. Ordinary scheduling leaves stopped Sandboxes on the drained Runner and unavailable for start. An operator may explicitly relocate a stopped, Snapshot-free Sandbox from a connected drained source to a healthy, non-draining compatible target. A drained Runner remains connected until it has released or explicitly failed every assignment and completed requested Workspace relocations.

Heartbeat expiry marks the Runner unavailable and makes its Sandboxes unavailable. It never authorizes Workspace reassignment. Recovery requires the same stable Runner identity and local WorkspaceStore to return; see [Recovery and reconciliation](recovery-and-reconciliation.md).

See [Service boundaries](service-boundaries.md), [Guest-agent protocol](guest-agent-protocol.md), and [Security](security.md).
