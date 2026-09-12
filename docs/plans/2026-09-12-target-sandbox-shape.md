---
title: Target Sandbox Shape
date: 2026-09-12
status: in-progress
owner: SecondStack
provenance: UX audit of the user-facing surface against E2B, Daytona, Modal, and Vercel Sandbox, 2026-09-12
---

# Plan: Target Sandbox Shape

## Outcome

Make the everyday Sandbox workflow one command per intent, without changing
what SecondBox is. After this plan:

```sh
secondbox run durable-coding --size large --name mybox --keep
secondbox run durable-coding --cpus 2 --memory 4GiB --disk 20GiB -- npm test
secondbox cp ./src mybox:/workspace/src
secondbox cp mybox:/workspace/out.txt ./out.txt
secondbox ports forward mybox 3000
secondbox snapshot mybox --name with-deps
secondbox run durable-coding --from mybox/with-deps -- npm test
secondbox stop mybox && secondbox start mybox && secondbox rm mybox
```

and a guided single-host install ends with a working `secondbox run`, not
with a platform login that cannot create a Sandbox.

## Findings this plan answers

The audit (session 2026-09-12) established, with file evidence:

- A Profile bundles five orthogonal concerns: image digests, machine size,
  network policy, lifecycle bounds, and port policy. `CreateSandboxRequest`
  accepts only `profile`, `metadata`, and `sourceSnapshotId`
  (`contracts/openapi/v1/secondbox.openapi.json`, `CreateSandboxRequest`,
  `additionalProperties: false`). Only the platform token may create Profiles
  (`internal/api/application_authority.go`). Every image × size × network
  combination is therefore an operator ticket.
- The three standard bundles offer two sizes: 1 vCPU/1 GiB/2 GiB and
  4 vCPU/8 GiB/50 GiB (`pkg/standardresources/standardresources.go`).
- The CLI has three friendly commands (`run`, `exec`, `shell`). The other 48
  operations in `commandAliases` (`cmd/secondbox/main.go`) are raw transport
  where the caller hand-writes `If-Match`, `SecondBox-Generation`, and
  `Idempotency-Key`, although both SDKs already compute all three.
- The guided installer applies standard Profiles but creates no Tenant,
  Subject, or application authority. `runInstalledSmoke` in
  `cmd/secondbox-deploy/installer_resume.go` reads the pool and runner records
  only; it never creates a Sandbox. `README.md` line 31 claims the installer
  "finishes by running a hello-world command inside a microVM". That claim is
  false today.
- `sourceSnapshotId` already exists on create, so a golden-snapshot workflow
  needs only a friendly surface.
- Create-to-ready is about 1.5 s p50 at low concurrency
  (`docs/operations/lifecycle-benchmark.md`). Speed is not the problem.

## Fixed architecture

Nothing in this plan changes these rules, and each task must preserve them:

- SecondBox stays a networked control plane; no daemonless mode, no local
  runtime, no second execution path.
- Profiles remain server-owned immutable policy. A Sandbox pins the exact
  ProfileRevision resolved at creation. This plan adds a *request within the
  Profile's bounds*, never a request that replaces Profile policy.
- Backend kind, image digests, host paths, and runner identity never enter
  public schemas. `image` is out of scope for this plan (see "Deferred").
- Composition lives in `sdk/go/secondboxclient` and `sdk/typescript`; the CLI
  consumes SDK helpers. New CLI verbs never re-implement header logic.
- `secondboxd` startup never bootstraps tenancy
  (`tests/deployment/configuration_surface_test.go`,
  `TestDevelopmentTenancyBootstrapIsExplicitAndPostStart`). Tenancy bootstrap
  is an explicit, recorded, post-start installer stage or an explicit
  `secondbox-deploy` command. The `deploy-development-up` recipe stays as is.
- Existing generic aliases and `operation <operationId>` remain; new verbs are
  additive. `--output json` on a new verb emits the same API bytes the alias
  emits.
- Standard bundle lineages are append-only and release-owned; this plan does
  not add or revise a standard bundle.

## Decisions

