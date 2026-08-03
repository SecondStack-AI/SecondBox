# External scenario qualification

`just test-scenario` is the qualified black-box gate for the complete SecondBox path: public HTTP API, PostgreSQL desired state, S3-compatible Artifact storage, the authenticated runner protocol, runner-local reflink Workspaces and Snapshots, and Firecracker guests on real KVM.

The suite never skips. It exits non-zero unless qualification is explicitly required and every host and artifact prerequisite is present. A passing `just test-compose` is not evidence for this gate: the Compose suite has no real runner or guest, while the scenario suite proves that public operations reach real compute.

## Qualified host

Use a dedicated Linux x86-64 host with:

- readable and writable character devices at `/dev/kvm` and `/dev/net/tun`;
- cgroup v2 mounted for the privileged runner container;
- Docker Engine and Docker Compose v2, with permission to start privileged, host-networked containers and create host mounts;
- an existing absolute workspace-root directory on XFS or Btrfs;
- working reflink support in that filesystem. The harness checks the filesystem type and runner readiness performs the real `FICLONE` and mutation-isolation probe;
- `curl`, `docker`, `findmnt`, `git`, `go`, `ip`, `jq`, `mountpoint`, `openssl`, `python3`, `seq`, and `sha256sum`.

The workspace root is a parent for an operation-scoped scenario directory. Do not point it at a live runner's workspace root. The harness removes only its generated child after stopping the runner and unmounting propagated guest mounts.

## Materialize and verify the microVM bundle

Building the approximately 11 GB bundle is deliberately outside this gate. Build or obtain a signed bundle by following [the microVM image pipeline](microvm-image-pipeline.md), then materialize it into an existing absolute, non-symbolic-link directory on the qualification host. For a release scratch image, one explicit extraction flow is:

```sh
artifact_image='ghcr.io/secondstack-ai/secondbox-runner-microvm-artifacts:<release-tag>'
artifact_target='/srv/secondbox/qualification/microvm'
artifact_container="$(docker create "$artifact_image")"
install -d -m 0755 "$artifact_target"
docker cp "$artifact_container:/secondbox-runner-microvm/." "$artifact_target/"
docker rm "$artifact_container"
```

Treat the signing key as an independently distributed trust anchor; do not trust a `signing.pub` copied from the bundle. Compute its canonical DER fingerprint and verify the materialized bundle before running the suite:

```sh
artifact_public_key='/etc/secondbox/trust/artifact-signing.pem'
artifact_public_key_sha256="$(
  openssl pkey -pubin -in "$artifact_public_key" -outform DER |
    sha256sum |
    awk '{print $1}'
)"
just -f runner/Justfile verify-microvm-images \
  "$artifact_target" \
  "$artifact_public_key" \
  "$artifact_public_key_sha256"
```

Verification checks the fixed artifact set, payload checksums, signed manifest, provenance bindings, architecture, guest protocol range, and rootfs contract. The scenario harness repeats the checks it relies on before Compose starts and refuses symbolic-link artifacts.

## Required variables

Set all five variables. Paths must be clean absolute paths that already exist.

```sh
export SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1
export SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR="$artifact_target"
export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY="$artifact_public_key"
export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256="$artifact_public_key_sha256"
export SECONDBOX_RUNNER_WORKSPACE_ROOT='/srv/secondbox/qualification/workspaces'
just test-scenario
```

- `SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1` turns every missing prerequisite into a named hard failure.
- `SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR` is the verified materialized bundle containing at least `SHA256SUMS`, `kernel`, `rootfs.ext4`, `shared.img`, `manifest.json`, `manifest.sig`, `runtime-manifest.json`, and `toolchain-manifest.json`.
- `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY` is the independently trusted PEM public key.
- `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256` is the 64-character lowercase SHA-256 of that key's canonical DER encoding.
- `SECONDBOX_RUNNER_WORKSPACE_ROOT` is the dedicated XFS or Btrfs parent described above.

`SECONDBOX_SCENARIO_TEST_PATTERN` is an optional Go regular expression for a focused diagnostic rerun. It does not qualify a commit; qualification requires the unfiltered command.

For CI, configure the four path and fingerprint values as repository or organization variables and attach the labels `self-hosted`, `linux`, `x64`, and `secondbox-kvm` only to hosts satisfying this document. The workflow sets `SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1`.

## Register the qualification runner

The qualified suite runs from the dedicated `scenario-qualification` workflow on its nightly schedule or by manual dispatch. It requires a repository-level self-hosted runner registered to `SecondStack-AI/SecondBox` with the labels `self-hosted`, `linux`, `x64`, and `secondbox-kvm`.

An administrator can obtain a short-lived registration token with `gh` and configure an installed GitHub Actions runner from its installation directory:

```sh
registration_token="$(gh api --method POST repos/SecondStack-AI/SecondBox/actions/runners/registration-token --jq .token)"
./config.sh \
  --url https://github.com/SecondStack-AI/SecondBox \
  --token "$registration_token" \
  --labels secondbox-kvm
```

The equivalent settings path is **SecondStack-AI/SecondBox → Settings → Actions → Runners → New self-hosted runner**. GitHub adds the default `self-hosted`, `Linux`, and `X64` labels; label matching is case-insensitive. Runners registered to other repositories, including repositories in the same organization, are not visible to or usable by SecondBox.

## Evidence and timing budgets

Preserve the beginning and end of the command output. A qualified run prints:

- the `findmnt` target, source, filesystem type, and mount options for the workspace root;
- the source commit;
- the Go version;
- the resolved `manifest.json` SHA-256;
- the allocated benchmark guest network;
- `SecondBox scenario qualification passed`.

The expected wall-clock duration is 6–10 minutes on the reference host, including control-plane and runner builds, image construction, Compose startup, 15 serial scenarios, and cleanup. CI allows 45 minutes and the Go test process has a 30-minute hard timeout so slow or wedged teardown remains bounded.

A normal cold guest reaches `microvm_ready` within 5 seconds on the reference host; the 2026-07-29 qualification observed approximately 2.5–2.7 seconds. The runner logs every `microVM cold start stage` with stage and cumulative milliseconds. The scenario deployment's 30-second assignment deadline is the hard boot budget. Treat a sustained rise above the 5-second expectation as a performance regression even when it remains below the hard deadline.

Archive failure output as well: the harness prints Compose state plus bounded control-plane, runner, PostgreSQL, object-store, and Firecracker logs before cleanup.
