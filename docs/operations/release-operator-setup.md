# Release operator setup

SecondBox releases are qualified and built locally, then published by one GitHub workflow. The local release flow stages a non-publishable installer candidate, qualifies that exact candidate in disposable virtual machines, and stages the final release from the resulting evidence.

## One-time setup

- Authenticate the local `gh` CLI for `SecondStack-AI/SecondBox`.
- Configure npm trusted publishing for `@secondstack-ai/secondbox` and `.github/workflows/release.yml`.
- Allow that workflow `contents: write`, `packages: write`, and `id-token: write`.
- Give the repository workflow access to the public `control-plane`, `runner`, `installer-tools`, and `microvm-artifacts` GHCR packages.

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

v0.7.0 through v0.8.1 carry the v0.6.0 Firecracker microVM bundle and trust anchor forward unchanged. Point `SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR` at the exact previously published signed bundle; do not rebuild it from the current experimental-backend guest sources. A different runtime or toolchain component-manifest digest makes the v1 guided updater reject these releases because existing Sandboxes remain pinned to their immutable Profile revisions. Microsandbox and gVisor use separate operator-local materializations that are not packaged by this release flow.

## Release

Tag the clean commit. On the qualified host, run the unfiltered scenario suite, stage the same commit, then upload the draft:

```sh
git tag v0.8.1
git push origin refs/tags/v0.8.1

export SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1
export SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR="$artifact_target"
export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY="$artifact_public_key"
export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256="$artifact_public_key_sha256"
export SECONDBOX_RUNNER_WORKSPACE_ROOT='/srv/secondbox/qualification/workspaces'
just test-scenario

export SECONDBOX_RELEASE_POSTGRES_IMAGE='docker.io/library/postgres@sha256:REVIEWED_DIGEST'
just release-candidate 0.8.1 /protected/releases/installer-candidate

export SECONDBOX_REQUIRE_QUALIFIED_INSTALLER=1
export SECONDBOX_INSTALLER_RELEASE_DIRECTORY=/protected/releases/installer-candidate
export SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT=/srv/secondbox/qualification/installer-workspaces
qualification_image=/protected/releases/images/ubuntu-24.04-installer-qualification.img
qualification_image_sha256="$(scripts/prepare-installer-qualification-image.sh "$qualification_image")"
export SECONDBOX_INSTALLER_QUALIFICATION_IMAGE="$qualification_image"
export SECONDBOX_INSTALLER_QUALIFICATION_IMAGE_SHA256="$qualification_image_sha256"
just test-installer-qualified

just release-stage 0.8.1 /protected/releases/secondbox-0.8.1
just release-upload 0.8.1 /protected/releases/secondbox-0.8.1
```

`test-scenario` writes `.tmp/scenario-qualification-evidence.json` only after the full suite and cleanup pass. Its `sourceCommit` must equal `HEAD`, so run it after the release pull request merges and before staging; do not reuse evidence from the review branch. `release-candidate` then builds an explicitly non-publishable manifest with the reviewed, digest-pinned bundled-service images and no installer-qualification claim. The repository-owned QEMU/libvirt driver tests that candidate and writes `.tmp/installer-qualification-evidence.json` after its clean-host, reboot, resume, uninstall, purge, and real-microVM assertions pass. The helper downloads the pinned Ubuntu qualification image only when the target path is absent and prints its reviewed SHA-256 for the explicit driver input; retain that image for subsequent releases or choose a new absent target after the repository pin changes. The candidate and final manifest share a qualification-subject digest: every final manifest field participates except the candidate marker and installer-evidence reference. `release-stage` requires both evidence documents, rejects evidence for different release bytes, and emits the publishable final manifest. `release-upload` creates a private draft and dispatches the GitHub workflow; the workflow does not rebuild or qualify anything.

Watch the dispatched run with:

```sh
gh run list --workflow release.yml --limit 1
gh run watch --exit-status
```

If publication fails, fix the cause and run `just release-upload` again while the release is still a draft. Never move a published tag; use a new patch version instead.
