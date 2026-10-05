# Release distribution

Release inputs may describe multiple backend materializations, but each is strict and immutable: backend kind, guest architecture, runtime and toolchain digests, local launch-artifact digests, agent protocol/features, and backend/helper build identity. Microsandbox entries also name the digest-pinned source OCI manifest and content-addressed flat root. The published release artifacts package the Firecracker `microvm` bundle and the gVisor `gvisor-artifacts` transport (flat root, `runsc`, guest agent, and materialization, built from the repository and its digest-pinned bases). Microsandbox materializations are not built or distributed through this release process and remain operator-local experimental inputs, assembled and digest-pinned from the operator's reviewed build as the Microsandbox operations guides describe. Runners advertise only materializations already present and revalidated locally.

A SecondBox release is a SemVer Git tag plus the locally built files attached to its stable GitHub Release. GitHub Actions publishes those supplied files without rebuilding them.

Each release carries the amd64 artifact set under its tag, and optionally an arm64 set. The arm64 set has its own artifact manifest, qualification evidence, standard bundles and images, and reuses the architecture-neutral files of the amd64 set (SDKs, CLI and deploy binaries, OpenAPI document, SBOM, install bootstrap) byte for byte. Its own files carry an `-arm64` suffix, and its images an `-arm64` tag suffix.

## Public coordinates

| Artifact | Coordinate |
| --- | --- |
| TypeScript SDK | `@secondstack-ai/secondbox@VERSION` |
| Go module and SDK | `github.com/SecondStack-AI/SecondBox@vVERSION` |
| Control plane | `ghcr.io/secondstack-ai/secondbox/control-plane:vVERSION`; arm64 set `:vVERSION-arm64` |
| Runner | `ghcr.io/secondstack-ai/secondbox/runner:vVERSION`; arm64 set `:vVERSION-arm64` |
| Installer tools | `ghcr.io/secondstack-ai/secondbox/installer-tools:vVERSION`; arm64 set `:vVERSION-arm64` |
| microVM artifacts | `ghcr.io/secondstack-ai/secondbox/microvm-artifacts:vVERSION`; arm64 set `:vVERSION-arm64` |
| gVisor runner | `ghcr.io/secondstack-ai/secondbox/runner-gvisor:vVERSION` |
| gVisor artifacts | `ghcr.io/secondstack-ai/secondbox/gvisor-artifacts:vVERSION` |
| gVisor materialization | `secondbox-VERSION-gvisor-materialization.json` |
| gVisor qualification evidence | `secondbox-VERSION-gvisor-qualification-evidence.json`; full releases also carry `secondbox-VERSION-gvisor-pod-qualification-evidence.json` |
| CLI binary | `secondbox_VERSION_OS_ARCH` |
| Deployment binary | `secondbox-deploy_VERSION_OS_ARCH` |
| Guided-install bootstrap | versioned `releases/download/vVERSION/install.sh`; stable `releases/latest/download/install.sh` |

The release also includes checksums, the OpenAPI document, the Go module archive, the TypeScript package, an SPDX SBOM, standard resource bundles, KVM scenario and installer qualification evidence, package and OCI metadata, and an artifact manifest containing digest-pinned OCI references. Every generated identity record carries the same version and exact source commit; binaries embed both. The npm publication uses trusted-publisher provenance, while the signed microVM manifest attests its component hashes and provenance under the independently provisioned release key. There is no separate locally manufactured qualification attestation. `install.sh` embeds the exact release version and Linux amd64 deployment-binary digest. It downloads and verifies only that binary; all release and host verification remains in `secondbox-deploy install`.

## Supported platforms

`secondbox` and `secondbox-deploy` ship for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`. The guided installer, gVisor Runner, and gVisor artifacts support `linux/amd64` only. Each artifact set targets one guest architecture: its Firecracker Runner, installer-tools image, microVM artifacts, and standard Profiles are all `linux/amd64` or all `linux/arm64`, and its manifest names the set:

| Guest architecture | Artifact manifest | Qualification |
| --- | --- | --- |
| amd64 | `secondbox-VERSION-artifact-manifest.json` | `just test-scenario` on a `linux-amd64` KVM host, gVisor host evidence, installer evidence |
| arm64 | `secondbox-VERSION-arm64-artifact-manifest.json` | `just test-scenario` on a `linux-arm64` KVM host only |

The arm64 set carries no gVisor artifacts and no installer qualification evidence, because the guided installer installs amd64 releases only. Release verification enforces both rules, and `secondbox-deploy install` and `update` refuse an arm64 set before planning. Its microVM bundle is signed by the trust anchor of the amd64 set. The amd64 set builds the control-plane image for `linux/amd64` (`just release VERSION --full` also builds `linux/arm64`); the arm64 set builds its own `linux/arm64` control-plane image. Read the selected manifest for its actual image platform matrix.

## Qualification and publication

Start from a clean checkout of `origin/main`, with the reviewed release-host
configuration described in [release operator setup](release-operator-setup.md):

```sh
just release VERSION
# Or select the full matrix for this release:
just release VERSION --full
# After a gate-only failure with a successful build and scenarios:
just release VERSION --resume
```

Choose the default or full tier for a new version; use `--resume` only to recover
a gate-only failure with the retained build and commit-exact scenario evidence. The flow creates the local tag,
qualifies the source, builds the artifacts, stages a non-publishable installer
candidate, qualifies those bytes in disposable guests, and stages the final
amd64 manifest. Optionally, on a Linux arm64 KVM host, `just release-arm64 VERSION AMD64_RELEASE_DIR`
then qualifies the same commit with `just test-scenario` and stages the arm64 set
beside a copy of the final amd64 release. Publication remains an explicit
continuation printed by the successful flows; do not independently tag or upload
an unqualified build.

The default tier qualifies Firecracker and local gVisor, builds amd64 images,
and runs the Btrfs-image installer guest. The full tier adds the nightly scenario
matrix, no-KVM gVisor pod evidence, the arm64 control-plane image, and all three
installer modes. A default release does not claim the full tier's coverage.

Candidate and final staging require evidence for the selected tier and exact
source commit. Installer evidence binds the candidate's qualification-subject
digest to the final manifest. Staging rejects absent or mismatched evidence;
the publisher rejects candidate manifests, incomplete sets, and sets staged
from different commits. GitHub Actions publishes the staged
bytes and supplies npm provenance without rebuilding or qualifying them.

Upload reads `docs/releases/vVERSION.md` from the tag when present, otherwise
uses a placeholder. An optional third `NOTES_FILE` argument supplies the body.
It appends the install and SDK footer, sets the draft body on creation or retry,
and dispatches the publisher, which preserves that body.

Use the [release skill](../../.agents/skills/secondbox-release/SKILL.md) for
release decisions, recovery, and publication verification.

See [scenario qualification](scenario-qualification.md) for the host contracts
and [release operator setup](release-operator-setup.md) for logs, failed-run
handling, and publication.
