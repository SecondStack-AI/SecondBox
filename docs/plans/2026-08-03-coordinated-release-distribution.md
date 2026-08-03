---
title: Coordinated Release Distribution
date: 2026-08-03
status: planned
owner: SecondStack
provenance: SecondBox and SecondStack integration boundary review, 2026-08-03
---

# Plan: Coordinated SecondBox Release Distribution

Release SecondBox as one public, qualified product that consumers can install without a source checkout, copied SDK, copied deployment code, or consumer-owned resource bootstrap. One immutable SemVer Git tag versions the existing Go and TypeScript SDKs, control-plane and Runner images, signed microVM artifact image, `secondbox` and `secondbox-deploy` binaries, standard resource bundles, checksums, provenance, and the machine-readable release manifest in lockstep.

This plan improves the SDKs already owned by this repository. It creates no replacement SDK, consumer-specific client, or second package lineage. Generic SecondBox lifecycle, fencing, transport, deployment and resource-application behavior belongs here; Agent identity, Chat, Flue shard ownership and other consumer product semantics remain outside SecondBox.

SecondBox is public. The TypeScript SDK publishes from `sdk/typescript` under its existing `@secondstack-ai/secondbox` identity, the Go SDK remains part of the repository module at the same Git tag, OCI artifacts publish to public GHCR packages, and binaries plus the release manifest publish as public GitHub Release assets.

## Fixed design

- One Git tag is the compatibility and release identity for every artifact. SDKs, binaries, images, standard resources and protocols are not independently versioned.
- A consumer recognizes a release only through its final validated release manifest. A tag or partially published registry artifact alone is not a complete release.
- Production consumers use immutable OCI digests and verified release assets. Floating tags and source checkouts are never runtime or release authority.
- `secondbox-deploy` owns deployment initialization, rendering and Compose integration. The existing `secondbox` CLI and SDKs own public resource operations. Consumers do not reproduce those behaviors with shell HTTP calls.
- SecondBox ships reviewed `agent-compartment` and `durable-coding` standard resource bundles. Production deployment explicitly selects bundles in `secondbox.toml`; development initialization selects both in its generated topology.
- Bundle selection is explicit, but consumers do not carry copies of the RunnerPool or Profile specifications. Deployment-specific authority, storage, network bindings, runner placement, architecture and capacity remain explicit operator input.
- Application authorities and secret references remain in `secondbox.toml`. Resource documents and release manifests contain no plaintext credentials.
- Applying a newer selected standard bundle may create only its declared immutable Profile revision and explicitly supported RunnerPool update. Omission never authorizes deletion.
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

Establish the schema and compatibility rules that every later task implements. The contract makes a single release manifest authoritative for an immutable set of public artifacts and qualification evidence.

- [ ] Define the accepted SemVer tag syntax and canonical public coordinates for the existing TypeScript SDK, Go module, control-plane image, Runner image, microVM artifact image, `secondbox` binaries and `secondbox-deploy` binaries.
- [ ] Define a versioned release-manifest schema containing the release version, Git tag, full source commit, public OpenAPI identity, Runner and guest protocol windows, supported platform matrix, SDK coordinates, OCI references and digests, binary checksums, SBOM and attestation references, standard resource bundle identities, signed microVM manifest digest, signing-key fingerprint and qualification evidence.
- [ ] Require every release artifact and embedded version surface to identify the same tag and full source commit.
- [ ] Define which host and Runner architectures are supported and qualified. Do not publish a Runner or guest artifact for an architecture without its required qualification.
- [ ] Define the release-completeness rule: the final manifest is published only after every referenced artifact is publicly readable and independently verified.
- [ ] Define immutable retry behavior for partial publication. An already published coordinate may be accepted only when it is byte- or digest-identical to the staged artifact.
- [ ] Define the mixed-version policy. Control plane, Runner and guest protocol combinations outside explicitly recorded compatibility evidence fail rather than silently proceeding.
- [ ] Implement release-manifest decoding and validation in a reusable SecondBox package consumed by release tooling and `secondbox-deploy`.
- [ ] Add tests rejecting missing artifacts, inconsistent versions or commits, mutable OCI references, malformed digests, absent checksums, unsupported platforms, incompatible protocol windows, signing-identity mismatch and missing qualification evidence.
- [ ] Document the release contract, artifact naming, compatibility policy and independent verification procedure.

