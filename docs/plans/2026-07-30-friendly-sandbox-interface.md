---
title: Friendly Sandbox Interface
date: 2026-07-30
status: complete-except-scenario-gate
owner: SecondStack
provenance: SecondBox client-ergonomics gap analysis against microsandbox, 2026-07-30
---

# Plan: Friendly Sandbox Interface

## Outcome

Make a one-off command and an interactive PTY each one command away, without changing what SecondBox is.

Every primitive already exists and is tested. The PTY path is complete end to end — `runner/internal/guest/protocol_pty.go` through `internal/runnercontrol/postgres_relay_terminal.go`, `internal/api/terminal_http.go`, the SDK `Terminal` type, and `cmd/secondbox/sandbox_shell_command.go`, which does raw mode, `SIGWINCH` forwarding, credit-window replenishment, detach and reconnect, and terminal-state restore on every exit path. Buffered exec, streaming exec with flow control, files, snapshots, artifacts, leases, and port sessions are all published operations with contract coverage.

Nothing composes them. Today a one-off command is four CLI invocations plus two hand-written JSON request files plus manual base64 decoding, and an interactive shell is four invocations. Four global flags precede every one of them, because `cmd/secondbox` has no environment or configuration fallback at all.

This plan adds the composition layer and the two contract changes it needs. It does not add a second execution path, a local runtime, or a daemonless mode.

## Fixed architecture

- SecondBox stays a networked control plane. Sandboxes run on separately deployed runners, PostgreSQL owns desired state, and clients reach the system only over the published HTTP API. "One command" means one command against a deployment that already exists, not an embedded local VM.
- Composition lives in `sdk/go/secondboxclient`, not in `cmd/secondbox`. Idempotency-key generation, generation refresh, lease acquisition and renewal, and the create-wait-exec sequence are SDK helpers the CLI consumes. TypeScript and Python parity is a follow-up, not part of this plan.
- The CLI resolves credentials with a fixed precedence: explicit flags, then `SECONDBOX_*` environment variables, then a configuration file written by `secondbox login`. This softens the AGENTS.md rule that every runtime setting is explicit — for the CLI only. `secondboxd` keeps requiring every variable explicitly with no application-supplied default, and `internal/config` is not touched by Task 1.
- The CLI's token variable is `SECONDBOX_TOKEN`, deliberately distinct from the `SECONDBOX_PLATFORM_TOKEN` that `internal/config` reads. Sharing one name would silently hand CLI credentials to any shell configured to run `secondboxd`.
- Sandbox names are metadata, not a new resource field. The CLI writes the reserved key `secondbox.dev/name`, and `listSandboxes` gains a server-side metadata filter so any client on any host resolves the same name. There is no local name cache.
- Migrations are append-only. `migrations/postgres/migrations.go` verifies each recorded checksum against the embedded file and fails on drift, so the name index is a new `0002_*.sql` and `0001_secondbox.sql` is never edited.
- List cursors stay scope-bound. `resolvePostgresListCursor` passes a `scope` string into `pagination.DecodeListCursor`, which rejects a mismatch. The metadata filter joins that scope string so a cursor issued for an unfiltered page cannot be replayed against a filtered one.
- Existing subcommand signatures keep taking resolved strings. Task 1 feeds them from one resolved session rather than rewriting `sandbox_shell_command.go`, `exec_stream_command.go`, `timing_command.go`, `log_command.go`, or `diagnostics_command.go`.
- Operators still create every RunnerPool and every explicit Profile. Task 6 makes the built-in profiles configurable with real digests; it does not auto-provision the pool they reference.

## Non-goals

- Do not add a daemonless or embeddable execution mode. `microsandbox` boots a microVM as a child process with no control plane; SecondBox deliberately does not, and matching that would mean a different product.
- Do not add image pulling, an OCI workflow, or a `--image` flag. Sandboxes are pinned to an immutable profile revision resolved at creation.
- Do not add a name field to `Sandbox`, `CreateSandboxRequest`, or any other public schema. Names are reserved metadata.
- Do not add PostgreSQL foreign keys or CHECK constraints. The name index in Task 4 is a partial unique index, which is neither.
- Do not build a local name-to-identifier cache file. Names resolve server-side or not at all.
- Do not change `internal/config` defaulting behavior for `secondboxd`, and do not give any required control-plane variable an application-supplied default.
- Do not port the new SDK helpers to TypeScript or Python in this plan.
- Do not replace the generic `operation <operationId>` escape hatch or any existing alias in `commandAliases`. New commands are additive.

