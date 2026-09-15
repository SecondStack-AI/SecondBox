# Configurable limits

This contract targets clean initialization and newly created resources.
There is no old-state adoption, Profile mutation, workspace resize, or live hardware update.
See the [deployment transition boundary](../operations/deployment.md#clean-initialization-boundary)
before changing a deployment that already owns Workspaces.

## Representation and ownership

A policy ceiling is a finite integer or explicit JSON `null` (unlimited at that scope).
Zero remains finite, including zero Snapshots and zero quota. Unknown observations have an
unavailable status, never a null limit or zero estimate. Complete policy and quota objects
require every dimension. An omitted optional creation policy inherits the pinned Profile;
a partial configuration edit leaves omitted settings unchanged.

SecondBox stores and enforces effective policy. A consuming service may keep its
own desired configuration, but must apply it through the public management API
using a server-held tenant-controller credential for the affected Tenant.
Application authorities can read their own effective policy; they cannot change
Subject quota or lifecycle selection. UI roles in a consuming product do not
grant SecondBox authority.

Profile defaults and allowed lifecycle ceilings are immutable. Subject lifecycle selections
apply only at future Sandbox creation; the effective lifecycle is pinned with that Sandbox.
The optional `lifecycleCeiling` object sets both delegated dimensions. If omitted, the
Profile's `lifecycle.idleSeconds` and `lifecycle.maximumDurationSeconds` are also the
ceilings. When supplied, both fields are required; explicit `null` removes that Profile
ceiling. For example, these fields of a Profile spec keep idle shutdown as the default
while allowing a Subject to select another finite or unlimited idle/runtime policy:

```json
{
  "lifecycle": {
    "initialState": "running",
    "drainGraceSeconds": 10,
    "idleSeconds": 60,
    "maximumDurationSeconds": null,
    "leaseSeconds": 60
  },
  "lifecycleCeiling": {
    "idleSeconds": null,
    "maximumDurationSeconds": null
  }
}
```

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

Idle timeout stops compute and preserves the Workspace. Sandbox deletion and
Subject cleanup remove retained Workspace state through acknowledged Runner
operations. Consumers that need to keep compute active between guest operations
must use the explicit Lease and useful-activity contract. Inventory and policy
reads do not touch activity, create Leases, start compute, or apply configuration.

Lifecycle selections affect future Sandboxes; quota changes affect new admission
immediately. Consumers should report the effective API response separately from
unsaved or unapplied desired configuration.

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
