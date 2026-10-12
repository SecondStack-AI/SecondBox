# Release operator setup

A SecondBox release starts when a maintainer pushes a `vVERSION` tag on `main`.
The `release-tag.yml` workflow builds the architecture-neutral files once on a GitHub-hosted runner, then qualifies and stages the release on two self-hosted release runners in parallel, and the hosted `release.yml` workflow publishes the staged bytes.
The amd64 runner stages a non-publishable installer candidate, qualifies that exact candidate in disposable virtual machines, and stages the final release from the resulting evidence.
The arm64 runner qualifies the same commit and stages the arm64 artifact set.
Both sets carry the same shared files byte for byte.

## One-time setup

- Configure npm trusted publishing for `@secondstack-ai/secondbox` and `.github/workflows/release.yml`.
- Allow that workflow `contents: write`, `packages: write`, and `id-token: write`.
- Give the repository workflow access to the public `control-plane`, `runner`, `installer-tools`, `microvm-artifacts`, `runner-gvisor`, and `gvisor-artifacts` GHCR packages.
- Register the two release runners (below) in an organization runner group that admits only this repository, and only `release-tag.yml` from tag refs.
  The repository is public: a runner that accepts other workflows would run pull-request code from forks on a privileged KVM host.

The release runners need Docker Buildx, `jq`, `openssl`, npm, Go, `gh`, and `/usr/bin/just`.
They must also satisfy the KVM, TUN, reflink-filesystem, and scenario bundle requirements in [external scenario qualification](scenario-qualification.md).
Release staging needs the microVM release bundle variables documented by `scripts/release-stage.sh` because the runner consumes that bundle at runtime.

## The microVM bundle

A release publishes the microVM bundle the runner executes. The bundle is built by the [microVM image pipeline](microvm-image-pipeline.md); staging verifies it with `verify.sh` before packaging it as the `microvm-artifacts` OCI archive, and the staged artifact manifest records the bundle `manifest.json` digest as `microvm.bundleManifestDigest`. Give staging the bundle directory:

```sh
export SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR=/absolute/path/to/bundle
```

Rebuilding the bundle rotates its identity. The guest agent, `/init`, and the microVM entrypoint live inside the rootfs, so any change to them requires a rebuilt bundle, and a rebuild changes the runtime and toolchain component-manifest digests. Profiles do not name those digests, so existing Sandboxes start on the new bundle once Runners install it. When a release rotates the bundle, announce the rotation in the release notes, naming what requires the new bundle and what still works against an older verified one.

Consult the target release notes before choosing bundle inputs. Point `SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR` at the exact bundle named there; do not rebuild a published bundle from other guest sources. The gVisor runner image and artifact transport are built by staging from the repository alone and need no operator input beyond Docker buildx; Microsandbox uses a separate operator-local materialization that is not packaged by this release flow.

## Release runners

| Runner | Labels | Configuration |
|---|---|---|
| amd64 | `self-hosted`, `Linux`, `X64`, `secondbox-release-amd64` | `deploy/release.env.example` |
| arm64 | `self-hosted`, `Linux`, `ARM64`, `secondbox-release-arm64` | `deploy/release-arm64.env.example` |

Register exactly one runner per label: each upload job reads the output that the staging job of its architecture left on its host.
Each runner account copies its example to `~/.config/secondbox/release.env` (or names another file in `RELEASE_ENV_FILE` in the runner service environment), and every path and digest in it is reviewed for that host.
Place the runner work directory on the reflink filesystem that holds the scenario workspace root, because scenarios reflink from the checkout.
The runner account needs access to `/dev/kvm`, `/dev/net/tun`, Docker and, on amd64, `qemu:///system` with nested KVM.
The Go suite gate provisions its own disposable PostgreSQL container unless `SECONDBOX_TEST_DATABASE_URL` names a database you manage.
Provision a named Buildx builder without changing the account's selected builder, and set `RELEASE_BUILDX_BUILDER`.
This selection applies only to artifact staging; it does not change the scenario harness's local Docker builds:

```sh
docker buildx create --name secondbox-suite-release --driver docker-container \
  --driver-opt image=moby/buildkit@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8
```

Create the configured `RELEASE_OUTPUT_ROOT` and, on amd64, `SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT` parents.
The installer workspace parent must be on Btrfs or XFS and traversable by the system libvirt account.
A release runner runs nothing else: release preflight requires its disk and memory headroom, refuses leftover `sbq-` libvirt domains, and a job holds the host for the whole stage.

## Cutting a release

Land the release notes and the changelog cut on `main` first (see the [release skill](../../.agents/skills/secondbox-release/SKILL.md)), then tag that merged commit and push the tag:

```sh
git fetch origin
git tag vVERSION origin/main
git push origin vVERSION
```

`release-tag.yml` then runs five jobs:

1. `build-shared` runs `scripts/release-build-shared.sh` on a GitHub-hosted runner, in a few minutes.
   It requires the tag to identify the checked-out commit on `origin/main` and the Go version of `go.mod`, then builds the OpenAPI document, Go module archive, TypeScript package, package metadata, SBOM, the `secondbox` and `secondbox-deploy` binaries of the four host platforms, and `install.sh`, which embeds the Linux amd64 deploy-binary digest.
   `shared-artifacts.json` records each file's digest, the version, tag and source commit, and the bundled PostgreSQL image pinned in that script.
   The job passes the directory to both staging jobs as a tar workflow artifact, which keeps the binaries' executable modes.
2. `stage-amd64` runs `just release VERSION SHARED_DIR` (`scripts/release.sh`).
   It requires the tag to identify the checked-out commit on `origin/main`, checks the toolchain, inputs and headroom, verifies the shared set, then runs qualification, the unbound artifact build, the installer candidate, installer qualification, and final staging, sequentially so their memory demands do not overlap.
   The installer candidate contains the shared deploy binary and `install.sh`, so installer qualification covers the bytes that are published.
3. `stage-arm64` runs `just release-arm64 VERSION SHARED_DIR` (`scripts/release-arm64.sh`) at the same time: `just test-scenario` for `linux-arm64` evidence, then the arm64 set.
4. `upload-arm64` uploads the arm64 set to the draft release after both staging jobs succeed.
5. `upload-amd64` then uploads the amd64 set from the amd64 runner, which dispatches the hosted publisher, and removes the intermediate build and candidate directories.

Each staging accepts the shared set only when `secondbox-release-tool verify-shared-artifacts` confirms that the directory holds exactly the shared files of this version with their modes, that each digest matches the record, that the record names this version and source commit, and that `install.sh` embeds the shared Linux amd64 deploy binary digest.
Both artifact manifests therefore pin the same shared files, and the publisher refuses sets that differ.

A tag such as `v0.0.0-dryrun.1` on `main` runs the shared build and both staging jobs and uploads nothing; use it to validate the runners, then delete the tag and the runners' outputs of that version.

Lean releases build amd64 images and test one `btrfs_image` installer guest: the wizard, reboot recovery, and hello-world microVMs.
Installer qualification installs the candidate with `--sandbox-profile agent-compartment --control-reserve-vcpus 1 --control-reserve-memory-mib 1024`, so its guest needs 2 GiB of usable RAM; `QUALIFY_GUEST_MEMORY_MIB` defaults to 4096 (minimum 2560, for kernel reservations); the larger default keeps page cache for the smoke's first cold preparation of the release image, which took 5m50s to 6m13s at 4096 MiB and over 8 minutes at 3072 MiB on a 4 vCPU nested guest.
Preflight requires 100 GiB free in each output and workspace filesystem and available memory for the guest plus a 2 GiB host reserve.
On smaller hosts, set `QUALIFY_GATES_FIRST=1` and `QUALIFY_MAX_STACKS=1` in `release.env` to limit scenario concurrency without omitting scenarios.
The final stage reuses the checksummed build and requires installer evidence for exactly those release bytes; unbound builds cannot be published.

Outputs are `RELEASE_OUTPUT_ROOT/VERSION`, `VERSION-build`, `VERSION-candidate` and, on arm64, `VERSION-arm64`.
Each staging job first removes the outputs of an earlier attempt of the same version, so a failed job is recovered with **Re-run failed jobs**.
Stage logs (`.tmp/release/RUN/` with `timing.md`, and `.tmp/qualify/RUN/`) are copied to `RELEASE_OUTPUT_ROOT/logs/` on the runner host after every attempt.
They are not uploaded as workflow artifacts, because the repository is public.

## Publication

Upload accepts one staged directory holding either the amd64 or the arm64 manifest and adds its files to the draft.
The amd64 upload dispatches the publisher; the publisher requires complete sets from one commit.
Upload reads `docs/releases/vVERSION.md` from the tag when present, otherwise uses a placeholder.
It appends the install and SDK footer and refreshes the draft on retry; the publisher preserves that body.

A pushed tag is immutable even when its release fails: the Go module proxy can cache the tagged source as soon as anyone requests it.
A code fix after a failed staging therefore lands on `main` and ships as the next patch version; retract the failed version in `go.mod` and note it in the changelog.
Validate a risky release with a dry-run tag first.
If publication fails while the release is still a draft, fix the cause and re-run `upload-arm64`, which also re-runs `upload-amd64` after it, because the publisher may already have deleted archives of either set.
Never move a published tag.