- **Ceiling equals default.** The existing `resources` block on a
  ProfileRevisionSpec becomes both the default and the ceiling. A create
  request may state `resources: {vcpuCount?, memoryBytes?, workspaceBytes?}`;
  each stated value must be at or below the Profile value and at or above the
  schema minimum; each omitted value takes the Profile value. No Profile spec
  changes, no standard-bundle revision, no existing caller changes behavior.
- **The Sandbox pins its resolved resources.** The `Sandbox` response gains a
  required `resources` object. Persisted per Sandbox and backfilled from the
  pinned revision by migration. Quota reservation, workspace capacity, runner
  admission, and the Assignment message all read the Sandbox's pinned values,
  never the Profile's, after creation.
- **Sizes are a CLI convenience, not a server catalog.** `--size
  small|medium|large` expands client-side to (1 vCPU, 1 GiB, 4 GiB),
  (2 vCPU, 4 GiB, 16 GiB), (4 vCPU, 8 GiB, 50 GiB); explicit `--cpus`,
  `--memory`, `--disk` override individual axes. A request above the ceiling
  is refused by the server with a typed problem that names the ceiling, and
  the CLI renders it. No clamping.
- **The guided development install bootstraps a local Tenant and Subject and
  logs the CLI in as the platform authority with those refs.** Loopback-only
  development mode already places the operator and the user on one host. The
  platform token may call application routes while asserting refs
  (`docs/design/profiles-and-authorization.md`). This keeps one CLI session
  that can both `run` and `resources apply`. Production installs are
  unchanged. `secondbox-deploy bootstrap-tenancy` is the explicit form for
  every other topology and can additionally mint an application authority.
- **Snapshot references are `<sandbox>/<snapshot-name>`.** Snapshot names are
  unique per Sandbox after this plan; listing is per Sandbox, so the reference
  needs the Sandbox. Opaque `snp_…` identifiers are still accepted anywhere a
  reference is.

## Validation Commands

Run the focused commands listed in each task while implementing it. Before
handoff, run every repository-wide gate below from the repository root.

- `just verify-generated`
- `just lint`
- `just test` (needs `SECONDBOX_TEST_DATABASE_URL`)
- `just test-contract`
- `just test-compose`
- `just test-deployment`
- `just test-installer`
- `just test-cli-ui`
- `just test-sdk-packages`
- `git diff --check`
- `just test-scenario` on the qualified KVM host: required for Tasks 3 and 5
  because they change persistence and the lifecycle sequence.

## Dependencies

Tasks 1 and 2 are independent and start in parallel. Task 3 changes the
contract and must land before Task 4 touches `run`. Task 5 depends on Task 2
(it reuses the reference resolver and the verb scaffolding). Task 6 is
documentation and runs last.

### Task 1: Guided install ends with a working `secondbox run`

The installer writes a platform session with no tenant or subject reference
(`internal/install/cli_login.go`), and its final stage reads records instead
of executing anything.

- [x] Add `internal/install/tenancy.go` with a Go implementation of the
  bootstrap the shell script performs, using `sdk/go/secondboxclient`
  management helpers: platform-authenticated `createTenant` (with the
  installer's generated egress context name, the three standard Profile grants,
  the full application scope set, and development quota), then `createSubject`
  through a transient tenant-controller authority that is revoked when the
  bootstrap completes. Optionally (`--application`) mint an application
  authority and return its bearer token once. Expiry ceilings use the
  contract maximum, not 24 hours; a development install must not stop working
  the next day.
- [x] Add `secondbox-deploy bootstrap-tenancy <operation-directory>` with
  `--tenant-ref`, `--subject-ref` (defaults `local` / `local-operator`),
  `--application`, and `--check`. It reads the recorded platform token path
  from the operation record, never from flags or environment. Idempotent: an
  existing Tenant or Subject with the same ref is a recorded no-op, not an
  error.
