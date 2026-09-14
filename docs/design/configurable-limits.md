# Configurable limits

This pre-release contract targets clean initialization and newly created resources.
There is no old-state adoption, Profile mutation, workspace resize, or live hardware update.
SecondStack's released pin remains v0.10.1; these APIs require a coordinated release after
SecondBox v0.12.0. Unpublished local verification must not change that release pin.

## Representation and ownership

A policy ceiling is a finite integer or explicit JSON `null` (unlimited at that scope).
Zero remains finite, including zero Snapshots and zero quota. Unknown observations have an
unavailable status, never a null limit or zero estimate. Complete policy and quota objects
require every dimension. An omitted optional creation policy inherits the pinned Profile;
a partial configuration edit leaves omitted settings unchanged.

SecondBox owns effective policy and enforces it. ControlTower owns installation desired
configuration in General Config and explicitly deploys it through public SecondBox APIs.
A CT administrator is necessary but not sufficient: a server-held tenant-controller
credential must authorize the installation's Tenant. No platform token enters CT, no
controller credential enters a browser or Agent, and the fleet token remains read-only.
External mode without an explicitly supplied controller is read-only.

Profile defaults and allowed lifecycle ceilings are immutable. Subject lifecycle selections
apply only at future Sandbox creation; the effective lifecycle is pinned with that Sandbox.
An unlimited selection remains finite under a finite Profile ceiling. Quotas apply to new
admission immediately, including existing Sandboxes' next starts or operations. Reductions
below committed usage fail; they never evict resources. A Subject's unlimited quota remains
bounded by its Tenant. Physical Runner capacity and storage admission always apply.

## Policy inventory

Defaults below describe fresh `agent-compartment`, not silent service defaults. All other
Profiles and every Tenant/Subject quota are explicitly operator-selected.

| Field | Unit | Scope / fresh Agent default | Unlimited | Enforcement | Write authority / effect |
| --- | --- | --- | --- | --- | --- |
| `lifecycle.idleSeconds` | seconds | Sandbox / 60 | null | lifecycle decision and durable schedule; useful activity holds idle window | Profile operator, delegated Subject selection / new Sandbox |
| `lifecycle.maximumDurationSeconds` | seconds since Instance readiness | Sandbox / null | null | lifecycle decision and schedule, independent of activity when finite | same / new Sandbox, each Instance |
| `lifecycle.drainGraceSeconds` | seconds | Profile / 10 | no: cancellation must finish under bounded drain | lifecycle drain barrier | operator / new Sandbox |
| `lifecycle.leaseSeconds` | seconds | Profile / 60 | no: renewable owner authority must expire | API, database and Runner fences | operator / new Sandbox; each lease remains finite |
| `execution.maximumDeadlineMilliseconds` | milliseconds | Profile / 900000 | null policy ceiling; each command still needs a finite deadline | data-plane admission, Runner and guest cancellation | operator / new Sandbox, next admitted command |
| `execution.maximumBufferedOutputBytes` | bytes | Profile / 1 MiB | no: control-plane and guest buffered response memory bound | API, relay, Runner, guest | operator / new Sandbox |
| `execution.maximumTransferBytes` | bytes | Profile / 256 MiB | unsupported: relay session byte accounting and Runner file transfer limit (1 GiB) are finite | API, relay, guest file stream | operator / new Sandbox |
| `execution.streamWindowBytes` | bytes in flight | Profile / 64 KiB | no: credit flow control requires a finite window | relay, Runner, guest | operator / new Sandbox |
| `execution.terminalDetachSeconds` | seconds | Profile / 0 (disabled) | no: detached terminal authority expires | terminal admission and sweeper | operator / new Sandbox |
| `resources.concurrentOperations` | reserved operation slots | Profile / 4 | unsupported: placement reserves this concrete finite capacity on a Runner | scheduler and data-plane admission | operator / new Sandbox |
| `resources.vcpuCount`, `memoryBytes`, `workspaceBytes` | CPUs / bytes | allocation / 1 CPU, 1 GiB, 2 GiB | no: concrete allocations | quota, scheduler, backend, WorkspaceStore | application within Profile ceiling / new Sandbox |
| `resourceCeiling.{vcpuCount,memoryBytes,workspaceBytes}` | CPUs / bytes | Profile / omitted means allocation defaults | existing explicit null per axis | creation validation and Runner admission | operator / new Sandbox; snapshot-resume fixes shape |
| `retention.snapshotLimit` | count | Profile per Sandbox / 0 (disabled) | null | Snapshot admission under quota locks | operator / new Sandbox |
| `retention.snapshotRetentionSeconds` | seconds | Profile per Snapshot / 3600 | null means no automatic expiry | Snapshot creation and expiry worker | operator / new Sandbox, future Snapshot |
| `ports[].maximumSessions` | count per named Sandbox port | Profile / no exposed ports | null | admission under Tenant/Subject locks | operator / new Sandbox |
| `ports[].maximumSessionSeconds` | seconds | Profile / no exposed ports | unsupported: sessions require absolute expiry and renewable bound lease | port admission and runtime authority | operator / new Sandbox |
| `attributedExecution.maximumConnections` | simultaneous connections | Profile generation / 2 | no: forwarder has explicit 1–4096 bound | attributed Runner forwarder | operator / new Sandbox |
| `quota.maxSandboxes` | durable Sandbox count | Tenant + Subject / explicit | null | create admission; stopped Workspaces retain charge | platform / controller; immediate admission |
| `quota.maxActiveInstances`, `maxVcpuCount`, `maxMemoryBytes` | count / CPUs / bytes | Tenant + Subject / explicit | null | accepted running intent and Instance admission | platform / controller; immediate admission |
| `quota.maxSnapshots`, `maxPortSessions`, `maxConcurrentOperations` | count | Tenant + Subject / explicit | null | Snapshot, unexpired port and operation admission | platform / controller; immediate admission |
| `aggregateQuota.maxActiveSubjects`, `maxApplicationAuthorities` | count | Tenant / explicit | null | management admission | platform / immediate admission |
| `expiryPolicy.maximumSubjectLifetimeSeconds` | seconds | Tenant / explicit | Subjects already allow explicit non-expiring ownership; finite expiries bounded | management admission and cleanup | platform / future Subject |
| `expiryPolicy.maximumAuthorityLifetimeSeconds` | seconds | Tenant / explicit, at most one year | no: credentials require bounded expiry and renewal | authority admission and authentication | platform / future credential |

