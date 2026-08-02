---
title: Configuration Surface Split
date: 2026-08-01
status: proposed
owner: SecondStack
provenance: Operator configuration-surface review, 2026-08-01
---

# Plan: Configuration Surface Split

## Outcome

Separate deployment authority from tuning constants in the control-plane
configuration contract, so that the no-defaults rule protects what it exists to
protect and stops charging operators for facts the binary already owns.

`secondboxd` requires 58 environment variables today. `internal/config/config.go`
makes 21 direct `required*` calls, and two helpers expand prefixes into more:
`requiredQuota` at `config.go:473` reads 9 values from one prefix, and
`requiredBuiltInProfileBinding` at `config.go:342` reads 3 per built-in Profile
for 2 Profiles. `deploy/environment.example` presents 145 variables across the
control plane, runner, and Compose deployment.

The governing rule is that every runtime setting is explicit and neither
application code nor deployment templates may supply defaults. That rule is
correct for deployment authority and wrong for tuning constants, and it is
currently applied uniformly to both.

Two symptoms show the conflation is not cosmetic:

- `config.go:186` rejects a session byte bound smaller than the frame byte bound,
  and `config.go:277` rejects a protocol minimum above the maximum. The code
  already owns these relationships; it declines to own the values.
- `docs/plans/2026-07-31-direct-port-data-plane.md` records
  `SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS` "at the deployed 250 ms"
  costing roughly 250 ms mean and 500 ms worst case per round trip, making SSH
  connection setup cost seconds. A tuning constant promoted to required operator
  configuration became a latency defect that an architecture change was needed to
  correct.

Success for this pass is:

- required control-plane variables reduced from 58 to 32, with no variable
  carrying deployment identity, authority, or a pinned asset digest losing its
  explicit requirement;
- no code path acquiring a default for a secret, endpoint, credential, path,
  digest, or quota;
- every removed variable either eliminated because the binary owns the fact, or
  retained as an optional override that the stress, scenario, and Compose suites
  continue to set;
- `config.go` cross-field validation preserved for every value that survives as
  an override.

## Fixed design

Each currently required variable falls into exactly one of four categories, and
the category determines its treatment. The categories are decided here rather
than left to the implementation, because the whole point is that the boundary
between "operator decides" and "code decides" stops being ad hoc.

### Category A — deployment authority. Stays required, no default. (32)

Identity, secrets, endpoints, pinned assets, and subject policy. A control plane
that boots with a default value for any of these is a defect, and in several
cases a security defect.

`SECONDBOX_LISTEN_ADDR`, `SECONDBOX_PUBLIC_BASE_URL`,
`SECONDBOX_RUNNER_LISTEN_ADDR`, `SECONDBOX_DATABASE_URL`, `SECONDBOX_LOG_PATH`,
`SECONDBOX_PLATFORM_TOKEN`, `SECONDBOX_RUNNER_CREDENTIAL`,
`SECONDBOX_APPLICATION_AUTHORITIES_JSON`,
`SECONDBOX_RUNNER_SERVER_CERTIFICATE`, `SECONDBOX_RUNNER_SERVER_PRIVATE_KEY`,
`SECONDBOX_RUNNER_CA_CERTIFICATE`, `SECONDBOX_SIGNED_ASSET_CATALOG_PATH`,
`SECONDBOX_OBJECT_STORE_ENDPOINT`, `SECONDBOX_OBJECT_STORE_REGION`,
`SECONDBOX_OBJECT_STORE_BUCKET`, `SECONDBOX_OBJECT_STORE_ROOT_USER`,
`SECONDBOX_OBJECT_STORE_ROOT_PASSWORD`, the 6 `SECONDBOX_BUILTIN_*` pool and
bundle digest values, and the 9 `SECONDBOX_DEFAULT_SUBJECT_*` quota values.

Quota is included deliberately. It reads like tuning and is not: it is the
multi-tenant policy boundary, and a default would silently admit load the
operator never sanctioned.

### Category B — code-owned facts. Removed entirely. (3)

Not defaulted. Removed, because accepting a value at all admits a value the
binary cannot honour.

- `SECONDBOX_RUNNER_PROTOCOL_MINIMUM` and `SECONDBOX_RUNNER_PROTOCOL_MAXIMUM`.
  The supported range is a property of the compiled runner-protocol
  implementation. An operator can currently declare a range the code does not
  implement, and `config.go:277` only checks the two values against each other,
  not against what exists. The version range moves to a constant beside the
  protocol definition, and the compatibility suite pins it.
- `SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES`. A wire-format bound, paired to the
  relay framing and validated at `config.go:186` against the session bound.

### Category C — tuning constants. Code constant with optional override. (20)

The constant lives beside the code that consumes it. The environment variable
remains readable and, when set, still passes the existing validation. Unset, it
is no longer an error.

