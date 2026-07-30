# Local stress qualification

`just test-stress` measures how a real local SecondBox deployment changes as concurrency increases.
It is a thin configuration adapter over the prerequisite checks, signed-artifact verification,
dynamic host network, Compose topology, and cleanup path used by `just test-scenario`. It then drives
the deployment exclusively through the published HTTP API with
`sdk/go/secondboxclient`. The driver does not read PostgreSQL, import a control-plane or runner
package, or call a runner-internal endpoint.

This is a qualified-compute gate. It has no mock, fake runner, reduced execution mode, or successful
skip. A host that cannot start the real runner exits nonzero and names the missing prerequisite.

## What the sweep runs

Every configured concurrency level runs all five workloads:

- `sandbox_create`: create through ready, including Phase 1 boot-stage timing collection, then delete.
- `buffered_exec`: a bounded successful command on a ready Sandbox.
- `streaming_exec`: a bounded output stream with explicit WebSocket output credit.
- `file_transfer`: a digest-checked write and byte-bounded read of identical binary content.
- `snapshot_restore`: write, stop, Snapshot, start, mutate, stop, restore, start, verify, and delete
  the Snapshot.

`durationSeconds` applies separately to each workload at each concurrency level. Setup and cleanup
time, and an iteration that was already admitted when the duration expired, are additional. A
five-workload, five-level, 60-second configuration therefore has at least 25 minutes of measured
work before Sandbox setup, lifecycle convergence, image build, and teardown.

The harness creates its RunnerPool and Profile through the API before starting the runner. Every
Sandbox, Exec, File request, Snapshot, restore, timing query, and cleanup mutation also uses the
published API. The runner enrolls through its authenticated outbound control stream.

## Qualified host and artifact prerequisites

The host must provide:

- readable and writable `/dev/kvm` and `/dev/net/tun`;
- Docker with Compose v2, Go, `curl`, `findmnt`, Git, `ip`, `jq`, OpenSSL, and `sha256sum`;
- the complete signed microVM artifact directory described by
  [the microVM image pipeline](microvm-image-pipeline.md);
- the trusted artifact public key and its lowercase DER SHA-256 fingerprint;
- an existing, non-root, non-symlink XFS or Btrfs directory for per-run Workspace storage;
- two unused loopback TCP ports selected by the shared scenario harness.

The shared harness chooses an unused bridge subnet and process-unique bridge and TAP names. The
runner removes the bridge and its exact firewall rules during trapped cleanup. The Workspace setting
names a qualified parent; the harness creates one unique per-run Workspace root beneath it and
removes that exact root after restoring file ownership.

The artifact verifier runs before Compose. It checks the public-key fingerprint, manifest signature,
checksums, component manifest digests, architecture, guest-protocol range, rootfs contract, and
browser surface. A missing or invalid artifact is a failed run, not a skipped test.

## Prepare a reusable local bundle

The normal local workflow separates the expensive image build from repeated stress runs:

```sh
just prepare-stress
just test-stress
```

`just prepare-stress` creates persistent, gitignored state beneath `.secondbox/stress/`. It generates
one local RSA signing key and separate public trust anchor, prepares the standard guest rootfs,
builds the pinned kernel, creates and signs the approximately 11 GB bundle, verifies that bundle
against the saved trust anchor, and installs a copy of the example stress configuration. It also
creates the result and Workspace parent directories. The private key has mode `0600` and is kept
outside the artifact bundle.

The one-time local build additionally requires `debootstrap`, `fakeroot`, a working Docker Engine,
the pinned-kernel build toolchain, and passwordless or interactive `sudo` access for rootfs staging.
The preparation command checks these before starting the build and names the first missing
prerequisite. They are not required when it only verifies and reuses an existing saved bundle.

Preparation is idempotent: when a saved artifact directory exists, the command verifies and reuses
it instead of rebuilding. Change `.secondbox/stress/stress.json` to tune the local sweep. To keep the
large state elsewhere, set `SECONDBOX_STRESS_LOCAL_ROOT` to a clean absolute path for both commands.
That path must reside on XFS or Btrfs because it also owns the local Workspace parent.

