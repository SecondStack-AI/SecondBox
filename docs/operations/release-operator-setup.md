# Release operator setup

SecondBox releases are qualified and built locally, then published by one GitHub workflow. The local release flow stages a non-publishable installer candidate, qualifies that exact candidate in disposable virtual machines, and stages the final release from the resulting evidence.

## One-time setup

- Authenticate the local `gh` CLI for `SecondStack-AI/SecondBox`.
- Configure npm trusted publishing for `@secondstack-ai/secondbox` and `.github/workflows/release.yml`.
- Allow that workflow `contents: write`, `packages: write`, and `id-token: write`.
- Give the repository workflow access to the public `control-plane`, `runner`, `installer-tools`, `microvm-artifacts`, `runner-gvisor`, and `gvisor-artifacts` GHCR packages.

The local build host needs Docker Buildx, `jq`, `openssl`, npm, Node.js, Go, and `just`. It must also satisfy the KVM, TUN, reflink-filesystem, and scenario bundle requirements in [external scenario qualification](scenario-qualification.md). Release staging needs the microVM release bundle variables documented by `scripts/release-stage.sh` because the runner consumes that bundle at runtime.

## The microVM bundle and its trust anchor

A release publishes the signed microVM bundle the runner executes. The bundle is built by the [microVM image pipeline](microvm-image-pipeline.md) and signed with the release signing key; staging verifies it against an independently held public anchor before packaging it as the `microvm-artifacts` OCI archive, and the staged artifact manifest records the verified fingerprint as `microvm.signingKeyFingerprint`.

The signing key is RSA — `Config.ValidateMicroVMTrustAnchor` rejects any other algorithm. Keep the private half outside the repository, mode 0600, alongside the release operator's other material and never inside a release artifact, a staged candidate, or command output. Keep the public key and its canonical DER SHA-256 fingerprint next to it, and give staging the public half only:

```sh
export SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR=/absolute/path/to/signed/bundle
export SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY=/absolute/path/to/manifest-public.pem
export SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256="$(
  openssl pkey -pubin -in "$SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY" -outform DER |
    sha256sum | awk '{print $1}'
)"
```

Rebuilding the bundle rotates its identity. The guest agent, `/init`, and the microVM entrypoint live inside the signed rootfs, so any change to them requires a rebuilt, re-signed bundle, and a rebuild changes the runtime and toolchain component-manifest digests that every Profile pins. When a release rotates the bundle it therefore also rotates what operators must install:

- Operators install the new bundle through the runner init flow and pin the fingerprint the release's artifact manifest records. Never pin the `signing.pub` carried inside the bundle — that degrades the anchor to the fingerprint alone, which is exactly what the verifier refuses to rely on.
- A runner refuses any Assignment whose pinned component digests differ from its own locally verified manifest, so the bundle install, the signed asset catalog update, and the standard-resource apply are one coordinated step.
- Announce the rotation in the release notes, naming what requires the new bundle and what still works against an older verified one.

v0.3.0 rotated the anchor and the bundle: snapshot-resume needs a guest agent that supports template mode and the one-time assignment bind, and both live in the rootfs.

v0.7.0 through v0.10.1 carry the v0.6.0 Firecracker microVM bundle and trust anchor forward unchanged. Point `SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR` at the exact previously published signed bundle; do not rebuild it from other guest sources. A different runtime or toolchain component-manifest digest makes the v1 guided updater reject these releases because existing Sandboxes remain pinned to their immutable Profile revisions. The gVisor runner image and artifact transport are built by staging from the repository alone and need no operator input beyond Docker buildx; Microsandbox uses a separate operator-local materialization that is not packaged by this release flow.

v0.12.0 rotated the bundle and signing authority again; v0.12.0 through v0.14.0 use the [v0.12.0 identities and reinstall boundary](../releases/v0.12.0.md). The checked-in release example pins that bundle and the independently provisioned public key.

## Automated release

Copy `deploy/qualify.env.example` to `~/.config/secondbox/qualify.env` for PR
qualification, and `deploy/release.env.example` to
`~/.config/secondbox/release.env` for releases. Review every path and digest. The Go
suite gate provisions its own disposable PostgreSQL container unless
`SECONDBOX_TEST_DATABASE_URL` names a database you manage.
The release file includes every qualification key and is the sole configuration
used by `just release`; it does not recover inputs from an older release directory.
Run `npm ci --ignore-scripts` once in the checkout. Provision your own named
Buildx builder without changing the caller's selected builder, and set
`RELEASE_BUILDX_BUILDER` in the release file. This selection applies only to
artifact staging; it does not change the scenario harness's local Docker builds:

```sh
docker buildx create --name secondbox-suite-release --driver docker-container \
  --driver-opt image=moby/buildkit@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8
```

 Create the configured
`RELEASE_OUTPUT_ROOT` and `SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT` parents;
the latter must be on Btrfs or XFS and traversable by the system libvirt account.
The checked-in examples describe the reviewed release host using public paths;
review and provision every path for another host.

```sh
just qualify                  # PR gates and four independent Firecracker shards
just qualify --tier release   # gates, sharded Firecracker and local gVisor host
just nightly                  # full scenarios and no-KVM gVisor pod suite
# After merging, from clean main:
just release VERSION           # lean amd64 release
just release VERSION --full    # alternative: nightly matrix and arm64 images
just release VERSION --resume  # after a gate-only failure: reuse the retained build
```

Release preflight checks source, tag identity, inputs, the pinned Go/protoc and
`/usr/bin/just` toolchain, libvirt availability, absence of `sbq-` domains, and
headroom (200 GiB free in each output/workspace filesystem and enough available
memory for a guest plus 16 GiB). It creates a local tag only when absent and
never pushes it. Existing tags must identify HEAD. All three versioned output
directories must be absent; failed output is retained for diagnosis, so archive
or remove only your own failed run's directories before retrying. When a run
failed only in a gate after the build and every scenario stage passed,
`--resume` keeps the retained `VERSION-build` and its commit-exact evidence,
requalifies the gates alone, and continues from the candidate.

Qualification and the unbound artifact build run concurrently. Once both pass,
staging binds commit-exact Firecracker and gVisor evidence into the candidate.
The default lean release builds amd64 images and tests one `btrfs_image` guest:
the wizard, reboot recovery, and hello-world microVM. The driver removes the
disposable guest afterwards. `--full` uses nightly qualification, adds the arm64 control-plane
image, and runs all three installer modes, including existing-filesystem
uninstall/resume and the v0.7.2 refusal/recreation boundary. Each selected mode
must satisfy its own required assertions in `tests/installer/vm-scenario.json`.
Guests have separate MACs and loopback SSH forwards. The manifest records the
built image platforms; CLI and deploy binaries retain all four host platforms.
Local gVisor host inputs use `QUALIFY_GVISOR_HOST_BUILD_ROOT`; KVM presence is
allowed for host evidence. Pod evidence is required only by full releases.
`QUALIFY_GUEST_MEMORY_MIB` defaults to 16384; concurrency is at most three and is
capped by available memory after an 8 GiB host reserve. The final stage reuses
the checksummed build and requires installer evidence for exactly those release
bytes. Unbound builds cannot be published.

Each release stage writes a log under `.tmp/release/RUN/`; `timing.md` records
its result and wall clock. Qualification has its own `.tmp/qualify/RUN/` logs and
supports `just qualify --wait RUN`. On hosts that terminate commands when their
terminal disappears, launch the release in a user service:

```sh
mkdir -p .tmp
systemd-run --user --unit="secondbox-suite-release-VERSION-$(date +%s)" --collect \
  --property="WorkingDirectory=$PWD" \
  --property="StandardOutput=file:$PWD/.tmp/release-console.log" \
  --property=StandardError=inherit --setenv="PATH=$PATH" --setenv="HOME=$HOME" \
  --setenv="SECONDBOX_TEST_DATABASE_URL=${SECONDBOX_TEST_DATABASE_URL:-}" \
  /usr/bin/just release VERSION           # append --full or --resume as needed
```

Reserve the KVM host before starting, and the dedicated no-KVM VM for `--full`. Do not stop
another operator's units, domains, Compose stacks, or live deployment to make
room. The automation leaves the no-KVM VM running. Candidate and final outputs
are `RELEASE_OUTPUT_ROOT/VERSION-candidate` and `RELEASE_OUTPUT_ROOT/VERSION`;
`VERSION-build` retains the checksummed intermediate build. The successful
command prints the exact tag-push and `release-upload` commands for explicit
publication. Neither command is run automatically.

## Appendix: manual release on hosts without automation

Mechanics reference for exceptional operator-run hosts. The project release skill
uses `just release` and its documented recovery sequence, not this manual chain.