`SECONDBOX_HTTP_TIMEOUT_SECONDS`,
`SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS`,
`SECONDBOX_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS`,
`SECONDBOX_RUNNER_COMMAND_DELIVERY_BATCH_SIZE`,
`SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_SIZE`,
`SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_WAIT_MILLISECONDS`,
`SECONDBOX_DATA_PLANE_CLAIM_DURATION_MILLISECONDS`,
`SECONDBOX_DATA_PLANE_RETENTION_SECONDS`,
`SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES`,
`SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS`,
`SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS`,
`SECONDBOX_ASSIGNMENT_CLAIM_DURATION_MILLISECONDS`,
`SECONDBOX_ASSIGNMENT_DEADLINE_MILLISECONDS`,
`SECONDBOX_ASSIGNMENT_RETRY_LIMIT`,
`SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT`,
`SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS`,
`SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS`,
`SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES`,
`SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY`,
`SECONDBOX_OBJECT_STORE_USE_PATH_STYLE`.

`SECONDBOX_DATA_PLANE_RETENTION_SECONDS` is listed here as a default, not as a
retention-policy change. Its scope is the separate concern of
[Relay Frame Retention Scoped to Replay](2026-08-01-relay-retention-scope.md).

### Category D — contested. Requires an explicit decision before implementation. (3)

These are not implemented by this plan until the repository owner rules on them,
because a prior implemented plan or an operator-policy argument cuts against
moving them.

- `SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS` and
  `SECONDBOX_RUNNER_COMMAND_POLL_INTERVAL_MILLISECONDS`.
  `docs/plans/2026-07-31-relay-data-plane-wakeups.md` states both "remain
  mandatory recovery bounds" rather than "tunable optimisations", and lists
  "reducing, renaming, or repurposing either configured poll interval" as a
  non-goal. Giving them defaults does not reduce, rename, or repurpose them, and
  they remain mandatory to *honour*. Whether mandatory-to-honour also means
  mandatory-to-declare is a judgement that plan reserved, so this plan does not
  take it.
- `SECONDBOX_RUNNER_ENABLED_FEATURES`. A rollout switch. An empty default is
  defensible as fail-closed, but feature gating is operator policy and a silent
  default changes what a deployment does.

### What does not change

The rule itself stays, narrowed to its purpose: no default may be supplied for
any value that identifies, authenticates, or authorises a deployment, or that
pins an execution asset or a tenancy limit. Category C overrides remain
explicit-when-set and keep their validation, so a deployment that sets everything
today behaves identically after this change.

`deploy/environment.example` shrinks to Category A plus the runner and Compose
variables, and gains a commented block listing every Category C override with its
code default, so the surface stays discoverable without being mandatory.

## Non-goals

- Introducing a configuration file format. The environment contract is retained.
- Changing any runner-side variable. `runner/internal/config` is a separate
  surface and a separate decision.
- Changing quota, authority, or Profile semantics.
- Altering the values themselves. Every Category C constant adopts the value
  currently deployed in `deploy/environment.example`, so this is a change to
  where a number lives, not to what it is.
- Adding a defaulting framework, struct tags, or reflection-driven binding.
- Deciding Category D.

## Validation Commands

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- focused configuration tests covering each category
- `just test-scenario` on a qualified host

## Tasks

### Task 1: Pin the current surface with a test

Add a test asserting the exact set of variables `Load` requires, so every later
task moves a name between categories visibly rather than by implication. This
test is the mechanism that keeps Category A from eroding later.

### Task 2: Remove the code-owned facts

Delete `SECONDBOX_RUNNER_PROTOCOL_MINIMUM`, `SECONDBOX_RUNNER_PROTOCOL_MAXIMUM`,
and `SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES`. Move the protocol range to a
constant beside the runner-protocol definition and the frame bound beside the
relay framing. Prove that the compatibility suite fails if the constant and the
implemented protocol disagree, which is the property the environment variable
never had.

### Task 3: Convert the tuning constants to defaulted overrides

Give each Category C variable a named constant beside its consumer, keep the
existing parsing and validation on the override path, and keep the cross-field
checks. Prove that an unset variable yields the constant, that a set variable
still wins, and that an invalid set variable still fails closed rather than
falling back to the constant.

### Task 4: Reduce the deployment template

Rewrite `deploy/environment.example` to Category A plus runner and Compose
variables, with Category C listed as commented overrides annotated with their
code defaults. Update `just deploy-bootstrap` and `just deploy-validate` so
validation checks Category A completeness and rejects an unknown variable rather
than requiring the full former set.

### Task 5: Revise the affected documents

Amend the configuration rule in `AGENTS.md` to state the narrowed property
explicitly: no default for deployment identity, authority, pinned assets, or
tenancy limits; tuning constants live in code with optional overrides. Update
`docs/operations/deployment.md` to describe the two tiers and to list the
overrides. This rule is load-bearing and must change deliberately rather than by
implication.

## Alternatives rejected

- Leaving the rule as is. It is already producing defects rather than preventing
  them: the deployed poll interval was a required operator value that nobody had
  grounds to choose, and the resulting latency needed a transport change to fix.
- Defaulting everything including secrets. It removes the property the rule
  exists to protect, which is that a deployment cannot come up with a credential
  or a database nobody chose.
- A configuration file with a schema. It replaces one surface with two and does
  not answer which values an operator should be deciding, which is the actual
  problem.
- Deriving tuning values from a single named profile such as `low-latency`. It
  hides the individual values behind a label and makes an override an all-or-
  nothing choice.
- Reducing the surface by grouping variables into JSON blobs. It shortens the
  variable count while leaving the same number of decisions on the operator, and
  it degrades the error messages, which currently name the exact missing
  variable.
