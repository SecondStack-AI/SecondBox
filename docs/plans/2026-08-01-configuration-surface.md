---
title: Configuration Surface Split and Deployment Manifest
date: 2026-08-01
status: completed
owner: SecondStack
provenance: Operator configuration-surface review, 2026-08-01
---

# Plan: Configuration Surface Split and Deployment Manifest

## Outcome

Give operators one small, typed deployment manifest while keeping every process
boundary explicit. Deployment tooling resolves that manifest into the complete
environment consumed by Compose, `secondboxd`, and the Runner. Application
binaries do not gain a second configuration source, and operators no longer have
to understand or edit the rendered environment transport.

Within that rendered contract, separate deployment authority from tuning
constants so that the no-defaults rule protects what it exists to protect and
stops charging operators for facts the binary already owns.

`secondboxd` requires 59 environment variables today. `FromEnvironment` in
`internal/config/config.go` makes 47 direct `required*` calls. Of those, 44
resolve one environment variable each; `requiredQuota` expands one call into 9
names, and the 2 calls to `requiredBuiltInProfileBinding` expand into 3 names
each, yielding 59 distinct required variables. `deploy/environment.example`
presents 146 variables across the control plane, runner, and Compose deployment.

The governing rule is that every runtime setting is explicit and neither
application code nor deployment templates may supply defaults. That rule is
correct for deployment authority and wrong for tuning constants, and it is
currently applied uniformly to both.

Two symptoms show the conflation is not cosmetic:

- `config.go:187` rejects a session byte bound smaller than the frame byte bound,
  and `config.go:286` rejects a protocol minimum above the maximum. The code
  already owns these relationships; it declines to own the values.
- `docs/plans/2026-07-31-direct-port-data-plane.md` records
  `SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS` "at the deployed 250 ms"
  costing roughly 250 ms mean and 500 ms worst case per round trip, making SSH
  connection setup cost seconds. A tuning constant promoted to required operator
  configuration became a latency defect that an architecture change was needed to
  correct.

Success for this pass is:

- a development machine with Docker Compose going from a clean checkout to a
  ready local control plane with one command and no manually authored
  configuration;
- production bootstrap requiring no more than 8 decision groups: deployment
  shape (public ingress, TLS, and images), database, object store, Runner
  topology, execution-asset trust, application authorities, tenancy policy, and
  lifecycle policy (retention plus contested recovery and rollout settings);
- one versioned TOML manifest as the only operator-edited deployment source,
  with generated secrets referenced by path and every accepted policy preset
  expanded into its individual visible values;
- no environment-versus-manifest precedence rules: outside the one-shot legacy
  migration, the environment file is a mode-0600 generated artifact, is never
  accepted as operator input, and can be reproduced from the manifest plus its
  referenced secret material;
- required control-plane variables reduced from 59 to 38, with no variable
  carrying deployment identity, authority, or a pinned asset digest losing its
  explicit requirement;
- no code path acquiring a default for a secret, endpoint, credential, path,
  digest, or quota;
- the 2 removed variables eliminated because both binaries own the protocol
  version, and the 19 tuning variables retained as optional overrides that the
  stress, scenario, and Compose suites continue to set;
- `config.go` cross-field validation preserved for every value that survives as
  an override;
- `just deploy-config` accepts the manifest, atomically renders and validates the
  complete environment, and proves every absent Category C mapping remains unset
  while every manifest override reaches its process unchanged.

## Fixed design

Each currently required variable falls into exactly one of four categories, and
the category determines its treatment. The categories are decided here rather
than left to the implementation, because the whole point is that the boundary
between "operator decides" and "code decides" stops being ad hoc.

### Operator surface — one compiled deployment manifest

`secondbox.toml` is the sole operator-edited deployment source. It is a strict,
versioned TOML document owned by a new unprivileged `secondbox-deploy` command.
The command has `init`, `validate`, `render`, `runner-init`, and `inspect`
operations; the Justfile exposes the supported workflows without making
operators invoke the binary directly.

The manifest describes decisions, not the process environment one field at a
time. `schema_version = 1` is the only top-level scalar. Its sections are:

- `deployment`: development or production mode, public ingress, TLS termination,
  and immutable production image references;