- [x] Add a recorded installer stage `tenancy_bootstrap` to
  `install.StageSequence` (`internal/install/types.go`) between `readiness`
  and `smoke_execution`, completed in `cmd/secondbox-deploy/installer_resume.go`
  like its neighbors and resumable by index like every other stage. In the wizard it is one
  confirm ("Create a local tenant and subject so `secondbox run` works")
  defaulting to yes; `--unattended` accepts `--tenancy=no`. The operation
  record stores the refs and the stage evidence; it never stores a token.
- [x] Change `internal/install/cli_login.go` to write tenant and subject refs
  into the platform session when the stage ran. Confirm `platform login`
  accepts both refs and that sandbox routes accept a platform session carrying
  them; if the CLI refuses, fix the CLI, not the design.
- [x] Make `runInstalledSmoke` in `cmd/secondbox-deploy/installer_resume.go`
  actually run `secondbox run agent-compartment-isolated -- /bin/echo hello`
  through the installed CLI when the tenancy stage ran (isolated needs no
  egress context and boots in 1 vCPU), then `secondbox run durable-coding`
  only when the runner advertises the generated context. Record the guest
  exit status and stdout in the evidence. Without the tenancy stage keep the
  present record-only smoke and say so in the receipt.
- [x] Fix `README.md` line 31 to describe what the installer does, and keep
  it true after this task.
- [x] Cover in `internal/install/tenancy_test.go` against an httptest control
  plane: happy path, idempotent rerun, refusal when the platform token file is
  absent, and that no token appears in the operation record or receipt.
  Extend `cmd/secondbox-deploy` tests for the new stage and the unattended
  flag, and `tests/cliui` golden output for the receipt.
- [x] Update `docs/operations/guided-single-host-install.md` and
  `docs/operations/deployment.md`; the shell script stays for the non-guided
  path and now documents that `bootstrap-tenancy` is the maintained form.
- [x] Run `just test-installer`, `just test-deployment`, `just test-cli-ui`,
  `just test-install-docs`.

### Task 2: Friendly lifecycle, file, port, and snapshot verbs

Route every everyday operation through `SandboxHandle` so no user computes
a header. Names and identifiers are accepted everywhere the existing
`resolveSandboxReference` (`cmd/secondbox/sandbox_reference.go`) is used.

- [x] Add `cmd/secondbox/verbs_lifecycle.go`: `create <profile> [--name]
  [--metadata k=v] [--from <ref>]`, `start <sandbox>`, `stop <sandbox>`,
  `rm <sandbox>` (alias `delete`), `ls` (alias `list`, with `--all` for other
  states and `--name` filter through the `metadata` query parameter), and
  `get <sandbox>`. `start`, `stop`, and `rm` wait for the terminal state by
  default with `--no-wait` to return the Operation. `rm --force` skips the
  interactive confirmation that runs only on a TTY.
- [x] Add `cmd/secondbox/verbs_files.go`: `cp <src> <dst>` where exactly one
  side is `<sandbox>:<absolute-path>`; directories recurse with `-r`; files
  stream through `SandboxHandle.WriteFile` / `ReadFile` with the `Digest`
  header the SDK already computes. Add `ls-files <sandbox> <path>` only if
  `listDirectory` output needs a human table; otherwise leave the alias.
- [x] Add `cmd/secondbox/verbs_ports.go`: `ports forward <sandbox>
  <local>:<remote>` (or `<port>` for same-on-both) that creates a port
  session, listens on `127.0.0.1:<local>`, and pumps each accepted connection
  through `ConnectPortTunnel` (`sdk/go/secondboxclient`), holding a Lease for
  the session and closing the port session on exit or signal. `--bind` for a
  different local address.
- [x] Add `cmd/secondbox/verbs_snapshots.go`: `snapshot <sandbox> --name
  <name>` (waits for `ready`), `snapshots <sandbox>` list, `restore <sandbox>
  <sandbox>/<name>`, `snapshot rm <sandbox>/<name>`.
- [x] Add a snapshot reference resolver next to `resolveSandboxReference`:
  `<sandbox>/<name>` lists that Sandbox's Snapshots and matches `name`;
  `snp_…` is used as is. Make the server reject a duplicate ready Snapshot
  name per Sandbox (typed `snapshot_name_conflict`, checked in
  `createSnapshot`), so the reference is unambiguous; existing duplicates are
  guarded by a migration message the way `0002_sandbox_name_index.sql` guards
  Sandbox names.
