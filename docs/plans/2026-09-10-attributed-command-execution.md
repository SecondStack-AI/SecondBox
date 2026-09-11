# Plan: Attributed command execution

Status: design agreed with Fable 5.1; implementation submitted in PR #123. Exact-release-commit qualification and publication are pending.
Provide trusted network attribution for one command and its descendants without making guest code an identity authority.
The application retains authorization and credential custody; SecondBox supplies isolated execution and lifecycle evidence.
No application credentials, provider names, tokens, or integration policies enter SecondBox.

## Boundary and rationale

The current guest command launcher in `runner/internal/guest/protocol_exec.go` sets a process group but does not separate the command's OS identity from the guest agent.
A command can therefore compromise the agent that reports its completion.
Adding an in-guest label, killing a process group, or trusting a later report of an empty process list does not establish an independent security boundary.

An attributed command must own a fresh, exclusive Sandbox generation and Instance.
Only the durable Workspace carries forward from an earlier generation; mutable rootfs state, processes, sockets, and guest-agent memory do not.
A qualified pristine execution template may accelerate startup, but a checkpoint of an Instance that ran untrusted commands cannot supply the initial state.
The generation runs exactly one exec and its descendants, with no PTY, port session, second exec, or other execution entry point.
There is no multi-command optimization in this contract.

This retains the existing Workspace writer lock, generation fence, immutable Profile, home Runner, and stopped-Sandbox relocation rules.
It adds compute startup and teardown to credentialed commands and excludes concurrent ordinary workspace execution.
Ordinary execution retains its current behavior and does not acquire credentialed authority.

## Admission and lifecycle

An immutable Profile must explicitly permit attributed execution.
The same Sandbox and Profile can support ordinary generations and explicitly requested attributed generations; applications cannot replace the Profile or attach its Workspace to an unrelated Sandbox to change modes.
The immutable revision declares the ordinary network policy and a separate forwarder-only policy for attributed generations; the selected mode cannot permit both routes.
An attributed start requires a stopped Sandbox, current authority, a finite deadline, and a bounded application authorization reference.
The reference is correlation evidence for the application, not a credential or permission understood by SecondBox.
The mode, reference, deadline, Tenant, Subject, Sandbox, generation, and assignment binding are fixed before guest networking opens.

A busy ordinary Instance is refused rather than implicitly killed.
The application must explicitly stop ordinary workspace activity before requesting attributed execution.
The control plane and Runner both enforce the single-exec rule, including buffered, streaming, terminal, reconnect, and internal tool paths.
The attributed generation must not accept a separate caller's exec or mutation that can alter the admitted command.
Read-only file operations can collect results; mutating file operations must wait until the attributed generation ends.

The Runner owns the deadline and terminal cleanup independently of guest cooperation.
Application cancellation, expiry, assignment loss, and a guest terminal frame all end the generation; the frame is a request to terminate, not proof that descendants are gone.
Close attributed network access and active relays, destroy compute, and acknowledge terminal lifecycle only after the host has established termination.
Keep existing uncertain-execution reporting when a host or transport failure prevents a confirmed outcome.
Runner restart or reconnect fences attributed generations instead of recovering their command or listener.
Never resume an attributed generation after relocation or from an untrusted compute snapshot.

## Runner-owned network attribution

The installation's existing Tenant egress context selects an operator-configured gateway destination.
It remains installation routing only; neither the application authorization reference nor guest traffic can change it.
Attributed mode uses only the declared attributed forwarding path, with no direct or ordinary-gateway bypass.
Unsupported Profile, backend, Runner, or gateway configuration rejects attributed start.

For each attributed generation, the Runner creates a private forwarding listener bound immutably to that assignment before enabling guest networking.
Only that Instance can reach it under the backend's enforced interface and source rules.
Firecracker must extend its per-TAP and host-input policy; gVisor must use its per-Instance network boundary.
Host listeners, other Instances, and another Tenant cannot enter this path by copying an address or forging a source address.
Keep private port allocation bounded and keep all paths and host network details outside public contracts.

