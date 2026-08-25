# Plan: Host-First Mergeable gVisor Backend Spike

Add gVisor as an explicitly selected experimental SecondBox compute backend alongside Firecracker
and Microsandbox. The spike must run the same durable Sandbox and complete data-plane lifecycle on
Linux hosts that have no KVM at all — first a plain Linux virtual machine, then a privileged,
node-pinned pod on a Kubernetes cluster — while preserving one provider-neutral public API, one
runner-owned WorkspaceStore, generation fencing, and exclusive workspace ownership.

This plan is stacked on the Microsandbox backend spike and depends on its shared contracts: the
provider-neutral `AssignmentBackend` seam and conformance suites, backend-homogeneous RunnerPools
sealed by first registration, versioned backend-materialization manifests, digest-pinned
`AssetReference` identity, and integer vCPU Profiles. It adds no new shared product-model changes.

Each runner selects exactly one backend with
`SECONDBOX_COMPUTE_BACKEND=firecracker|microsandbox|gvisor`. There is no per-assignment backend
selection and no fallback between backends. Backend kind remains private runner/control-plane
evidence rather than a public Profile or Sandbox field. Operators offer gVisor through separate
named pools and Profiles. gVisor supports `cold_boot` only during the spike.

The compute shape is one `runsc` sandbox (sentry plus gofer) launched as a directly supervised
local child process per Instance on the systrap platform, requiring no `/dev/kvm`, no containerd,
and no Kubernetes API. The unchanged raw-ext4 Workspace image is attached by loop device from the
existing `ComputeAttachment` descriptor, mounted by the host kernel inside a runner-private mount
namespace, and served to the sandbox through the gofer at `/workspace`. WorkspaceStore semantics —
reflink snapshots, deterministic ext4 UUID evidence, flock writer exclusion, receipts, and
relocation — are not modified. The pinned content-addressed flat root introduced for Microsandbox
is reused verbatim as the read-only sandbox root filesystem with a runsc-managed writable overlay.
The guest agent is the existing `secondbox-guest-agent` speaking the unchanged guest protocol,
with Unix-socket listener variants replacing its two vsock listeners; no relay process exists.
Guest resource bounds are enforced through the sandbox's cgroup limits rather than a hypervisor.

Network policy keeps the fail-closed resolved-policy contract, including exact domain rules, by
enforcing in a per-Instance network namespace on a routed veth. The existing Firecracker policy
enforcer and DNS proxy are bridge-family nftables implementations keyed to a TAP enslaved to a
bridge and live inside the Firecracker package; this plan extracts their policy core into a shared
runner package and adds an inet-family rendering for routed veth traffic. The bridge-family and
ARP rules remain Firecracker-only, and Firecracker behavior must not change. Caller Port traffic
reaches the guest exclusively through the guest agent's port feature over the agent transport,
never by dialing the sandbox netstack across the policed egress path.

Isolation is a documented backend property chosen through homogeneous RunnerPools: gVisor provides
a userspace-kernel syscall-interception boundary, not hardware virtualization. Documentation must
state this class distinction explicitly and never present a gVisor pool as equivalent to a KVM
pool. The host kernel mounts only ext4 images whose metadata was written by the host kernel's own
VFS through the gofer; pool homogeneity and backend-matched relocation already prevent a gVisor
runner from ever receiving an image raw-written by a KVM guest, and Task 3H proves that invariant.

The evaluated baseline is the most recent upstream gVisor release at spike start. Task 0H records
the exact release tag, `runsc` build digest, and provenance as the production dependency pin. The
pin may advance only as a separately reviewed dependency change with provenance and tests, never as
an unrecorded local substitution. The spike adds no Rust code and no patched upstream sources; the
pinned `runsc` binary is a distributed launch artifact recorded in the gVisor materialization
manifest.

## Sequential gates and scope control

Task 0H is a hard feasibility gate on a real Linux host without KVM and performs no SecondBox
public-contract, database, deployment, or WorkspaceStore migration. If any mandatory Task 0H proof
fails, stop the spike, retain its bounded evidence, and do not begin Task 1H. Tasks 1H through 3H
establish contracts and foundations. Task 4H is the first production composition change. Tasks 5H
through 7H complete and qualify the host vertical slice. Task 8P is a second hard gate proving the
completed mechanisms inside a privileged containerd-managed pod on a no-KVM Kubernetes node; Task
9P packages and qualifies that deployment profile and closes the spike. Host-track completion is
necessary evidence but never substitutes for the real pod suite.

