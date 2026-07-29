# Plan: SecondBox Simplification

Status: completed 2026-07-28.

## Overview

SecondBox was extracted from `SecondStack/apps/sandbox-service` and generalized into a standalone,
self-hostable, multi-tenant network service. That generalization added an identity hierarchy, a
release-qualification apparatus, and a compatibility-freeze regime sized for untrusted third-party
consumers and mutually distrusting tenants.

The actual consumer is SecondStack: a single trusted AI conversational platform on a private network,
using sandboxes for two things — ephemeral agent turns under Flue, and longer-running coding-agent
sandboxes with PTY access. The platform already owns end-user identity (Authentik → ControlTower) and
already runs a dedicated reconciliation service for identity sync across three systems.

This plan removes the guarantees SecondBox makes to strangers and keeps the guarantees it makes to the
sandbox.

**Guiding principle:** simple over complex; trusted environment; do not over-protect locally; do not
over-guarantee. Do the thing — running sandboxes — well, and not much more.

### Problem it solves

- A release/qualification/publication apparatus (~25 scripts, 4 workflows, `release/`, and the
  matching test packages) proving qualified releases to external consumers that do not exist, and
  which currently fails the test suite.
- A Project → ServiceAccount → APIKey hierarchy (7 tables) that would become a fourth identity-sync
  target in a platform that already needed `apps/sync-jobs` for three.
- A runner enrollment/rotation/revocation lifecycle (822 LOC) built for enrolling untrusted hosts.
- Operator-authored profiles where the predecessor shipped working built-ins.
- A bespoke 1,510-LOC three-language SDK generator for two internal callers.

### What this explicitly preserves

