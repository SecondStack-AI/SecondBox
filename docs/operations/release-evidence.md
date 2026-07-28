# Release evidence and publication gate

The `SecondBox Release Evidence` workflow is manually dispatched for one candidate version and commit. It has read-only repository permissions and contains no release, package-registry, container-registry, signing-identity, tag, or push operation. Its output is an Actions artifact for review, not a release.

The workflow first proves clean-clone isolation and runs the non-KVM matrix. The isolation gate refuses a dirty source tree, clones the exact commit without local object sharing, uses isolated Go caches, installs the npm lockfile, verifies generated contracts, and builds the binaries. This prevents a local SecondStack checkout or uncommitted file from silently satisfying a release check.

The evidence job packages the exact commit, emits the available SBOM, vulnerability, dependency-age, license, checksum, signature, and provenance records, copies the canonical compatibility manifest, and records unavailable qualification dimensions as `blocked`. It never converts missing evidence into a skip or a passing empty record. A manual dispatch may name a same-repository `qualification-run-id`; the workflow downloads only the fixed `release-qualification-<commit>` artifact from that run. Omitting the run leaves the qualification gates blocked.

`release-subjects.json` is the 13-subject candidate byte inventory. It contains exactly the Linux release package, six standalone binaries, the control-plane and Runner images, the signed guest execution bundle, its GHCR artifact-transport image, and the Go and TypeScript SDK packages. Local subjects carry an evidence-relative path, size, and SHA-256. OCI subjects carry a digest-pinned registry locator only after `docker buildx imagetools inspect` resolves that exact reference. `guest-artifact-image` also carries a binding to the exact `guest-execution-bundle` digest; it cannot pass when either its own registry digest or that signed bundle is unavailable. A missing or unverified subject remains `blocked`.

Every supply-chain report is a subject-coverage document. `scripts/verify-release-supply-chain-coverage.mjs` rejects duplicate or omitted subjects, subject-digest drift, unsafe artifact paths, symbolic links, missing artifacts, and artifact checksum mismatches. An aggregate `passed` status is invalid unless the exact subject manifest and every per-subject record pass.

## Evidence contract

`release/evidence-schema.json` requires these artifact-backed gates:

- clean-clone and non-KVM qualification;
- packaged KVM qualification;
- multi-runner qualification;
- durability and restore qualification;
- complete data-plane qualification;
- network-policy qualification;
- security-boundary qualification;
- compatibility qualification;
- SBOMs and vulnerability reports for every exact image, binary, guest bundle, and SDK package;
- registry-backed dependency age and license evidence;
- subject checksums, a trusted release-key signature over the complete subject manifest, and per-subject SLSA-format provenance.

KVM, multi-runner, durability, data-plane, network, and security evidence uses `release/qualification-record-schema.json`. Every passing record identifies its candidate version and commit, hashes the exact `release-subjects.json` bytes, repeats every subject ID, kind, locator, and digest from that manifest, identifies the qualified hosts and deployment modes, and provides checksum-bound evidence for every scenario required by `release/qualification-requirements.json`. The qualification verifier dynamically requires the manifest's complete subject set, including any subject added to the release contract.

`scripts/import-release-qualification-evidence.mjs` verifies each supplied record before copying it into the aggregate evidence directory. It rejects symbolic links, files outside `qualification/`, unreferenced payloads, overwrites, invalid records, subject drift, and artifact drift. A partial valid import is allowed so the aggregate can represent the remaining gates as blocked; it does not turn absent records into passing gates.

Every artifact reference is relative to the evidence directory and carries a SHA-256 digest. `scripts/verify-release-publication-eligibility.sh` validates the document against the JSON Schema, rejects absolute paths, traversal, symbolic links, boundary escapes, missing files, checksum mismatches, malformed compatibility JSON, non-passing statuses, empty passing records, and invalid OpenSSL signatures. It independently re-runs the qualification verifier for every passing structured gate rather than trusting the aggregate status.

The signature generator writes no signature when `SECONDBOX_RELEASE_SIGNING_PRIVATE_KEY` is absent. An authorized release process must also set `SECONDBOX_RELEASE_TRUSTED_PUBLIC_KEY_SHA256` to the independently approved SHA-256 of the exact derived PEM public-key file. The generator and publication verifier compare that trust anchor before accepting the OpenSSL signature over `release-subjects.json`.

Guest packaging has an independent trust boundary. `scripts/package-release-guest-assets.sh` verifies the raw Firecracker bundle with `SECONDBOX_RELEASE_GUEST_TRUSTED_PUBLIC_KEY` and its DER SHA-256 fingerprint, requires kernel and rootfs provenance to identify the candidate commit, and creates the deterministic guest archive. An ad-hoc qualification key is not a release trust anchor.

