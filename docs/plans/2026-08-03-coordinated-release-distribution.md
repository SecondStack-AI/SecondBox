---
title: Coordinated Release Distribution
date: 2026-08-03
status: in_progress
owner: SecondStack
provenance: SecondBox and SecondStack integration boundary review, 2026-08-03
---

# Plan: Coordinated SecondBox Release Distribution

Release SecondBox as one public, qualified product that consumers can install without a source checkout, copied SDK, copied deployment code, or consumer-owned resource bootstrap. One immutable SemVer Git tag versions the existing Go and TypeScript SDKs, control-plane and Runner images, signed microVM artifact image, `secondbox` and `secondbox-deploy` binaries, standard resource bundles, checksums, provenance, an immutable artifact manifest, qualification evidence and a final machine-readable release index in lockstep.

This plan improves the SDKs already owned by this repository. It creates no replacement SDK, consumer-specific client, or second package lineage. Generic SecondBox lifecycle, fencing, transport, deployment and resource-application behavior belongs here; Agent identity, Chat, Flue shard ownership and other consumer product semantics remain outside SecondBox.

SecondBox is public. The TypeScript SDK publishes from `sdk/typescript` under its existing `@secondstack-ai/secondbox` identity, the Go SDK remains part of the repository module at the same Git tag, OCI artifacts publish to public GHCR packages, and binaries plus the artifact manifest, qualification attestation and final release index publish as public GitHub Release assets.

## Fixed design

- One Git tag is the compatibility and release identity for every artifact. SDKs, binaries, images, standard resources and protocols are not independently versioned.
- A consumer recognizes a release only through its final validated release index. The index references one immutable artifact manifest and one qualification attestation bound to that artifact-manifest digest. A tag, artifact manifest, qualification attestation or partially published registry artifact alone is not a complete release.
- Production consumers use immutable OCI digests and verified release assets. Floating tags and source checkouts are never runtime or release authority.
- `secondbox-deploy` owns deployment initialization, rendering and Compose integration. The existing `secondbox` CLI and SDKs own public resource operations. Consumers do not reproduce those behaviors with shell HTTP calls.
- SecondBox ships reviewed `agent-compartment` and `durable-coding` standard resource bundles. Production deployment explicitly selects bundles in `secondbox.toml`; development initialization selects both in its generated topology. The standard-resource engine is their sole owner; the current control-plane built-in Profile bootstrap and reserved-name mutation path are removed rather than retained as a second reconciler.
- Bundle selection is explicit, but consumers do not carry copies of the RunnerPool or Profile specifications. Each standard Profile revision is a byte-identical release-owned document identified by its stable name, revision number and canonical spec digest. Profile pool selectors, architecture, asset identities and logical gateway destinations are never deployment-parameterized.
- Deployment-specific authority, storage, runner placement, RunnerPool capacity and architecture inventory, and logical-gateway address mapping remain explicit operator input. A selected bundle fails validation when that input cannot satisfy its fixed Profile requirements; materially different policy uses an explicitly operator-owned custom Profile.
- Application authorities and secret references remain in `secondbox.toml`. Resource documents, artifact manifests, qualification attestations and release indexes contain no plaintext credentials.
- A standard bundle carries the complete ordered lineage of its immutable Profile revisions. Application validates the installed lineage prefix and sequentially appends only missing declared revisions, so a clean install and an upgrade converge on the same name, revision number and spec digest. It may also make only explicitly supported RunnerPool updates. Omission never authorizes deletion.
- Release qualification is tied to the exact source commit and signed guest artifact identity. Passing an ordinary Compose test is not KVM qualification.
- The first release may remove the obsolete TypeScript Flue beta.9 compatibility layer. No compatibility fallback or duplicate SDK surface is retained.

## Validation Commands

Existing gates remain mandatory throughout implementation:

- `just verify-generated`
- `just test-non-kvm`
- `just build-artifacts`
- `npm run pack:sdk-typescript`
- `git diff --check`

The implementation adds stable release gates; run them after the task that introduces each command:

- `just test-standard-resources`
- `just test-release-stage`
- `just test-release-workflow`
- `just test-source-free-release`
- `SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1 just test-scenario` on the qualified KVM host

No load-bearing gate may skip because a required registry artifact, credential, Runner, signed bundle, architecture, or qualification record is missing. Publication-specific tests use isolated test coordinates or dry-run boundaries until the real release task.