## Dependencies

Tasks 1, 2, and 6 are independent and can land in any order. Task 3 depends on Task 2. Task 5 depends on Tasks 1 through 4.

Task 6 blocks no code, but until it lands there is no bootable built-in profile, so the end-to-end demonstration in Task 5 requires an operator-created explicit Profile and RunnerPool. Task 4 is the only task that changes a published contract and a persisted schema.

## Validation Commands

Run the focused commands listed in each task while implementing it. Before handoff, run all repository-wide gates below from the repository root.

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- `just test-deployment`
- `just preship`
- `git diff --check`
- `just test-scenario` on a qualified KVM host, required for Task 4 because it changes persistence, and for Task 5 because it changes the lifecycle sequence

### Task 1: Resolve CLI credentials from flags, environment, and configuration

`cmd/secondbox` contains no `os.Getenv` call. Every invocation repeats `--url`, `--token`, `--tenant-ref`, and `--subject-ref`, including the ones documented throughout `docs/operations/sdk-cli-and-flue.md`. This task removes that repetition and changes nothing else.

- [x] Add `cmd/secondbox/session.go` defining a resolved session carrying the four values plus the origin of each, for diagnosis.
- [x] Resolve with fixed precedence: explicit flag, then `SECONDBOX_URL` / `SECONDBOX_TOKEN` / `SECONDBOX_TENANT_REF` / `SECONDBOX_SUBJECT_REF`, then the configuration file. Resolution never fails for a missing value; each subcommand keeps its own required-value check and its existing error text, extended to name the environment and configuration alternatives.
- [x] Locate the configuration file at `SECONDBOX_CONFIG` when set and absolute, otherwise `os.UserConfigDir()/secondbox/config.json`, which honors `XDG_CONFIG_HOME`.
- [x] Read the file defensively, following `openRegularLog` in `cmd/secondbox/log_command.go`: an absent file yields no values and no error; a symbolic link or non-regular file is an error; any group or other permission bit is an error; unknown JSON fields are rejected with `DisallowUnknownFields`. Trailing content after the document is rejected too.
- [x] Add `secondbox login`, writing the four values to a `0700` directory as a `0600` file, created `O_EXCL` under a temporary name and renamed into place so a concurrent reader never observes a partial file.
- [x] Verify credentials during `login` by issuing `listSandboxes` with `limit=1`. `Client.Do` at `sdk/go/secondboxclient/transport.go:239` returns a typed `*APIError` for any non-2xx, so a bad token fails immediately with the server's problem detail instead of being written to disk.
- [x] Add `secondbox logout`, removing the configuration file and succeeding when it is already absent.
- [x] Add `secondbox whoami`, printing the resolved endpoint, tenant reference, subject reference, and the origin of each. Never print the token, in any form.
- [x] Route the three commands through `runOperationalCommand` in `cmd/secondbox/main.go` alongside `logs` and `diagnostics`, and add them to `commandSummary`.
- [x] Feed the resolved session into the existing `runOperationalCommand` and `resolveCommand` paths without changing any subcommand signature.
- [x] Cover in `cmd/secondbox/session_test.go`: precedence across all three sources, absent file, symbolic-link rejection, permissive-mode rejection, unknown-field rejection, atomic overwrite on repeated `login`, `logout` idempotence, and that `whoami` output contains no token substring.
- [x] Update the CLI section of `docs/operations/sdk-cli-and-flue.md` and the invocations in `docs/operations/deployment.md` and `docs/operations/observability-and-diagnostics.md`.
- [x] Run `go build ./...`, `go vet ./...`, `go test ./cmd/secondbox`, and `just test`.

## Follow-ups closed after the tasks

### Migration 0002 could refuse to start an existing deployment (fixed)

