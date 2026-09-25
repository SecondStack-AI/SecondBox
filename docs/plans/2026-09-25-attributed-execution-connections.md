# Plan: Delegated attributed execution connection limits

Raise the latest standard `agent-compartment` default to 128 simultaneous open TCP connections per attributed execution and let a tenant controller select a finite Subject limit. Apply changed numeric policy when the next attributed Assignment is committed, including for existing Sandboxes pinned to an older two-connection Profile revision. Preserve their pinned gateway, attribution permission, network policy, assets, deadlines, resources, lifecycle, and all other authority. Existing Assignments and connections keep their admitted limit and are not terminated by policy edits.

Status: parent `thr_n2kxi2hcan` confirmed Fable 5.1 final agreement and authorized implementation. Product implementation is frozen for independent Opus 5.5 review in `thr_4xwgb9tuke`. Non-live mandatory gates passed; qualified scenario execution remains blocked on a verified retrievable signed image. Parent coordinates the subsequent independent Opus 5.5 review. Never merge or publish a release.

## Scope and evidence

- Worktree: `/mnt/bulk/bb/plugins/environment-git-worktree/host-data/worktrees/thr_wvrndp5rua-1/SecondBox`.
- Branch: `bb/issue-893-secondbox-execution-connection-policy-thr_wvrndp5rua`; configured upstream: `origin/main`.
- Base: fetched current `origin/main`, `6a605c6f2714ccb4cd33848657a32f9524258bad`, on 2026-09-25. Other worktrees remain untouched.
- `pkg/standardresources/standardresources.go` builds historical attributed revisions with `maximumConnections: 2`. Published history and canonical spec digests are append-only. The later lifecycle revision still carries that value.
- `internal/lifecycle/postgres_effects.go` currently copies both gateway and maximum connections from the Sandbox's pinned Profile into the Assignment command.
- `runner/internal/egressforwarder/execution_forwarder.go` counts live sockets with a bounded channel. At capacity, it closes a newly accepted socket before opening the gateway connection or writing bytes. A closed relay releases its slot; this is not request concurrency or HTTP parsing.
- `internal/store/postgres_sandbox_policy.go` already stores one optional selection per Subject in `sandbox_policy_json`; GET/PUT use the current Profile head, Tenant Profile grant, Subject revision, idempotency, and audit. Lifecycle selections affect future Sandbox creation only.
- `internal/scheduler/postgres.go` commits the cloned Assignment command in a serializable transaction, checks for an existing Assignment first, and returns that Assignment on replay. This is the appropriate durable boundary for numeric resolution; a read in the earlier lifecycle planning step alone could go stale.
- Reported by the parent, not rerun here: staging Sandbox `sbx_aXRGyB1qI0ywYhim5y73iZsB` pins `agent-compartment` revision 5 with limit 2; a source reproduction admits 2 sockets and returns 18 EOFs. The downstream real OpenAI path passed 31/31 at concurrency 1/2/3/5/20 without retries. These findings do not establish capacity at 128 or 10000.

No UI, shared runtime changes, HTTP parser, retry, backpressure protocol, alternate execution transport, capacity benchmark, or load campaign belongs in this patch. SecondStack owns its independently configurable proxy limit (planned default 10000) and CT settings.

## Proposed public contract

Use the existing endpoints and authority:

- Controller `GET /v1/subjects/{subjectRef}/sandbox-policy?profile=agent-compartment`.
- Controller `PUT /v1/subjects/{subjectRef}/sandbox-policy`, with existing `If-Match: "revision-8"` and `Idempotency-Key` headers.
- Application read-only `GET /v1/subject-policy?profile=agent-compartment`, with existing Subject ownership, `sandbox:read`, and Profile grant checks.

Extend the complete PUT body with optional `attributedExecution`, containing only the numeric selection:

```json
{
  "profile": "agent-compartment",
  "lifecycle": {"idleSeconds": 60, "maximumDurationSeconds": null},
  "attributedExecution": {"maximumConnections": 128}
}
```

`profile` and complete `lifecycle` remain required. `attributedExecution` may be omitted or explicitly `null` to select the current Profile default; an object requires `maximumConnections`. It accepts integers 1 through 4096 inclusive, with no unlimited representation. Reject null numeric values, missing numeric fields, zero, negatives, fractions, out-of-range values, unknown properties, and gateway or other authority fields. PUT replaces the complete Subject selection: omission resets the numeric selection to inheritance; downstream lifecycle-only edits must preserve a saved numeric selection if that is intended. This is the existing PUT replacement model, not a new PATCH contract.

