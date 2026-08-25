---
title: Customer-Shared Tenancy
date: 2026-08-25
status: planned
owner: SecondBox
provenance: Customer-shared SecondBox architecture review, 2026-08-25
---

# Plan: Customer-Shared Tenancy

Deliver SecondBox v0.6.0 as one implementation pull request that makes a single customer-operated deployment safely consumable by multiple SecondStack installations in the same trust domain. Implement the work as tested vertical slices: establish the public contract, make persisted authorities work end to end, add delegated management, retire static application credentials, enforce quotas, complete subject lifecycle cleanup, then finish the operator, deployment, Profile, and release surfaces. Every task must leave every gate in `Validation Commands` green.

The canonical target design is [Customer-shared tenancy](../design/customer-shared-tenancy.md). Keep the control plane unprivileged, keep Runners independently deployed and privileged, and preserve the existing Sandbox, Instance, Workspace, Runner, and immutable Profile contracts. This plan changes management isolation and credential lifecycle; it does not introduce a configurable role system, public registration, billing, tenant-specific Profile copies, Kubernetes-native compute, automatic Sandbox relocation, or a tenant-aware shared egress gateway.

## Fixed decisions

- One SecondBox deployment serves one customer trust domain and may serve several independent SecondStack installations.
- The deployment-wide platform token remains the operator authority and never enters application workloads.
- A tenant-controller authority is fixed to one tenant. An application authority is fixed to one tenant, one subject, exact Sandbox scopes, and explicit Profile grants.
- PostgreSQL is the only runtime source for tenants, subjects, tenant-controller authorities, and application authorities. Tokens are returned once and stored only as non-recoverable verifiers.
- Tenant ceilings and subject quotas are enforced together. Runner storage-pressure admission remains authoritative for retained workspace capacity.
- Subject closure and expiry converge through one durable cleanup Operation that removes compute and Runner-owned workspace state.
- v0.6.0 is a clean-install boundary. It removes static application authorities and provides no import, compatibility, fallback, or dual-source mode for v0.5.2 installations.
- The same pull request includes the release-owned `agent-compartment-isolated` Profile and complete release qualification so downstream SecondStack work can consume one finished release.
- Existing disposable installations may be rebuilt only during downstream rollout. This repository plan must not delete or mutate a deployed environment.

## Validation Commands

Run the fastest relevant gates after every task and the complete set before handoff:

- `just verify-generated`
- `just test-contract`
- `just test-sdk-packages`
- `just test-standard-resources`
- `just test-compose`
- `just test-deployment`
- `just test-installer`
- `just test-release-stage`
- `just test-release-workflow`
- `just test`
- `just test-scenario` on a qualified KVM Runner host after lifecycle, Runner cleanup, or network-policy changes
- `just preship`
- `git diff --check`

Load-bearing tests fail when their selected Profile, authority, PostgreSQL service, control plane, or qualified Runner is unavailable. They must not turn a broken enabled configuration into a skip. Keep generated OpenAPI clients and release artifacts reproducible and verify them in the same commit that changes their source contract.

### Task 1: Establish the v0.6.0 management contract

Define the complete external shape before implementing storage or handler behavior. Contract conformance tests require every documented operation to be registered through the authenticated HTTP router, so new routes land here with authentication, routing, and typed errors while their behavior arrives in later tasks. Preserve provider-neutral SecondBox terminology and make the three fixed authority kinds obvious in OpenAPI, SDKs, errors, and audit concepts.

