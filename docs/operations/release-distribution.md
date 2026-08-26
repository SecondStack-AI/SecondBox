# Release distribution

Release inputs may describe multiple backend materializations, but each is strict and immutable: backend kind, guest architecture, runtime and toolchain digests, local launch-artifact digests, agent protocol/features, and backend/helper build identity. Microsandbox entries also name the digest-pinned source OCI manifest and content-addressed flat root. The published release artifacts themselves package only the Firecracker `microvm` bundle: Microsandbox materializations are not built or distributed through this release process and remain operator-local experimental inputs, assembled and digest-pinned from the operator's reviewed build as the Microsandbox operations guides describe. Publication does not make the experimental backend supported, and runners advertise only materializations already present and revalidated locally.

A SecondBox release is a SemVer Git tag plus the locally built files attached to its stable GitHub Release. GitHub Actions publishes those supplied files without rebuilding them.

## Public coordinates

| Artifact | Coordinate |
| --- | --- |
| TypeScript SDK | `@secondstack-ai/secondbox@VERSION` |
| Go module and SDK | `github.com/SecondStack-AI/SecondBox@vVERSION` |
| Control plane | `ghcr.io/secondstack-ai/secondbox/control-plane:vVERSION` |
| Runner | `ghcr.io/secondstack-ai/secondbox/runner:vVERSION` |
| Installer tools | `ghcr.io/secondstack-ai/secondbox/installer-tools:vVERSION` |
| microVM artifacts | `ghcr.io/secondstack-ai/secondbox/microvm-artifacts:vVERSION` |
| CLI binary | `secondbox_VERSION_OS_ARCH` |
| Deployment binary | `secondbox-deploy_VERSION_OS_ARCH` |
| Guided-install bootstrap | versioned `releases/download/vVERSION/install.sh`; stable `releases/latest/download/install.sh` |

The release also includes checksums, the OpenAPI document, the Go module archive, the TypeScript package, an SPDX SBOM, standard resource bundles, KVM scenario and installer qualification evidence, package and OCI metadata, and an artifact manifest containing digest-pinned OCI references. Every generated identity record carries the same version and exact source commit; binaries embed both. The npm publication uses trusted-publisher provenance, while the signed microVM manifest attests its component hashes and provenance under the independently provisioned release key. There is no separate locally manufactured qualification attestation. `install.sh` embeds the exact release version and Linux amd64 deployment-binary digest. It downloads and verifies only that binary; all release and host verification remains in `secondbox-deploy install`.

## Supported platforms

`secondbox` and `secondbox-deploy` ship for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`. The guided installer, installer-tools image, Firecracker Runner, and microVM artifacts support `linux/amd64`. The control-plane image supports `linux/amd64` and `linux/arm64`.

## Publishing

From a clean checkout at the release tag:

```sh
just test-scenario
just release-candidate VERSION CANDIDATE_OUTPUT_DIR
just test-installer-qualified
just release-stage VERSION OUTPUT_DIR
just release-upload VERSION OUTPUT_DIR
```

Run the scenario suite, build the non-publishable installer candidate, point `SECONDBOX_INSTALLER_RELEASE_DIRECTORY` at that candidate, and then run installer qualification on the qualified release host. The candidate contains the exact binaries, digest-pinned images, bundles, and protocol windows but no installer-evidence claim. Installer qualification records their shared qualification-subject digest. `release-stage` refuses absent, dirty, commit-mismatched, or release-mismatched evidence and emits the final publishable manifest; scenario evidence must name `HEAD` exactly. `release-publish` rejects candidate manifests.

Installer qualification also requires the repository's `scripts/installer-qualification-driver`, the explicitly pinned Ubuntu image and SHA-256 documented in [scenario qualification](scenario-qualification.md), and a dedicated existing XFS/Btrfs host directory. The driver performs all guest mutation inside uniquely named disposable libvirt resources and uses a candidate-only local release transport. It does not publish candidate images or weaken the normal installer's HTTPS and immutable-registry requirements.

The installer candidate is the only pre-final phase and cannot be published. There is no separate qualification-attestation or hosted finalization phase. GitHub Actions does not rebuild or qualify the release; it publishes the staged bytes and supplies npm provenance.

See [release operator setup](release-operator-setup.md) for one-time permissions and the exact operator commands.