- [x] Register every verb in `commandSummary`, the `cliui` help, and the
  output-classification table so the coverage test in `cmd/secondbox` passes.
  `--output json` emits the API response bytes; TTY output uses the bounded
  views in `bounded_views.go`.
- [x] Cover each verb in `cmd/secondbox/*_test.go` against the existing
  httptest transport: header computation is never in the CLI (assert the
  requests carry `If-Match`, `SecondBox-Generation`, `Idempotency-Key` from the
  SDK), name and identifier resolution, `cp` both directions and the
  refusal of two remote sides, `ports forward` end-to-end over a local
  WebSocket stub, and `rm` confirmation on TTY only.
- [x] Run `just test-cli-ui`, `go test ./cmd/secondbox -race`, `just test`.

### Task 3: Requested resources within the Profile ceiling

This is the only task that changes a published contract and persisted
schema.

- [x] OpenAPI: add `SandboxResourceRequest` (`vcpuCount?`, `memoryBytes?`,
  `workspaceBytes?`, same minimums as `ProfileResources`,
  `additionalProperties: false`) as optional `resources` on
  `CreateSandboxRequest`; add required `resources` (`vcpuCount`,
  `memoryBytes`, `workspaceBytes`) to `Sandbox`. Add problem code
  `resources_exceed_profile` with `ceiling` and `requested` detail fields.
  Regenerate both SDKs (`just verify-generated`).
- [x] Migration `00NN_sandbox_resources.sql`: add non-null resource columns to
  the sandbox table, backfilled from each row's pinned ProfileRevision spec.
  Follow the forward-only migration conventions in `migrations/postgres`.
- [x] Creation (`internal/service`, `internal/store/postgres_store.go`):
  resolve requested against the pinned revision, refuse above-ceiling with
  the typed problem before allocating durable intent, persist resolved values,
  reserve Tenant and Subject quota from the resolved vCPU and memory.
- [x] Every later read of resources uses the Sandbox row, never a join to
  `profile_revisions.spec_json`. Known sites: quota usage sums in
  `readSubjectQuotaUsage`, `readTenantQuotaUsage`, and
  `readDeploymentQuotaUsage` (`internal/store/postgres_store.go`); the start
  reservation delta in `internal/store/postgres_lifecycle.go`; workspace
  `logical_capacity_bytes` and `LocalWorkspaceCommand.LogicalCapacityBytes`
  in `CreateSandbox`; the snapshot-clone capacity guard; home selection in
  `runnerPlacementCompatible` (`internal/store/postgres_runner_placement.go`);
  `durableRunnerReservation` (`internal/runnercontrol/postgres.go`); and
  `loadStartPlan` plus `AssignmentCommand.Requirements` and the scheduler
  `Capacity` in `internal/lifecycle/postgres_effects.go`.
  `concurrentOperations` is not requestable and stays on the revision.
- [x] Runner protocol: `ProfileRequirements` already carries explicit
  `vcpu_count`, `memory_bytes`, and `disk_bytes`, so no protocol change is
  expected. Do not bump the protocol generation.
- [x] Runner: no protocol change if the Assignment already carries explicit
  vCPU, memory, and workspace capacity. If it carries a revision reference
  instead, add the fields; do not bump the protocol generation unless the
  runner cannot otherwise honor them.
- [x] SDK: `CreateSandboxRequest.Resources` in Go and TypeScript; `Run` and
  `createSandbox` pass it through.
- [x] Cover: contract tests for the new schema and problem; store tests for
  ceiling refusal, partial requests, backfill migration; integration test in
  `tests/integration` creating below, at, and above the ceiling and asserting
  quota accounting uses the resolved values.
- [x] Run `just verify-generated`, `just test-contract`, `just test`,
  `just test-compose`.
- [ ] Shepherd: run `just test-scenario` on the qualified KVM host before merge.