### Task 2: Improve the existing Go and TypeScript SDKs for consumers

Use the required Agent Platform and durable coding/Agent Claude operation patterns as input while keeping all exported concepts generic to SecondBox. Move shared protocol mechanics into the SDKs already present under `sdk/go` and `sdk/typescript` so a consumer adapter expresses only its own mapping and orchestration.

- [ ] Record a consumer-operation matrix for Profile validation, Sandbox creation and adoption, listing, metadata updates, lifecycle transitions, waiting, buffered and streaming execution, filesystem access, Snapshots, Artifacts, Lease takeover, Ports, terminals and direct tunnels.
- [ ] Audit the existing Go and TypeScript public surfaces against that matrix and remove consumer-side reasons to construct operation names, ETags, generation headers, raw polling loops or response decoders.
- [ ] Add missing high-level operations to the existing clients and `SandboxHandle` surfaces using the generated transport and wire types as the canonical contract.
- [ ] Preserve caller-owned Sandbox lifetime. Closing a handle, stream, terminal, Lease helper or Flue environment never implicitly deletes a Sandbox.
- [ ] Preserve explicit generation and Lease fencing. Never refresh and replay an operation automatically against a replacement Instance.
- [ ] Preserve or add typed idempotency, cancellation, bounded wait/poll behavior, output limits and terminal outcomes with idiomatic Go and TypeScript APIs.
- [ ] Provide semantically equivalent Go and TypeScript behavior for the shared lifecycle surface, while allowing language-appropriate API shapes.
- [ ] Add a TypeScript Node-specific export for generic authenticated WebSocket and direct-port transport mechanics. Keep browser-neutral transport interfaces in the main export and avoid consumer-specific dependencies.
- [ ] Upgrade the existing TypeScript `/flue` export to the exact Flue 2.0 public contract and remove the vendored beta.9 compatibility module, source snapshot and compatibility-only license payload.
- [ ] Keep admissions, Agent identity, Chat threads, external-effect receipts, Flue shard ownership and consumer database mappings outside the SDKs.
- [ ] Extend schema-drift, public-surface, package-content, unit, live-API and cross-language behavior tests for every added capability.
- [ ] Add minimal Go and TypeScript examples demonstrating a durable Sandbox lifecycle and a thin Flue 2.0 integration without importing consumer source.
- [ ] Make the TypeScript package installable and executable from a clean temporary project, and make the Go SDK consumable from a clean temporary module at a synthetic release tag during qualification.

### Task 3: Add declarative resources and standard bundles

Provide one generic, idempotent resource engine so consumers do not implement RunnerPool and Profile bootstrap. SecondBox owns standard bundle contents and revisions; operators explicitly select their capabilities through the deployment manifest.

- [ ] Define a versioned, non-secret desired-resource document covering RunnerPools and immutable Profile revisions.
- [ ] Implement a shared resource check/apply engine over the existing Go SDK with deterministic idempotency and optimistic resource revisions.
- [ ] Add `secondbox resources check` to validate a file or built-in bundle and report intended changes without mutation.
- [ ] Add `secondbox resources apply` to apply a file or named built-in bundle and return a structured per-resource result.
- [ ] Create missing RunnerPools before Profiles that reference them, treat an exact existing resource as a no-op and make interrupted partial application safely repeatable.
- [ ] Require explicit expected/current Profile revision information before adding an immutable revision. Fail incompatible drift rather than silently replacing, revising or weakening policy.
- [ ] Support only deliberate RunnerPool fields as mutable and use optimistic revision checks for every update race.
- [ ] Do not delete or drain a resource because it is absent from the desired document. Pruning is outside this release.
- [ ] Define release-owned `agent-compartment` and `durable-coding` bundles with stable names, explicit revisions and generic SecondBox semantics.
- [ ] Give `agent-compartment` bounded short-lived execution, no public Ports, no direct Internet, and only its explicitly bound Runner gateway when selected.
- [ ] Give `durable-coding` bounded long-lived workspace, Snapshot, Artifact and named-Port capabilities plus only its explicitly bound platform gateway.
- [ ] Resolve release-owned signed runtime/toolchain identities from the verified release manifest rather than copying digests into consumer repositories.
- [ ] Add a strict `[standard_resources]` selection to `secondbox.toml` and typed deployment-specific bundle bindings for capacity, architecture and allowed gateway endpoints.
- [ ] Make production selection explicit. Make generated development initialization select both reviewed bundles and show that selection in `inspect` output.
- [ ] Apply selected bundles through `secondbox-deploy` after the control plane is ready, using the same resource engine rather than invoking shell HTTP calls or another binary.
- [ ] Keep application authorities and token file references in the protected deployment manifest; never put them in a bundle or resource document.
- [ ] Add tests for validation, dry-run reporting, creation order, exact replay, explicit Profile revision, RunnerPool update, revision race, incompatible drift, partial failure, retry, bundle parameter validation and omission without deletion.
- [ ] Add a live control-plane qualification proving a fresh deployment and a repeated deployment converge to the same selected standard resources.
- [ ] Update development rules and deployment/Profile documentation to distinguish explicit standard-bundle selection from silent universal defaults.