After preparation, `just test-stress` discovers the bundle, public key, key fingerprint,
configuration, and Workspace parent automatically. Each run writes a new timestamped result below
`.secondbox/stress/results/`.

The parser refuses unknown fields, missing workloads, duplicated workloads, non-increasing
concurrency levels, inconsistent runner/Profile capacity, out-of-range timing windows, and transfer
sizes above either configured bound. Host-network, Compose, and artifact settings are deliberately
absent from the stress JSON because the shared scenario harness owns them.

The example deliberately makes the memory budget the first theoretical binding limit:
`4096 MiB / 512 MiB = 8` concurrent Instances. This is an example configuration, not a published
performance baseline. Change it to the qualified host's intended limits.

## Explicit qualification and CI inputs

External qualification hosts and CI may bypass the local layout by setting every input explicitly:

```sh
export SECONDBOX_REQUIRE_QUALIFIED_STRESS=1
export SECONDBOX_STRESS_CONFIG=/absolute/path/stress.json
export SECONDBOX_STRESS_OUTPUT=/absolute/path/stress-result.json
export SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR=/absolute/path/firecracker-artifacts
export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY=/absolute/path/manifest-public.pem
export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
export SECONDBOX_RUNNER_WORKSPACE_ROOT=/absolute/path/reflink-qualification-parent
just test-stress
```

When omitted, `SECONDBOX_REQUIRE_QUALIFIED_STRESS` is set to `1` by the dedicated stress recipe.
Supplying any other value still fails. Explicit path and fingerprint values override all locally
prepared values.

`SECONDBOX_STRESS_OUTPUT` must be an absent absolute path whose parent already exists. The harness
refuses to overwrite it and creates it with mode `0600`. The shared topology's distinct test-only
platform, runner, database, and object-store credentials are supplied only to their consumers and
are never written to the result.

The gate prints the source commit, Go version, `findmnt` evidence, and signed artifact-manifest
digest before starting the sweep. Failure logs come from the control plane, runner, PostgreSQL, and
object store. Cleanup runs for both success and failure.

## Reading the result

The terminal table and JSON file report, for every workload and concurrency level:

- attempts, successes, admission refusals, and other failures;
- completed operations per second;
- nearest-rank p50, p95, and p99 wall-clock latency;
- whether p95 crossed `latencyDegradationRatio` relative to that workload's first measured level;
- whether concurrency reached the first theoretical configured limit.

The result separately names:

- `configuredFirstBinding`: the minimum of
  `SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL`, runner memory budget divided by per-Sandbox memory,
  `SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL` divided by Profile concurrent operations,
  guest addresses available from the shared harness's selected bridge CIDR, and the first applicable subject
  Sandbox, active-Instance, CPU, memory, concurrent-Operation, or Snapshot quota;
- `problemCounts`: the actual provider-neutral API outcomes, including quota or execution-node
  refusal;
- `bootStages`: p50/p95/p99 from the persisted Phase 1 stage evidence;
- `dominantBootStage`: the stage with the highest observed p95;
- `deploymentTiming`: the Phase 2 aggregate boot, Exec, API, Operation, and stage timing view for the
  explicit window.

The configured first binding is a capacity calculation, not a claim that it caused a measured
refusal. The refusal counts and problem codes are the observed evidence. A level is marked for
latency saturation only when its measured p95 crosses the configured ratio. This separation keeps a
run honest when several limits tie or an unrelated host bottleneck degrades latency first.

## Establishing a healthy baseline

A healthy baseline is host- and artifact-specific. Establish it on a named qualified host and retain
the JSON result with the source and artifact digests. Below the first configured binding, expect no
admission refusals or unexplained failures, throughput to increase with concurrency, and p95 to stay
below the selected degradation ratio. At or above the binding, refusal or latency saturation is
expected and must agree with the configured-capacity calculation or be investigated.

Do not publish invented universal millisecond thresholds. Compare later runs with the same host,
artifact digest, Profile, workload sizes, duration, and concurrency sweep. A change to any of those
inputs creates a new baseline.