- `database`: bundled or external mode and the authority required by that mode;
- `object_store`: bundled or external mode, endpoint addressing, bucket, region,
  temporary storage, and credential references;
- `runner_trust`: deployment-wide enrollment credential and CA references;
- repeatable `[[runners]]`: stable identity, same-host or remote placement, pool,
  capacity, host integration, network policy, execution assets, and identity-file
  references for each separately deployed Runner;
- `applications`: explicit application authorities;
- `policy`: the 9 subject quota values, relay retention, and contested rollout
  or recovery settings;
- `overrides`: the optional Category C tuning values, absent unless the operator
  intentionally changes one.

The manifest contains no generic `${ENV}` interpolation and the deployment
command does not merge it with ambient environment variables. Fields containing
secret material are file references. Development initialization generates those
files beneath a mode-0700 deployment directory; production requires supplied
references for external authorities and trust material. `inspect` is redacted
and shows every resolved non-secret value, including individually expanded quota
and retention values. There is no opaque runtime preset.

Local source and secret references are absolute or resolve relative to the
manifest directory, never the caller's working directory. Runner host paths are
separate typed values: they must be absolute but remain opaque when the Runner is
remote. Development initialization writes relative references within its own
directory so the deployment can be moved before first use without rewriting
machine-specific absolute source paths.

`secondbox-deploy render` strictly decodes the manifest, rejects unknown or
duplicate keys, validates cross-field mode requirements, reads the referenced
secrets, and atomically writes a mode-0600 generated environment file with a
do-not-edit header. The existing process loaders remain the final validation
boundary. The generated file is transport for Compose, not a second supported
configuration surface, and is never read back as input to update the manifest.
Secret text files contain one value: resolution removes at most one terminal LF
and rejects CR, additional line breaks, or NUL without trimming any other byte.
The renderer has separate canonical encoders for the Compose env file and the
systemd `EnvironmentFile` used by remote Runners; it never assumes their quoting
grammars are interchangeable. Both encoders handle `$`, quotes, backslashes,
spaces, and JSON literally and never emit an interpolation expression.

Before invoking Compose, deployment tooling removes every ambient `SECONDBOX_*`
and `COMPOSE_*` variable so shell precedence and Compose self-configuration cannot
override the rendered artifact. Only a small documented allowlist of Docker
client connectivity variables survives, while the Compose file, env file,
project name, and overlay list are supplied as explicit command arguments.
The Compose environment contains the control-plane and infrastructure values plus
the optional same-host Runner's values when that overlay is selected. Rendering
produces a separate protected systemd environment artifact for each remote
Runner. Each remote artifact is a handoff for that Runner host and is never
copied, opened, or executed by the unprivileged control-plane deployment.

Compose topology follows the same ownership split. `deploy/compose.yml` contains
only the unconditional control plane and shared resources.
`deploy/compose.development.yml` adds bundled PostgreSQL and object storage, and
`deploy/compose.same-host-runner.yml` adds the privileged Runner. Deployment
tooling selects overlays from the resolved manifest and passes every file
explicitly. Inactive services never remain behind a profile with required
interpolation, because Compose expands those expressions even when their profile
is inactive.

Development mode owns one reviewed local topology. `just deploy-development-up
<directory>` creates `secondbox.toml` and its secret directory when absent,
renders the environment, validates Compose, prepares development assets, and
starts the deployment. It is idempotent and refuses to replace an existing
manifest, secret, identity, workspace, or generated asset. Production has no
silent authority defaults: `init` emits the strict manifest shape and reports
the unresolved decisions, while flags allow the same inputs to be supplied
non-interactively by automation.

### Category A — deployment authority. Stays required, no default. (35)

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
`SECONDBOX_OBJECT_STORE_ROOT_PASSWORD`,
`SECONDBOX_OBJECT_STORE_USE_PATH_STYLE`,
`SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY`,
`SECONDBOX_DATA_PLANE_RETENTION_SECONDS`, the 6 `SECONDBOX_BUILTIN_*` pool and
bundle digest values, and the 9 `SECONDBOX_DEFAULT_SUBJECT_*` quota values.