### Task 4: Build a complete release candidate locally

Create one deterministic staging path that assembles and verifies a prospective release without registry credentials or publication. A locally staged candidate must contain every byte and identity needed by the tag workflow.

- [ ] Add a stable staging command such as `just release-stage VERSION OUTPUT_DIR` and a non-publishing `just test-release-stage` gate.
- [ ] Require a clean repository, a valid SemVer version and an exact prospective tag/source-commit relationship. Permit an explicit test mode for synthetic versions without weakening the real gate.
- [ ] Build the existing TypeScript SDK package from `sdk/typescript`, inject the release version during packaging and leave its repository manifest at the declared development version between releases.
- [ ] Verify the staged TypeScript tarball contains only its declared runtime, type, license, provenance and documentation files.
- [ ] Verify the Go module and existing SDK packages are valid at the same prospective repository tag.
- [ ] Build `secondbox` and `secondbox-deploy` for every declared host platform and the control-plane/Runner binaries required by every declared OCI platform.
- [ ] Build control-plane and Runner OCI images with full source-commit, release-version and public-contract labels and immutable base inputs.
- [ ] Verify the independently signed microVM bundle against the separately configured trust anchor and stage only its fixed allowlist into the architecture-specific artifact image.
- [ ] Include the standard resource documents, revisions and bundle parameter schemas in the candidate.
- [ ] Generate checksums, SBOMs, OCI metadata, package metadata, provenance inputs and a draft release manifest.
- [ ] Verify every artifact reports the same version and full source commit and every digest in the draft manifest matches the staged object.
- [ ] Smoke-test the packaged SDK, binaries, embedded Compose assets, release-manifest verifier, resource bundles and OCI configuration using only the staging directory.
- [ ] Make repeated staging from identical inputs reproduce the same distributable identities; isolate unavoidable attestation timestamps from content identities.
- [ ] Refuse dirty source, mutable base inputs, missing signed artifacts, unknown extra files, incompatible architectures, stale generated code and a candidate that depends on repository-relative runtime files.

### Task 5: Publish a qualified tagged release

Implement one tag workflow that publishes the staged candidate to public registries and exposes the final release only after public re-verification. Publication is a gated promotion of already tested content, not a second build design.

- [ ] Add a workflow triggered only by immutable SemVer tags and pin every external workflow action to a reviewed full commit.
- [ ] Bind the workflow to the exact tag commit and require normal CI plus generated-contract verification for that commit.
- [ ] Extend the dedicated KVM workflow so a release candidate qualifies the exact tag commit and emits machine-readable evidence bound to the signed guest manifest, architecture and Runner environment.
- [ ] Require release qualification evidence before any release is finalized; a scheduled or unrelated-commit scenario pass is insufficient.
- [ ] Re-run deterministic staging in the release workflow and compare it with qualification inputs.
- [ ] Publish the existing TypeScript SDK as the public `@secondstack-ai/secondbox` npm package with npm provenance.
- [ ] Use the same Git tag as the Go SDK/module version and verify public module resolution before finalization.
- [ ] Publish digest-addressable control-plane, Runner and microVM artifact images to public GHCR with the canonical release coordinates.
- [ ] Publish `secondbox` and `secondbox-deploy` binaries, checksums, SBOMs, attestations and the release manifest as GitHub Release assets.
- [ ] Use GitHub workload identity for attestations and registry publication where supported; add no long-lived publication credential when a trusted-publisher mechanism exists.
- [ ] Keep the GitHub Release in draft state until every referenced npm, Go, GHCR and GitHub artifact is publicly readable and independently matches the staged candidate.
- [ ] Publish the final release manifest only after public re-verification, then finalize the GitHub Release.
- [ ] Make partial publication non-consumable and retry-safe. Refuse a retry if any immutable coordinate contains different content.
- [ ] Never overwrite a released artifact, move a release tag, reuse an npm version or make a floating tag part of the consumer contract.
- [ ] Add dry-run and isolated-coordinate tests for tag/version mismatch, absent qualification, partial publication, digest mismatch, registry replay and attempted mutation.
- [ ] Add an operator setup runbook for npm trusted publishing, GHCR permissions, GitHub Release permissions, KVM runner variables and the independently held guest signing trust anchor.