Transport HTTP deadlines, admission-proof redemption windows, protocol message sizes,
Runner process limits, and concrete storage limits are not Sandbox lifetime policy.
Unlimited runtime does not disable cancellation, owner loss, revocation, quota refusal,
or the deadline of an individual command. No current backend promises a universal guest
PID or CPU-share policy beyond its supported concrete resource enforcement.

## Retention and interest

Agent Platform retains a Workspace until its conversation or compartment is explicitly
deleted. Idle timeout stops compute and preserves files. Its settlement watch holds useful
interest through model thinking between guest operations. Reads of inventory or policy
neither touch activity nor create Leases, start compute, or reconcile configuration.

CT shows lifecycle first, then Subject/Tenant quota restrictions and Profile execution/resource restrictions.
A save shows its scope before deployment: future Sandboxes for lifecycle; immediate new
admission for quotas. Desired values and SecondBox effective values remain distinct when
application fails or the service has not received a compatible release.

## Public operations and validation

`GET /v1/subject-policy?profile=NAME` requires an application authority with `sandbox:read`
and that Profile grant. It returns only its own Subject policy and applicable Tenant limits.
Controller `GET` and `PUT /v1/subjects/{subjectRef}/sandbox-policy` select lifecycle limits
for that Subject and Profile. PUT requires the complete `{profile,lifecycle}` object,
`If-Match`, and `Idempotency-Key`; replay returns the original result, stale revisions fail.
A Subject has one selected Profile policy; creation with another granted Profile inherits
that Profile's defaults. Reads do not apply desired configuration.

`GET /v1/subject-usage` returns finite or null `available` per dimension and
`constrainingScopes` (`subject`, `tenant`, `tenant_and_subject`, or `none`). It exposes no
peer identities. Expired port sessions do not consume observed admission headroom.
Policy integers must fit exact JSON integers (at most 2^53−1); durations additionally must
fit the implementation's duration clock. These representation bounds are not unlimited.

The latest standard `agent-compartment` and `agent-compartment-isolated` revisions use
60-second idle shutdown, null maximum runtime, and null delegated lifecycle ceilings.
Published historical revision identities remain immutable. No existing Sandbox changes
its pinned policy when those standard bundles are published.

A newly requested finite lifecycle selection above its current Profile ceiling is rejected
with `profile_policy_ceiling_exceeded`. If an operator subsequently publishes a tighter
Profile ceiling, future effective policy is the minimum of the stored selection and that
ceiling. Existing Sandboxes still retain their creation policy.

## Verification evidence (2026-09-14)

`just verify-generated`, `just test`, and `just test-contract` passed against the changed
contract and a disposable PostgreSQL database. Public HTTP integration tests cover clean
initialization, explicit null and missing fields, effective inheritance, quota admission,
zero Snapshots, delegated ceilings, revision conflicts, and idempotent replay.
Controlled lifecycle clocks cover finite runtime termination despite activity, unlimited
runtime after 24 hours, and idle shutdown after interest ends.

Qualified Firecracker `just test-scenario` ran these four selected scenarios without skips:

- `TestScenarioDirectExecDeadlineDeliversTerminalAndReleasesQuota`
- `TestScenarioOrdinaryLifecycleAndCapacityRelease`
- `TestScenarioTouchExtendsIdleExpiry` (unlimited runtime, idle stop, restart and retained file)
- `TestScenarioWorkspaceStorageObservationPreservesStoppedFilesAndActivity`

SecondStack passed its Agent Platform typecheck, 36 focused provider/activity/fleet tests,
35 PostgreSQL integration tests, and the live Flue kernel orchestration test.
The controlled keepalive test holds active interest for 30 simulated minutes without guest
operations. This is separate from the real Runner scenarios: a combined live Flue/Runner
turn beyond the former 900-second deadline was not run.

ControlTower build, focused lint, formatting, six-locale validation, and the configuration
deployment unit test passed. Real browser and administrator API checks used
`http://localhost:18080` through the regular proxy, a source-built SecondBox API, real
PostgreSQL, and test-owned management authority. Checks covered finite/unlimited saves,
effective Profile clamping, rejected writes, stale revisions, zero Snapshot quota,
unauthorized writes, pending state, loading/error states, and desktop/mobile layouts.
The scoped Compose render validated; broad `just init-config` was not run because it
imports and deploys unrelated configuration and can publish artifacts outside this task.

Test delegation and test-owned desired configuration were removed, and CT's original
connection settings restored. Retained user Sandboxes and files were not modified.
The full scenario matrix, gVisor qualification, and coordinated released-consumer rollout
remain unverified. No release pin or published artifact was changed.