Quota is included deliberately. It reads like tuning and is not: it is the
multi-tenant policy boundary, and a default would silently admit load the
operator never sanctioned.

The three additional deployment-owned values are required deliberately.
`SECONDBOX_OBJECT_STORE_USE_PATH_STYLE` selects request-addressing semantics for
the configured S3-compatible endpoint, and
`SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY` names a deployment filesystem path that
must exist and be writable. Neither is a tuning constant. Platform-wide relay
retention remains operator policy, as assigned by
`docs/design/service-boundaries.md`, so
`SECONDBOX_DATA_PLANE_RETENTION_SECONDS` also remains required.

### Category B — code-owned facts. Removed entirely. (2)

Not defaulted. Removed, because accepting a value at all admits a value the
binary cannot honour.

- `SECONDBOX_RUNNER_PROTOCOL_MINIMUM` and `SECONDBOX_RUNNER_PROTOCOL_MAXIMUM`.
  The supported range is a property of the compiled control-plane and Runner
  protocol implementations. An operator can currently declare a range the code
  does not implement, and `config.go:286` only checks the two values against each
  other, not against what exists. Identical constants live beside both generated
  protocol packages, the generation verifier rejects drift between them, and
  both protocol implementations and their compatibility suites consume those
  constants.

### Category C — tuning constants. Code constant with optional override. (19)

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
`SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES`,
`SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES`,
`SECONDBOX_LIFECYCLE_RECONCILE_BATCH_SIZE`,
`SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS`,
`SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS`,
`SECONDBOX_ASSIGNMENT_CLAIM_DURATION_MILLISECONDS`,
`SECONDBOX_ASSIGNMENT_DEADLINE_MILLISECONDS`,
`SECONDBOX_ASSIGNMENT_RETRY_LIMIT`,
`SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT`,
`SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS`,
`SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS`,
`SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES`.

The frame bound remains an override because the relay currently honours every
valid configured value; removing that capability would be a compatibility
change, not merely a transfer of ownership. Its code default is the currently
deployed value, and the existing session-greater-than-or-equal-to-frame check
still applies when either bound is overridden.

The scope of `SECONDBOX_DATA_PLANE_RETENTION_SECONDS` remains the separate
concern of [Relay Frame Retention Scoped to Replay](2026-08-01-relay-retention-scope.md).
This plan leaves both its value and its required operator ownership unchanged.

### Category D — contested. Stays required pending an explicit decision. (3)

These remain required by `FromEnvironment`, the deployment manifest, the
rendered environment, and `deploy/compose.yml`. They are not defaulted by this
plan because a prior implemented plan or an operator-policy argument cuts
against moving them. A future repository-owner decision may reclassify them in a
separate change.

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
any value that identifies, authenticates, authorises, locates, or selects a
deployment integration, or that pins an execution asset, tenancy limit, or
operator-owned retention policy. Category C overrides remain explicit-when-set
and keep their validation. A manifest initialized with the values in today's
`deploy/environment.example` behaves identically after deleting the two obsolete
protocol-range declarations; a noncanonical declared protocol range is an
unsupported configuration and intentionally has no compatibility path.

`deploy/environment.example` is replaced by `deploy/secondbox.example.toml`.
Operators copy or generate the manifest, never the environment transport. The
generated environment contains Categories A and D plus the Runner and Compose
values. Category C is omitted unless `overrides` selects it, so the Compose
service does not synthesize an empty value. `secondbox-deploy inspect` and the
example manifest keep every available override discoverable without making it
mandatory.

## Non-goals

- Adding native TOML readers to `secondboxd` or the Runner. Their environment
  contracts remain explicit and provider-neutral; only deployment tooling reads
  the manifest.
- Adding environment overrides on top of the manifest, manifest interpolation,
  include files, inheritance, or a general-purpose configuration framework.
- Changing any Runner process variable or its parsing semantics. The deployment
  manifest resolves the existing Runner surface but does not default inside
  `runner/internal/config`.
- Orchestrating remote Runner hosts, copying their generated bundles, qualifying
  their KVM, network, or workspace setup, or relocating a Runner identity or its
  Workspace root.