- [x] Add Tenant, Subject, TenantControllerAuthority, and ApplicationAuthority schemas with bounded references, metadata, revisions, lifecycle state, timestamps, expiry, grants, and quota fields.
- [x] Add operator operations for tenant lifecycle and tenant-controller authority creation, inspection, listing, rotation, and revocation.
- [x] Add tenant-controller operations for subject lifecycle, subject cleanup, application-authority lifecycle, and tenant-scoped usage inspection; derive the tenant from authentication rather than request input.
- [x] Register every new operation through the authenticated HTTP router with its conformance inventory entry in the same change; handlers fail closed with typed errors until their implementing task supplies behavior.
- [x] Define one-time credential creation responses, non-secret authority reads, public lookup identifiers, idempotency behavior, optimistic revision checks, and the uncertain-response revoke-and-replace procedure.
- [x] Define typed errors for invalid lifecycle transitions, expiry, suspension, grant escalation, quota exhaustion, revision conflict, and cleanup state while keeping cross-tenant resource failures non-enumerating.
- [x] Specify audit attribution for actor authority, tenant, optional subject, operation, result, and bounded correlation metadata without exposing bearer material.
- [x] Regenerate the Go and TypeScript SDKs and update contract fixtures and conformance coverage in the same change.
- [x] Add negative contract coverage for unknown fields, undocumented routes, tenant assertions on controller routes, recoverable token fields, and authority-kind escalation.

### Task 2: Make persisted authorities work end to end

Complete the first runnable vertical slice: a clean database can persist tenant-scoped credentials, authenticate them after a control-plane restart, revoke them immediately, and never recover their secrets. Integration coverage seeds credentials through the store layer until Task 3 delivers management routes. The static authority path stays untouched so existing gates remain green; Task 4 retires it completely, and it must gain no new behavior in the meantime.

- [x] Add ordered PostgreSQL migrations for tenants, subjects, tenant-controller authorities, and application authorities, including lifecycle, expiry, grants, revisions, verifier data, and required uniqueness.
- [x] Keep cross-resource references logical strings without foreign keys or non-uniqueness constraints; use physical uniqueness only for tenant references, tenant-scoped subject identity, authority identity, token lookup identity, and idempotency invariants.
- [x] Generate high-entropy bearer tokens server-side, return each complete token only from its successful creation response, and persist only its lookup identifier and one-way verifier.
- [x] Resolve the presented credential on every newly admitted request, verify it in constant time, and enforce current authority, tenant, subject, expiry, scopes, and Profile grants before route handling.
- [x] Keep platform-token authentication separate and prevent generated tenant or application credentials from colliding with it or crossing authority kinds.
- [x] Ensure reads, lists, logs, errors, metrics, audit events, diagnostics, support bundles, and database inspection surfaces cannot disclose credential secrets or verifier material.
- [x] Add PostgreSQL and HTTP integration coverage for restart persistence, immediate revocation, expiry, token rotation, invalid verifiers, identical tenant-local names, and non-enumerating cross-tenant access.

### Task 3: Implement delegated tenant management end to end

Turn the contract and persisted credentials into a usable customer management plane. The platform operator establishes tenant ceilings and delegates a fixed, code-owned management capability; a tenant controller can create only resources narrower than that ceiling.

- [x] Implement platform-operator tenant creation, inspection, listing, suspension, reactivation, and tenant-controller authority lifecycle through service, store, HTTP, audit, and generated-client layers.
- [x] Implement tenant-controller subject creation, inspection, and listing, and application-authority creation, inspection, listing, rotation, and revocation, with the tenant taken only from the authenticated principal; subject closure and cleanup behavior arrive in Task 6.
- [x] Enforce tenant ceilings for allowed application scopes and Profile grants, and require every application authority to bind exactly one existing active subject.
- [x] Make tenant suspension deny new controller and application admission while preserving resources for explicit recovery or later cleanup.
- [x] Apply idempotency and optimistic revisions to management mutations so retries converge without silently overwriting a concurrent operator decision.
- [x] Audit every successful mutation and denial with bounded correlation metadata and stable, greppable operation names.
- [x] Add two-tenant integration scenarios proving a controller cannot discover or mutate another tenant, widen its ceiling, administer Runners or Profiles, or use ordinary Sandbox APIs as another subject.
- [x] Add restart and concurrency coverage for idempotent credential creation, uncertain creation responses, rotation, revocation, and lifecycle revision conflicts.

### Task 4: Retire static application authorities everywhere