The Runner inserts `SECONDBOX_EXECUTION_GATEWAY` into the command environment as an IPv4 literal and port, without a scheme. The shared guest dispatch derives it from the Instance-owned listener and rejects caller collisions with that exact name in ordinary and attributed execs, including whitespace-normalized names. Ordinary execs receive no injected value. Applications own HTTP proxy variables and command wrappers. Guest reconnection reuses the same live listener; it never allocates a replacement endpoint. The value is routing information, not credential authority, and is not added to public responses or Runner evidence. This refines the endpoint delivery agreed with Fable without reserving unrelated environment names.

The listener forwards to the installation's configured gateway Unix socket.
This requires a gateway on the Runner host with the socket made available through the operator's deployment; unsupported topologies cannot advertise attributed execution.
The Runner writes one versioned, length-bounded attribution preface before forwarding any guest byte.
The preface contains the Tenant, Subject, Sandbox, generation, non-secret assignment identity, application authorization reference, and expiry.
Do not expose the raw assignment fencing token.
The gateway verifies the Runner's Unix peer under explicit operator configuration and accepts no preface on its ordinary TCP listener.
Missing, malformed, truncated, or expired attribution closes the connection.

The accepted connection retains that immutable binding for its entire lifetime.
There is no later lookup of who currently owns a source address or port: such a lookup can confuse an old queued connection with a newer generation.
Teardown prevents new accepts, closes all active relays, destroys compute, and removes the old network path before releasing listener ports, interfaces, or addresses for reuse.
Use bounded buffers and cancellation-aware relay I/O; bulk-transfer optimization is outside this change.

The application gateway strips guest context claims and carries only Runner-established attribution over its authenticated application transport.
The downstream authorization service checks current permission for each credentialed request, including requests inside an existing CONNECT tunnel.
An explicit credential selector without valid attribution is denied, never downgraded to public traffic.
Ordinary public traffic remains a separate supported path.
SecondBox revocation closes the attributed connection; application revocation can close it sooner and never extends the Runner deadline.

## Validation Commands

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- `just test-sdk-packages`
- `just test-firecracker`
- `just test-scenario`
- Run the existing gVisor qualification and scenario targets with their explicit build and work directories on a qualified host.
- `git diff --check`

Read `AGENTS.md`, `docs/design/domain-lifecycle.md`, `docs/design/runner-protocol.md`, `docs/design/guest-agent-protocol.md`, `docs/design/networking-and-ports.md`, and `docs/design/threat-model.md` before implementation.
Preserve existing worktrees and release assets.
Publishing or releasing is a separate external action; an unpublished build is not a qualified downstream dependency.

### Task 1: Freeze attributed generation admission

Use existing Profile, lifecycle, exec-session, assignment, and lease structures in `internal/service` and the public and Runner contracts.

The optional immutable `attributedExecution` Profile block now declares a canonical logical `gateway` and `maximumConnections` (1–4096).
Profile validation requires the Tenant egress-context pin and rejects missing, wildcard, IP, URL, or noncanonical gateway values.
The existing start API and generated Go/TypeScript SDKs expose the optional attributed request. Admission requires stopped, inactive compute, Profile permission, a routing pin, a bounded future expiry, and an attributed-capable home Runner. It stores the binding before changing Workspace ownership. Scheduling carries Tenant, Subject, reference, deadline, gateway, and connection limit in the assignment, removes ordinary destinations, and requires the attributed capability. Automatic restart cannot create an attributed generation without an explicit start Operation.

Control-plane admission reserves one exec session on the assignment, independently of result retention and current Sandbox intent. It rejects PTYs, file mutation, ports, expired authority, and overlong deadlines; read-only files remain accessible. Replay retains the admitted session, later validation failure rolls back the reservation, and scheduling refuses assignment reuse under a different binding.

The Runner retains the single-exec allowance on the host Instance across guest connection replacement. Shared Firecracker/gVisor protocol paths reject terminals, ports, file mutation, mismatched assignments, and expired or overlong execs. A failed send consumes the allowance because delivery is uncertain. Firecracker's legacy tool, workspace-write, and secret-mutation endpoints are also denied. Assignment replay compares the complete attributed binding, and admission requires its presence to match the explicit capability declaration; omitting that declaration cannot bypass unsupported-backend checks.

