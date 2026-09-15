# Release distribution

Release inputs may describe multiple backend materializations, but each is strict and immutable: backend kind, guest architecture, runtime and toolchain digests, local launch-artifact digests, agent protocol/features, and backend/helper build identity. Microsandbox entries also name the digest-pinned source OCI manifest and content-addressed flat root. The published release artifacts package the Firecracker `microvm` bundle and the gVisor `gvisor-artifacts` transport (flat root, `runsc`, guest agent, and materialization, built from the repository and its digest-pinned bases). Microsandbox materializations are not built or distributed through this release process and remain operator-local experimental inputs, assembled and digest-pinned from the operator's reviewed build as the Microsandbox operations guides describe. Runners advertise only materializations already present and revalidated locally.

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
| gVisor runner | `ghcr.io/secondstack-ai/secondbox/runner-gvisor:vVERSION` |
| gVisor artifacts | `ghcr.io/secondstack-ai/secondbox/gvisor-artifacts:vVERSION` |
| gVisor materialization | `secondbox-VERSION-gvisor-materialization.json` |
| gVisor qualification evidence | `secondbox-VERSION-gvisor-qualification-evidence.json`, `secondbox-VERSION-gvisor-pod-qualification-evidence.json` |
| CLI binary | `secondbox_VERSION_OS_ARCH` |
| Deployment binary | `secondbox-deploy_VERSION_OS_ARCH` |
| Guided-install bootstrap | versioned `releases/download/vVERSION/install.sh`; stable `releases/latest/download/install.sh` |

The release also includes checksums, the OpenAPI document, the Go module archive, the TypeScript package, an SPDX SBOM, standard resource bundles, KVM scenario and installer qualification evidence, package and OCI metadata, and an artifact manifest containing digest-pinned OCI references. Every generated identity record carries the same version and exact source commit; binaries embed both. The npm publication uses trusted-publisher provenance, while the signed microVM manifest attests its component hashes and provenance under the independently provisioned release key. There is no separate locally manufactured qualification attestation. `install.sh` embeds the exact release version and Linux amd64 deployment-binary digest. It downloads and verifies only that binary; all release and host verification remains in `secondbox-deploy install`.

## Supported platforms

`secondbox` and `secondbox-deploy` ship for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`. The guided installer, installer-tools image, Firecracker Runner, microVM artifacts, gVisor Runner, and gVisor artifacts support `linux/amd64`. The control-plane image supports `linux/amd64` and `linux/arm64`.

## Publishing

From clean `main` equal to fetched `origin/main` on the configured release host:

```sh
just release VERSION        # lean amd64 release
# Or: just release VERSION --full
# After a gate-only failure: just release VERSION --resume
```

The driver qualifies and builds concurrently, binds commit-exact scenario evidence
into an installer candidate, qualifies that candidate, then stages the final
release from the retained checksummed build. Lean releases require Firecracker
and gVisor host evidence; full releases also require no-KVM pod evidence and all
installer modes. Candidate manifests cannot be published.

Only after staging succeeds, execute the printed tag-push and
`just release-upload VERSION OUTPUT_DIR` commands in order. Upload reads
`docs/releases/vVERSION.md` from the tag when present, otherwise uses a placeholder.
An optional third `NOTES_FILE` argument supplies an explicit body. It appends the
install and SDK footer, sets the draft body on creation or retry, and dispatches
the publisher. Publication preserves that body and publishes the staged bytes
with npm provenance; GitHub Actions does not rebuild or qualify them.

See [release operator setup](release-operator-setup.md) for setup, the user-service
launch command, and the manual mechanics appendix for hosts without automation.
Use the [release skill](../../.agents/skills/secondbox-release/SKILL.md) for release
decisions, ownership checks, recovery and verification.