- Changing quota, authority, or Profile semantics.
- Altering the values themselves. Every Category C constant adopts the value
  currently deployed in `deploy/environment.example`, and the generated manifest
  example pins those same literals, so this is a change to where a number lives,
  not to what it is.
- Adding a defaulting framework, struct tags, or reflection-driven binding to
  either application binary. Deployment resolution is an explicit typed path.
- Deciding Category D.

## Validation Commands

- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- `just test-deployment`
- `go test ./internal/deployconfig ./cmd/secondbox-deploy -count=1`
- `go test ./internal/config ./internal/runnercontrol -count=1`
- `(cd runner && go test ./cmd/secondbox-runner ./internal/firecracker
  ./internal/runnercontrol ./scripts -count=1)`
- `just deploy-init-development <temporary-directory>` followed by
  `just deploy-config <temporary-directory>/secondbox.toml`
- production manifest validation with each decision group independently absent
- generated-manifest and rendered-environment drift tests
- focused configuration tests covering each category
- `just test-scenario` on a qualified host

## Tasks

### Task 1: Pin the current surface with a test

- [x] Complete

Add a test asserting the exact set of 59 variables `FromEnvironment` initially
requires, so every later task moves a name between categories visibly rather
than by implication. Extend it with the final 38-name assertion: all 35 Category
A and all 3 Category D variables remain required, Category C is optional, and
Category B is no longer read. This test is the mechanism that keeps deployment
authority from eroding later.

### Task 2: Revise the governing configuration rule

- [x] Complete

Before adding defaults, amend the configuration rule in `AGENTS.md`: no default
for deployment identity, authority, integration selection, paths, pinned assets,
tenancy limits, or operator-owned retention; code-owned tuning constants may
have optional validated overrides. The rule must distinguish static deployment
templates from the manifest compiler: application code and templates never
invent authority, while `secondbox-deploy init` may generate secrets and
materialize the reviewed development topology or values explicitly accepted by a
production operator. Generated values are written into the manifest or referenced
secret files and are never silent runtime defaults.

### Task 3: Define the deployment-manifest contract

- [x] Complete

Add `internal/deployconfig` with explicit `ManifestV1` and
`ResolvedDeployment` types, and add the unprivileged `cmd/secondbox-deploy`
entrypoint. Implement `validate`, `render`, `runner-init`, and redacted `inspect`
commands here; Task 4 implements `init`. Add
`deploy/secondbox.example.toml` with `schema_version = 1` and the sections fixed
above. Decode strictly: unknown and duplicate keys, unsupported schema versions,
ambiguous bundled/external fields, missing mode-specific authority, empty secret
references, and invalid cross-field relationships all fail with greppable
`SecondBox deployment manifest` errors.

Resolution does not read ambient environment variables, include another file, or
apply operator overrides from another source. It produces one typed resolved
model and one deterministic, sorted environment mapping. Secret values enter only
by reading the exact regular, non-symbolic-link files named by the manifest.
`inspect` redacts secret values and paths that reveal secret material, but shows
all non-secret derived values and the 19 optional override names with their code
defaults. Unit tests cover every field, mode, rejection, and redaction, and map
each name in the current 146-name deployment surface deliberately to a resolved
field, generated value, optional override, or removed fact.

Do not duplicate a second unchecked process schema in the deployment package.
Generate conformance fixtures from the resolved model. Root-module tests feed the
control-plane artifact through `config.FromEnvironment`. Runner-module tests feed
each Runner artifact through the production composition:
`runnercontrol.LoadRunnerProtocolConfigFromEnv`,
`firecracker.LoadRunnerFirecrackerConfigFromEnv`, the Runner log-path validator,
and the container entrypoint and host-network environment contracts. Extract one
shared composition validator from the Runner entrypoint if that is needed to keep
the test and PID-1 path identical; do not create a parallel schema. Remote Runner
conformance starts with the rendered systemd `EnvironmentFile`, round-trips it
through the canonical systemd consumer fixture, and only then invokes that
production composition; same-host conformance likewise starts with the rendered
Compose artifact rather than the raw resolved mapping. The suite fails when the
renderer omits a required name, emits an unknown name, changes a type, misencodes
an artifact, or no longer triggers the owning loader's validation. The process
loaders and entrypoint consumers remain the authorities for their respective
runtime contracts.