`secondbox.dev/name` is ordinary caller-writable Metadata: `validateSandboxMetadata` bounded only entry count, key length, and value length, so any client could already set that exact key through `createSandbox` or `updateSandboxMetadata`. A database holding two live Sandboxes with the same reserved name would therefore fail `CREATE UNIQUE INDEX`. Migrations run under advisory lock before listeners open, so the result was `secondboxd` refusing to start on upgrade with a raw unique violation.

`0002_sandbox_name_index.sql` now checks first and raises a message naming every conflicting `tenant/subject=name` pair with the action to take. Covered in `migrations/postgres/sandbox_name_guard_test.go`, which also proves distinct names, names shared across subjects and tenants, and deleted predecessors all migrate cleanly, and that the guard and the index agree on what is a duplicate.

### A reserved name could shadow an identifier (fixed)

Nothing stopped `--name sbx_anything`. Clients tell an identifier from a name by the `sbx_` prefix, so such a Sandbox was unresolvable by name.

`contracts.SandboxIDPrefix` is now the single definition of that prefix, consumed by both the CLI resolver and the service. `validateSandboxMetadata` rejects a reserved name that is blank, surrounded by whitespace, or begins with the prefix, so the rule holds for every writer rather than only the CLI. A test pins `NewOpaqueID("sbx")` against the declared prefix so the two cannot drift.

## Known defects found while implementing

### The generic operation path sent a typed-nil request body (fixed)

`parseOperationOptions` in `cmd/secondbox/main.go` declares `var body *os.File` and returns it as `CallOptions.Body`, which is an `io.Reader`. When no `--body` is given, a nil `*os.File` becomes a non-nil interface holding a nil pointer, so `http.NewRequestWithContext` treats the request as having a body and reads from it. `(*os.File)(nil).Read` returns `os.ErrInvalid`, and every affected invocation fails with `invalid argument` before reaching the network.

This makes every operation invoked without `--body` fail — `sandboxes list`, `sandboxes get`, `profiles list`, `runners list`, `files read`, and every other route reached through the alias table or `operation <operationId>`. It is reproducible against a binary built before this plan started, so it predates the plan and is not caused by Task 1.

No test caught it because `cmd/secondbox` covers only alias resolution and the hand-written commands, which build their own requests through SDK helpers. Nothing exercises `parseOperationOptions` through `Client.Request`.

The fix is to leave `CallOptions.Body` nil unless a body was requested, plus a regression test that drives the generic path against an `httptest` server and asserts the request carries no body.


### Task 2: Add lifecycle helpers to the Go SDK

`SandboxHandle` already tracks generation and offers `Refresh` and `Wait` at `sdk/go/secondboxclient/sdk.go:143` and `:157`. Callers still hand-supply every idempotency key and thread every generation and lease by hand.

- [x] Generate idempotency keys from `crypto/rand` when a caller supplies none, in `dataPlaneJSON` and `lifecycle`, and in the new `CreateSandbox` and `AcquireLease` helpers. A caller-supplied key is always preserved.
- [x] **Deviation, deliberate:** do not retry on a generation fence. Instead expose `ProblemCodeOf` so a fence surfaces as the typed `generation_fenced` code, and let `SandboxHandle` continue to carry the generation it observed. See "Rejected: automatic retry on a generation fence" below.
- [x] Add lease helpers that acquire, renew in the background, and release on close: `AcquireLease`, `RenewLease`, `ReleaseLease`, and `LeaseKeeper` via `KeepLease`.
- [x] Drive renewal from the expiry the service actually granted rather than the requested duration, so the pinned Profile's `leaseSeconds` bound is respected without a Profile lookup the application authority may not be granted.
- [x] Add `WaitFor`, which issues repeated bounded waits against the caller's context deadline, because a single `waitForSandbox` request is capped at 60 seconds.
- [x] Add `Run`: create, wait for `ready`, execute, and return decoded stdout, stderr, and exit status as one call. `Run` never deletes the Sandbox, preserving the existing guarantee that this handle deletes nothing implicitly.
- [x] Move the outcome mapping into `ExecOutcomeError` and `DecodeExecOutcome`, and make `sandboxShellOutcomeError` at `cmd/secondbox/sandbox_shell_command.go` delegate to it, so exactly one switch interprets the union.
- [x] Cover in `sdk/go/secondboxclient/lifecycle_test.go` against the existing httptest transport: key generation and preservation, every `ExecOutcome` variant, output decoding on failure, lease acquire, renew, release, background renewal, renewal failure, wait retry after expiry, create, and run.
- [x] Run `go test ./sdk/go/secondboxclient -race`, `just test`, `just test-contract`, and `just verify-generated`.

