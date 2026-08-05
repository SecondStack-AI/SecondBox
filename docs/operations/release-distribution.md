# Release distribution

A SecondBox release is a SemVer Git tag plus the locally built files attached to its stable GitHub Release. GitHub Actions publishes those supplied files without rebuilding them.

## Public coordinates

| Artifact | Coordinate |
| --- | --- |
| TypeScript SDK | `@secondstack-ai/secondbox@VERSION` |
| Go module and SDK | `github.com/SecondStack-AI/SecondBox@vVERSION` |
| Control plane | `ghcr.io/secondstack-ai/secondbox/control-plane:vVERSION` |
| Runner | `ghcr.io/secondstack-ai/secondbox/runner:vVERSION` |
| microVM artifacts | `ghcr.io/secondstack-ai/secondbox/microvm-artifacts:vVERSION` |
| CLI binary | `secondbox_VERSION_OS_ARCH` |
| Deployment binary | `secondbox-deploy_VERSION_OS_ARCH` |

The release also includes checksums, the OpenAPI document, standard resource bundles, KVM scenario qualification evidence, and an artifact manifest containing digest-pinned OCI references.

## Supported platforms

`secondbox` and `secondbox-deploy` ship for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`. The control plane image supports `linux/amd64` and `linux/arm64`. The Firecracker runner and microVM artifacts support `linux/amd64`.

## Publishing

From a clean checkout at the release tag:

```sh
just test-scenario
just release-stage VERSION OUTPUT_DIR
just release-upload VERSION OUTPUT_DIR
```

Run the scenario suite on the qualified release host with the required variables from [external scenario qualification](scenario-qualification.md). A successful unfiltered run records evidence for the exact clean commit. `release-stage` refuses other evidence, then builds all files locally and includes the evidence in the manifest and `SHA256SUMS`. `release-upload` creates a private GitHub draft, uploads the output, and dispatches `.github/workflows/release.yml`. That workflow publishes the OCI archives to GHCR, publishes the TypeScript package to npm under `latest`, removes transport-only archives from the draft, and opens the stable GitHub release.

There is no candidate, attestation, or finalization phase. GitHub Actions does not rebuild or qualify the release; local staging is the only qualification-evidence gate.

See [release operator setup](release-operator-setup.md) for one-time permissions and the exact operator commands.