### Task 4: `run` and `create` accept sizes

- [x] Add `--cpus`, `--memory`, `--disk` (byte-size parsing with `GiB`, `MiB`,
  `g`, `m` suffixes) and `--size small|medium|large` to `run` and `create`.
  Explicit axes override the preset. The preset table is one exported map in
  `cmd/secondbox` and appears in `--help`.
- [x] Render the `resources_exceed_profile` problem on a TTY as a next-command
  hint naming the ceiling and the Profile.
- [x] Show resolved resources in the `run --keep` summary and in `get`.
- [x] Cover parsing, preset override, and the problem rendering.
- [x] Run `just test-cli-ui`.

### Task 5: Golden-snapshot workflow

- [x] `run` and `create` accept `--from <sandbox>/<name> | snp_…` and send
  `sourceSnapshotId`. Cross-Sandbox creation from a Snapshot already exists
  server-side; verify the Profile compatibility rule (same Profile or same
  workspace capacity ceiling) and surface its typed problem.
- [x] Add `examples/resources/durable-coding-registries.json`: an operator
  Profile document derived from `durable-coding` that additionally allows
  `registry.npmjs.org`, `pypi.org`, `files.pythonhosted.org`,
  `deb.debian.org`, and `proxy.golang.org` over HTTPS, so a user can install a
  toolchain, snapshot, and reuse. This is an example document, not a standard
  bundle.
- [x] Document the workflow in `docs/operations/sdk-cli-and-flue.md`.
- [x] Run `just test-cli-ui`, `just test-standard-resources`.

Task 5 implementation note: the existing clone guard requires exact equality
between Snapshot capacity and the target Sandbox's resolved disk capacity,
not Profile identity or ceiling equality. Its public problem is HTTP 409
`state_conflict`; no contract was changed. The shepherd owns KVM scenario
validation of this workflow.

### Task 6: Documentation and README

- [x] Rewrite the "Using the CLI" section of `README.md` around the target
  shape, keep the transport section as "Everything else", and keep the
  installer sentence truthful.
- [x] Update `docs/operations/cli-output-contract.md` classification for every
  new verb.
- [x] Add a CHANGELOG entry under Unreleased following `update-changelog`.
- [x] Run `just test-install-docs` and `scripts/test-install-docs.sh`.

### Task 7: Qualified CLI target-shape scenario

The implementor writes and locally verifies the test; the shepherd runs the
qualified Firecracker scenario suite before merging.

- [x] Add `tests/scenario/cli_target_shape_test.go` under `scenario_live`,
  using the existing fixture, explicit scenario application authority,
  runner/Profile helpers, polling, and failure cleanup. Build the real CLI
  once per test binary in a temporary directory and isolate subprocess
  configuration from ambient sessions. Never print credentials.
- [x] Create a Snapshot-enabled harness Profile with a 1 GiB memory ceiling
  and explicitly grant it in the scenario bootstrap. Exercise retained
  `run` with 1 vCPU/1 GiB, JSON `get`, and an above-ceiling refusal.
- [x] Exercise file upload, guest `exec`, download with byte comparison,
  named Snapshot creation, `create --from <sandbox>/golden`, clone start,
  and guest reads proving the cloned Workspace contains the file.
- [x] Exercise stop/start, filtered `ls`, and `rm --force` for both
  Sandboxes, asserting subprocess stdout/exit status and terminal states.
  Bound the workflow to two minutes; omit ports because this Profile has none.
- [x] Add untagged subprocess checks against an HTTP stub for resource
  arguments/refusal, session isolation, file bytes/digest, guest streams and
  exit status, and cancellation. Keep real compute verification separate.
- [x] Run `go vet ./tests/scenario ./cmd/secondbox`,
  `go test -c -o /dev/null ./tests/scenario`, their `scenario_live`
  equivalents, the stub test, `just verify-generated`, `just test`,
  `just lint`, and `git diff --check`.
- [ ] Shepherd: run `just test-scenario` on the qualified Firecracker host
  and confirm the workflow runtime before merge.