### Task 6: Qualify source-free installation and consumption

Prove that public artifacts from one tagged release are sufficient to install and operate SecondBox. This gate must not read a SecondBox checkout, compile product source or use workflow-local artifacts as substitutes for public distribution.

- [ ] Add release-manifest verification commands to `secondbox-deploy`, including public artifact identity, digest, protocol and standard-bundle validation.
- [ ] Allow deployment initialization to consume a verified release manifest as immutable software facts without treating those facts as operator authority defaults.
- [ ] Keep database, object storage, secrets, ingress, trust anchors, Runner placement, workspace paths, network bindings and capacity as explicit operator input.
- [ ] Start a clean deployment using only downloaded release binaries, the public release manifest, public digest-pinned images and protected operator configuration.
- [ ] Apply the explicitly selected standard bundles through the released deployment binary and prove repeated deployment converges without creating extra Profile revisions.
- [ ] Install the public TypeScript SDK tarball from npm into an empty project and exercise a live durable Sandbox lifecycle.
- [ ] Resolve the Go SDK from the public repository tag in an empty module and exercise the equivalent live lifecycle.
- [ ] On the qualified KVM host, pull the released Runner and signed microVM artifact images and run the complete scenario path without a repository checkout.
- [ ] Prove the installed topology contains no source path, compiler requirement, local image build, mutable tag authority, copied SDK, copied Compose file or consumer-owned resource reconciliation code.
- [ ] Test missing or malformed release manifests, changed digests, wrong tags, unsupported platforms, incompatible protocol windows, unsigned or differently signed guest bundles and standard-resource revision drift.
- [ ] Archive source-free qualification evidence bound to the public release manifest digest and exact public artifact digests.
- [ ] Make `just test-source-free-release` fail hard whenever any required public artifact, credential, Runner, signed bundle or expected evidence is absent.

### Task 7: Cut and hand off the first qualified release

Complete one real public release and provide a stable downstream contract. The release is complete only after its public artifacts pass the source-free gate.

- [ ] Select the first SemVer version under the coordinated release policy and finalize the changelog and current release documentation.
- [ ] Run generated checks, the complete non-KVM suite, SDK package tests, release staging, security/image policy, deployment tests and the qualified KVM scenario for the exact candidate commit.
- [ ] Create the immutable Git tag and execute the public release workflow.
- [ ] Verify npm, Go module resolution, GitHub binaries, GHCR images, SBOMs, attestations and the release manifest from their public locations.
- [ ] Run source-free qualification against the public release artifacts rather than local or workflow-staged substitutes.
- [ ] Confirm the standard `agent-compartment` and `durable-coding` bundles can be selected without a consumer-owned RunnerPool or Profile definition.
- [ ] Record the final release-manifest URL and digest, npm integrity, OCI digests, binary checksums, standard bundle names/revisions, supported platforms and protocol windows.
- [ ] Publish a concise downstream integration handoff explaining how to verify and pin the release, initialize a deployment, select standard resources and import the existing SDKs.
- [ ] Confirm no provisional tag, development package version, mutable image reference, source checkout or unpublished required artifact remains in the downstream instructions.
- [ ] Preserve the qualification evidence and release manifest as the canonical inputs for the subsequent SecondStack Agent Platform and stacked Agent Claude integration work.