### Task 1: Define the coordinated release contract

Establish the schema and compatibility rules that every later task implements. The contract separates immutable artifact identity from qualification so the final release index can reference both without a digest cycle.

- [x] Define the accepted SemVer tag syntax and canonical public coordinates for the existing TypeScript SDK, Go module, control-plane image, Runner image, microVM artifact image, `secondbox` binaries and `secondbox-deploy` binaries.
- [x] Define a versioned artifact-manifest schema containing the release version, Git tag, full source commit, public OpenAPI identity, Runner and guest protocol windows, supported platform matrix, SDK coordinates, OCI references and digests, binary checksums, SBOM and artifact-attestation references, standard resource bundle identities, signed microVM manifest digest and signing-key fingerprint.
- [x] Define a versioned qualification-attestation schema that binds the exact artifact-manifest digest to the source-free test suite, signed guest identity, architecture, Runner environment and qualification result without referring to the final release index.
- [x] Define a versioned final release-index schema that references the immutable artifact manifest and qualification attestation by public location and digest and contains no self-referential evidence field.
- [x] Require every release artifact and embedded version surface to identify the same tag and full source commit.
- [x] Define which host and Runner architectures are supported and qualified. Do not publish a Runner or guest artifact for an architecture without its required qualification.
- [x] Define the release-completeness rule: the final release index is published only after every artifact referenced by the artifact manifest is publicly readable and the qualification attestation has passed independent verification.
- [x] Define immutable retry behavior for partial publication. An already published coordinate may be accepted only when it is byte- or digest-identical to the staged artifact.
- [x] Define the mixed-version policy. Control plane, Runner and guest protocol combinations outside explicitly recorded compatibility evidence fail rather than silently proceeding.
- [x] Implement artifact-manifest, qualification-attestation and final release-index decoding and validation in a reusable SecondBox package consumed by release tooling and `secondbox-deploy`.
- [x] Add tests rejecting missing artifacts, inconsistent versions or commits, mutable OCI references, malformed digests, absent checksums, unsupported platforms, incompatible protocol windows, signing-identity mismatch, qualification bound to a different artifact manifest, self-referential evidence and an incomplete final release index.
- [x] Document the release authority chain, artifact naming, compatibility policy and independent verification procedure.

### Task 2: Improve the existing Go and TypeScript SDKs for consumers

Use the required Agent Platform and durable coding/Agent Claude operation patterns as input while keeping all exported concepts generic to SecondBox. Move shared protocol mechanics into the SDKs already present under `sdk/go` and `sdk/typescript` so a consumer adapter expresses only its own mapping and orchestration.

- [x] Record a consumer-operation matrix for Profile validation, Sandbox creation and adoption, listing, metadata updates, lifecycle transitions, waiting, buffered and streaming execution, filesystem access, Snapshots, Artifacts, Lease takeover, Ports, terminals and direct tunnels.
- [x] Audit the existing Go and TypeScript public surfaces against that matrix and remove consumer-side reasons to construct operation names, ETags, generation headers, raw polling loops or response decoders.
- [x] Add missing high-level operations to the existing clients and `SandboxHandle` surfaces using the generated transport and wire types as the canonical contract.
- [x] Preserve caller-owned Sandbox lifetime. Closing a handle, stream, terminal, Lease helper or Flue environment never implicitly deletes a Sandbox.
- [x] Preserve explicit generation and Lease fencing. Never refresh and replay an operation automatically against a replacement Instance.
- [x] Preserve or add typed idempotency, cancellation, bounded wait/poll behavior, output limits and terminal outcomes with idiomatic Go and TypeScript APIs.
- [x] Provide semantically equivalent Go and TypeScript behavior for the shared lifecycle surface, while allowing language-appropriate API shapes.
- [x] Add a TypeScript Node-specific export for generic authenticated WebSocket and direct-port transport mechanics. Keep browser-neutral transport interfaces in the main export and avoid consumer-specific dependencies.
- [x] Upgrade the existing TypeScript `/flue` export to the exact Flue 2.0 public contract and remove the vendored beta.9 compatibility module, source snapshot and compatibility-only license payload.
- [x] Keep admissions, Agent identity, Chat threads, external-effect receipts, Flue shard ownership and consumer database mappings outside the SDKs.
- [x] Extend schema-drift, public-surface, package-content, unit, live-API and cross-language behavior tests for every added capability.
- [x] Add minimal Go and TypeScript examples demonstrating a durable Sandbox lifecycle and a thin Flue 2.0 integration without importing consumer source.
- [x] Make the TypeScript package installable and executable from a clean temporary project, and make the Go SDK consumable from a clean temporary module at a synthetic release tag during qualification.