Task 7 deviations from the requested command sequence: Snapshots require a
stopped Sandbox, so explicitly stop the source before Snapshot creation.
The running Profile required by `run` also boots cloned Sandboxes; wait for
clone creation and stop it before testing explicit `start`. Piped lifecycle
and Snapshot stdout currently contains the admitted Operation, so assert that
JSON plus the terminal resource state through the fixture. The original
harness Profile has a 256 MiB ceiling; this dedicated Profile raises only
memory to 1 GiB and retains the 64 MiB Workspace and four-Snapshot policy.

Local gates passed with protoc 35.1 and isolated Go/linter caches. Additional
`scenario_live` lint reports existing findings in `snapshot_test.go:76` and
`runner_enrollment_test.go:171`; the Task 7 files have no reported findings.

### Task 8: Size bounded by quota and Runner admission, not by the Profile

Task 3 made the Profile's `resources` both default and ceiling. That was the
smallest compatible step, not a principle: Tenant and Subject quota already
account vCPU and memory per Sandbox, and Runner admission already refuses
what a host cannot hold. The Profile keeps owning image, network, ports,
execution bounds, and startup mode; it stops being the size catalog.

Decisions:

- `ProfileRevisionSpec.resourceCeiling` is an optional object. Absent means
  what Task 3 shipped: `resources` is the ceiling. Present, it carries all
  three axes `vcpuCount`, `memoryBytes`, `workspaceBytes`, each either a
  positive integer at or above the matching `resources` value or `null`,
  meaning the Profile places no bound on that axis and only quota and Runner
  admission do. No axis may be omitted; the control plane never infers one.
- A `snapshot_resume` revision rejects `resourceCeiling` at publish time,
  because a resume template is keyed by vCPU count and memory bytes
  (`runner/internal/firecracker/snapshot_template.go`). A create request on
  a resume Profile that states any axis different from `resources` is refused
  with the new typed problem `resources_fixed_by_profile`; equal values are
  accepted.
- A requested `workspaceBytes` is rounded up to the next power of two before
  the ceiling check, so the WorkspaceStore serves a small set of ext4
  templates rather than one per arbitrary value. The Sandbox pins and reports
  the rounded value. An omitted axis still takes the Profile value unrounded
  (standard bundles use 50 GiB).
- The Runner prewarms every power-of-two ext4 template from the schema
  minimum that is at or below its configured maximum Workspace capacity,
  in addition to that maximum, so a rounded request never formats on the
  create path.
- vCPU and memory stay continuous integers.

- [x] OpenAPI (`contracts/openapi/v1/secondbox.openapi.json`,
  `ProfileRevisionSpec`) and `pkg/contracts/contracts.go`: add
  `resourceCeiling` with per-axis `integer | null`; add problem code
  `resources_fixed_by_profile`; regenerate both SDKs and the TypeScript
  public surface (`just verify-generated` with the pinned protoc on PATH).
- [x] `validateProfileRevisionSpec` (`internal/service/control_plane_service.go`):
  ceiling axes at or above `resources`; ceiling rejected on
  `snapshot_resume`; every axis present when the object is present.
- [x] `resolveSandboxResources` (`internal/store/sandbox_resources.go`):
  round `workspaceBytes` requests up to a power of two (never below the
  schema minimum), apply the per-axis ceiling or the `resources` value when
  the object is absent, skip the bound for a `null` axis, refuse any
  difference on resume Profiles with `resources_fixed_by_profile`. The
  `resources_exceed_profile` problem's `ceiling` reports the effective bound
  and omits a `null` axis.
- [x] Runner prewarm (`runner/internal/workspacestore/store.go`, the
  `ensureTemplate` call at open with `templateCapacityBytes`): also ensure
  each power-of-two capacity from `minimumExt4Bytes` up to
  `templateCapacityBytes`. Keep the validation of existing templates exact.
- [x] Standard bundles are unchanged (absent ceiling). Add
  `examples/resources/durable-coding-flexible.json`: `durable-coding`'s spec
  with `resourceCeiling: {vcpuCount: null, memoryBytes: null,
  workspaceBytes: 274877906944}` validated by a test through the resource
  engine like the registries example.