The spike is mergeable but remains explicitly experimental until both real-environment suites
pass. It does not include `snapshot_resume` (runsc checkpoint/restore is a later decision), a Kata
or any KVM-based Kubernetes backend, per-Instance Kubernetes pods, CNI-enforced network policy, an
unprivileged runner pod, the gVisor KVM platform, macOS or Windows, general-purpose Helm charts,
or a performance release gate.

## Final validation commands

These commands are the final aggregate suite. Each task below also has its own required validation
commands and must be checked before the next task begins.

- `just verify-generated`
- `just lint`
- `just test`
- `just test-contract`
- `just test-non-kvm`
- `just test-firecracker`
- `cd runner && go test ./... -count=1`
- `just test-workspacestore-linux`
- `just test-microsandbox-linux`
- `just test-gvisor`
- `just test-scenario-gvisor`
- `just test-gvisor-pod`
- `just test-scenario-gvisor-pod`
- `just test-scenario`

The gVisor targets are introduced by this plan. `test-gvisor` and `test-scenario-gvisor` require a
real Linux host with the working pinned `runsc`; the `-pod` variants require execution inside the
qualified privileged pod environment. Real-environment targets must fail clearly when invoked
without their required environment; they must not report a skipped suite as success. Existing
Firecracker, Microsandbox, and WorkspaceStore suites must stay green throughout.

### Task 0H: Prove the risky mechanisms on a no-KVM Linux host

Build a bounded standalone probe against the pinned gVisor release before committing to contract
work. The probe may live under `runner/gvisor-probe` and may reuse generated test fixtures, but no
production composition may depend on it.

- [x] Launch a minimal runsc sandbox on the systrap platform with no `/dev/kvm` present, from a
  directly supervised child process with parent-death signal delivery, and prove sentry and gofer
  exit when the parent dies. Record the forced-kill-after-deadline path separately.
- [x] Prove cgroup v2 enforcement of the sandbox: a CPU quota derived from an integer vCPU count
  and a hard memory limit constrain sandbox workloads, and breach produces an observable, bounded
  outcome the backend can classify.
- [x] Attach an already-open writable raw-ext4 descriptor by creating a loop device through the
  `/proc/self/fd/<n>` path, mount it read-write in a runner-private mount namespace, bind the
  mountpoint into the sandbox through the gofer, write a marker from inside the sandbox, detach
  cleanly (syncfs, umount, loop release), reopen the image, and verify the marker, the ext4 UUID,
  and that the inode was never replaced.
- [x] Prove ENOSPC at image capacity surfaces through the gofer as a sane in-sandbox error and the
  image stays consistent after detach.
- [x] Run the existing `secondbox-guest-agent` inside the sandbox with both of its listeners on
  gofer-passed host Unix sockets; complete `Hello`/`Welcome` negotiation, one buffered exec, one
  streaming exec with credit, one PTY open/resize/close, one binary file write/read, and one Port
  relay through the guest agent's port feature over the agent transport. If host-UDS passthrough
  proves unreliable, record a veth-TCP fallback with the same proofs; the selected transport
  becomes the single transport Task 2H implements.
- [x] Create a per-Instance network namespace with a routed veth pair and NAT egress, run runsc
  with its netstack attached to that device, and enforce an inet-family nftables translation of
  one `deny_all` policy and a representative domain/port allow-list with DNS pinning: allowed
  request, denied request, DNS change, private-address target, and metadata target. Prove the
  agent transport is unreachable from the policed path. This proves the rule family and namespace
  shape that Task 6H's shared extraction must render; the existing bridge-family rules are not
  reused here.
- [x] Run a representative released toolchain bundle's standard workloads under runsc and record
  every syscall-compatibility failure verbatim.
- [x] Record 30 sandbox cold-start samples and bounded `/workspace` throughput samples with
  directFS enabled and disabled. Observations only; no gate.
