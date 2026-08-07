# Release operator setup

SecondBox releases are qualified and built locally, then published by one GitHub workflow. There is no candidate, attestation, or finalization phase.

## One-time setup

- Authenticate the local `gh` CLI for `SecondStack-AI/SecondBox`.
- Configure npm trusted publishing for `@secondstack-ai/secondbox` and `.github/workflows/release.yml`.
- Allow that workflow `contents: write`, `packages: write`, and `id-token: write`.
- Give the repository workflow access to the public `control-plane`, `runner`, and `microvm-artifacts` GHCR packages.

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

## Release

Tag the clean commit. On the qualified host, run the unfiltered scenario suite, stage the same commit, then upload the draft:

```sh
git tag v0.1.5
git push origin refs/tags/v0.1.5

export SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1
export SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR="$artifact_target"
export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY="$artifact_public_key"
export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256="$artifact_public_key_sha256"
export SECONDBOX_RUNNER_WORKSPACE_ROOT='/srv/secondbox/qualification/workspaces'
just test-scenario

just release-stage 0.1.5 /protected/releases/secondbox-0.1.5
just release-upload 0.1.5 /protected/releases/secondbox-0.1.5
```

`test-scenario` writes `.tmp/scenario-qualification-evidence.json` only after the full suite and cleanup pass. `release-stage` requires that evidence to name its exact source commit and a clean repository, then records it in the staged manifest and checksums. `release-upload` creates a private draft, uploads the local output, and dispatches the GitHub workflow. The workflow does not rebuild or qualify anything. It publishes the three OCI archives to GHCR, publishes the TypeScript package directly under npm `latest`, removes the transport-only archives from the draft, and opens a stable GitHub release marked latest.

Watch the dispatched run with:

```sh
gh run list --workflow release.yml --limit 1
gh run watch --exit-status
```

If publication fails, fix the cause and run `just release-upload` again while the release is still a draft. Never move a published tag; use a new patch version instead.