The correctness core is not over-engineering — it is why a coding agent's workspace survives a runner
dying. See [Do not cut](#do-not-cut).

## Context (from discovery)

**Verified state at plan time**

- Both Go modules build clean; `go vet ./...` clean on both.
- Zero `TODO`/`FIXME`/`HACK` markers in non-test code across ~115k LOC.
- Two test failures: `TestPublicPortTunnelIsBinarySingleUseBackpressuredAndAccounted` (flake — failed
  1 of 2 full-suite runs, passes 3/3 isolated) and
  `TestInitialV1CompatibilityBaselineFreezesExecutableUpgradeInputs` (deterministic — OpenAPI contract
  edited without re-freezing its baseline hash).

**Reference pattern — the predecessor**

`SecondStack/apps/sandbox-service` is the proven prior art and the target shape:

- Single shared bearer token gates every endpoint (`internal/api/http.go:35`).
- `TenantRef` + `SubjectRef` are opaque caller-asserted strings on `Environment` and `Workspace`.
- Natural key is `(TenantRef, SubjectRef, EnvironmentKey)` (`internal/store/postgres_store.go:99`).
- `WorkspaceUsage` aggregates quota per subject.
- Built-in lifecycle policies: `agent-compartment`, `chat-thread`, `coding-environment`.
- Consumers: `apps/chat/chainlit` (Python), `apps/chat/chat-app` (Python),
  `apps/controltower` (TypeScript). No Go consumer.

**Dependencies and constraints discovered during plan review**

- `internal/ports/ports.go` is driver-pure (zero pgx references) — the store seam survives the rewrite.
- `internal/scheduler/placement.go` has no project coupling — scheduling is unaffected by Phase 2.
- **`tests/integration` (16,470 LOC, 25 files, 170 fixture call sites)** builds every fixture from
  `BootstrapAdmin`, `createProjectAccountAndCredential`, and `CreateAPIKey`. This is the single
  largest consumer of everything Phase 2 deletes.
- **`tests/integration/schema_contract_test.go:22-40`** requires all 7 tenancy tables to exist *and*
  explicitly forbids the literal strings `tenant_ref` and `subject_ref` in the baseline SQL. It blocks
  the schema rewrite in both directions.
- **`tests/integration/upgrade_compatibility_postgres_test.go:30`** imports
  `tests/compatibility/initialv1client`. Deleting `tests/compatibility/` breaks the whole
  `integration_test` package, not just the compatibility tests.
- **The migration catalog fingerprint is not the sequencing blocker.**
  `adoptExactInitialV1Baseline` runs only when a schema exists with an empty ledger
  (`migrations/postgres/migrations.go:86-98`); the integration harness drops the schema first
  (`tests/integration/control_plane_postgres_test.go:52-56`), so that branch never executes. What
  actually rejects a `0001` edit against a live database is the checksum-drift check at
  `migrations.go:208-215`. The frozen *contract* hashes are the real Phase 2 blocker.
- **`service_account_id` is a live ownership check, not a label.** `internal/store/postgres_lifecycle.go`
  at `:672`, `:730`, `:1439`, `:1869` rejects lease renew, lease release, and artifact publication when
  the caller's service account differs from the one on the lease. It must map to `subject_ref`, not
  vanish.
- **The Go SDK is used by the CLI** (`cmd/secondbox/main.go:15,119,181-183`), and `sdk/typescript/`
  contains hand-written `client.ts`, `flue.ts`, and `flue-runtime-beta9-compat.ts` alongside the one
  generated file. `flue.ts` is the Flue `SandboxApi` adapter — a core use case. Only `*.gen.*` files
  are generated.
- **`internal/reconcile/` (824 LOC) and `internal/runnercontrol/` carry project scoping** and were
  absent from the first draft of this plan. `internal/reconcile` is part of the correctness core.
- **`objectstore.PutImmutable` already streams** (`internal/objectstore/s3.go:109-137`) and requires
  `sizeBytes` + `expectedSHA256` up front. A bare `io.Reader` from the request body therefore cannot
  work; artifacts need spool → hash → stage → stream.

## Development Approach

- **Testing approach**: Regular (code first, then tests).
- Complete each task fully before moving to the next.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task. Deletion
  tasks must still assert the removal (route unregistered, table absent, import gone).
- **CRITICAL: all tests must pass before starting the next task** — no exceptions. Phase 2 is
  decomposed specifically so this remains achievable; see [Phase 2 strategy](#phase-2-strategy).
- **CRITICAL: update this plan file when scope changes during implementation.**
- Backward compatibility is explicitly **not** maintained for the public API, the database schema, or
  the SDKs. SecondBox is pre-release and consumed only by SecondStack; both sides deploy together.

## Testing Strategy

- **Unit tests**: required for every task.
- **Integration tests**: `tests/integration` runs against a real disposable PostgreSQL. There is no
  mock mode and none is to be introduced.
- **E2E**: no UI. `tests/integration` plus `tests/sdk-live` are the equivalent; same rigor.
- **Known flake**: fixed in Task 1 before anything structural moves.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.

## Validation Commands

```sh
SECONDBOX_TEST_DATABASE_URL='postgres://…/secondbox_test?sslmode=disable' go test ./... -count=1
go vet ./...
cd runner && go test ./... -count=1 && go vet ./...
```

## Do not cut

Applying "simple over complex" to these would remove the reason the product works:

- Generation, fencing token, and Assignment model.
- Lifecycle reconciliation (`internal/reconcile`, `internal/lifecycle`), revision-fenced CAS,
  `FOR UPDATE SKIP LOCKED` claiming.
- Idempotency on lifecycle mutations, including the per-scope unique indexes.
- The lease-holder ownership binding currently carried by `service_account_id`.
- Capability tokens for PTY and port tunnels — single-use, session-bound, expiring.
- Checkpoint publication, verified restore, workspace durability, and the backup/restore fencing tests.
- Immutable ProfileRevision pinning.

## Phase 2 strategy

The schema change is made **additive first** so the tree stays green at every step:

1. Add `tenant_ref`/`subject_ref` alongside `project_id`, backfilled identically (Task 7).
2. Migrate one seam at a time, each independently green (Tasks 8–12).
3. Only then drop `project_id` and the identity-admin surface (Task 15).

This costs one extra edit to `0001` — which is free, since schema backward compatibility is already
out of scope — and avoids a multi-thousand-line task where nothing compiles.

---

## Implementation Steps

### Phase 0 — Make the ground trustworthy

### Task 1: Fix the port-tunnel relay flake

**Files:**
- Modify: `internal/runnercontrol/postgres_relay.go`
- Modify: `tests/integration/port_tunnel_http_test.go`

- [x] reproduce under full-suite concurrency until the failure recurs
- [x] note the assertion is **negative**: `assertNoRunnerBoundPortBytes`
      (`tests/integration/port_tunnel_http_test.go:296-318`) fails when a frame *is* claimable, so a
      late-poll race would make it spuriously pass, not fail. The observed `bytes=[]` is a claimable
      Port frame with an empty payload existing before credit was granted
- [x] investigate frame-state leakage and claim-expiry reuse under shared-database contention — not
      `DataPlanePollInterval`
- [x] fix the root cause; do not add a sleep and do not weaken the assertion
- [x] write a test that fails deterministically against the old behavior
- [x] run the single test 10x and the full suite 2x — must pass before Task 2

### Task 2: Replace response-write panics with logged aborts

**Files:**
- Modify: `internal/api/http.go`

- [x] replace panics at `:174` (binary write), `:401` (Artifact write), `:1301` (`writeJSON` encode),
      and `:1313` (metrics write) with logged aborts
- [x] leave `:1321` (`requestPrincipal` missing Principal) as a panic — genuine programmer error
- [x] write a test asserting a mid-download client disconnect logs and does not panic
- [x] run tests — must pass before Task 3

---

### Phase 1 — Delete the guarantee apparatus

### Task 3: Delete the release qualification and publication apparatus

**Files:**
- Delete: `tests/release/`, `release/`
- Delete: `tests/operations/{clean_clone_ci_policy,multirunner_qualification_controller,upgrade_compatibility}_test.go`
- Move: `tests/operations/deployment_policy_test.go` → `tests/deployment/`
- Delete: all ~25 `scripts/*release*`, `scripts/*publish*`, `scripts/*qualification*` files
- Delete: `.github/workflows/{release-evidence,publish,release-candidate,release-qualification}.yml`
- Modify: `Justfile`, `scripts/test-non-kvm.sh`, `README.md`

- [x] enumerate the full deletion set first (`ls scripts/ | grep -E 'release|publish|qualification'`)
      rather than working from this list — the first draft of this plan named 5 of ~25
- [x] **preserve `tests/operations/deployment_policy_test.go` by moving it to `tests/deployment/`** —
      1,270 of 1,848 lines are compose/env parity and backup/restore fencing coverage, including
      `TestBackupHoldsSharedDatabasePublicationFenceThroughDump` and
      `TestRestoreDrillSupervisesFreshRunnerVerifierThroughLiveChecks`, which guard a "Do not cut" item
      and are exactly what Tasks 13 and 18 are about to churn
- [x] decide and record: keep or delete `cmd/secondbox-multirunner-admin/` and
      `docs/operations/multi-runner-qualification.md`. If kept, keep their test coverage too — do not
      leave a qualification path with no tests

      Decision: deleted both, together with the release-bound multi-runner controller and its tests.
- [x] reduce release to a git tag plus a CI run of the Go suite
- [x] write a CI smoke check asserting `just test-non-kvm` still passes end to end
- [x] run tests — the full suite should be green here for the first time; must pass before Task 4

### Task 4: Delete the compatibility freeze machinery

**Files:**
- Delete: `tests/compatibility/`, `docs/design/compatibility-policy.md`,
  `docs/operations/compatibility.md`
- Delete: `tests/integration/upgrade_compatibility_postgres_test.go`
- Modify: `runner/internal/runnercontrol/upgrade_compatibility_test.go`

- [x] delete `tests/integration/upgrade_compatibility_postgres_test.go` (653 lines) **in the same
      task** — it imports `tests/compatibility/initialv1client` at `:30`, so deleting
      `tests/compatibility/` alone breaks the entire `integration_test` package
- [x] this also removes `TestPostgresMigrationAdoptsOnlyTheExactInitialV1Catalog` (`:35`) and
      `initialV1MigrationSHA256` (`:641`), which freeze `0001`
- [x] delete adjacent-generation and rolling-replacement qualification assertions
- [x] keep runner/guest protocol *generation negotiation* itself — only the freeze regime goes
- [x] write a test asserting generation negotiation still rejects an out-of-window guest
- [x] run tests — must pass before Task 5

### Task 5: Unfreeze the schema contract

**Files:**
- Modify: `tests/integration/schema_contract_test.go`

- [x] invert the required-table set at `:22-30` and remove `tenant_ref`/`subject_ref` from the
      forbidden-string list at `:34` — as written the test blocks Task 7 in both directions
- [x] keep the genuinely useful forbidden fragments (`foreign key`, ` references `, ` check `) and the
      `resource_classes`/`lifecycle_policies`/`agent_service` extraction guards
- [x] leave `migrations/postgres/migrations.go` alone — the catalog fingerprint is not the blocker
      (see Context); re-derive `initialV1CatalogSHA256` after Task 15 instead
- [x] write a test asserting migrations still apply cleanly and still reject a reordered lineage
- [x] run tests — must pass before Task 6

---

### Phase 2 — Collapse tenancy

### Task 6: Consolidate integration test fixtures behind one helper

**Files:**
- Create: `tests/integration/fixture_test.go`
- Modify: all 25 files under `tests/integration/`

- [x] add `newControlPlaneFixture(t)` and `newSubject(t) (tenantRef, subjectRef string)` covering the
      170 existing call sites of `BootstrapAdmin`, `createProjectAccountAndCredential`, `CreateAPIKey`,
      `UpdateServiceAccount`, and `createGrantedProfile`
- [x] route every integration test through the helper with **no behavior change** — this task is pure
      indirection so that Task 13 rewrites one file instead of twenty-five
- [x] do the same for `tests/contract/` and `tests/sdk-live/` fixtures
- [x] run tests — must pass unchanged before Task 7

### Task 7: Add ownership columns additively

**Files:**
- Modify: `migrations/postgres/0001_secondbox.sql`

➕ Clarification from implementation: the prose said 13 tables while the following list named 14.
`instances` has no `project_id`; the 13 project-owned tables receive the additive generated backfill.
Instance authority continues to be fenced through its logically scoped Sandbox.

- [x] add `tenant_ref` and `subject_ref` **alongside** `project_id` on all 13 tenant-owned tables:
      `sandboxes`, `workspaces`, `instances`, `leases`, `activity_sessions`, `activity_touches`
      (`:404`), `workspace_checkpoints` (`:442`), `snapshots`, `artifacts`, `port_sessions`,
      `data_plane_sessions` (`:536`), `operations`, `idempotency_records` (`:643`), `audit_events`
      (`:660`)
- [x] backfill both from `project_id` so existing behavior is unchanged
- [x] add `subject_quotas` keyed by `(tenant_ref, subject_ref)`
- [x] do **not** drop anything yet and do **not** change the idempotency indexes yet
- [x] write a migration test asserting a fresh apply produces the expected columns and indexes
- [x] run tests — must pass before Task 8

### Task 8: Migrate the sandbox and workspace seam

**Files:**
- Modify: `internal/ports/ports.go`, `internal/store/postgres_store.go`,
  `internal/service/control_plane_service.go`

- [x] change sandbox and workspace scoping parameters from `projectID` to `tenantRef, subjectRef`
- [x] keep every `WHERE` clause exactly as strict as it is today
- [x] write store tests asserting cross-subject sandbox reads return not-found
- [x] run tests — must pass before Task 9

### Task 9: Migrate the lease and activity seam

**Files:**
- Modify: `internal/store/postgres_lifecycle.go`, `internal/ports/ports.go`

- [x] migrate `leases`, `activity_sessions`, `activity_touches` scoping
- [x] **map `service_account_id` → `subject_ref`** on these tables and keep the equality checks at
      `postgres_lifecycle.go:672`, `:730`, `:1439`, `:1869` intact against the caller's asserted
      subject ref — this is the lease-holder ownership binding, not a label
- [x] migrate the `activity_touches_idempotency_idx` (`0001:414-415`) scope
- [x] write a test asserting lease renewal by a *different subject* still fails
- [x] write tests asserting activity-session fencing still closes on generation advance
- [x] run tests — must pass before Task 10

### Task 10: Migrate the snapshot, artifact, and checkpoint seam

**Files:**
- Modify: `internal/store/postgres_snapshots.go`, `internal/store/postgres_lifecycle.go`,
  `internal/service/artifacts.go`, `internal/service/snapshots.go`

- [x] migrate `snapshots`, `artifacts`, `workspace_checkpoints` scoping
- [x] keep the artifact publication ownership check (`postgres_lifecycle.go:1439`) against subject ref
- [x] write tests asserting cross-subject artifact and snapshot reads return not-found
- [x] write a test asserting retention-expired objects remain invisible
- [x] run tests — must pass before Task 11

### Task 11: Migrate the port-session and data-plane seam

**Files:**
- Modify: `internal/runnercontrol/postgres_relay.go` (24 `project_id` sites),
  `internal/runnercontrol/postgres_port_sessions.go` (19),
  `internal/runnercontrol/postgres_session_cancellation.go` (5),
  `internal/runnercontrol/port_sessions.go`,
  `internal/service/port_sessions.go`, `internal/service/data_plane.go`,
  `internal/service/terminal_sessions.go`

- [x] migrate `port_sessions` and `data_plane_sessions` scoping and their idempotency indexes
      (`0001:529-530`, `:592-594`)
- [x] leave the capability-token design untouched — it is not tenancy-derived
- [x] write tests asserting a port tunnel bound to one subject rejects another's token
- [x] run tests — must pass before Task 12

### Task 12: Migrate the operations, idempotency, and audit seam

**Files:**
- Modify: `internal/reconcile/postgres.go` (`:482`, `:598`), `internal/store/postgres_store.go`,
  `internal/lifecycle/postgres_effects.go`

- [x] migrate `operations`, `idempotency_records`, `audit_events` scoping
- [x] **rewrite the `idempotency_records` unique index** from
      `(project_id, operation, target_id, idempotency_key)` (`0001:654-655`) to the subject-scoped
      equivalent — leaving it on `project_id` silently changes idempotency scope
- [x] write a test asserting idempotent replay is scoped per subject and cross-subject keys do not
      collide
- [x] write a test asserting reconciliation claiming is unaffected
- [x] run tests — must pass before Task 13

### Task 13: Replace API-key authentication with a platform token

**Files:**
- Modify: `internal/api/http.go`, `internal/service/control_plane_service.go`,
  `internal/config/config.go`, `cmd/secondboxd/main.go`, `pkg/contracts/contracts.go`
- Modify: `tests/integration/fixture_test.go`
- Modify: `deploy/compose.yml`, `deploy/environment.example`,
  `deploy/bin/validate-environment.sh`, `deploy/bin/bootstrap-environment.sh`,
  `scripts/compose-test.yml`

- [x] add required `SECONDBOX_PLATFORM_TOKEN`; remove `SECONDBOX_BOOTSTRAP_ADMIN_TOKEN` and
      `SECONDBOX_API_KEY_HASH_SECRET` (`internal/config/config.go:91,95`)
- [x] replace the API-key middleware with a constant-time bearer comparison; read `tenantRef` and
      `subjectRef` from bounded request headers into request context
- [x] delete the 12 scopes and `requireAdminScope` (35+ call sites in `control_plane_service.go`),
      plus `AuthenticateCredential`, `bootstrapCredentialHash` (`:112`), and `BootstrapAdmin` (`:135`)
- [x] update all deploy/env/compose wiring in the same task or `just test-compose` breaks
- [x] rewrite `tests/integration/fixture_test.go` (the single helper from Task 6)
- [x] write tests for missing/wrong token, missing/malformed refs, and cross-subject denial
- [x] run tests — must pass before Task 14

### Task 14: Delete the administrative surface and realign the contract

**Files:**
- Delete: `cmd/secondbox/operator_commands.go`
- Modify: `internal/api/http.go` (routes `:66-74`), `internal/api/runner_admin_http.go`,
  `cmd/secondbox/main.go`, `contracts/openapi/v1/secondbox.openapi.json`
- Modify: `tests/contract/openapi_contract_test.go`,
  `tests/contract/openapi_http_conformance_test.go`

- [x] remove the 11 admin routes and handlers together with their OpenAPI declarations **in one task**
      — `TestCanonicalOpenAPIOperationsReachHTTPRouter` (`:129`) and
      `TestCanonicalOpenAPICoversEveryPublicHTTPRoute` (`:163`) enforce bidirectional equivalence, so
      splitting them leaves the suite red
- [x] update `TestAuditedHTTPHandlersContainConformanceMarkers` (`:207`) for the surviving handlers
- [x] **decision, recorded now:** `runner_admin_http.go` currently gates on `ScopeAdminRunners`, which
      Task 13 deletes. Runner administration is gated by the platform token alone from here — write
      that into `docs/design/security.md`
- [x] write contract tests asserting removed schemas are absent and the new headers are required
- [x] run tests — must pass before Task 15

### Task 15: Drop the legacy columns and identity-admin surface

**Files:**
- Modify: `migrations/postgres/0001_secondbox.sql`, `internal/ports/ports.go`,
  `internal/store/postgres_store.go`
- Modify: `internal/store/postgres_admin_idempotency.go`
- Modify: `internal/service/control_plane_service.go`
- Modify: `tests/integration/schema_contract_test.go`

- [x] drop `project_id` from all 13 tables and drop `operators`, `operator_credentials`, `projects`,
      `project_quotas`, `service_accounts`, `api_keys`, `profile_quotas`
- [x] remove the 16 identity-admin methods from `ControlPlaneStore` (67 methods today → 51)
- [x] delete **only** `sealIdempotentCredential`, `openIdempotentCredential`,
      `idempotencyCredentialAEAD`, `adminIdempotencyAssociatedData`, and the
      `idempotency_records.response_secret` column (`0001:650`)
- [x] **keep `AdminIdempotencyInput`/`AdminIdempotencyResult`** — `CreateProfile`, `ReviseProfile`, and
      `DisableProfile` take them (`ports.go:293-295`) and Task 19 keeps operator-defined profiles
- [x] re-derive `initialV1CatalogSHA256` and update `schema_contract_test.go` to the final shape
- [x] write tests asserting the dropped tables are absent and profile idempotency still replays
- [x] run tests — must pass before Task 16

### Task 16: Implement per-subject quota

**Files:**
- Modify: `internal/service/control_plane_service.go`, `internal/store/postgres_lifecycle.go`,
  `pkg/contracts/contracts.go`

- [x] replace the project/profile `QuotaLimits` duality with one per-subject limit set
- [x] enforce transactionally inside the admission transaction — no read-then-write
- [x] expose aggregate usage per subject, modeled on the predecessor's `WorkspaceUsage`
- [x] write a concurrency test asserting a quota race commits exactly one reservation
- [x] write tests for each limit's exceeded path
- [x] run tests — must pass before Task 17

### Task 17: Strip audit threading from store signatures

**Files:**
- Modify: `internal/ports/ports.go`, `internal/store/*.go`

- [x] re-count `contracts.AuditEvent` occurrences first — roughly half of the 22 in `ports.go` sat on
      methods already deleted in Task 15
- [x] remove the parameter from the surviving store signatures; write audit rows from the service layer
- [x] write a test asserting lifecycle mutations still produce an audit row
- [x] run tests — must pass before Task 18

---

### Phase 3 — Simplify the runner trust story

### Task 18: Replace runner enrollment with a pre-shared credential

**Files:**
- Modify: `internal/runnercontrol/identity.go`, `internal/config/config.go`
- Delete: `cmd/secondbox-runner-identity/`
- Modify: `migrations/postgres/0001_secondbox.sql`, `deploy/compose.yml`,
  `deploy/bin/bootstrap-runner-trust.sh`, `docs/operations/deployment.md`

- [x] replace the enrollment-token, rotation, and revocation lifecycle with a pre-shared credential
- [x] drop `runner_enrollment_tokens` and `runner_credentials`; remove
      `SECONDBOX_RUNNER_ENROLLMENT_HASH_SECRET` (`config.go:99`)
- [x] keep mTLS on the connection and keep runner identity derived from the verified credential
- [x] update `deploy/bin/bootstrap-runner-trust.sh` and the moved `tests/deployment/` coverage
- [x] write tests asserting an unknown or mismatched runner credential is rejected at connect
- [x] run tests — must pass before Task 19

---

### Phase 4 — Built-in profiles

### Task 19: Ship built-in lifecycle profiles

**Files:**
- Create: `internal/service/builtin_profiles.go`
- Modify: `internal/service/control_plane_service.go`,
  `docs/design/profiles-and-authorization.md`

- [x] add built-ins modeled on the predecessor's `agent-compartment` (ephemeral Flue turns) and
      `coding-environment` (long-running coding agents with PTY)
- [x] built-ins carry inline resource limits; `subject_quotas` remains the only persisted limit set
      (resolves the `profile_quotas` gap left by Task 15)
- [x] keep operator-defined profiles and immutable ProfileRevision pinning
- [x] write tests asserting each built-in resolves, pins, and cannot be mutated in place
- [x] write a test asserting a Sandbox on a built-in survives a later built-in change
- [x] run tests — must pass before Task 20

---

### Phase 5 — Thin SDKs

### Task 20: Delete generated clients, keep the hand-written layer

**Files:**
- Delete: `cmd/generate-clients/`, `sdk/go/secondboxclient/client.gen.go`,
  `sdk/typescript/secondbox-client.gen.ts`, `sdk/python/secondbox_client_gen.py`
- Modify: `cmd/secondbox/main.go`, `cmd/secondbox/sandbox_shell_command.go`,
  `cmd/secondbox/exec_stream_command.go`
- Modify: `scripts/verify-generated.sh`, `package.json`, `Justfile`

- [x] delete **only** the generated files — `sdk/typescript/client.ts`, `flue.ts`, and
      `flue-runtime-beta9-compat.ts` are hand-written, and `flue.ts` is the Flue `SandboxApi` adapter
      for a core use case
- [x] `cmd/secondbox` imports the Go SDK (`main.go:15,119,181-183`) — either migrate the CLI onto a
      thin hand-written client or keep `sdk/go/secondboxclient/{sdk,exec_stream,terminal}.go` as that
      layer. Decide before deleting
- [x] hand-write the Python client covering only what `apps/chat/chainlit` and `apps/chat/chat-app`
      call; `sdk/python/` currently contains nothing else, so this is a replacement
- [x] remove generator verification from `verify-generated.sh:19-28`, keeping protocol descriptor
      checks; fix the four `package.json` scripts
- [x] write client tests against a live control plane (extend `tests/sdk-live`)
- [x] run tests — must pass before Task 21

---

### Phase 6 — Do the core thing well

### Task 21: Stop buffering artifacts in control-plane memory

**Files:**
- Modify: `internal/service/artifacts.go`, `internal/api/http.go`

- [x] `internal/objectstore/s3.go` needs **no change** — `PutImmutable` already streams and spools
      (`:109-137`) and `GetVerified` already returns a verified `io.ReadCloser` (`:194-247`)
- [x] the constraint: `PutImmutable` requires `sizeBytes` and `expectedSHA256` up front, and
      `StageArtifact` needs both before publication (`artifacts.go:56,73`). Implement
      **spool → hash → stage → hand the file to `PutImmutable`**, not a bare `io.Reader`
- [x] replace `io.ReadAll` in `DownloadArtifact` (`:171`) with a streamed copy from `GetVerified`
- [x] rework `decodeArtifactUpload` (`internal/api/http.go:420`) to spool rather than buffer
- [x] write a test asserting a large artifact round-trips without buffering the whole body
- [x] write a test asserting integrity failure is still detected before any byte is published
- [x] run tests — must pass before Task 22

### Task 22: Add package-level tests to the store

**Files:**
- Create: `internal/store/postgres_lifecycle_test.go`, `internal/store/postgres_store_test.go`

- [x] test `finish_stop` generation advance, lease fencing, activity-session closure
- [x] test revision conflict and idempotency replay at the store boundary
- [x] test garbage-object listing and completion
- [x] run tests — must pass before Task 23

### Task 23: Shrink ControlPlaneStore into role interfaces

**Files:**
- Modify: `internal/ports/ports.go`, `internal/service/control_plane_service.go`,
  `internal/lifecycle/worker.go`

- [x] split the remaining ~51 methods into consumer-side role interfaces declared where consumed
- [x] name every parameter — `GetLease(ctx, tenantRef, subjectRef, sandboxID, leaseID string)`
- [x] keep `internal/ports` driver-pure
- [x] write compile-time assertions that the Postgres store satisfies each role interface
- [x] run tests — must pass before Task 24

### Task 24: Split the remaining oversized files

**Files:**
- Modify: `internal/runnercontrol/postgres_relay.go`, `internal/store/postgres_lifecycle.go`

- [x] split along data-plane / port-session / cancellation and lease / activity / checkpoint / artifact seams
- [x] no behavioral changes — pure file movement
- [x] run tests — must pass before Task 25

### Task 25: Verify acceptance criteria

- [x] verify every deletion target is gone and nothing references it
- [x] verify the [Do not cut](#do-not-cut) list is intact — fencing, reconciliation, idempotency
      scoping, lease-holder binding, capability tokens, checkpoint durability, revision pinning
- [x] verify `tests/deployment/` (moved in Task 3) still guards compose/env parity and backup fencing
- [x] run the full suite twice consecutively to confirm no flakes
- [x] run `go vet ./...` and the runner suite
- [x] measure and record actual LOC removed

Measured against the pre-change worktree with `git diff --numstat`, including all untracked
replacement files: 38,693 tracked lines deleted, 3,217 tracked lines added, 7,458 untracked lines
added, for a net reduction of **28,018 lines**.

### Task 26: [Final] Update documentation

- [x] update `README.md` for the single-token model, built-in profiles, simplified release flow
- [x] update `docs/design/{service-boundaries,domain-lifecycle,profiles-and-authorization,security,threat-model}.md`
      to describe the trusted-caller model honestly — remove claims about untrusted tenants and
      server-derived tenancy
- [x] delete design docs describing deleted machinery
- [x] move this plan to `docs/plans/completed/`

---

## Technical Details

### Ownership model

```
Authorization: Bearer <SECONDBOX_PLATFORM_TOKEN>
X-SecondBox-Tenant-Ref: <opaque bounded string>
X-SecondBox-Subject-Ref: <opaque bounded string>
```

Both refs are opaque to SecondBox; it never resolves them against an identity provider. Every
tenant-owned read and write carries `AND tenant_ref = $n AND subject_ref = $n+1`. The scoping
discipline stays exactly as strict as today's `project_id` scoping — only the source of the value
changes, from server-derived to caller-asserted. `service_account_id` becomes `subject_ref` and keeps
its ownership-check semantics.

**Accepted risk, recorded deliberately:** a SecondStack authorization bug can cross subjects, because
SecondBox trusts the asserted refs. This is the same risk `sandbox-service` carries in production
today, accepted in exchange for not adding a fourth identity-sync target.

### What still protects the user-facing path

PTY and port-tunnel connections are authorized by a single-use, session-bound, expiring HMAC
capability token carried in a WebSocket subprotocol — not by the tenancy model. That path is unchanged
and remains safe for direct browser connections.

### Sequencing rationale (corrected)

Phase 1 precedes Phase 2 because the frozen **contract** hashes and the schema-contract assertions
reject Phase 2 edits — not because of the migration catalog fingerprint, which never fires in the test
harness. Tasks 3–5 clear those assertions; Task 5 deliberately leaves `migrations.go` alone.

### Explicitly out of scope

- **SQLite.** SecondStack already runs PostgreSQL. A second backend doubles the invariant surface.
- **Additional compute backends** (smolmachine, Virtualization.framework) and native macOS support.
  Features, not simplifications; they collide with Phases 2 and 4. The backend seam
  (`AssignmentBackend`, `CheckpointBackend`, `RestoreBackend`, `PortBackend`) is unaffected by this
  work, so sequencing them afterward costs nothing.

### Scale estimate

Deletion totals are larger than first estimated and should be measured, not asserted:
`scripts/*release*|*publish*|*qualification*` ≈ 8,709 lines; `release/` ≈ 999; `tests/release` +
deleted `tests/operations` ≈ 3,800 after preserving `deployment_policy_test.go`; generated SDKs ≈
8,768; tenancy ≈ 2,000. Task 25 records the actual figure.

## Post-Completion

*Items requiring manual intervention or external systems — informational only.*

**External system updates**

- SecondStack callers switch from API keys to the platform token plus tenant/subject headers:
  `apps/chat/chainlit/backend/chainlit/sandbox_service.py`,
  `apps/chat/chat-app/src/tools/owned/chat_workspace_tools.py`,
  `apps/controltower/packages/server/src/lib/config.ts`.
- Provision `SECONDBOX_PLATFORM_TOKEN` in SecondStack's compose/env rendering.
- Provision the pre-shared runner credential at deploy time.
- Confirm SecondBox is **not** added to `apps/sync-jobs` — avoiding that was a primary goal.

**Manual verification**

- Exercise both real use cases end to end: an ephemeral Flue agent turn, and a long-running coding
  sandbox with PTY attach, detach, and reattach across an instance replacement.
- Confirm checkpoint durability across a deliberate runner kill.
- Firecracker and multi-runner qualification still require dedicated Linux hosts.