- [x] Record exact host environment, gVisor release tag and runsc build digest, kernel versions,
  commands, outcomes, and bounded logs in a dated evidence document under `docs/plans/evidence/`.
- [x] Declare Task 0H passed only if every proof succeeds on a real no-KVM host. A KVM-capable
  host run or a compile-only result is not sufficient.

#### Task 0H validation

- `just build-gvisor-probe`
- `just test-gvisor-probe` (real no-KVM host; fails clearly elsewhere)

### Task 1H: Extend the shared backend contracts for a third kind

Make `gvisor` a first-class private backend kind without touching any public schema.

- [x] Add `GVISOR` to `ComputeBackendKind` in `contracts/runner/v1/runner.proto`, regenerate both
  independently built protocol packages, and extend the frozen fixtures and breaking-change
  validation.
- [x] Map the new kind in the control plane's backend-kind translation used by registration so a
  gVisor runner registers instead of failing prerequisites, and extend the registration tests.
- [x] Extend control-plane materialization-evidence validation with an explicit gvisor branch:
  require the source OCI manifest digest and flat-root digest exactly as Microsandbox does, and
  carry the pinned runsc build digest in the existing helper-build identity field. Document that
  field's per-backend meaning where the message is defined.
- [x] Extend `runner/internal/materialization` manifest validation and the backend-kind
  allow-list with the same gvisor branch and fixtures.
- [x] Add strict `gvisor` parsing to `SECONDBOX_COMPUTE_BACKEND` with a Linux-only platform gate;
  Darwin composition rejects it. A gVisor runner must not require KVM, jail, TAP, bridge,
  signature-key, trust-anchor, or nested-virtualization configuration.
- [x] Keep the WorkspaceStore formatter on `mke2fs` for the gvisor backend kind.
- [x] Prove RunnerPool sealing needs no mechanism change: add control-plane tests that a pool
  seals to `gvisor` on first registration and rejects Firecracker and Microsandbox runners
  afterward, and the reverse.
- [x] Define the gvisor capability semantics: `hypervisor_ready` attests a successful sentry
  platform probe, `isolation_ready` attests the pinned runsc identity check,
  `resource_limits_ready` attests cgroup enforcement availability. Record the semantics in the
  runner-protocol design doc without changing the message shape.
- [x] Generalize runner lifecycle evidence so the mandatory local process identity field accepts
  the sandbox supervisor process and keeps rejecting absent identity; update the evidence schema
  tests.
- [x] Keep backend kind out of public Profile, Sandbox, Instance, operation, and data-plane
  schemas; extend the contract tests that prove it.
- [x] Add an operator-defined `amd64` gVisor RunnerPool, materialization, and Profile fixture. Do
  not change the architecture or backend semantics of any existing standard Profile name.

#### Task 1H validation

- `just verify-generated`
- `just lint`
- `just test`
- `just test-contract`

### Task 2H: Add the Unix-socket guest-agent transport

Keep the guest protocol and its generations unchanged; add only the transport selected by Task 0H
(written below as Unix sockets; a veth-TCP selection revises these checkboxes before work begins).

- [ ] Add Unix-socket listener flags to `secondbox-guest-agent` for both mandatory listeners —
  the HTTP control service and the gRPC guest protocol — alongside the vsock flags. Exactly one
  transport family must be selected for both listeners; mixed or absent selection fails startup.
- [ ] Keep `Hello`/`Welcome` binding (Instance, generation, nonce, image identity) as the
  connection authentication; no transport-level credential is introduced.
- [ ] Deliver the startup environment by having the runner write the runtime-private directory
  contents before launch and bind it into the sandbox; the vsock secrets-delivery path is not
  used, and the agent's secrets-wait behavior must work unchanged against the pre-written file.
- [ ] Make the agent correct as the sandbox's initial process: reap orphaned grandchildren and
  propagate shutdown to its process group; add tests for both behaviors.
- [ ] Add a runner-side guest-protocol dialer for a filesystem Unix socket without the
  Firecracker vsock CONNECT framing, reusing the existing negotiated client.
- [ ] Enforce socket-path length bounds and runner-owned socket-directory permissions; the socket
  directory is created per Instance and removed in the cleanup stack.