The local SLSA statement covers binaries and SDK/package outputs built in the candidate workflow. The externally built images and guest bundle require their own SLSA v1 statements through `SECONDBOX_RELEASE_CONTROL_PLANE_IMAGE_PROVENANCE`, `SECONDBOX_RELEASE_RUNNER_IMAGE_PROVENANCE`, `SECONDBOX_RELEASE_GUEST_BUNDLE_PROVENANCE`, and `SECONDBOX_RELEASE_GUEST_ARTIFACT_IMAGE_PROVENANCE`. Each statement must bind the exact subject digest and source commit. The guest artifact image statement must additionally list the exact signed guest bundle digest in `resolvedDependencies`.

## Current blockers

The repository does not yet contain passing structured qualification records for packaged KVM execution, two-runner recovery, durability faults, the complete release-candidate data plane, network policy, or security boundaries. It also lacks adjacent released-version compatibility skew evidence and a trusted release-key signature. The review workflow has no registry image subjects, approved guest bundle, or external image/guest provenance by default. That includes the digest-pinned GHCR guest artifact image and its bundle-bound external provenance. It installs age-qualified Syft 1.44.0 and Grype 0.112.0 only after verifying their official checksum files and exact Linux-amd64 archive digests, but cannot scan absent subjects. Dependency-age generation covers Firecracker, kernel, Debian snapshot, and every resolved guest Python package, but those guest records can pass only when the exact signed guest subject is present. The canonical compatibility manifest records the relevant format and skew gaps.

Consequently, the publication-eligibility job is expected to fail. Removing a gate, changing a blocked status to passed without artifacts, or omitting the verifier does not make a release eligible. Publication becomes possible only after the missing matrices run against the same commit and their immutable artifacts are assembled into a schema-valid evidence directory.

Local evidence generation may be used for development:

```sh
scripts/build-artifacts.sh
install -d .tmp/release-evidence/dist .tmp/release-evidence/package .tmp/release-evidence/sdk .tmp/release-evidence/guest
cp -a dist/. .tmp/release-evidence/dist/
scripts/package-release-artifacts.sh .tmp/release-evidence/dist .tmp/release-evidence/package VERSION "$(git rev-parse HEAD)"
scripts/package-release-sdk-artifacts.sh .tmp/release-evidence/sdk VERSION "$(git rev-parse HEAD)"
scripts/package-release-guest-assets.sh RAW_GUEST_DIR .tmp/release-evidence/guest VERSION "$(git rev-parse HEAD)"
node scripts/generate-release-subject-manifest.mjs .tmp/release-evidence .tmp/release-evidence/release-subjects.json
scripts/generate-release-sboms.sh .tmp/release-evidence/sbom .tmp/release-evidence/dist .tmp/release-evidence/release-subjects.json
scripts/generate-vulnerability-evidence.sh .tmp/release-evidence/vulnerabilities .tmp/release-evidence/dist .tmp/release-evidence/release-subjects.json
scripts/generate-dependency-age-evidence.sh .tmp/release-evidence/dependency-age.json .tmp/release-evidence/release-subjects.json
scripts/generate-license-evidence.sh .tmp/release-evidence/licenses .tmp/release-evidence/release-subjects.json
scripts/generate-release-checksum-evidence.sh .tmp/release-evidence/release-subjects.json .tmp/release-evidence/checksums
scripts/generate-release-signature-evidence.sh .tmp/release-evidence/release-subjects.json .tmp/release-evidence/signatures
scripts/generate-release-provenance.sh .tmp/release-evidence/release-subjects.json .tmp/release-evidence/provenance.intoto.json
```

Set `SECONDBOX_RELEASE_VERSION`, `SECONDBOX_RELEASE_SOURCE_COMMIT`, `SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP`, `SECONDBOX_RELEASE_BUILDER_IDENTITY`, and `SECONDBOX_DEPENDENCY_MINIMUM_AGE_SECONDS` explicitly. Supply image, guest-trust, external-provenance, and release-signing variables only from their independently approved authorities. Use a clean committed source tree before treating any local result as candidate evidence.

The image inputs include `SECONDBOX_RELEASE_CONTROL_PLANE_IMAGE`, `SECONDBOX_RELEASE_RUNNER_IMAGE`, and `SECONDBOX_RELEASE_GUEST_ARTIFACT_IMAGE`; each must be a registry reference ending in `@sha256:<digest>`. The guest artifact image is the GHCR transport image built from `runner/deploy/microvm-artifact-transport.Dockerfile` using the verified raw files packaged into the signed guest bundle.
