# Profiles and authorization

Profiles are server-owned policy. Application clients select an authorized profile name; they do not assemble compute policy in a Sandbox request.

## Identity model

Bootstrap creates the first operator through a local, one-time administrative flow. Operators create Projects, ServiceAccounts, APIKeys, Profiles, RunnerPools, and runner enrollment credentials through administrative scopes.

An application authenticates with an API key belonging to one ServiceAccount and therefore exactly one Project. The authenticated Project is the tenancy boundary; request bodies cannot contain a tenant, subject, project override, or arbitrary actor identity. Authorization evaluates the key state, expiry, scopes, ServiceAccount state, Project state, and profile grant on every request. Revocation takes effect at admission and prevents renewal of existing Leases.

API keys are scoped independently for Sandbox lifecycle, execution, files, artifacts, ports, and read-only access. Administrative project, key, profile, runner, audit, and diagnostics scopes are not available to ordinary application keys. Plaintext key material is never stored or returned after its one creation response.

## Profile and revision

A Profile is a stable operator-chosen name with an enabled state and current revision. Creation and each revision operation produce an immutable ProfileRevision. Updating the Profile only changes the revision selected by future Sandbox creation. Existing Sandboxes remain pinned; there is no silent migration.

Every ProfileRevision contains:

- backend kind `firecracker`;
- RunnerPool selector and required architecture/capability set;
- immutable runtime and toolchain component-manifest digests bound by one signed execution-bundle manifest; the runtime component covers the kernel, rootfs, and guest agent, while the toolchain component covers the shared tool payload and its locked provenance;
- vCPU, memory, workspace disk, process, and concurrent-operation limits;
- exec deadline, buffered output, streaming window, transfer, PTY, and port-session bounds;
- drain grace, idle timeout, maximum Instance duration, lease duration, and desired create state;
- checkpoint-on-stop, checkpoint cadence, retention, snapshot, artifact, and retained-byte policy;
- outbound network and DNS policy;
- approved exposed ports, protocols, and session limits.

There is no built-in or fallback profile. Installation examples may show explicit profile creation, but the server starts with none. Profile names do not trigger application-specific behavior. Disposable-compute and durable-coding behavior are ordinary policy combinations in the immutable revision pinned to each Sandbox. In particular, `checkpoint.onStop=false` drains and stops the active Instance without publishing its latest writes, while `checkpoint.onStop=true` publishes the current generation before stopping. Start continues from the Workspace's last published checkpoint when one exists, or from an empty Workspace otherwise; it never adopts a newer Profile head.

## Creation and compatibility

`POST /v1/sandboxes` contains only `profile` and bounded string metadata. The client supplies `Idempotency-Key` as a header. Resource, backend, image, lifecycle, storage, network, timeout, port, and placement fields are rejected as unknown properties.

Creation fails before allocating durable intent when the profile is absent, disabled, not granted to the ServiceAccount, or has no RunnerPool capable of its immutable requirements. Successful creation persists the exact ProfileRevision ID and a resolved compatibility summary. Later runner availability changes do not rewrite that selection.

Profiles may be disabled to stop future creation. Disablement does not mutate pinned Sandboxes. A profile revision and its referenced assets cannot be deleted while reachable from a Sandbox, checkpoint, or retention record.

## Quotas

Project and profile quotas cover total Sandboxes, active Instances, vCPU, memory, retained bytes, snapshots, artifacts, exposed-port sessions, and concurrent data-plane operations. Admission and quota reservation are transactional. A concurrent race either commits one authorized reservation or returns a typed quota error; it never overcommits and repairs later.

Metrics use fixed-cardinality labels. Project names, ServiceAccount IDs, API key prefixes, Sandbox IDs, profile names, workspace paths, and artifact names are audit fields rather than metric dimensions.

See [Domain and lifecycle](domain-lifecycle.md), [API conventions](api-conventions.md), [Security](security.md), and [Compatibility policy](compatibility-policy.md).