- [x] CLI: `--disk` help and the retained-Sandbox summary state that disk
  rounds up to a power of two; render `resources_fixed_by_profile` with a
  hint naming the Profile's fixed size.
- [x] Cover: contract tests for the schema and both problem codes; service
  tests for ceiling validation and the resume rejection; store tests for
  rounding, `null` axes, absent object, and the resume refusal; a
  `tests/integration` test creating above the Profile default but under
  quota on a `null`-ceiling Profile and refused by quota beyond it; a
  workspacestore test proving every power-of-two template exists after open;
  extend `TestScenarioCLITargetShape` (or add a sibling scenario) so a
  `null`-memory-ceiling Profile boots a Sandbox with `--memory` above the
  Profile default, and `--disk` reports the rounded value.
- [x] Update `docs/design/profiles-and-authorization.md`, the README size
  section, `docs/operations/sdk-cli-and-flue.md`, and the CHANGELOG entry.
- [x] Run `just verify-generated`, `just test-contract`, `just test`,
  `just test-standard-resources`, `just test-cli-ui`, `just lint`,
  `go test ./runner/internal/workspacestore`, `git diff --check`.
  `just test-scenario` on the KVM host is the shepherd's gate.

Task 8 implementation notes: prewarming uses the Runner's existing
`minimumExt4Bytes` (64 MiB), not the OpenAPI minimum (1 MiB). The qualified
formatter requires that floor; lowering it is outside this task. Explicitly
equal resume resources bypass disk rounding so non-power-of-two defaults keep
their exact resume identity. Disk rounding makes the unchanged large preset's
50 GiB request resolve to 64 GiB; the unchanged standard durable-coding ceiling
refuses it. Documentation now uses omitted disk defaults or an explicit 32 GiB
override. The existing granted `scenario-cli-target-shape` Profile now has a
null memory ceiling and exercises 1 GiB above its 256 MiB default plus a 33 MiB
disk request resolved to 64 MiB; no new Profile grant is needed.

The implementor's local gates pass. The shepherd still owns qualified KVM
execution of `TestScenarioCLITargetShape`; no live scenario was run here.

## Defects found by the qualified CLI scenario

- **Cross-Sandbox clone of a used Workspace failed on the Runner (fixed).**
  `tune2fs -U` refuses a `metadata_csum` filesystem whose last mount time is
  newer than its last check, and every Snapshot of a Workspace a guest
  mounted is in that state. The prior cross-Sandbox Snapshot scenario cloned
  a Workspace that had never been mounted, so it never saw the refusal. The
  Linux and Darwin drivers now run `e2fsck -f -y` only on that refusal and
  rewrite again; a never-mounted template keeps the zero-fsck fast path.
  Covered by `TestLinuxSetUUIDRewritesMountedSinceCheckFilesystem`.
- **A Runner-failed clone command is retried without bound (open).** With
  the defect above, the control plane re-dispatched the failing
  `CLONE_FROM_SNAPSHOT` command about three times a second (2 076 attempts
  in two minutes) instead of failing the create Operation, and the HTTP API
  became unresponsive for the rest of the suite. Deleting the Sandbox then
  reported `workspace_local_data_absent`. A permanent
  `LOCAL_WORKSPACE_TERMINAL_KIND_RUNNER_FAILED` result should fail the
  Operation, or at least back off; this needs its own plan.

## Deferred

- **Images as a first-class axis.** Needs a Firecracker runner that verifies
  and advertises N bundles instead of one
  (`runner/internal/firecracker/assignment_backend.go`), an
  OCI-to-ext4 builder that injects the guest agent, and
  `secondbox images import` that writes the materialization and the asset
  catalog entry together. Until then one image per pool is the honest limit.
- **A standard bundle with registry egress.** Standard lineages are
  release-owned; adding one is a release decision. Task 5 ships an example
  operator document instead.
- **Named size catalog on the server.** Presets stay client-side until a
  second client needs them.