### Task 3: Add declarative resources and standard bundles

Provide one generic, idempotent resource engine so consumers do not implement RunnerPool and Profile bootstrap. SecondBox owns standard bundle contents and revisions; operators explicitly select their capabilities through the deployment manifest.

- [x] Remove the current `BuildBuiltInProfiles`, `EnsureBuiltInProfile`, reserved-name mutation checks and request-time built-in reconciliation before making the standard-resource engine authoritative. Remove the replaced control-plane environment and deployment-policy fields rather than retaining a compatibility path.
- [x] Rename the current `coding-environment` product Profile to the release-owned `durable-coding` standard Profile. Update current Profile, deployment and authorization documentation so only the two standard-resource names remain.
- [x] Define a versioned, non-secret desired-resource document covering RunnerPools and immutable Profile revisions.
- [x] Implement a shared resource check/apply engine over the existing Go SDK with deterministic idempotency and optimistic resource revisions.
- [x] Add `secondbox resources check` to validate a file or standard bundle and report intended changes without mutation.
- [x] Add `secondbox resources apply` to apply a file or named standard bundle and return a structured per-resource result.
- [x] Create missing RunnerPools before Profiles that reference them, treat an exact existing resource as a no-op and make interrupted partial application safely repeatable.
- [x] Represent each standard Profile by its complete ordered revision lineage and canonical spec digest. Define release identity as Profile name, revision number and spec digest rather than the deployment-local database ID.
- [x] On a clean deployment, create the lineage sequentially from revision 1. On an upgrade, validate every installed revision in the declared prefix and append only missing revisions with explicit expected/current Profile revision information.
- [x] Fail gaps, unknown future heads, altered historical specs and incompatible current drift rather than silently replacing, revising or weakening policy.
- [x] Support only deliberate RunnerPool fields as mutable and use optimistic revision checks for every update race.
- [x] Do not delete or drain a resource because it is absent from the desired document. Pruning is outside this release.
- [x] Define release-owned `agent-compartment` and `durable-coding` bundles with stable names, complete revision lineages, canonical spec digests and generic SecondBox semantics.
- [x] Give `agent-compartment` bounded short-lived execution, no public Ports, no direct Internet, and only its explicitly bound Runner gateway when selected.
- [x] Give `durable-coding` bounded long-lived workspace, Snapshot, Artifact and named-Port capabilities plus only its explicitly bound platform gateway.
- [x] Resolve release-owned signed runtime/toolchain identities from the verified artifact manifest rather than copying digests into consumer repositories.
- [x] Give standard Profiles fixed logical pool selectors, qualified architecture and logical gateway destinations. If a future release supports multiple architectures, ship distinct architecture-qualified Profile identities instead of parameterizing one revision.
- [x] Add a strict `[standard_resources]` selection to `secondbox.toml` and typed deployment-specific RunnerPool capacity and architecture-inventory input. Keep logical-gateway-to-address mappings in Runner deployment configuration and validate that selected bundles can resolve every required gateway.
- [x] Make production selection explicit. Make generated development initialization select both reviewed bundles and show that selection in `inspect` output.
- [x] Apply selected bundles through `secondbox-deploy` after the control plane is ready, using the same resource engine rather than invoking shell HTTP calls or another binary.
- [x] Keep application authorities and token file references in the protected deployment manifest; never put them in a bundle or resource document.
- [x] Add tests for validation, dry-run reporting, creation order, exact replay, clean-install and upgrade lineage convergence, canonical Profile spec identity, RunnerPool update, revision race, incompatible drift, partial failure, retry, bundle parameter validation and omission without deletion.
- [x] Add a live control-plane qualification proving a fresh deployment, an upgrade and a repeated deployment converge to the same selected standard Profile names, revision numbers and spec digests without the removed built-in reconciler.
- [x] Update development rules and deployment/Profile documentation to distinguish explicit standard-bundle selection from silent universal defaults.

### Task 4: Build a complete release candidate locally