Persist the optional object in the existing Subject JSON; historical lifecycle-only JSON remains valid. Do not add a second Subject revision, endpoint, idempotency scope, or policy table.

Preserve every existing observation field and its meaning. Add required nullable `attributedExecution` to GET and PUT observations:

```json
{
  "subjectRef": "subject-example",
  "revision": 8,
  "profile": "agent-compartment",
  "profileRevisionId": "current-profile-revision-id",
  "desired": {
    "profile": "agent-compartment",
    "lifecycle": {"idleSeconds": 60, "maximumDurationSeconds": null},
    "attributedExecution": {"maximumConnections": 256}
  },
  "attributedExecution": {
    "defaultMaximumConnections": 128,
    "maximumConnections": 256,
    "maximumConnectionsCeiling": 4096
  }
}
```

This is a projection excerpt; existing `effective`, `ceiling`, resources, execution, retention, quotas, and timestamps remain in the complete response. `effective` and `ceiling` remain lifecycle objects; do not reshape them. New numeric observation is `null` when the current Profile has no attributed permission. `desired` remains `null` when no matching Profile selection exists. All numeric observation fields are finite integers. `profileRevisionId` continues to identify the current Profile revision used for this prospective policy observation, not a Sandbox's pinned revision or an active generation. The projection exposes no gateway selector and makes no claim to report an already-running Assignment's limit.

Proposed Go/schema names: `AttributedExecutionConnectionLimits { maximumConnections }` for selection/ceiling; `AttributedExecutionConnectionObservation { defaultMaximumConnections, maximumConnections, maximumConnectionsCeiling }` for observation. Keep the existing `AttributedExecutionPolicy` (gateway and default) for operator Profiles.

### Operator ceiling and default

Add optional `ProfileRevisionSpec.attributedExecutionCeiling` alongside the existing block:

```json
{
  "attributedExecution": {
    "gateway": "agent-gateway.secondbox.internal",
    "maximumConnections": 128
  },
  "attributedExecutionCeiling": {"maximumConnections": 4096}
}
```

An absent ceiling means the same revision's `attributedExecution.maximumConnections` is both default and ceiling. A supplied ceiling requires attributed permission, an integer 1..4096, and a value at least the default; reject an explicit null ceiling and null numeric values. Existing historical specs need no mutation. A new explicit Subject selection above the current head's ceiling returns existing `profile_policy_ceiling_exceeded`; invalid shapes/ranges or selecting attributed policy on a non-attributed Profile return `invalid_request`. A stored selection above a subsequently tightened ceiling resolves to that ceiling for later Assignments; retain the desired value for readback. Controller writes never change the ceiling or gateway. A complete PUT may preserve a value-equal existing lifecycle or attributed block for the same Profile despite later tightening or removal of attributed permission. New/changed blocks and Profile switches still validate against current grants; both effective resolvers continue enforcement. Lifecycle remains required; omission/null clears only the optional attributed selection.

The new standard revision explicitly grants default 128 and ceiling 4096. This grants CT a finite range; it is not evidence that 4096 connections have been capacity-qualified. Custom Profiles get no hardcoded 128 floor or default: their operator-supplied numeric policy remains authoritative.

### Resolution for old pins and subsequent generations

