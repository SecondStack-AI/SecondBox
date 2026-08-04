# Release operator setup

SecondBox builds and qualifies release candidates on a local Linux x86-64 host with KVM. GitHub Actions only publishes the exact locally supplied bytes from a private draft Release. It never builds product artifacts and needs no self-hosted Runner, repository deployment variables, or source-free authority secrets.

## One-time GitHub and npm setup

Protect canonical release tags against update and deletion. Permit `.github/workflows/release.yml` to request `contents: write`, `packages: write`, `id-token: write`, `attestations: write`, and `checks: read`. Configure npm trusted publishing for `@secondstack-ai/secondbox` with this repository and that workflow file. Package bytes are published under the `candidate` distribution tag with npm provenance.

GHCR packages for `control-plane`, `runner`, and `microvm-artifacts` must inherit repository Actions access and be public once published. GitHub Releases must permit the workflow token to upload assets and change a draft into a public prerelease. The workflow uses no publication PAT.

The local operator needs authenticated `gh` and npm CLI sessions. The npm session is used only after qualification to move the already published version from `candidate` to `latest`; it never overwrites package bytes.

## Qualified local host

The preparation host needs Docker Buildx, `jq`, `openssl`, `curl`, npm, Node.js, Go, `just`, writable `/dev/kvm` and TUN, cgroup v2, and an XFS or Btrfs reflink-capable workspace root on the same filesystem as the checkout and signed artifact directory. Configure the normal scenario inputs described in [scenario qualification](scenario-qualification.md), plus:

- `SECONDBOX_TEST_DATABASE_URL`: a disposable PostgreSQL database used by the full non-KVM suite.
- `SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR`: an absolute directory containing exactly the signed release-bundle allowlist.
- `SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY`: the independently held public verification key.
- `SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256`: the lowercase SHA-256 fingerprint of that key's DER encoding.

The guest signing private key is never a repository secret, GitHub input, or release-host input. Preparation consumes only the already signed bundle and the public trust anchor.

## Prepare and upload private transport

Merge the release changes and wait for all required checks on the final `main` commit. Create and push the immutable tag only after that commit is final:

```sh
git tag v0.1.0
git push origin refs/tags/v0.1.0
```

From a clean checkout at that exact tag, prepare the candidate and pre-publication KVM evidence:

```sh
just release-local-prepare 0.1.0 /protected/releases/secondbox-0.1.0
scripts/release-local-upload.sh --dry-run 0.1.0 /protected/releases/secondbox-0.1.0
just release-local-upload 0.1.0 /protected/releases/secondbox-0.1.0
```

Preparation runs the full non-KVM suite, release staging tests, release workflow tests, and the qualified scenario before staging the real candidate. The output contains `candidate/`, exact-commit KVM evidence, and a publication-input manifest binding every filename, role, size, and SHA-256 digest. Upload creates or reuses only a private draft Release and accepts existing assets only when byte-identical. The publication-input manifest is uploaded last.

## Publish the incomplete candidate

Dispatch the hosted publisher and monitor it to completion:

```sh
gh workflow run release.yml --ref main -f version=0.1.0
gh run watch --exit-status
```

The workflow checks the immutable tag and required CI results, verifies the complete draft transport, publishes the supplied OCI archives and npm tarball without rebuilding, uploads the public assets, records GitHub provenance, removes transport-only assets, and exposes an incomplete public prerelease. Verify that npm has only the `candidate` tag, every GHCR digest matches the artifact manifest, and no qualification attestation or final release index exists.

GitHub native release immutability must be disabled for this repository. The coordinated release protocol intentionally publishes an incomplete prerelease, removes private transport assets, and later adds source-free qualification plus the final release index. GitHub's native setting freezes the prerelease at the first public transition and prevents those required later phases. SecondBox instead enforces immutability through the exact Git tag, write-once npm version, digest-addressed OCI objects, staged checksums, byte-identical retry checks, and the final release index.

## Qualify and finalize locally

Keep the reviewed production operator manifest outside the checkout. Export the live application coordinates used by the SDK probes:

```sh
export SECONDBOX_URL=https://secondbox.example
export SECONDBOX_TOKEN=...
export SECONDBOX_TENANT_REF=...
export SECONDBOX_SUBJECT_REF=...

just release-local-qualify \
  0.1.0 \
  /protected/secondbox/operator.toml \
  /protected/releases/secondbox-0.1.0-source-free
```

The qualifier downloads the suite from the public prerelease, verifies its checksum, and runs it from a fresh directory outside the checkout. It deploys only public digest-pinned images and binaries, reapplies standard resources, installs both public SDKs, executes live Sandbox lifecycles, and writes the qualification attestation.

After `npm whoami` confirms the intended operator account, finalize:

```sh
just release-local-finalize \
  0.1.0 \
  /protected/releases/secondbox-0.1.0-source-free/secondbox-0.1.0-qualification-attestation.json
```

Finalization downloads the released deployment binary, uploads qualification first and the acyclic final index last, verifies both publicly, promotes the GitHub prerelease, and moves npm `latest`. An interrupted retry accepts only byte-identical public objects. Any conflicting tag, npm version, OCI coordinate, draft asset, qualification, or index requires a new version; never move the tag or overwrite release content.