Runner declarations are keyed by immutable `runner_id`. At most one may use the
same-host Compose placement; any number may describe separately deployed remote
Runners. `runner-init` issues one client identity from the deployment CA and
writes that Runner's protected identity and rendered configuration directory. It
refuses an existing target or an identity mismatch and never regenerates another
Runner's credentials. Rendering requires the exact declared identity evidence
but never resolves or validates remote host paths on the control-plane host.

### Task 4: Build initialization and the one-command development path

- [x] Complete

Implement `secondbox-deploy init`. Development initialization requires only an
output directory, writes a mode-0600 complete manifest with the reviewed loopback
Compose topology, expands the starter quota and retention policy into individual
literal fields, and generates the current credentials, Runner PKI, synthetic
signed-asset catalog, and bundle bindings beneath a mode-0700 secret directory.
Production initialization writes no usable authority implicitly: it accepts the
eight decision groups as flags for automation or writes an annotated incomplete
manifest and reports every unresolved group in one error.

All writes use validated explicit paths, temporary siblings, fsync, and atomic
rename. Initialization refuses symlinks, existing secret material, and partial
identity replacement; a failed run removes only artifacts it created. Delete the
replaced shell bootstrap path rather than retaining two generators. Add
`deploy-init-development`, `deploy-init-production`, and
`deploy-development-up` recipes. The last recipe initializes only when the
manifest is absent, then builds the control-plane image, renders, validates,
prepares assets, starts the development dependencies and control plane, and
requires `/readyz`; otherwise it uses the existing manifest without rewriting
it. Starting and qualifying a privileged same-host or remote Runner remains an
explicit subsequent operation on a qualified host.

### Task 5: Remove the code-owned protocol declarations

- [x] Complete

Delete `SECONDBOX_RUNNER_PROTOCOL_MINIMUM` and `SECONDBOX_RUNNER_PROTOCOL_MAXIMUM`
from the loader, manifest schema, rendered environment, Compose, and validation.
Add identical handwritten protocol-version constants beside `gen/runner/v1` and
`runner/internal/runnerprotocol`; extend
`scripts/verify-runner-protocol-generated.sh` to reject drift between those
files. Make the control plane, Runner, stale-runner probe, and both modules'
compatibility tests consume their local mirrored constant rather than a literal.
Prove that supported peers negotiate it and adjacent unsupported versions are
rejected. This gives both independently built binaries one verified protocol
window, which the environment variables never did.

### Task 6: Convert the tuning constants to validated overrides

- [x] Complete

Give each Category C variable a named constant beside its consumer, keep the
existing parsing and validation on the override path, and keep the cross-field
checks. Prove that an unset variable yields the constant, that a set variable
still wins, and that an invalid set variable still fails closed rather than
falling back to the constant. Tests name all 19 literal deployed values rather
than merely comparing a result with the constant under test. The manifest
override registry references those constants and owns their TOML names, help
text, and environment mapping. `deploy/secondbox.example.toml` is generated from
that registry; `just verify-generated` fails if the constants, registry,
rendered help, or example adds, loses, or changes a name or value.

### Task 7: Compile the manifest into the deployment

- [x] Complete

Replace `deploy/environment.example` as an operator input. `render` converts the
resolved manifest into the full environment expected by `deploy/compose.yml`,
writes it atomically with mode 0600 and a generated-file header, and then invokes
the existing environment validation as an internal postcondition. No runtime,
render, validation, or Compose command accepts an operator-supplied environment
path; the one-shot legacy migration in Task 8 is the only exception.
Reimplement the environment validator in the typed deployment package and delete
the replaced shell validator once its tests move; do not retain two schemas.