- [ ] Cover listener selection, negotiation, rejection of a second concurrent connection, and
  transport loss with unit tests in both the agent and the dialer.

#### Task 2H validation

- `just lint`
- `just test`
- `cd runner && go test ./... -count=1`

### Task 3H: Implement the host-attachment path for gofer-served Workspaces

Attach the unchanged raw-ext4 image to a sandbox through a runner-owned loop mount while
preserving every WorkspaceStore invariant.

- [ ] Implement a backend-owned attachment component that creates a loop device from the
  `ComputeAttachment` descriptor through `/proc/self/fd/<n>`, mounts it `rw,nosuid,nodev` in a
  runner-private mount namespace, and exposes exactly one mountpoint per Instance.
- [ ] Release in strict reverse order on every path — sandbox exit, fence, and pre-ready failure:
  syncfs, umount, loop detach, then `ComputeAttachment.Close`. The flock is never released while
  the mount or loop device exists.
- [ ] Reconcile stale loop devices and mounts left by a crashed runner at startup before
  readiness, keyed by the runner-owned mount root, and emit bounded evidence for each cleanup.
- [ ] Validate the deterministic ext4 UUID after mount against the attachment's declared identity
  and fail the assignment on mismatch.
- [ ] Prove the provenance invariant with control-plane and runner tests: a gVisor pool can never
  become home to a Workspace created under another backend kind, and relocation only pairs
  runners of matching backend kind.
- [ ] Prove marker durability across attach/detach cycles, journal replay after a simulated crash
  mid-write, ENOSPC behavior, and that snapshots and restores observe the store's existing
  detach-before-mutation ordering, on a real host.

#### Task 3H validation

- `just lint`
- `just test-workspacestore-linux`
- `just test-gvisor` (attachment suite subset on a real host)

### Task 4H: Compose the gVisor assignment backend

Bind the new backend to the existing runner protocol and portable WorkspaceStore, mirroring the
Microsandbox composition.

- [ ] Add `runner/internal/gvisor` implementing `AssignmentBackend` — readiness, validation,
  startup with the exact shared progress-stage order, fencing, capacity reservation and release,
  startup timing, terminal events, and shutdown — plus the `LocalWorkspaceBackend` and
  `WorkspaceRelocationBackend` adapters over the shared WorkspaceStore, so protocol generation 2
  registration and the local-workspace mandatory feature succeed exactly as they do for
  Microsandbox.
- [ ] Generate one OCI bundle per Instance: the pinned flat root as a read-only rootfs with a
  runsc writable overlay, the Workspace mountpoint bound at `/workspace`, the agent socket and
  runtime-private directories bound, and the guest agent as the sandbox's initial process with
  its required identity flags. Record the directFS mode chosen from Task 0H measurements as an
  explicit composition decision.
- [ ] Launch `runsc` as a directly supervised child with parent-death signaling and process-group
  kill; observe exit through `wait` semantics and convert unexpected post-ready exit into exactly
  one provider-neutral terminal event. Never adopt a sandbox after runner restart.
- [ ] Apply the Task 0H-proven cgroup limits for integer vCPU and guest memory to every sandbox
  and record the enforcement mechanism as backend evidence.
- [ ] Make readiness probe the real environment: boot and tear down a trivial sandbox, verify the
  pinned runsc digest, verify the flat-root digest against the materialization manifest, and
  advertise exactly the cached materializations.
- [ ] Admit only `cold_boot`, a supported architecture, available integer vCPU/memory capacity,
  an exact cached materialization tuple, expressible network policy, and required agent features.
- [ ] On fence, atomically reject new operations, cancel active streams, kill the sandbox process
  group, enforce the deadline, reap, detach the Workspace in Task 3H order, and only then release
  attachment authority.
- [ ] Use a reverse-order cleanup stack for capacity, Workspace attachment, sandbox process,
  socket directory, and network state on every pre-ready failure.
- [ ] Run the shared assignment conformance suite against the backend with a real runsc.

#### Task 4H validation

- `just lint`
- `cd runner && go test ./... -count=1`
- `just test-gvisor` (real no-KVM host)

### Task 5H: Implement the complete data plane through the guest agent