Delegated management can now mint credentials, so remove the static authority path in one slice that leaves every deployment, Compose, installer, and test surface green. No import, compatibility, fallback, or dual-source mode may remain after this task.

- [ ] Remove in-memory static application-authority resolution, `SECONDBOX_APPLICATION_AUTHORITIES_JSON`, and any process-start parsing or runtime fallback to an authority file.
- [ ] Remove `applications.application_authorities_file` from the deployment manifest, example manifests, resolved deployment state, Compose environment, secret inventory, diagnostics, installer planning, and update logic.
- [ ] Rework `deploy/compose.yml`, `scripts/compose-test.yml`, and `scripts/scenario-compose.yml` so composed environments bootstrap tenants, controllers, subjects, and application authorities through authenticated management operations after startup.
- [ ] Replace store-level credential seeding in earlier integration coverage with management-operation bootstrap so no test depends on private store access.
- [ ] Update deployment, installer, Compose, CLI, configuration-surface, diagnostics, support-bundle, and source-free package tests to prove the retired secret has disappeared completely.

### Task 5: Enforce tenant aggregate and subject quota atomically

Extend existing subject admission rather than creating a parallel quota system. Every chargeable operation must reserve against both levels in one stable transaction so concurrency cannot temporarily overcommit and repair later.

- [ ] Persist tenant aggregate ceilings and usage alongside the existing subject quota dimensions for Sandboxes, active Instances, vCPU, memory, Snapshots, Port sessions, and concurrent data-plane operations.
- [ ] Add limits for active subjects and application authorities so delegated management cannot create unbounded records.
- [ ] Require subject quotas to remain within tenant ceilings and reject narrowing changes that conflict with committed usage through a typed revision-aware response.
- [ ] Reserve and release tenant and subject usage in the same transaction and stable lock order at every existing admission and cleanup path.
- [ ] Preserve Profile resource bounds and Runner storage-pressure admission as their existing distinct authorities; do not invent retained-byte tenant accounting for reflink-backed workspaces.
- [ ] Expose tenant-scoped usage to its controller and deployment-wide usage to the platform operator without adding high-cardinality metric labels.
- [ ] Add concurrent integration tests that race admissions and releases at every quota dimension and prove neither level can overcommit.
- [ ] Prove an exhausted, suspended, or heavily contended tenant does not block or consume the entitlement of another tenant.

### Task 6: Make subject closure, expiry, and cleanup durable

Represent application-environment teardown as one observable Operation. Closing a subject must stop new admission immediately, while cleanup remains restart-safe and reconciles Runner-owned state through the existing acknowledged protocol.

- [ ] Make subject closure atomically deny new authorities and newly admitted Sandbox operations without pretending already admitted bounded work was never accepted.
- [ ] Revoke all remaining application authorities as part of the close workflow and keep repeated close requests idempotent.
- [ ] Create or return one durable subject-cleanup Operation that cancels active sessions and operations, stops and deletes Sandboxes, releases Leases and quota reservations, and requests removal of Runner-owned workspace state.
- [ ] Advance cleanup through explicit persisted stages and existing Runner acknowledgements; retries must continue the same operation identity rather than launch untracked deletion work.
- [ ] Add expiry reconciliation that closes expired authorities and subjects and advances cleanup even when the upstream lifecycle controller has failed.
- [ ] Keep tenant suspension non-destructive and place any eventual whole-tenant cleanup behind a separate explicit platform-operator operation.
- [ ] Preserve operator-visible terminal errors for irrecoverable Runner workspace loss and avoid fabricating successful cleanup when acknowledgement is unavailable.
- [ ] Test active streams, concurrent operations, partial deletion, already absent resources, control-plane restart, Runner disconnect/reconnect, retry, expiry, and isolation between two tenants during cleanup.

### Task 7: Complete CLI and clean-install deployment workflows

Give operators and tenant controllers a source-free management surface. Production initialization creates only the platform authority; delegated credentials are created after startup through authenticated management operations.