#### Rejected: automatic retry on a generation fence

The original plan said to refresh the generation and retry once when a data-plane call is fenced. That is wrong and was not implemented.

A generation fence means the Instance was replaced. The Workspace is durable across generations, but process state is not. Silently retrying an `exec` against generation N+1 would run the caller's command inside a different Instance than the one they targeted, which is precisely what the fence exists to prevent. Turning an explicit rejection into a silent success is the opposite of the fence's purpose.

The ergonomic problem the item was aiming at is real but different: callers should not have to compute the generation by hand. `SandboxHandle` already solves that by tracking generation from create, wait, and refresh, and `GenerationHeaders` applies it. A fence now surfaces as the typed `generation_fenced` code through `ProblemCodeOf`, so callers decide explicitly whether re-running is safe.

### Task 3: Make `secondbox exec` a real command

`"exec"` is a bare alias to `executeSandboxCommand` at `cmd/secondbox/main.go:48`, resolved through the generic `parseOperationOptions` path. Callers write a JSON request file and receive JSON with base64 stdout and stderr.

- [x] Add `cmd/secondbox/exec_command.go`, structured like the existing `cmd/secondbox/exec_stream_command.go`.
- [x] Accept `secondbox exec <sandbox> -- <argv...>`, building an `ArgvCommand`, with `--shell` selecting a `ShellCommand`. The Sandbox operand precedes every option, because Go's `flag` package stops parsing at the first non-flag argument; `splitLeadingOperand` takes it before the flag set runs and rejects an option in its place.
- [x] Resolve the Sandbox through `getSandbox` and apply its generation through `SandboxHandle`, so neither the generation nor an idempotency key is supplied by hand. `exec` claims no Lease it was not given.
- [x] Decode `stdoutBase64` and `stderrBase64` to the process's own stdout and stderr, keeping the two streams separate, and write them before reporting any failure.
- [x] Map `ExecOutcome` to a process exit status through `commandExitError`, which `main` unwraps. A guest that exits 23 makes the CLI exit 23 and print nothing of its own, because the guest already wrote its diagnosis. An outcome with no exit status is described on standard error and exits 1.
- [x] Keep the raw outcome available behind `--json`, which retains base64 output and still carries the exit status. `operation executeSandboxCommand` remains the untouched escape hatch; only the shadowed `exec` alias was removed from `commandAliases`.
- [x] Cover in `cmd/secondbox/exec_command_test.go`: argv and shell construction, guest operands that look like CLI options, automatic generation and key, stream separation, exit-status propagation, every non-exited outcome, truncated output on exhaustion, `--json` fidelity, malformed invocations, and routing.
- [x] Run `go test ./cmd/secondbox`, `just test`, and `just test-contract`, and verify exit-status propagation with the built binary, since `os.Exit` is unreachable from a unit test.

### Task 4: Resolve Sandboxes by name

`listSandboxes` accepts only `limit` and `cursor`. There is no server-side way to find a Sandbox by anything an operator chose, so a friendly name cannot resolve without paging the entire list.