Map the provider-neutral runner interfaces directly onto the negotiated guest-protocol session; no
relay process exists in this backend.

- [ ] Implement buffered exec with exact stdout/stderr bounds, typed spawn failures, deadlines,
  cancellation, exit status, and signal results.
- [ ] Implement streaming exec with stdin, end-of-input, signals, caller credit, bounded queues,
  output-channel preservation, and cancellation propagated to the guest.
- [ ] Implement binary-safe file stat, read, write, list, exists, mkdir, and remove with the same
  workspace-relative path and response contracts.
- [ ] Implement PTY open, input, output, resize, and cancellation with bounded binary output.
- [ ] Implement Port open as a generation-fenced relay through the guest agent's port feature
  over the agent transport, with bounded bidirectional relay, no host listener introduced by the
  backend, and no runner connection across the policed egress path.
- [ ] Check the full assignment generation before every operation and again at stream terminal
  publication.
- [ ] Map every known runsc and guest-agent outcome to an existing provider-neutral result;
  unknown outcomes are infrastructure failures, never synthesized success.
- [ ] Run the shared data-plane conformance suite; prove cancellation races, sandbox death,
  caller disconnect, fence, output exhaustion, and duplicate terminal delivery.

#### Task 5H validation

- `just lint`
- `just test-gvisor` (real no-KVM host, data-plane conformance included)

### Task 6H: Translate network policy and finish lifecycle evidence

Preserve the fail-closed resolved-policy contract — domain rules included — on a routed veth,
without changing Firecracker behavior.

- [ ] Extract the policy-enforcement core and DNS proxy from the Firecracker package into a
  shared runner package; Firecracker keeps its bridge-family and ARP rendering and its tests
  unchanged.
- [ ] Add an inet-family nftables rendering for routed veth traffic in a per-Instance network
  namespace with NAT egress, matching the rule shape proven in Task 0H. Pass every assignment
  policy through the shared provider-neutral compiler first; reject any rule with no exact
  representation; never omit or broaden.
- [ ] Prove the agent transport is unreachable from the policed network path and carries no
  policy exception, and that Port relay traffic flows only through the agent transport.
- [ ] Make network readiness prove namespace creation, veth attachment, inet-family rule
  programming, and DNS pinning without TAP, bridge, or KVM prerequisites.
- [ ] Record bounded lifecycle evidence containing runner, Sandbox, Instance, assignment,
  generation, sandbox process identity, backend/platform versions, materialization digest, stage,
  and stream ID while excluding environment values, payloads, file contents, and network bodies.
- [ ] Map unsupported shape or policy to incompatible Profile, absent capacity to capacity
  rejection, digest mismatch to artifact rejection, runsc failure to infrastructure failure, and
  agent negotiation or mount failure to guest rejection.
- [ ] Record exit status, signal, and a bounded stderr/event-tail digest after unexpected exit
  without inventing attribution the backend cannot observe.
- [ ] Update operational diagnostics and metrics to distinguish backend and deployment
  environment using fixed-cardinality dimensions.

#### Task 6H validation

- `just lint`
- `just test`
- `just test-firecracker` (unchanged behavior)
- `just test-gvisor` (real no-KVM host, network policy suite included)

### Task 7H: Qualify the complete host vertical slice

Prove the mergeable vertical slice end to end on a real no-KVM Linux host and ship the host
deployment profile.

- [ ] Distribute the pinned `runsc` binary with the runner artifacts, digest-recorded in the
  materialization manifest and verified at readiness.
- [ ] Extend the installer preflight with a gvisor path that requires reflink storage, loop and
  mount capability, and network-namespace authority, and does not require KVM, TUN/TAP, or
  nested-virtualization flags.
- [ ] Add a real-host scenario driver that uses the normal control plane, authenticated runner
  protocol, both data-plane transports, and durable WorkspaceStore: create a durable Sandbox; run
  buffered and streaming exec; exercise binary files; open/resize/close a PTY; relay a Port; and
  prove deny-all plus an exact allow-list including a domain rule.
- [ ] Stop and restart the Sandbox while preserving Workspace data; create a Snapshot, mutate,
  restore, and prove the earlier contents return.
