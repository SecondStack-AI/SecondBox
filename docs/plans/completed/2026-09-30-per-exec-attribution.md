# Plan: Per-exec attributed execution

## Outcome

Replaces [attributed command execution](2026-09-10-attributed-command-execution.md).
Attribution now binds one admitted non-PTY exec inside an ordinary, long-lived
generation instead of a dedicated single-exec generation. Current contracts are in
[Networking and ports](../../design/networking-and-ports.md#attributed-execution),
[Profiles and authorization](../../design/profiles-and-authorization.md),
[API conventions](../../design/api-conventions.md),
[Runner protocol](../../design/runner-protocol.md), and the
[threat model](../../design/threat-model.md).

## Decision

The owner rejected rebooting a VM for every credentialed command. The previous
design stopped the Sandbox, booted a fresh generation that admitted exactly one
exec with deny-all ordinary egress and no PTY, Port, or file write, and fenced it
afterwards. Its isolation came from that fresh generation: no earlier process
could observe or use the attributed route.

The owner explicitly accepts the residual risk of the replacement. While an
attributed exec's window is open, any process in the Instance, including a daemon
started earlier, can reach that exec's listener and send traffic under its
identity. The sandbox never holds real credentials: the Runner adds only the
`SBXATTR1` identity, and the application's gateway, egress proxy, and Integrations
authorize each credentialed request. Integrations deny credential selectors on
traffic that arrives without attribution, including traffic over the ordinary
Runner gateway routes the attributed exec still has.

## Design

- Public API: `StartSandboxRequest.attributedExecution` is removed.
  `BufferedExecRequest` and `StreamingExecRequest` accept
  `attributedExecution {authorizationRef, expiresAt}`. Admission uses the ordinary
  `sandbox:exec` scope and Profile grant recheck, and requires Profile permission,
  a pinned Tenant egress context, `0 < expiresAt - now <=
  execution.maximumDeadlineMilliseconds`, an exec deadline at or before
  `expiresAt`, and a home Runner advertising `per-exec-attribution`. The data-plane
  session persists the binding, and the request hash covers it.
- Migration `0034_per_exec_attribution` drops the Assignment execution columns
  and adds `execution_authorization_ref` and `execution_expires_at` to
  `data_plane_sessions`.
- Runner protocol generation 6: `ExecOpen.attributed_execution` carries Tenant,
  Subject, authorization reference, and expiry;
  `AssignmentCommand.attributed_execution_permission` carries the pinned gateway
  and resolved per-exec connection limit for every Assignment of a permitting
  Profile; `per_exec_attribution_ready` replaces `attributed_execution_ready`.
- Runner: an attributed ExecOpen opens a window. The shared Firecracker/gVisor
  window starts an execution forwarder on an ephemeral port of the Runner side of
  the Instance interface, installs its per-exec listener table, adds the listener
  to the Instance policy by atomic re-render, and injects
  `SECONDBOX_EXECUTION_GATEWAY` into that exec only. Exec end, cancellation,
  deadline, expiry, fence, and control-connection loss revoke the listener and
  relays and then remove the rules, without stopping the Instance. A forwarder
  failure cancels the exec; an unprovable rule removal terminates the Instance.
- The `SBXATTR1` payload is unchanged. Generation-level guards, the single-exec
  rule, the attributed lifecycle fencing, and snapshot-resume ineligibility are
  removed. Attributed exec streams use the proxied transport, and the Runner
  refuses attribution on direct streams.

## Evidence

Unit tests cover the window lifecycle and ordering, the enforcer re-render and its
failure handling, per-exec listener table naming and interface sweeping, control
plane admission, persistence, replay, and HTTP decoding. Scenario tests cover an
attributed exec in an ordinary running Instance with a surviving daemon, the
accepted in-window daemon connection, revoked listeners after the window, distinct
listeners and identities for sequential execs, terminals, Ports, and files on the
same generation, revocation by deadline expiry, cancellation, and control-plane
loss without Instance loss, concurrent Sandboxes, per-exec connection limits, and
admission refusals.