HTTP/database and SDK checks cover ordinary and attributed starts, replay, changed-reference conflicts, malformed attribution, and caller-selected routing refusal. Database/race checks cover concurrent exec admission, rollback, result removal, cleared lifecycle metadata, and assignment persistence. Runner race tests cover the protocol and legacy endpoint guards. Firecracker and gVisor report attributed readiness only with a configured attributed gateway; registration persists it as an optional placement capability. Assignment validation still requires the exact pinned-context route. The complete disruption matrix remains to be qualified.
The current control-plane patch passes `just test`, `just test-contract`, `just verify-generated`, and the focused race tests. The migration lineage includes `0022_attributed_execution`.

- [x] Add the explicit Profile capability and attributed start contract, with a bounded authorization reference and finite deadline.
- [ ] Persist mode and binding before assignment; retain exact Tenant, Subject, Profile, generation, and current-authority checks.
- [ ] Reject active ordinary compute, conflicting operations, unsupported Runners, and attempts to select installation routing through command fields.
- [ ] Enforce exactly one authorized exec across every public and internal execution path.
- [x] Regenerate Go, TypeScript, OpenAPI, and protocol artifacts through the existing generator and add request-level admission regressions.

### Task 2: Enforce fresh compute and host-owned termination

Extend the existing assignment and data-plane lifecycle in the Firecracker and gVisor backends.

The protocol service fences attributed assignments before initial registration and after a disconnected session has cancelled and joined its pending starts. It closes direct channels before requesting backend termination and removes advertised authority only after a confirmed stop. Cleanup has a bounded host context independent of caller cancellation; failed or unconfirmed termination stops reconnection.

Attributed startup receives the admitted deadline. Once compute starts, a host timer enforces that deadline independently of guest completion and control-stream writes. Expiry, explicit fencing, and disconnect cleanup serialize teardown per assignment; a confirmed stop removes its timer. Failed expiry enters protocol failure handling while retaining the assignment for cleanup. Protocol tests cover expiry without guest completion, failure reporting, timer cancellation, stalled-start cancellation, recovered compute, and ordinary-instance retention. Real backend process and network isolation are still unqualified.

Buffered and streaming exec results now wait for host-confirmed compute termination. Their result channel remains open through delivery. Exec cancellation and its own shorter deadline request host teardown without waiting for guest cooperation; a late guest success cannot override the host cancellation outcome. An unconfirmed stop produces a non-retryable infrastructure failure and enters cleanup failure handling. Protocol tests exercise both transports, blocked teardown, failed teardown, and a guest that ignores cancellation and reports success after host termination.

- [ ] Launch attributed generations only from verified pristine compute state with the existing exclusive Workspace attachment.
- [ ] Enforce expiry and cancellation on the host and destroy compute before confirming termination.
- [ ] Fence attributed generations during restart, reconnect, relocation, and uncertain cleanup instead of replaying commands.
- [ ] Prove that daemonization, process-group escape, forged guest completion, and an unresponsive or compromised guest cannot retain authority or reach the next generation.

### Task 3: Forward immutable connection attribution

Extend Runner network policy and teardown; keep application gateway and credential decisions outside SecondBox.

The existing operator-owned egress-context file now accepts an attributed Unix-socket route per logical gateway. Deployment rendering and Runner loading share path validation. Context lookup is exact; socket-only routes are excluded from ordinary IP gateway admission. Config and deployment tests cover isolation and missing routes.

The listener firewall renderer reserves an exact IPv4 address and TCP port for one interface and guest source. Bridged traffic is checked at bridge input before host IP delivery; routed traffic is checked at host input. Host-originated connections to the endpoint are denied. Live tests in a disposable container exercise both topologies, peer and local-host denial, wrong and spoofed sources, owner access, unrelated ports, and removal of the rules. Run them by compiling `runner/internal/egressforwarder` with the `listener_qualification` build tag and executing only `TestExecutionListenerFirewallQualification` in an isolated root container with NET_ADMIN and SYS_ADMIN. These tests qualify the firewall rules, not the complete backend forwarding lifecycle.

`StartExecutionForwarder` reserves a bound socket without listening, installs the firewall, and only then enables accepts. Its immutable attribution and connection limit feed the existing TCP-to-Unix relay. `Close` waits for relays before removing firewall state; failed startup also removes rules if the command committed before reporting failure. Live tests cover the owned resource in both topologies, Unix-peer authentication, exact attribution, active-client EOF on close, and cleanup after a committed-but-failed firewall install.