Create one deterministic staging path that assembles and verifies a prospective release without registry credentials or publication. A locally staged candidate must contain every byte and identity needed by the tag workflow.

- [x] Add a stable staging command such as `just release-stage VERSION OUTPUT_DIR` and a non-publishing `just test-release-stage` gate.
- [x] Require a clean repository, a valid SemVer version and an exact prospective tag/source-commit relationship. Permit an explicit test mode for synthetic versions without weakening the real gate.
- [x] Build the existing TypeScript SDK package from `sdk/typescript`, inject the release version during packaging and leave its repository manifest at the declared development version between releases.
- [x] Verify the staged TypeScript tarball contains only its declared runtime, type, license, provenance and documentation files.
- [x] Verify the Go module and existing SDK packages are valid at the same prospective repository tag.
- [x] Build `secondbox` and `secondbox-deploy` for every declared host platform and the control-plane/Runner binaries required by every declared OCI platform.
- [x] Build control-plane and Runner OCI images with full source-commit, release-version and public-contract labels and immutable base inputs.
- [x] Verify the independently signed microVM bundle against the separately configured trust anchor and stage only its fixed allowlist into the architecture-specific artifact image.
- [x] Include the standard resource documents, revisions and bundle parameter schemas in the candidate.
- [x] Generate checksums, SBOMs, OCI metadata, package metadata, provenance inputs and the candidate artifact manifest. Do not manufacture qualification evidence or a final release index during local staging.
- [x] Verify every artifact reports the same version and full source commit and every digest in the artifact manifest matches the staged object.
- [x] Smoke-test the packaged SDK, binaries, embedded Compose assets, artifact-manifest verifier, resource bundles and OCI configuration using only the staging directory.
- [x] Make repeated staging from identical inputs reproduce the same distributable identities; isolate unavoidable attestation timestamps from content identities.
- [x] Refuse dirty source, mutable base inputs, missing signed artifacts, unknown extra files, incompatible architectures, stale generated code and a candidate that depends on repository-relative runtime files.

### Task 5: Publish an immutable public release candidate

Implement one tag workflow that publishes the staged candidate to public registries without yet creating a complete release. Publication is a gated promotion of already tested content, not a second build design; the public candidate exists so source-free qualification can prove anonymous consumption before finalization.

- [x] Add a workflow triggered only by immutable SemVer tags and pin every external workflow action to a reviewed full commit.
- [x] Bind the workflow to the exact tag commit and require normal CI plus generated-contract verification for that commit.
- [x] Extend the dedicated KVM workflow so a release candidate qualifies the exact tag commit and emits machine-readable evidence bound to the signed guest manifest, architecture and Runner environment.
- [x] Require pre-publication KVM candidate evidence for the exact tag commit before publishing a candidate; a scheduled or unrelated-commit scenario pass is insufficient and cannot become the final source-free qualification attestation.
- [x] Re-run deterministic staging in the release workflow and compare it with qualification inputs.
- [x] Publish the existing TypeScript SDK as the public `@secondstack-ai/secondbox` npm package with npm provenance.
- [x] Use the same Git tag as the Go SDK/module version and verify public module resolution before finalization.
- [x] Publish digest-addressable control-plane, Runner and microVM artifact images to public GHCR with the canonical release coordinates.
- [x] Publish `secondbox` and `secondbox-deploy` binaries, checksums, SBOMs, artifact attestations and the artifact manifest as GitHub prerelease assets. Do not publish the qualification attestation or final release index yet.
- [x] Use GitHub workload identity for attestations and registry publication where supported; add no long-lived publication credential when a trusted-publisher mechanism exists.
- [x] Use draft state only while uploading assets, then publish the candidate as an explicitly incomplete public prerelease so anonymous verification can read every GitHub asset. Authenticated access to a draft is not public-distribution evidence.
- [x] Re-read every referenced npm, Go, GHCR and GitHub candidate artifact anonymously where the public service permits and independently match it to the staged artifact manifest.
- [x] Keep the public candidate non-consumable under the SecondBox contract: no final release index, stable npm distribution tag or final GitHub release state exists before source-free qualification.
- [x] Make partial candidate publication non-consumable and retry-safe. Refuse a retry if any immutable coordinate contains different content.
- [x] Never overwrite a released artifact, move a release tag, reuse an npm version or make a floating tag part of the consumer contract.
- [x] Add dry-run and isolated-coordinate tests for tag/version mismatch, absent qualification, partial publication, digest mismatch, registry replay and attempted mutation.
- [x] Add an operator setup runbook for npm trusted publishing, GHCR permissions, GitHub Release permissions, KVM runner variables and the independently held guest signing trust anchor.