- [ ] Add `secondbox` CLI command groups for platform-operator tenant management and tenant-controller subject, authority, usage, close, and cleanup workflows using the generated client.
- [ ] Keep human and structured output stable, show bearer tokens only after successful creation, and prevent session/config helpers from treating tenant-controller and application credentials as platform tokens.
- [ ] Generate and materialize only the platform token during a production installation; do not create an implicit tenant, subject, controller, or application authority.
- [ ] Make v0.6.0 validation and update refuse a v0.5.2-style manifest or deployment with a direct clean-reinstall diagnostic and no import or compatibility option.
- [ ] Keep development setup explicit: any sample tenant and credentials must be created by an observable post-start development step, not a runtime default or hidden bootstrap path.
- [ ] Test a clean source-free install through platform-token login, tenant/controller creation, controller login, subject/application credential creation, and an authenticated Sandbox request.

### Task 8: Ship the isolated agent Profile as a release-owned resource

Add a standard Profile for consumers that need compute and workspace capabilities but no outbound network. Keep it an ordinary immutable release-owned lineage selected explicitly by operators, with no reserved-name behavior or tenant-specific copies.

- [ ] Add the `agent-compartment-isolated` standard bundle and immutable Profile lineage with command, file, workspace, cancellation, and bounded lifecycle capabilities but no outbound destinations, DNS access, exposed Ports, or network gateway dependency.
- [ ] Extend standard-resource documents, validation, declarative application, installer selection, release manifests, artifact identity, and SDK-facing Profile inspection for the third explicit bundle.
- [ ] Preserve existing `agent-compartment` behavior and its dedicated gateway mapping for network-enabled installations; do not silently replace or mutate its lineage.
- [ ] Ensure tenant Profile ceilings and application grants can select the isolated Profile without creating per-tenant Profile copies.
- [ ] Add standard-resource tests for canonical digest stability, ordered lineage application, installed-prefix validation, immutable revision pinning, and explicit bundle selection.
- [ ] Add real network-policy coverage proving an isolated Sandbox cannot reach the configured Runner gateway, management networks, metadata endpoints, DNS resolvers, or arbitrary Internet destinations.
- [ ] Prove an isolated and a network-enabled Sandbox can run concurrently on the same qualified Runner without sharing network policy or credentials.
- [ ] Keep a tenant-aware shared egress capability outside this release and document that each current network-enabled RunnerPool maps to one trusted egress context.

### Task 9: Qualify and document the complete v0.6.0 release

Close the one pull request only after the full customer-shared path works through source-free artifacts on a clean deployment. Validate security properties across two tenants and ensure current architecture and operator documentation describes implemented behavior rather than prospective design.

- [ ] Add an end-to-end two-tenant scenario that creates controllers, subjects, and application authorities; runs same-named Sandboxes concurrently; and proves every read, mutation, execution, file, Port, and diagnostic boundary is tenant-scoped.
- [ ] Prove authority revocation, subject close, expiry reconciliation, quota release, durable cleanup, control-plane restart, and Runner reconnect while resources in the other tenant remain usable.
- [ ] Prove application credentials cannot call management, Profile mutation, aggregate timing, Runner administration, or another subject's routes, and that tenant controllers cannot call Sandbox routes.
- [ ] Run the isolated/network-enabled concurrent scenario on a qualified KVM Runner and capture bounded evidence without tenant identifiers in metric labels or secrets in artifacts.
- [ ] Update authorization, service-boundary, security, threat-model, recovery, deployment, backup, diagnostics, CLI, declarative-resource, downstream-integration, and release documentation to match the implemented contract.
- [ ] Remove prospective wording from [Customer-shared tenancy](../design/customer-shared-tenancy.md), reconcile linked design documents, and verify their claims against tests and operator commands.
- [ ] Stage v0.6.0 release artifacts and verify OpenAPI, Go SDK, TypeScript SDK, binaries, OCI images, standard bundles, source-free suite, SBOMs, attestations, and qualification evidence all carry one immutable release identity.
- [ ] Run every command in `Validation Commands`, resolve failures without weakening gates, and leave the branch ready for its single implementation pull request and subsequent release publication.