1. The pinned revision must already permit attributed execution. A newer head cannot grant an old non-attributed Sandbox that capability.
2. Identify the current head of the same logical Profile through the pinned revision's Profile identity. Use only its attributed numeric default and numeric ceiling. Do not replace the pinned spec or copy its gateway, network, assets, deadlines, or other fields.
3. Select the numeric grant: if the current head has attributed permission, use its default and ceiling. Otherwise, use the pinned default as both default and ceiling. When the current head supplies the grant, the old pinned default of 2 is not an additional ceiling.
4. Apply one resolver to either grant: use the grant's default when there is no matching Subject numeric selection; otherwise use the minimum of the selection and the grant's ceiling. A current head without attributed permission cannot accept new numeric selections, but a previously stored selection still participates in resolution for an existing attributed pin. This preserves pinned permission while honoring a lower Subject selection. Profile disablement continues to stop new Sandbox creation without rewriting existing grants; a disabled head with attributed policy can still supply the numeric bound for existing Sandboxes.
5. Resolve at new durable Assignment creation inside the scheduler transaction, after the existing-Assignment check. Set only the cloned command's `MaximumConnections`; gateway remains pinned. Store the result in the existing serialized command. No runner protobuf extension or new database column is required.
6. A completed policy PUT or Profile publication before Assignment scheduling is visible to that scheduling transaction. Concurrent policy writes and scheduling may serialize in either order; each Assignment gets one coherent default/ceiling/selection. Policy reads participate in the serializable transaction's dependency tracking; serialization failures use the scheduler's existing bounded retry path. Avoid fresh policy reads when replaying an already-persisted Assignment. Reconciliation, delivery retries, reconnect, and crash recovery use the original serialized command.
7. No active listener resizing or socket cancellation. A change affects the next Assignment; a start already accepted but not yet assigned can adopt it. Ordinary non-attributed starts remain unchanged.

Examples:

| Pinned policy | Current Profile numeric default / ceiling | Subject selection | Next attributed Assignment |
| --- | --- | --- | --- |
| gateway A, default 2 (including development revision 2) | 128 / 4096 | omitted | gateway A, 128 |
| gateway A, default 2 | 128 / 4096, head gateway B | 256 | gateway A, 256 |
| gateway A, default 2 | 128 / 256 | previously selected 512 | gateway A, 256 |
| gateway A, default 2 | legacy 2 / absent (therefore 2) | new 128 | PUT refused until operator applies revised grant |
| no attributed permission | 128 / 4096 | 128 | attributed start still refused |
| gateway A, default 2 | no attributed permission | previously selected 128 | gateway A, pinned 2 |
| gateway A, default 2 | no attributed permission | previously selected 1 | gateway A, 1 |
| gateway A, default 2 | no attributed permission | omitted | gateway A, pinned 2 |

Code deployment alone cannot authorize raising an operator-owned ceiling. The standard resource update must append and apply the reviewed new revision. Once that head is installed, old attributed pins adopt 128 without recreating Sandboxes or unpinning authority. This intentionally narrows the existing blanket documentation that all Profile policy is permanently pinned: only this numeric generation bound follows the current head.

## Validation Commands

Run after implementation, with a disposable PostgreSQL database explicitly configured as `SECONDBOX_TEST_DATABASE_URL`; record prerequisite failures and skips rather than calling them successful checks.

- `go test ./pkg/contracts ./internal/service ./pkg/standardresources ./internal/store ./internal/lifecycle ./internal/scheduler -count=1`
- `go test ./tests/integration -run 'SandboxPolicy|Attributed|StandardResources' -count=1`
- `cd runner && go test ./internal/egressforwarder ./internal/runtime ./internal/runnercontrol -count=1`
- `cd runner && go test -race ./internal/egressforwarder -count=1`
- `go run ./cmd/secondbox-sdkgen -output-root .`
- `just verify-generated` (install pinned npm dependencies with `npm ci --ignore-scripts` if absent).
- `just test-contract`
- `just test-standard-resources`
- `just test`
- `just test-scenario` on a qualified host: required because Assignment/lifecycle policy changes. Use the repository's existing explicit host configuration and isolated scenario deployment; coordinate host/resource use with parent. The scenario is functional qualification, not a capacity benchmark.
- `git diff --check`

Run additional relevant CI gates if required by the touched paths. No benchmark or stress qualification is claimed. A plan-only handoff does not require executing implementation gates.

### Task 1: Define and validate the delegated numeric contract

Relevant files: `pkg/contracts/contracts.go`, `pkg/contracts/sandbox_policy.go`, `internal/service/control_plane_service.go`, `internal/service/sandbox_policy.go`, contract tests and `contracts/openapi/v1/secondbox.openapi.json`.

- [x] Add the finite numeric selection/ceiling and observation types and optional Profile ceiling, with strict decoding and validation matching the JSON contract above.
- [x] Add pure resolution helpers for current numeric default/ceiling, desired selection validation, and clamping; keep gateway-bearing operator types separate from controller selection types.
- [x] Extend Subject request validation and observation schema without changing existing lifecycle meanings, required lifecycle fields, ownership, or complete PUT behavior.
- [x] Test omitted/null selection, explicit inheritance, malformed/unknown fields, default and ceiling boundaries (1, 128, 4096), default above ceiling, absent permission, and stored desired values above a tightened ceiling.