`CompileExecutionListener` permits only that resource's exact TCP endpoint, retains protected destinations, and grants no DNS or ordinary gateway access. Both backend firewall renderers accept this policy without changes to ordinary policy behavior. Policy and renderer tests pass under the race detector.

The gVisor backend retains the forwarder with its network allocation, selects this private policy before compute launch, revokes forwarding when fencing begins or the supervisor exits, and closes the resource before network-slot release. Forwarding failure or expiry sends the supervisor's compute-kill command, allowing it to flush and detach the Workspace. `TestAttributedExecutionNetworkQualification`, built with `listener_qualification`, exercises the actual backend network setup and teardown in an isolated container: Unix attribution, host denial, revocation, missing-route refusal, and slot reuse across generations. Shared dispatch tests cover endpoint injection and collisions, including an actual guest service executing a child shell.

Firecracker now resolves the same context-pinned socket before launch and creates the listener after reserving its TAP. The host reservation transfers listener ownership to the registered Instance. Stop revokes forwarding, final cleanup waits for it before releasing the guest IP, and failed cleanup retains the identity reservation. Guest negotiation uses the retained endpoint and refuses a stopped listener. `TestFirecrackerExecutionNetworkQualification`, built with `listener_qualification`, verifies real TAP/bridge and firewall creation, host denial, cleanup, injected post-network startup failure, and IP reuse inside an isolated container with `/dev/net/tun`. A regression also reproduces and fixes the earlier named-return cleanup bug that leaked an IP after failed host setup. These checks do not boot a VM or qualify process-failure handling.

Listener-table names derive from the exclusively owned guest interface. gVisor startup cleans only its configured profile's interfaces; Firecracker orphan cleanup derives each TAP from the existing run directory. Firecracker creates that directory and UID lease before networking, reclaims partially created TAPs, and retains the recovery directory if cleanup fails. No credential or authority journal is added. Live tests revoke the listener while leaving its rules behind, then exercise both recovery paths; another interface's tables survive scoped cleanup. Killing a complete Runner with active compute still needs qualification.

- [ ] Add explicit installation gateway socket configuration and a bounded per-generation listener lifecycle.
- [x] Define and test the versioned preface, peer authentication contract, size/deadline bounds, and a small public consumer port for application gateways.
- [ ] Install interface-specific forwarding policy before guest networking; deny peer Instances, source spoofing, direct bypass, and ordinary gateway access from attributed mode.
- [ ] Close listeners and active relays before reusing network resources; do not use a mutable source-address ownership lookup.
- [ ] Verify malformed prefaces, missing gateway, old queued connections, address/port reuse, cancellation, and Runner failure with real sockets and backend enforcement.

`pkg/egressattribution` supplies the bounded `SBXATTR1` preface and a Linux reader that checks the configured Unix peer UID before accepting identity.
Its production sources are mirrored in `runner/egressattribution` under `just verify-generated`, keeping the two module dependency graphs separate.
Real Unix-socket tests cover peer refusal, read timeout, expiry, malformed/oversized frames, and preservation of subsequent guest bytes; race tests pass.
`just verify-generated` passes with the repository-pinned protoc 35.1, and `just test` passes against the dedicated `secondbox_test_attributed_command` database with explicit `sslmode=disable`.
`runner/internal/egressforwarder` now owns a TCP listener with a fixed attribution value, a connection limit, and a host cancellation/deadline boundary.
It sends the preface to the configured Unix gateway before guest bytes, preserves half-close responses, and waits for relays to close before returning.
Gateway or relay failure ends forwarding; capacity exhaustion refuses only the new connection.
Real socket race tests cover bidirectional bytes, forged guest headers, half-close, active-connection cancellation/expiry, capacity refusal, and an absent gateway.
The complete `just test` and `just verify-generated` checks also pass with this forwarding component present.
These network checks alone do not qualify complete sandbox execution. Compute and public lifecycle evidence is recorded below; downstream gateway consumption requires the qualified release.

### Task 4: Qualify and prepare consumption

The retained Firecracker command qualification log (`/tmp/secondbox-attribution-firecracker-compute-final.log`) records one local sample: 273 ms from instance allocation to guest-protocol negotiation and 1,107 ms for teardown (9 ms freeze, 2 ms protocol close, 1,095 ms terminate). These are backend timings, excluding application admission, scheduler polling and Workspace creation; they are not latency percentiles or a release performance guarantee. The no-KVM gVisor scenario log has terminal lifecycle timestamps but no matching start/teardown interval measurements. Do not substitute its total scenario duration for command overhead.

