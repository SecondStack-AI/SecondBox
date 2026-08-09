# Guest-agent protocol

The guest-agent protocol is the runner-to-guest control and data-plane boundary. It evolves independently from the public API, runner protocol, and Firecracker version.

`contracts/guest/v1/guest.proto` is the canonical schema. Each supported protocol generation has a committed descriptor set and frozen request, response, event, and negative binary fixtures. The runner test matrix exercises the current runner against every supported released guest generation.

The generated image exposes this canonical bidirectional gRPC stream on a dedicated, explicitly configured vsock port. The separate HTTP guest-control port is limited to bootstrap and legacy lifecycle operations; it is not a guest-protocol fallback. Firecracker's host-side Unix socket CONNECT framing is transport only and does not replace the `Hello`/`Welcome` binding.

## One negotiated connection generation

The runner opens one authenticated transport connection to the guest agent for an Instance. The first frame is `Hello`; no lifecycle or data-plane message is valid before negotiation. `Hello` binds the expected Instance, Sandbox generation, random connection nonce, runner-supported protocol range, and requested features.

The guest answers `Welcome` with the selected protocol generation, guest build identity, execution asset identity, supported features, and echoed binding. The negotiated generation and feature set are immutable for that connection. Renegotiation requires closing it and creating a new connection. A mismatched Instance, generation, image identity, unsupported range, or missing mandatory feature fails readiness before workspace mutation or command admission.

The runner supports the current guest protocol generation and the two immediately preceding released generations. A release may remove the oldest generation only after compatibility gates prove existing signed execution assets and pinned ProfileRevisions are handled explicitly. Signed image metadata declares its guest generation and mandatory features.

## Feature gating

Features are named protocol capabilities such as streaming exec, PTY resize, descriptor-pinned filesystem access, activity events, and port proxying. The sender checks the negotiated feature set before sending a feature-specific frame. Unknown protobuf fields are tolerated according to protobuf rules, but an unknown feature is never treated as supported.

A mandatory ProfileRevision capability that the guest lacks makes the Instance startup fail with `guest_feature_unsupported`. The runner does not emulate the operation with a shell fallback.

## Command and terminal messages

Command admission carries a unique operation ID, current Sandbox generation, shell or argv request, cwd, bounded environment, deadline, output bounds, and optional PTY dimensions. The guest returns an admission result before execution events.

Exec events include stdout, stderr, byte-credit accounting, process identity evidence, and exactly one terminal outcome. Terminal outcomes are `exited`, `spawn_failed`, `deadline_exceeded`, `cancelled`, and `output_exhausted`. Guest crashes and transport loss are runner/infrastructure outcomes and are not fabricated guest exits.

Generation 1 executes shell requests through `/bin/sh -c` and argv requests directly. Buffered binary stdin is attached before spawn without text conversion; streaming binary stdin is written to the same live process in frame order. An `end_of_input` frame writes any final bytes and closes the process stdin pipe exactly once. The guest rejects empty non-EOF input and every input frame after EOF. The guest pins the requested working directory, creates a process group, and kills the group on cancellation, deadline, connection loss, or output exhaustion. Stdout and stderr writers consume retained byte credit before emitting each bounded chunk, which applies backpressure at the process pipe without accumulating unbounded guest memory. If an output write crosses the limit, the allowed prefix is emitted before the guest terminates with `output_exhausted`. Terminal evidence carries exit signal, typed spawn failure, elapsed deadline, and output-limit values without reconstructing them at the public API.

PTY requests allocate a real pseudoterminal with the requested initial dimensions and merged stdout/stderr. Binary input, byte credit, and resize controls share the operation's ordered sequence. Cancellation, deadline, output exhaustion, and connection loss kill the PTY process group and wait for process exit before emitting the terminal acknowledgement; a disconnected host stream never silently abandons the guest process. Public detach and reconnect state remains a control-plane concern and does not create a second guest process or guest-side session authority.

## Filesystem messages

All paths are workspace-relative protocol strings. The guest opens and pins the workspace root for each operation, then resolves each component relative to pinned descriptors with no symlink traversal. Operations cover read, write, stat, direct-child list, exists, mkdir, remove, and bounded streaming transfer. Mkdir and remove apply the caller's exact recursive and force flags. Stat and direct-child list results carry path kind, byte size, and modification timestamp for every returned entry. Writes use a temporary sibling, checksum, fsync, and atomic commit where the filesystem supports it.

Binary reads and writes remain bytes throughout the protocol. Read chunks consume runner byte credit; writes require declared size, ordered offsets, an exact SHA-256 checksum, a bounded create mode, and an atomic commit. Filesystem cancellation does not convert partial bytes into a completed write.

Public Snapshot operations run only while the Sandbox is stopped, after the VM
and all host-side Workspace users have detached. They are runner-local
WorkspaceStore operations and require no guest-protocol message or freeze
feature.

## Health and useful activity

Heartbeat reports guest liveness and protocol health. Useful-activity events separately identify active exec, filesystem transfer, PTY, and port sessions plus explicit touch acknowledgements. Health traffic and read-only polling do not update idle time.

The guest never receives database, runner enrollment, application API-key, or model-provider credentials. SecondBox v1 does not provide a general secret-injection protocol.

See [API conventions](api-conventions.md), [Runner protocol](runner-protocol.md), and [Workspace durability](workspace-durability.md).