### Task 2: Persist and project Subject selection through existing management APIs

Depends on Task 1. Relevant files: `internal/store/postgres_sandbox_policy.go`, `internal/api/sandbox_policy_http.go`, `tests/integration/sandbox_policy_http_test.go` and persisted authority fixtures.

- [x] Store the optional numeric selection in existing `sandbox_policy_json`, retaining compatibility with existing lifecycle-only rows and one selected Profile per Subject.
- [x] Validate under the existing Subject and Profile-head locks; preserve Tenant Profile-grant enforcement, audit, shared Subject revision, and idempotent replay of the original response.
- [x] Project current numeric default, effective value and ceiling through both controller and application reads, including null when unavailable. Use the same pure resolver as Assignment admission.
- [ ] Exercise the actual HTTP and PostgreSQL path through SDK clients: authorized update/readback, stale revision, exact replay, changed payload with reused key, lifecycle-only replacement/reset, cross-Tenant denial, missing Profile grant, and application write refusal.
- [x] Verify historical lifecycle-only selections decode and that creating Sandboxes still pins lifecycle exactly as before.

### Task 3: Resolve only the numeric limit at durable Assignment admission

Depends on Tasks 1–2. Relevant files: `internal/scheduler/postgres.go`, `internal/lifecycle/postgres_effects.go`, scheduler/store/lifecycle PostgreSQL regression tests.

- [x] Read the pinned permission, current head's numeric grant and matching Subject selection in the scheduling transaction after the existing-Assignment fast path. Use a small shared database helper only where it removes actual duplication; do not introduce a policy service.
- [x] Clone the command and change only its numeric `MaximumConnections`. Validate its pinned gateway/permission against the pinned revision before persistence; do not let a caller-supplied command select current-head authority.
- [x] Preserve the serializable transaction and existing lock order. Do not add Subject/Profile row locks after Sandbox/Workspace locks that invert management/lifecycle admission ordering; coherent read-only policy lookups participate in serializable dependency tracking, with serialization failures handled by the existing bounded scheduler retry path.
- [x] Remove any implication in lifecycle planning that its pinned numeric placeholder is the final admitted limit; keep all other pinned construction unchanged.
- [ ] Prove with PostgreSQL fixtures that an old attributed pin of 2 adopts 128 from the new head; a head gateway/network/assets/deadline change does not cross the pin; a non-attributed old pin cannot gain permission.
- [ ] Verify selection changes and ceiling tightening affect the next Assignment, ordinary starts stay unchanged, head-without-permission and disabled-head semantics match the contract, and different selected Profiles do not leak policy. For a head without attributed permission, cover absent selection, selection below the pinned default, and selection above it through the same resolver.
- [ ] Exercise update/scheduling concurrency and repeated scheduling/command delivery: committed commands remain byte-stable for the same Assignment after later policy edits. Recovery never recomputes a persisted command's numeric value.

### Task 4: Append the standard 128-connection operator grant

Depends on Task 1 and regenerated Go types from Task 5. Relevant files: `pkg/standardresources/standardresources.go`, its tests, `tests/integration/standard_resources*`, and affected release-bundle identity fixtures.

- [x] Append a new latest `agent-compartment` revision with default 128 and explicit ceiling 4096 to both published and development lineages. Deep-copy the attributed pointer before edits so historical specs remain unchanged.
- [x] Preserve all existing historical spec digests, including the old attributed/lifecycle revisions; do not edit `attributedAgentSpec` in a way that rewrites that prefix. Keep isolated and durable-coding Profiles unchanged.
- [ ] Test fresh materialization, installed-prefix upgrade, replay, exact revision identities, and unchanged historical specs. Derive actual revision numbers from each lineage rather than assume staging and development use the same number.
- [ ] Verify the new head activates 128 for an existing two-connection attributed Sandbox on its next Assignment without altering its ProfileRevision ID or gateway.

### Task 5: Generate SDKs and document the adoption boundary

Depends on the agreed Task 1 schema; generation may precede Task 4 compilation. Relevant files: `cmd/secondbox-sdkgen/main.go`, generated Go wire/transport files, TypeScript generated transport/public surface and exports, SDK tests, design and operations documentation.

