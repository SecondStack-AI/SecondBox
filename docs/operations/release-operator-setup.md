# Release operator setup

The coordinated release workflow runs only for an immutable canonical SemVer tag. Protect release tags against update and deletion, require the normal CI checks on the tagged commit, and require approval for the GitHub `release` environment if the repository uses an environment gate.

Configure npm trusted publishing for `@secondstack-ai/secondbox` with this GitHub repository and `.github/workflows/release.yml`. Do not create an npm automation token. The workflow requests `id-token: write`, publishes the version under the intentionally non-stable `candidate` distribution tag with npm provenance, and moves no stable tag before source-free finalization.

The finalization workflow needs a narrowly scoped `NPM_DIST_TAG_TOKEN` because npm trusted publishing authorizes package publication but does not currently provide a separate trusted-publisher operation for moving an existing version's distribution tag. Scope it only to `@secondstack-ai/secondbox`, permit no version overwrite, and rotate it independently. It is never used to publish package bytes.

Grant the workflow `contents: write`, `packages: write`, `id-token: write`, and `attestations: write`. GHCR packages for `control-plane`, `runner`, and `microvm-artifacts` must be public and inherit repository Actions access. GitHub Releases must permit the workflow token to create a draft, upload assets, and expose it as a public prerelease. No publication PAT is used.

Register the dedicated KVM runner with `self-hosted`, `linux`, `x64`, and `secondbox-kvm`. It must have Docker Buildx, `skopeo`, `jq`, `openssl`, `curl`, `npm`, Go, `just`, KVM access, and the ordinary scenario prerequisites. Configure these repository variables:

- `SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR`: absolute directory containing exactly the signed release-bundle allowlist.
- `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY`: absolute path to the independently held public verification key.
- `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256`: lowercase SHA-256 fingerprint of that key's DER encoding.
- `SECONDBOX_RUNNER_WORKSPACE_ROOT`: qualified reflink-capable runner workspace root.
- `SECONDBOX_SOURCE_FREE_OPERATOR_MANIFEST`: complete protected production manifest with one qualified same-host Runner and explicit authority, storage, placement, capacity, gateway, host-path, and trust inputs.
- `SECONDBOX_SOURCE_FREE_URL`, `SECONDBOX_SOURCE_FREE_TENANT_REF`, and `SECONDBOX_SOURCE_FREE_SUBJECT_REF`: live application coordinates used by both public SDK lifecycle probes.

Store `SECONDBOX_SOURCE_FREE_TOKEN` as a repository secret with only the Profile and Sandbox scopes required by the lifecycle probes. The source-free job creates fresh absent work and artifact directories for every run and does not check out the repository.

The signing private key is never a repository secret or runner input. The candidate workflow only receives the already signed bundle and the separately configured public trust anchor.

Before tagging, run `just test-release-workflow` and the full preship gates. `scripts/release-publish-candidate.sh --dry-run VERSION STAGING_DIR CANDIDATE_EVIDENCE` validates the exact candidate/evidence binding and prints the incomplete publication state without contacting a registry. A retry is safe only when npm integrity, GHCR digest, and every existing GitHub asset are identical. Any mismatch requires a new version; never move the tag, reuse the npm version, overwrite an asset, or replace an OCI version tag.

After the workflow finishes, verify the GitHub prerelease is public, the npm version is only on the `candidate` tag, each GHCR version resolves to the digest in the artifact manifest, and no qualification attestation or final release index exists. The candidate remains non-consumable under the SecondBox contract until the source-free finalization workflow publishes the final index last.