Tag the clean commit. On the qualified host, run the unfiltered scenario suite, stage the same commit, then upload the draft:

```sh
# Tag locally only; the tag is pushed right before the upload, once every
# qualification has passed, so a failed qualification never leaves an
# unusable public module tag.
git tag v0.10.1

export SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1
export SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR="$artifact_target"
export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY="$artifact_public_key"
export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256="$artifact_public_key_sha256"
export SECONDBOX_RUNNER_WORKSPACE_ROOT='/srv/secondbox/qualification/workspaces'
just test-scenario

# On the no-KVM qualification host (root, Docker with Compose and Buildx, a
# node-local k3s for the pod placement), at the same tag: assemble the build
# directory as gvisor-runtime.md describes (bin/runsc, bin/secondbox-guest-agent,
# rootfs), export the scenario inputs, run both suites, then copy both evidence
# files to the release host (defaults: .tmp/gvisor-linux-scenario-qualification-evidence.json
# and .tmp/gvisor-pod-linux-scenario-qualification-evidence.json).
export SECONDBOX_GVISOR_LINUX_BUILD=/absolute/path/to/build
export SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1
export SECONDBOX_RUNNER_WORKSPACE_ROOT=/absolute/path/on/reflink-fs/gvisor-scenario-workspaces
just test-scenario-gvisor
just test-scenario-gvisor-pod
export SECONDBOX_GVISOR_QUALIFICATION_EVIDENCE=/protected/releases/evidence/gvisor-linux-scenario-qualification-evidence.json
export SECONDBOX_GVISOR_POD_QUALIFICATION_EVIDENCE=/protected/releases/evidence/gvisor-pod-linux-scenario-qualification-evidence.json

export SECONDBOX_RELEASE_POSTGRES_IMAGE='docker.io/library/postgres@sha256:REVIEWED_DIGEST'
just release-candidate 0.10.1 /protected/releases/installer-candidate

export SECONDBOX_REQUIRE_QUALIFIED_INSTALLER=1
export SECONDBOX_INSTALLER_RELEASE_DIRECTORY=/protected/releases/installer-candidate
export SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT=/srv/secondbox/qualification/installer-workspaces
qualification_image=/protected/releases/images/ubuntu-24.04-installer-qualification.img
qualification_image_sha256="$(scripts/prepare-installer-qualification-image.sh "$qualification_image")"
export SECONDBOX_INSTALLER_QUALIFICATION_IMAGE="$qualification_image"
export SECONDBOX_INSTALLER_QUALIFICATION_IMAGE_SHA256="$qualification_image_sha256"
just test-installer-qualified

just release-stage 0.10.1 /protected/releases/secondbox-0.10.1
git push origin refs/tags/v0.10.1
just release-upload 0.10.1 /protected/releases/secondbox-0.10.1
```

`test-scenario` writes `.tmp/scenario-qualification-evidence.json` only after the full suite and cleanup pass. Its `sourceCommit` must equal `HEAD`, so run it after the release pull request merges and before staging; do not reuse evidence from the review branch. `release-candidate` then builds an explicitly non-publishable manifest with the reviewed, digest-pinned bundled-service images and no installer-qualification claim. The repository-owned QEMU/libvirt driver tests that candidate and writes `.tmp/installer-qualification-evidence.json` after its clean-host, reboot, resume, uninstall, purge, and real-microVM assertions pass. The helper downloads the pinned Ubuntu qualification image only when the target path is absent and prints its reviewed SHA-256 for the explicit driver input; retain that image for subsequent releases or choose a new absent target after the repository pin changes. The candidate and final manifest share a qualification-subject digest: every final manifest field participates except the candidate marker and installer-evidence reference. `release-stage` requires both evidence documents, rejects evidence for different release bytes, and emits the publishable final manifest. `release-upload` creates a draft with notes from `docs/releases/vVERSION.md` at
the tag (or an explicit third `NOTES_FILE` argument), appends the install/SDK
footer, and dispatches the GitHub workflow. Retries refresh the draft body;
the publisher preserves it; the workflow does not rebuild or qualify anything.

Watch the dispatched run with:

```sh
gh run list --workflow release.yml --limit 1
gh run watch --exit-status
```

If publication fails, fix the cause and run `just release-upload` again while the release is still a draft. Never move a published tag; use a new patch version instead.