- [x] Register the new schemas with SDK generation and regenerate all tracked artifacts using the existing generator; export types from handwritten TypeScript entrypoints where needed. Existing client method signatures and routes stay stable.
- [x] Add Go/TypeScript request and response coverage for optional/null inheritance, numeric updates and new observation fields; ensure generated SDK drift and OpenAPI HTTP checks pass.
- [x] Update `docs/design/configurable-limits.md`, `profiles-and-authorization.md`, `networking-and-ports.md`, relevant runner protocol text, and standard-resource/downstream integration docs to describe the numeric exception, finite ceiling, exact JSON, complete PUT reset semantics, and next-Assignment activation.
- [x] Document the continuing above-limit zero-byte close as the protocol-agnostic safety bound. Do not suggest HTTP 429/503, retries, or live resizing.
- [x] Document rollout: updated control plane and generated client, operator application of the new standard revision, then next attributed generation. Existing protobuf consumers already carry a numeric bound; report tested binary combinations rather than invent a minimum released version.

### Task 6: Verify the real forwarding path and finish the authorized PR workflow

Depends on Tasks 2–5 and parent implementation go-ahead.

- [x] Extend the existing real TCP/Unix forwarder regression with a bounded functional case above 2 simultaneous open streams, plus exact configured ceiling refusal and slot reuse. Keep handshakes deterministic; test bytes/identity/half-close/cancellation, not throughput.
- [x] Add cheap bounded refusal diagnostics: at most one structured warning per generation on first capacity refusal, naming configured maximum and safe generation identifiers, without gateway paths, credentials, authorization references, or per-socket log floods. Verify capacity refusal does not revoke the generation.
- [ ] Extend the public scenario to apply delegated Subject policy and run a subsequent generation of an existing old-pinned Sandbox. Observe the real configured gateway path and unchanged attribution; retain explicit small-limit safety tests. Run mandatory focused and repository gates above.
- [x] Coordinate the requested independent Opus 5.5 implementation review with parent; do not launch duplicate plan or implementation reviewers. Assess findings, remediate confirmed defects, and rerun affected checks.
- [ ] After review, commit only issue-893 changes, verify the push destination and upstream, push this feature branch, and open a focused non-draft SecondBox PR targeting `main`. Handle CI and Dark Review findings without merging.
- [ ] Send parent exact final API examples, PR/commit and validation evidence, and the outstanding merge/release/consumer-pin dependency. No release version, artifact digest or availability may be fabricated. No merge, tag push, release upload, or deployment to shared staging is authorized by this plan.

## External dependencies and agreement points

Parent owns Fable 5.1 plan agreement across repositories. The recommendation resolves the design questions locally: current-head numeric default/ceiling only, separate optional Profile ceiling, complete Subject PUT, durable Assignment activation, and legacy permission retained. These exact semantics must be included in that agreement before implementation; no additional user permission is requested for routine design choices.

SecondStack CT must distinguish current prospective effective policy from active generations, send a complete lifecycle/connection selection on edits, and surface finite ceiling refusal. Its proxy default 10000 is independent of this default 128 and maximum 4096. Parent owns deploying/applying any upstream release or pinned build; this thread cannot make production adoption happen by opening a PR. Operator application of the new standard revision is required even after new code is deployed.

## Outcome

Implementation includes the public contract, durable numeric resolution, appended standard revision, generated SDKs, bounded refusal logging, regression coverage and documentation. Focused PostgreSQL/HTTP tests, real socket forwarding under the race detector, generated artifact verification, and the contract gate pass. `just test` (including root/runner tests and vet), `just verify-generated`, `just test-contract`, and `just test-standard-resources` passed, including reruns after the review follow-up. The public scenario compiles but live qualification is blocked: escalated execution reaches the missing signed-image prerequisite, and the supplied local registry credentials return HTTP 401. No live scenario pass or image digest is claimed. No commit, push, PR, merge, or release yet. Opus 5.5 approved the initial diff. The authorized follow-up preserves unchanged desired blocks in both directions, uses field-local ceiling decoding without restricting ordinary Profile reads, and gives the scenario its own Profile. Cross-direction HTTP regressions and strict API ingress tests pass; all captured published/development lineage digests match before and after the decoder change. Opus 5.5 approved the follow-up delta; parent authorized publication and CI/Dark Review remediation.
