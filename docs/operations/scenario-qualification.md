# External scenario qualification

`just test-scenario` is the qualified black-box gate for the complete SecondBox path: public HTTP API, PostgreSQL desired state, S3-compatible Artifact storage, the authenticated runner protocol, runner-local reflink Workspaces and Snapshots, and Firecracker guests on real KVM.

The suite never skips. It exits non-zero unless qualification is explicitly required and every host and artifact prerequisite is present. A passing `just test-compose` is not evidence for this gate: the Compose suite has no real runner or guest, while the scenario suite proves that public operations reach real compute.

## Qualified host

Use a dedicated Linux x86-64 host with:

- readable and writable character devices at `/dev/kvm` and `/dev/net/tun`;
- cgroup v2 mounted for the privileged runner container;
- Docker Engine and Docker Compose v2, with permission to start privileged, host-networked containers and create host mounts;
- an existing absolute workspace-root directory on XFS or Btrfs;
- the workspace root, signed artifact directory, and checkout on the same filesystem, so rootfs staging into the operation run directory and jail remains reflink/link-only;
- working reflink support in that filesystem. The harness checks the filesystem type and runner readiness performs the real `FICLONE` and mutation-isolation probe;
- `curl`, `date`, `docker`, `findmnt`, `git`, `go`, `ip`, `jq`, `mountpoint`, `openssl`, `python3`, `seq`, and `sha256sum`.

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

Run this command directly on the qualified local host before release staging. GitHub Actions does not run the KVM suite and no self-hosted Actions runner or repository path variable is required.

## Evidence and timing budgets

The harness removes `.tmp/scenario-qualification-evidence.json` when any scenario run starts. Only a complete, unfiltered `test-scenario` suite whose teardown also succeeds writes a replacement. Focused, failed, stress, and lifecycle runs leave no release qualification evidence.

The JSON records schema `secondbox.release/qualification-evidence/v1`, the full source commit, whether the repository was dirty, suite name, top-level pass count, total wall-clock seconds, UTC completion time, KVM and TUN availability, and the checked workspace mount and filesystem type. `release-stage` requires the file to name its exact embedded `sourceCommit` and to record a clean repository. It stages the document as `secondbox-<version>-qualification-evidence.json` and binds its digest in the artifact manifest and `SHA256SUMS`.

Preserve the beginning and end of the command output. A qualified run prints:

- the `findmnt` target, source, filesystem type, and mount options for the workspace root;
- the source commit;
- the Go version;
- the resolved `manifest.json` SHA-256;
- the allocated benchmark guest network;
- the separately allocated Compose backend network;
- `SecondBox scenario qualification passed`.

The expected wall-clock duration is 6–10 minutes on the reference host, including control-plane and runner builds, image construction, Compose startup, the serial scenario suite, and cleanup. The Go test process has a 30-minute hard timeout so slow or wedged teardown remains bounded.

A normal cold guest reaches `microvm_ready` within 5 seconds on the reference host; the 2026-07-29 qualification observed approximately 2.5–2.7 seconds. The runner logs every `microVM cold start stage` with stage and cumulative milliseconds. The scenario deployment's 30-second assignment deadline is the hard boot budget. Treat a sustained rise above the 5-second expectation as a performance regression even when it remains below the hard deadline.

Archive failure output as well: the harness prints Compose state plus bounded control-plane, runner, PostgreSQL, object-store, and Firecracker logs before cleanup.

## Installer qualification

`just test-installer` is the ordinary-host suite for installer contracts, release verification, bootstrap generation, fake orchestration, resume, uninstall, and confined purge. It does not substitute for KVM.

`just test-installer-vm` drives disposable systemd guests when an explicit VM controller configuration is present. `just test-installer-qualified` is the non-skipping real-host gate for the published-style bootstrap, Btrfs-image and existing-filesystem paths, reboot recovery, retained-workspace uninstall/resume, purge confinement, and a real hello-world microVM. The harness independently derives a qualification-subject digest from the tested release manifest and requires the driver to report that exact identity. Its evidence is separate from the scenario evidence described above because installer qualification proves host mutation and reboot behavior while `test-scenario` proves the public runtime contract.

The repository-owned qualification driver uses `qemu:///system` and creates two sequential, uniquely named Ubuntu guests. Each guest receives its own QEMU user network, deterministic MAC address, explicit NoCloud DHCP configuration, and verified localhost-only SSH forward; no host bridge, libvirt network, or firewall exception is required. The driver stores the pinned base-image copy and guest overlays beneath the explicit qualification workspace root, with traversal permissions for system libvirt; use a dedicated, capacious XFS or Btrfs mount that libvirt can traverse. Cleanup targets only that run's domains and disks and never uses the libvirt default network or an existing domain. Each guest receives nested KVM, a fresh root disk, and a separate data disk. One guest exercises the bounded Btrfs image; the other formats and mounts the data disk as the explicit existing Btrfs filesystem.

Download the dated qualification image once. The preparation command refuses a different image digest:

```sh
qualification_image="$PWD/.tmp/installer-qualification/ubuntu-24.04-20260725-amd64.img"
scripts/prepare-installer-qualification-image.sh "$qualification_image"
```

After creating the non-publishable candidate, run the gate from the clean tagged checkout:

```sh
export SECONDBOX_REQUIRE_QUALIFIED_INSTALLER=1
export SECONDBOX_INSTALLER_RELEASE_DIRECTORY='/absolute/path/to/candidate'
export SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT='/srv/secondbox/qualification/installer-workspaces'
export SECONDBOX_INSTALLER_QUALIFICATION_IMAGE="$qualification_image"
export SECONDBOX_INSTALLER_QUALIFICATION_IMAGE_SHA256='d1940f7d69d343355e183dff1e08a59852d32e7309baa7a4bad8365b11b005ac'
just test-installer-qualified
```

Provision `SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT` on the dedicated XFS or Btrfs qualification mount before the run. The release operator must own it, and every ancestor must be traversable by the system libvirt QEMU account; do not place it below a private home directory.

The candidate-only installer path verifies the staged v5 manifest, every referenced release object, and the running deployment binary's embedded version and source commit. The guest then exposes the four staged OCI archives through a guest-local TLS registry under their exact manifest digest references. Public PostgreSQL and object-store images are pulled by digest before the registry override. This path is accepted only through the explicit `--candidate-directory` qualification argument; ordinary install and resume continue to fetch canonical HTTPS release objects and immutable public registry references.

See [guided single-host installation](guided-single-host-install.md) for the installed topology and authority boundary.