- [x] Add a repeatable `metadata` query parameter, encoded `key=value`, to `/v1/sandboxes` in `contracts/openapi/v1/secondbox.openapi.json`. Only the first `=` separates, so a value may contain one.
- [x] Add `migrations/postgres/0002_sandbox_name_index.sql` with a partial unique index on `(tenant_ref, subject_ref, (metadata_json->>'secondbox.dev/name'))` where that expression is not null and the Sandbox is not deleted, plus a GIN index supporting the containment filter. `0001_secondbox.sql` is untouched, because the lineage is checksummed and editing it would fail every existing database.
- [x] Extend `ListSandboxes` in `internal/store/postgres_store.go` with a `metadata_json @>` predicate.
- [x] Join the filter into the `scope` string passed to `resolvePostgresListCursor` and `encodePostgresListNextCursor`, so a cursor cannot cross filter boundaries in either direction.
- [x] Thread the filter through all three signatures: the interface at `internal/service/control_plane_service.go`, the method, and the `listSandboxes` handler in `internal/api/http.go`.
- [x] Surface the reserved-name conflict as `ports.ErrSandboxNameConflict`, mapped to the existing `state_conflict` problem code rather than widening the `ProblemCode` enum, from both Sandbox creation and metadata replacement.
- [x] Add `contracts.SandboxNameMetadataKey` as the single definition of the reserved key.
- [x] Cover the filter, AND semantics, cursor-scope rejection in both directions, paging within one filter, name uniqueness, name release on deletion, tenant and subject scoping, and unnamed Sandboxes not colliding, in `internal/store/postgres_sandbox_names_test.go`; cover query parsing and its bounds in `internal/api/sandbox_metadata_filter_test.go`.
- [x] Run `just verify-generated`, `just test`, `just test-contract`. **`just test-scenario` has not been run: it requires a qualified KVM host that is not available here.**

### Task 5: Add the `run` and `shell` composites

- [x] Add `secondbox run <profile> [--name X] -- <argv...>`: create, wait for ready, execute, print, and delete unless `--keep` is given. Disposal runs even when the command fails, and skips a Sandbox already reported deleted.
- [x] Add `secondbox shell <name-or-id>`: resolve the reference, apply the observed generation, acquire and renew a Lease, and release it on exit.
- [x] Keep `--generation`, `--lease`, and `--idempotency-key` as explicit overrides. **Deviation from "call `runSandboxShellCommand` unchanged", in its favour:** the composite injects its resolved values *before* the caller's own arguments and delegates to the existing terminal command untouched. The flag package keeps the last occurrence, so every injected value is overridable by construction, and the tested PTY path is not refactored at all.
- [x] Write the reserved `secondbox.dev/name` metadata key on create when `--name` is given, and reject a `--metadata` pair that restates it.
- [x] Resolve a reference by the fixed `sbx_` identifier prefix or else by name, skipping deleted Sandboxes, which keep their metadata and remain listable. The page walk is bounded and says so explicitly when it gives up.
- [x] Cover the composites in `cmd/secondbox/run_command_test.go`, `cmd/secondbox/shell_command_test.go`, and `cmd/secondbox/sandbox_reference_test.go`.
- [x] Run `go test ./cmd/secondbox`, `just test`. **`just test-scenario` has not been run: it requires a qualified KVM host that is not available here.**

### Task 6: Make the built-in profiles bootable

`ControlPlaneService.BuiltInProfiles` was never populated by `internal/config` or `cmd/secondboxd/main.go`, so `resolveBuiltInProfiles` always fell through to a default carrying the placeholder digests `sha256:aaaa…` and `sha256:bbbb…` and a `default-pool` that nothing creates. Both built-in Profiles were present in the API and impossible to boot.

- [x] Add required environment variables to `internal/config/config.go` for each built-in Profile's pool, runtime bundle digest, and toolchain bundle digest. Digests must be `sha256:` and 64 lowercase hexadecimal characters. No value has a default.
- [x] Add `service.BuildBuiltInProfiles`, which applies a deployment binding to the fixed built-in policy, and populate `service.ControlPlaneConfig.BuiltInProfiles` from configuration in `cmd/secondboxd/main.go`.
- [x] **Remove the implicit default entirely.** `resolveBuiltInProfiles` now rejects an unstated binding rather than substituting placeholders, so the failure is a refusal to start instead of Sandboxes that can never be placed. Every test construction site states its own binding through one shared fixture helper.
- [x] Add the variables to `deploy/environment.example`, `deploy/bin/validate-environment.sh`, `deploy/compose.yml`, `scripts/compose-test.yml`, and `scripts/scenario-compose.yml`, using each deployment's real pool name.
- [x] Document the one-time operator step that creates the referenced RunnerPool in `docs/operations/deployment.md`. It is not auto-provisioned; AGENTS.md requires operators to create every pool and Profile explicitly.
- [x] Cover in `internal/config/builtin_profile_test.go` and `internal/service/builtin_profiles_test.go`.
- [x] Run `just test`, `just test-deployment`, `just test-contract`, `just verify-generated`.
