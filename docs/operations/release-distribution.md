# Coordinated release distribution

A SecondBox release is one immutable Git tag and one source commit carried by
every SDK, binary, image, protocol declaration, standard-resource document,
SBOM, attestation, and release record. A tag or a partially populated GitHub
Release is not release authority. Consumers recognize a release only after its
final release index is public and independently verifies.

## Version and coordinates

Release tags use `vMAJOR.MINOR.PATCH` or a SemVer prerelease suffix such as
`v1.2.3-rc.1`. Leading zeroes and build metadata are refused so npm, Go, OCI,
and filenames retain exactly one spelling. The version embedded in artifacts is
the tag without `v`; the source identity is the full lowercase Git object ID.

The v1 public coordinates are:

| Artifact | Coordinate |
| --- | --- |
| TypeScript SDK | `@secondstack-ai/secondbox@VERSION` |
| Go module and SDK | `github.com/SecondStack-AI/SecondBox@vVERSION` |
| Control plane | `ghcr.io/secondstack-ai/secondbox/control-plane@sha256:DIGEST` |
| Runner | `ghcr.io/secondstack-ai/secondbox/runner@sha256:DIGEST` |
| microVM artifacts | `ghcr.io/secondstack-ai/secondbox/microvm-artifacts@sha256:DIGEST` |
| CLI binary | `secondbox_VERSION_OS_ARCH` |
| Deployment binary | `secondbox-deploy_VERSION_OS_ARCH` |
| Artifact manifest | `secondbox-VERSION-artifact-manifest.json` |
| Qualification | `secondbox-VERSION-qualification-attestation.json` |
| Final index | `secondbox-VERSION-release-index.json` |

The final-index and qualification schemas live in [`contracts/release/v1`](../../contracts/release/v1); the artifact manifest uses [`contracts/release/v2`](../../contracts/release/v2) so it carries the distinct signed runtime and toolchain component identities required by Runner admission.
The reusable strict decoder and cross-document verifier live in
`pkg/releasecontract`. Unknown JSON fields and trailing values are rejected.

## Supported and qualified platforms

The first coordinated release builds `secondbox` and `secondbox-deploy` for
`linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`. The unprivileged
control-plane image supports `linux/amd64` and `linux/arm64`.

The Firecracker Runner and signed guest are released only for `linux/amd64`.
That exact pair requires source-free KVM qualification. A later architecture is
not added to either artifact until that architecture has its own passing
qualification evidence and architecture-qualified standard Profiles. The
artifact-manifest validator rejects every advertised Runner or guest platform
that is absent from `qualifiedRunnerGuest`.

## Authority chain

Publication deliberately forms an acyclic chain:

1. The artifact manifest identifies all immutable release objects, the public
   OpenAPI digest, protocol windows, platform matrix, standard bundle lineage,
   signed guest identity, its distinct runtime and toolchain component records,
   and exact source-free qualification-suite bytes. It
   contains no source-free qualification claim.
2. The qualification attestation identifies the exact artifact-manifest URL and
   digest and records the source-free suite, selected protocols, signed guest,
   architecture, Runner image and environment, and a passing result. It does
   not refer to the final index.
3. The final release index repeats the coordinated release identity and refers
   to the exact artifact manifest and qualification attestation by public HTTPS
   location and SHA-256 digest. It has no evidence or self-digest field.

The final index is uploaded last. It may be published only after every object
named by the artifact manifest can be read through its public coordinate, all
digests match, and the qualification attestation passes independent validation.
Before that point the public candidate remains an incomplete prerelease and is
not accepted by the normal deployment verifier.

## Compatibility and retries

Runner protocol and guest protocol windows are explicit in the artifact
manifest. Qualification records the exact selected values. The v1 contract has
no cross-release compatibility-evidence collection, so the control plane,
Runner, and guest must all report the manifest's exact tag and source commit.
Any mixed identity fails. A future schema may add evidence without weakening
this v1 behavior.

Publishing is retry-safe but never mutable. If an npm version, OCI digest,
GitHub asset name, or other immutable coordinate already exists, a retry may
continue only after the public bytes or digest are proved identical to the
staged object. Different content aborts the release. Tags are never moved,
versions are never reused, and floating OCI tags are not consumer authority.

## Independent verification

An independent verifier starts from the final release-index URL, without a
source checkout:

1. Download the final index and strictly validate its schema, release identity,
   HTTPS locations, and canonical SHA-256 digests.
2. Download the referenced artifact manifest and qualification attestation and
   hash the exact response bodies. Match both hashes to the final index.
3. Match version, tag, and full source commit across all three documents.
4. Match the qualification's artifact-manifest reference, signed guest identity,
   Runner image, architecture, and selected protocol values to the manifest.
5. Fetch every SDK, binary, SBOM, attestation, bundle document, and OCI manifest
   named by the artifact manifest. Verify checksums or digest-pinned references
   and reject redirects to mutable authority.
6. Verify artifact attestations with GitHub workload identity and verify the
   signed microVM manifest with the independently configured trust anchor.

Successful verification establishes release completeness. Failure or absence
of any object leaves the candidate incomplete; it never authorizes a fallback
to a checkout, local build, copied resource specification, mutable image tag,
or different release identity.

## Deployment verification and initialization

Normal production initialization starts from the final index:

```text
secondbox-deploy verify release-index https://github.com/SecondStack-AI/SecondBox/releases/download/vVERSION/secondbox-VERSION-release-index.json
secondbox-deploy init --mode production --input operator.toml --release-index URL /srv/secondbox/deployment
```

The verifier downloads the exact manifest and qualification bytes, checks their digests and coordinated identity, checks protocol and signed-guest compatibility, verifies every referenced release asset, and validates each standard-bundle document against its recorded Profile lineage. Each standard Profile names the distinct runtime and toolchain component-manifest digests bound by the signed top-level microVM manifest. Initialization copies the verified artifact manifest and replaces only release-owned software facts: digest-pinned control-plane and Runner images, Runner software version, and the standard-resource artifact-manifest reference. Database, object storage, authorities, secret files, trust anchors, Runner placement, host paths, gateways, capacity, and retention remain exactly as supplied by the operator manifest.

Before finalization, only the dedicated qualification job may use the explicit candidate form:

```text
secondbox-deploy verify artifact-manifest ARTIFACT_MANIFEST_URL
secondbox-deploy init --mode production --input operator.toml --qualification-artifact-manifest ARTIFACT_MANIFEST_URL TARGET
```

That form proves artifact integrity but reports `qualified: false`; it is not accepted as normal release authority. The public `secondbox-VERSION-source-free-qualify` asset drives the KVM gate without a checkout. It downloads the released deployment binary, pulls all three digest-pinned images, initializes a clean operator topology, applies and reapplies standard resources, installs both SDKs from public coordinates, and runs live durable Sandbox lifecycles. Every required input is mandatory and absence fails the gate.

The local finalization command preserves the resulting qualification attestation, constructs the acyclic index with the released deployment binary, publishes the qualification, and publishes the index as the last release asset. Only after the public index verifies does it promote the GitHub prerelease and npm `latest` distribution tag. Candidate bytes are built and KVM-qualified locally; the GitHub-hosted publisher verifies and promotes those supplied bytes without rebuilding them.