Publication remains a separate action in `SecondStack-AI/SecondBox`, on branch `execution-credential-context`, based on current upstream main `eebb488`. The latest published release is v0.9.2 and does not expose this contract. Prepare the source PR before a coordinated feature release. After the release commit is fixed, produce clean exact-commit Firecracker, no-KVM gVisor, gVisor pod and installer-candidate evidence through the normal release scripts. Existing dirty-worktree qualification must not be relabeled as release evidence. The downstream SDK, image, manifest and compatibility pins must change together after publication.

The complete `scripts/test-scenario-gvisor.sh` gate passed in 376.8 seconds in an Ubuntu amd64 VM with no `/dev/kvm` or VMX/SVM CPU flags. File hashes confirmed that the guest tested this exact dirty worktree, including untracked source. The official wrapper verified pinned runsc and generated the local materialization from the freshly built guest agent. Consecutive generations, control-plane and Runner loss, and concurrent attributed Sandboxes passed. The Firecracker-only bridged network scenario was explicitly skipped. Evidence and source hashes are retained at `/home/sasha/Developer/tries/secondbox-no-kvm-evidence`; the temporary VM was shut down and removed. This is local source qualification, not release qualification or downstream consumption.

`TestQualifiedGVisorAttributedCommand` passes through real runsc compute, a reflink-backed Workspace, the injected endpoint, and the Unix gateway reader. It verifies host attribution despite forged guest context bytes, denies a second command, and confirms explicit fencing. `TestQualifiedGVisorAttributedRevocation` verifies that expiry, fencing, and supervisor loss close an established descendant connection and end the command. Expiry and fencing clean up successfully; supervisor loss reports failed Workspace cleanup and requires recovery. The ordinary gVisor qualification also covers streams, files, PTYs, cancellation, network policy, assignment conformance, and attachment crash recovery. These tests use the pinned v0.9.2 runtime/root filesystem with a guest agent rebuilt from this worktree, in isolated Docker network and cgroup namespaces.

`TestSmokeFirecrackerAttributedCommand` passes against a jailed KVM guest with verified signed artifacts and a reflink Workspace. It checks endpoint injection, host attribution before forged guest context bytes, second-command refusal, confirmed removal, and correlated teardown evidence. `TestSmokeFirecrackerAttributedRevocation` verifies live descendant connection closure and compute teardown on expiry, explicit stop, and compute loss. The qualification container has its own network and cgroup namespaces and read-only artifact mounts.

The full Firecracker scenario gate passed all 24 ordinary groups plus the first attributed public-API scenario. The expanded `TestScenarioAttributedCommandsRetireEachGeneration` now verifies two explicit starts with distinct Instances and increasing generations, preserved Workspace data, immutable host attribution, and second-exec refusal. It exposed running intent remaining after compute retirement; lifecycle retirement now clears that intent using the durable assignment binding. `TestScenarioAttributedConnectionLossRevokesExecution` passes control-plane reconnect and Runner SIGKILL: each closes the descendant connection, ends execution, clears running intent, and permits a fresh explicit admission with flushed Workspace data preserved. Unflushed guest writes are not crash-durable. The standalone host-input firewall permits TCP to the bridge address only with the per-Instance policy authorization mark. The focused live scenarios, full Go test gate, and full Firecracker scenario suite (346 seconds) pass after these fixes. `TestScenarioAttributedConcurrentSandboxesKeepOwnAuthority` also passes: two live Sandboxes retain their own host-established identities despite forged guest context bytes, and retiring the first preserves the second's established gateway connection.

- [ ] Run generated-contract, unit, integration, SDK, Compose, and scenario gates without weakening assertions or substituting mocks for host isolation.
- [ ] Qualify Firecracker and gVisor on actual supported hosts, with two concurrent Sandboxes and successive generations sharing the same Workspace.
- [ ] Measure attributed-command startup and teardown cost and document supported deployment topology and failure behavior.
- [ ] Update current-state docs and prepare the separately authorized upstream release prerequisite.
- [ ] Downstream consumption must use qualified released artifacts through the normal compatibility boundary; do not claim completion from an unpublished local build.