- [ ] Reject a stale generation during active work; kill the runner and prove sentry and gofer
  exit, the mount and loop device are reconciled, and the Workspace lock becomes available
  without reconstructing empty state.
- [ ] Reject an incorrect logical or materialization digest, a Runner registering into a pool
  sealed to another backend, and an unsupported `snapshot_resume` assignment before creating
  compute.
- [ ] Run at least two concurrent Instances with independent operations, streams, mounts, and
  terminal events and prove no cross-Instance data or frame delivery.
- [ ] Record 30 cold-start samples with start-to-ready p50/p95, stage breakdown, and peak
  sentry/gofer memory in the evidence document. Observations only; no gate.
- [ ] Document the host install path for the experimental backend without altering the qualified
  Firecracker installer path.

#### Task 7H validation

- `just verify-generated`
- `just lint`
- `just test`
- `just test-contract`
- `just test-firecracker`
- `just test-microsandbox-linux`
- `just test-gvisor`
- `just test-scenario-gvisor` (real no-KVM host)

### Task 8P: Prove the completed mechanisms inside a privileged pod

A second hard gate: every host-proven mechanism must work inside a privileged containerd-managed
pod on a Kubernetes node without KVM, before packaging. If a mandatory proof fails, stop the pod
track, retain evidence, and keep the host profile as the spike's sole qualified deployment.

- [ ] Launch the runsc sandbox from the runner inside the privileged pod: seccomp/SIGSYS
  behavior, clean reaping under the pod's PID namespace, and parent-death cleanup.
- [ ] Prove nested cgroup enforcement: per-sandbox limits apply inside the pod's own resource
  limits, and the pod's limits equal the node's declared sandbox budget without breaking
  per-sandbox classification.
- [ ] Prove the loop-mount attachment path inside the pod, including mount-namespace containment,
  crash reconciliation, and that no mount leaks to the host mount table beyond the pod's scope.
- [ ] Prove the selected agent transport and the per-Instance network namespace with NAT egress
  through the pod's interface, with the full Task 0H policy matrix repeated.
- [ ] Record the pod environment — cluster distribution, containerd and kernel versions, pod
  security context, node taints — and all outcomes in the dated evidence document.

#### Task 8P validation

- `just test-gvisor-pod` (probe subset inside the qualified privileged pod; fails clearly
  elsewhere)

### Task 9P: Package the runner pod, qualify it end to end, and close the spike

- [ ] Add a runner container image variant including runsc and the mount/loop toolchain, built
  and digest-pinned by CI.
- [ ] Add a reference privileged, node-pinned runner pod specification under `runner/deploy/`
  with a dedicated tainted node pool, node-local reflink volume, per-runner identity Secret, and
  resource requests equal to the node's sandbox budget. Document that this reference manifest is
  the qualified surface for the gvisor runner pod and that broader Kubernetes manifests remain
  operator-authored.
- [ ] Document the pod install path, the proxied-only data-plane default in clusters, and the
  hostPort direct-transport option.
- [ ] Run the identical scenario driver from Task 7H inside the pod environment, including
  Snapshot/restore, runner-kill reconciliation, concurrency, and the rejection matrix, and record
  30 cold-start samples for the evidence document.
- [ ] Run the existing Firecracker and Microsandbox suites unchanged; prove backend selection
  introduced no fallback and their backend-specific checks still operate.
- [ ] Update current architecture, security, operations, and the Kubernetes-boundary document
  only after both environment suites pass. Describe the gVisor isolation class, the host-mount
  provenance invariant, and the privileged-pod trust posture accurately, and keep
  `snapshot_resume` absent from gVisor capabilities.
- [ ] Remove the experimental label only in a later decision with production distribution,
  sustained stress, upgrade/recovery evidence, and an explicit support policy; those are not
  spike exit criteria.

#### Task 9P validation

- `just verify-generated`
- `just lint`
- `just test`
- `just test-contract`
- `just test-non-kvm`
- `just test-firecracker`
- `just test-workspacestore-linux`
- `just test-microsandbox-linux`
- `just test-gvisor`
- `just test-scenario-gvisor`
- `just test-gvisor-pod`
- `just test-scenario-gvisor-pod`
- `just test-scenario`
