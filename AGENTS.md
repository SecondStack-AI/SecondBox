# SecondBox Development Rules

- `Sandbox` is the durable public resource. `Instance` is replaceable compute fenced to one Sandbox generation.
- The control plane is unprivileged. Only separately deployed SecondBox runners may use KVM, Firecracker, TUN/TAP, host cgroups, or host workspace paths.
- The runner image boots a full systemd in a privileged container, so Docker bind-mounts the host's `/dev` into it. Keep `console-getty.service`, `getty.target`, `getty@.service`, `getty-static.service`, `serial-getty@.service`, and `systemd-logind.service` masked there. Unmasked, `getty@.service` satisfies `ConditionPathExists=/dev/tty0` against the host console and starts an `agetty` on the host's VT 1, and `systemd-logind` registers the host's seat0; both steal the VT, input, and DRM master from a compositor running on that host, so every runner start kills the desktop session of a workstation host. The runner needs no console login and no seat management. Never add a unit to the runner image that touches host consoles, seats, or input devices.
- Runners establish authenticated outbound connections to the control plane. Application API credentials and runner credentials are separate authorities.
- PostgreSQL owns desired state, immutable home assignments, generations, leases, profiles, audit, and reconciliation. Each runner's reflink-capable workspace root owns the Workspaces and Snapshots homed there. S3-compatible storage owns Artifacts and immutable execution assets only.
- A Sandbox never relocates after initial placement. Loss of an unbacked home-runner workspace filesystem loses that Sandbox; PostgreSQL or S3 recovery alone is insufficient.
- Workspace persistence is reflink-only and runner-local. Do not stream image bytes, expose local paths, add copy fallbacks, or reconstruct an empty Workspace when local data is absent.
- Only the runner's WorkspaceStore resolves local paths. Compute backends receive an opaque provider-neutral Workspace attachment and must preserve its generation fence and exclusive writer lock.
- Firecracker is the only implemented v1 backend. Keep the provider-neutral compute port and its conformance suite, but do not add placeholder backends or fallback execution.
- Operators create every profile explicitly. A Sandbox is pinned to the immutable profile revision resolved at creation.
- Public contracts use provider-neutral SecondBox domain language. Firecracker, KVM, runner credentials, host paths, storage keys, fencing tokens, and backend references do not enter public schemas.
- Cross-resource references are logical strings. Do not add PostgreSQL foreign keys or CHECK constraints.
- Every runtime setting is explicit. Application code and deployment templates must not provide defaults for required environment variables.
- Do not catch, log, and swallow errors. Implement one intended path and fail explicitly when its prerequisites are absent.
- Keep exported names and error prefixes greppable and domain-specific. Remove replaced code instead of retaining compatibility paths.
- Run `just verify-generated`, `just test`, and the relevant contract, Compose, runner, or Firecracker suite before handoff. Run `just test-scenario` on a qualified host when a change touches the runner protocol, lifecycle reconciliation, or workspace durability.