Split the Compose model into the unconditional `deploy/compose.yml`, bundled
infrastructure in `deploy/compose.development.yml`, and the privileged Runner in
`deploy/compose.same-host-runner.yml`; do not use profiles to hide services whose
variables may be absent. Delete Category B mappings, retain required interpolation
for Categories A and D, and use Compose's value-less pass-through form for
Category C so an absent override is omitted while a selected manifest value is
forwarded unchanged. Change `deploy-config`, deployment prepare, up, and down
workflows to accept the manifest, select the exact overlay list, and render the same
protected artifact before invoking Compose. The artifact is always overwritten
from authoritative inputs and never parsed as operator intent. Compose is spawned
without ambient `SECONDBOX_*` or `COMPOSE_*` variables so shell and Compose
self-configuration precedence cannot change the resolved deployment.

Extend deployment tests beyond `scripts/compose-test.yml`: bootstrap a temporary
development manifest, prove `just deploy-config` succeeds with every Category C
name absent, and inspect the rendered control-plane environment to prove those
names have null values and therefore remain unset rather than becoming empty
strings. Then set representative positive and zero-allowed integer overrides in
the manifest and prove their exact values are rendered. Qualify production mode
with bundled and external database/object-store combinations, secret-file
failures, immutable image enforcement, deterministic rerendering, and rejection
of poisoned ambient `SECONDBOX_*` and `COMPOSE_*` values. Feed values containing
Compose and systemd interpolation and quoting metacharacters through the target
encoders. Use `docker compose config` plus a real probe container for the Compose
artifact, and qualify the Runner artifact through the real systemd
`EnvironmentFile` consumer on a Linux host; prove byte-exact process environment
values in both. Prove that a manually altered generated environment is overwritten
before Compose reads it and cannot affect the resolved deployment. Cover zero,
one same-host, and multiple remote Runner declarations, immutable Runner IDs,
per-runner artifact isolation, and refusal to resolve a remote Runner's host
paths locally. Render the base model with all same-host Runner variables absent,
and render external production modes with all bundled database and object-store
variables absent, proving that no inactive overlay can reintroduce a requirement.

### Task 8: Revise operator documentation and migration

- [x] Complete

Update `docs/operations/deployment.md` to lead with the one-command development
path and the production decision groups, then document manifest initialization,
strict validation, redacted inspection, secret references, rendering, and
recovery. Document required authority, required contested settings, optional
tuning, and removed code-owned facts as manifest concepts rather than a list of
environment variables.

This is an intentional deployment-interface replacement. Add a changelog entry
and a migration command that reads one legacy validated environment file,
extracts its secret values into a new protected target without modifying the
source, and writes a manifest with references to those files exactly once. It
refuses an existing target and cleans up only files it created on failure. The
migration rejects unknown, duplicate, placeholder, or invalid legacy values and
never becomes a runtime compatibility path. Remove instructions that tell
operators to copy or edit `deploy/environment.example`.

## Alternatives rejected

- Leaving the rule as is. It is already producing defects rather than preventing
  them: the deployed poll interval was a required operator value that nobody had
  grounds to choose, and the resulting latency needed a transport change to fix.
- Defaulting everything including secrets. It removes the property the rule
  exists to protect, which is that a deployment cannot come up with a credential
  or a database nobody chose.
- Keeping environment variables as the operator-facing source. Bootstrap can
  generate them, but a flat 146-name document remains difficult to understand,
  review, and evolve; it improves keystrokes without improving the deployment
  model.
- Teaching `secondboxd` and the Runner to read TOML. It creates native config and
  environment precedence in both binaries and makes every orchestrator carry two
  public runtime contracts. Compiling one manifest into the existing environment
  boundary gets the operator benefit without that duplication.
- Allowing environment overrides on top of the manifest. It makes the effective
  deployment depend on hidden ambient state and prevents the manifest from being
  a reproducible authority.
- Storing production secret values inline in the manifest. One logical source of
  deployment intent does not require one physical file containing every secret;
  explicit file references preserve rotation and secret-manager integration.
- Deriving tuning values from a single named profile such as `low-latency`. It
  hides the individual values behind a label and makes an override an all-or-
  nothing choice.
- Reducing the surface by grouping variables into JSON blobs. It shortens the
  variable count while leaving the same number of decisions on the operator, and
  it degrades the error messages, which currently name the exact missing
  variable.
- Removing the relay frame bound entirely. The current relay honours valid
  nonstandard bounds, so preserving it as an optional override avoids an
  unrelated compatibility break while still removing a mandatory tuning choice.