### Task 6: Qualify source-free consumption and finalize the release

Prove that public candidate artifacts from one tag are sufficient to install and operate SecondBox, then create the qualification attestation and final release index. This gate must not read a SecondBox checkout, compile product source or use workflow-local artifacts as substitutes for public distribution.

- [x] Add artifact-manifest and final release-index verification commands to `secondbox-deploy`, including public artifact identity, digest, protocol, qualification and standard-bundle validation.
- [x] Allow deployment initialization to consume a verified artifact manifest referenced by a final release index as immutable software facts without treating those facts as operator authority defaults. Add an explicit qualification-only mode that accepts the public candidate artifact manifest before the final index exists.
- [x] Keep database, object storage, secrets, ingress, trust anchors, Runner placement, workspace paths, network bindings and capacity as explicit operator input.
- [x] Start a clean deployment using only downloaded candidate binaries, the public artifact manifest, public digest-pinned images and protected operator configuration.
- [x] Apply the explicitly selected standard bundles through the released deployment binary and prove repeated deployment converges without creating extra Profile revisions.
- [x] Install the public TypeScript SDK tarball from npm into an empty project and exercise a live durable Sandbox lifecycle.
- [x] Resolve the Go SDK from the public repository tag in an empty module and exercise the equivalent live lifecycle.
- [x] On the qualified KVM host, pull the released Runner and signed microVM artifact images and run the complete scenario path without a repository checkout.
- [x] Prove the installed topology contains no source path, compiler requirement, local image build, mutable tag authority, copied SDK, copied Compose file or consumer-owned resource reconciliation code.
- [x] Test missing or malformed artifact manifests and release indexes, changed digests, wrong tags, unsupported platforms, incompatible protocol windows, unsigned or differently signed guest bundles and standard-resource revision drift.
- [x] Produce and archive a machine-readable qualification attestation bound to the public artifact-manifest digest, exact public artifact digests, source-free suite identity, signed guest identity, architecture and Runner environment.
- [x] Construct the final release index from the verified artifact-manifest and qualification-attestation digests, publish it as the last release artifact, re-read it publicly, then promote the GitHub prerelease and stable npm distribution tag.
- [x] Prove the final release index contains no digest cycle, references only public immutable objects and becomes acceptable to the normal `secondbox-deploy` verifier only after qualification succeeds.
- [x] Make `just test-source-free-release` fail hard whenever any required public artifact, credential, Runner, signed bundle or expected evidence is absent.

### Task 7: Cut and hand off the first qualified release

Complete one real public release and provide a stable downstream contract. The release is complete only after its public candidate artifacts pass the source-free gate and its final release index is publicly verifiable.

- [x] Select the first SemVer version under the coordinated release policy and finalize the changelog and current release documentation.
- [ ] Run generated checks, the complete non-KVM suite, SDK package tests, release staging, security/image policy, deployment tests and the qualified KVM scenario for the exact candidate commit.
- [ ] Create the immutable Git tag and execute the public candidate, source-free qualification and finalization workflow.
- [ ] Verify npm, Go module resolution, GitHub binaries, GHCR images, SBOMs, artifact attestations, the artifact manifest, qualification attestation and final release index from their public locations.
- [ ] Confirm source-free qualification ran against the public candidate artifacts rather than local or workflow-staged substitutes before the final release index was published.
- [ ] Confirm the standard `agent-compartment` and `durable-coding` bundles can be selected without a consumer-owned RunnerPool or Profile definition.
- [ ] Record the final release-index URL and digest, artifact-manifest and qualification-attestation URLs and digests, npm integrity, OCI digests, binary checksums, standard bundle names/revisions/spec digests, supported platforms and protocol windows.
- [ ] Publish a concise downstream integration handoff explaining how to verify and pin the release, initialize a deployment, select standard resources and import the existing SDKs.
- [ ] Confirm no provisional tag, development package version, mutable image reference, source checkout or unpublished required artifact remains in the downstream instructions.
- [ ] Preserve the final release index and its referenced artifact manifest and qualification attestation as the canonical inputs for the subsequent SecondStack Agent Platform and stacked Agent Claude integration work.
