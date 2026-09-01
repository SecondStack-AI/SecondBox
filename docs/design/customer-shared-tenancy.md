# Customer-shared tenancy

## Purpose

One SecondBox deployment serves multiple SecondStack installations inside one customer-operated trust domain.
The deployment is not owned by any one SecondStack installation, and it never serves unrelated customer trust domains.
The control plane and PostgreSQL hold shared management state, while one or more customer-operated Runners provide compute and durable workspace storage.

This document describes the implemented management and isolation contract for that shared deployment.
The Sandbox, Instance, Lease, Profile, RunnerPool, and Runner contracts remain as described in the other design documents.

## Deployment ownership

The customer operates SecondBox as independent infrastructure.
The unprivileged control plane, PostgreSQL, TLS ingress, operator secrets, backups, release state, and logs have their own installation root and lifecycle.
SecondStack installation, upgrade, rollback, and removal operations cannot create, update, restart, or delete these resources.

Runners remain separate privileged services on qualified Linux hosts.
They connect outbound to the control-plane Runner listener and receive no HTTP operator or tenant credential.
The control plane may share a physical host with another service, but this placement does not transfer lifecycle ownership to that service.

One customer trust domain may share Runner capacity between its tenants.
A Runner can inspect the workspaces and guest memory assigned to its host, so a Runner or RunnerPool is a customer-operated trust and capacity boundary rather than a cryptographic tenant boundary.
Customers that require physical separation use separate RunnerPools or Runner hosts.

## Management resources

`secondbox.tenants` stores one stable, uninterpreted tenant reference, at most one nullable operator-controlled egress-context name, lifecycle state, allowed Profile grants, allowed application scopes, aggregate quota, expiry policy, bounded metadata, revision, and timestamps.
A tenant represents one independent SecondStack installation in the qualified deployment flow.
An installation group such as a preview service can use one tenant and create one subject for each managed environment.

An egress-context name is an opaque routing identifier, not a hostname, address, CIDR, Tenant reference, gateway identity, or gateway-address digest. The one canonical syntax is 1 through 63 lowercase ASCII letters, digits, or hyphens, beginning and ending with a letter or digit: `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`. HTTP schemas, persisted values, Runner configuration and protocol, audit, and diagnostics use that same bound. SecondStack hostnames, proxy addresses, certificates, and network ranges remain outside the control plane.

`secondbox.subjects` stores a tenant-scoped subject reference, lifecycle state, subject quota, optional expiry, bounded metadata, revision, and timestamps.
The unique identity is `(tenant_ref, subject_ref)`.
An active subject may own Sandboxes and receive application authorities.
A closed subject cannot receive a new authority or admit another Sandbox operation.

`secondbox.tenant_controller_authorities` stores tenant-scoped management authorities.
`secondbox.application_authorities` replaces process-start configuration as the runtime source of application authentication.
Each application authority fixes one tenant, one subject, exact Sandbox scopes, and explicit Profile grants.
Authority records contain a public lookup identifier and a one-way verifier for a server-generated high-entropy secret.
SecondBox returns the complete bearer token only in the successful creation response and never returns it from a read, list, audit, or diagnostic surface.

Cross-resource references remain logical strings without PostgreSQL foreign keys.
Required uniqueness constraints protect tenant references, subject identity, authority identifiers, and token lookup identifiers.

## Authority hierarchy

The deployment-wide platform token remains the operator authority.
It creates and suspends tenants, selects or clears a Tenant's egress context, manages tenant-controller authorities, manages Profiles and RunnerPools, enrolls Runners, and inspects deployment-wide operations.
It never enters a SecondStack application container.

A tenant-controller authority is fixed to one tenant and has one code-owned permission set.
It can create, inspect, list, rotate, and revoke application authorities, create and close subjects, set subject quota within its tenant ceiling, and request cleanup for its own subjects.
It cannot select another tenant, change its tenant ceiling or egress context, grant an unapproved Profile or scope, administer Runners, or call ordinary Sandbox routes as another subject.
Management routes derive the tenant from the authenticated controller authority and do not accept a tenant assertion from request headers.

An application authority retains the existing fixed-reference request contract.
The caller presents its bearer token plus the exact tenant and subject headers bound to that token.
The HTTP layer rejects a mismatch before route handling and applies the stored scope and Profile grants.
Application authorities cannot call management, Profile mutation, aggregate timing, or Runner administration routes.

There is no configurable role language, nested organization model, credential inheritance, public self-registration, OAuth provider, or billing model.
The three authority kinds are explicit in the service and OpenAPI contract.

## Management API and authentication

Operator routes create, inspect, list, suspend, and reactivate tenants and create, inspect, list, rotate, and revoke tenant-controller authorities.
Tenant-controller routes create, inspect, list, close, and clean subjects and create, inspect, list, rotate, and revoke application authorities.
All management mutations use idempotency keys and optimistic resource revisions where an existing resource can change.

Authentication in `internal/api/http.go` resolves the presented token lookup identifier, verifies the secret in constant time, and checks persisted state and expiry on every newly admitted request.
The random secret has sufficient entropy for a fast cryptographic verifier; it is not a user password.
Revocation and expiry therefore require no configuration render or process restart.
An already admitted bounded operation can finish unless subject closure or cleanup cancels it.

The service writes an audit event for every management mutation and denial.
Audit records include the acting authority identifier, tenant, subject when present, operation, result, and bounded external correlation metadata.
They never contain token material.

If a client does not receive a credential creation response, it cannot recover the token.
It inspects the authority by its idempotency or external correlation identity, revokes that record, and creates a replacement.
SecondBox does not encrypt tokens for later retrieval.

## Quotas and admission

Tenant aggregate quota is enforced in the same transaction as subject quota and lifecycle admission.
It covers Sandboxes, active Instances, vCPU, memory, Snapshots, Port sessions, and concurrent data-plane operations.
It also limits active subjects and application authorities so a tenant controller cannot create unbounded management records.

A subject quota cannot exceed its tenant ceiling.
Concurrent reservations lock and evaluate tenant and subject quota in one stable order, so neither limit can be overcommitted.
One tenant cannot borrow unused capacity as an entitlement when its own quota is exhausted.
Runner storage-pressure admission remains the authority for retained workspace allocation because reflink sharing prevents exact unique-byte charging.
Profile workspace limits still bound each Sandbox.

Tenant suspension denies new tenant-controller and application admission but preserves resources.
Destructive tenant cleanup is a separate explicit operator operation.

## Subject closure and cleanup

An application lifecycle closes a subject in this order:

1. Stop the upstream application from admitting new work.
2. Revoke every application authority for the subject.
3. Change the subject state to closed.
4. Submit one durable subject-cleanup Operation.

The cleanup Operation cancels active sessions and operations, stops and deletes Sandboxes, releases Leases and quota reservations, and removes Runner-owned workspace state through the existing acknowledged Runner protocol.
The Operation remains queryable until all work completes or an operator-visible terminal error occurs.
Retries continue the same operation identity instead of starting untracked deletion work.

Authority and subject expiry deny new work independently of the upstream controller.
The control-plane reconciler closes expired subjects and advances their cleanup Operations so a failed external lifecycle controller cannot retain preview resources indefinitely.

## Profiles and network isolation

Tenant Profile grants are ceilings, and application Profile grants are subsets.
Applications never create tenant-specific copies of release-owned Profiles.

The release-owned `agent-compartment-isolated` Profile permits command, file, and workspace operations with no outbound network destinations.
Ephemeral previews use this Profile.
They may share a Runner with a network-enabled tenant because the Runner compiles network policy for each Sandbox and the isolated Profile cannot reach a gateway present on the host.

A ProfileRevision's required `network.requiresTenantEgressContext` Boolean states whether Sandbox creation needs the Tenant's egress context. It is required and has no default. The release-owned `agent-compartment` and `durable-coding` network-enabled Profiles state `true`; `agent-compartment-isolated` states `false`. The behavior is never inferred from a destination domain suffix.

Sandbox creation copies the Tenant value into an immutable Sandbox pin. A later operator change to the Tenant affects only new Sandboxes. A Profile that requires a context fails closed when the Tenant has none; a Profile that does not require one sends no context to the Runner even if its Tenant has one. Subjects, end users, application requests, guest headers, and guest source addresses neither select nor override the pin. Application and tenant-controller authorities have no context-capacity or context-selection API.

A Runner may advertise several context names. Each advertised name selects one Runner-local mapping from logical gateway names to addresses, and one existing `agent-runner-gateway` instance serves one context and authenticates to exactly one SecondStack installation's egress proxy. Logical gateway mappings remain network-policy authorization rather than guest DNS: the DNS proxy does not synthesize or rewrite them, and Agent Platform continues to inject its installation's Runner-local gateway address through its existing proxy variables. Gateway and egress-proxy protocols do not change.

The context pin selects the routing indirection, not a digest of the mapping. Addresses may repeat within or across contexts. Operators create distinct addresses where isolation requires them; SecondBox does not impose address uniqueness. Replacing or removing a mapping requires draining the Runner, stopping its active Sandboxes for that context, and restarting it. This release has no dynamic gateway health discovery or live context remapping.

Missing Tenant context, Runner advertisement, Runner-local mapping, or assignment context fails closed. There is no default context, cross-context retry, automatic reassignment, or fallback to the v0.7.2 global gateway mapping.

## v0.7.2 recreation boundary

Tenant-aware egress is a clean-recreation boundary from v0.7.2. Operators quiesce every consuming application, retire every old Sandbox, stop and replace the old deployment, initialize the new release, and recreate its resources and Sandboxes. The new release does not decode an omitted context requirement into historical Profile behavior, preserve legacy assignments, or provide a Sandbox migration operation. A coordinated v0.7.2 database, Runner identity, and complete Workspace backup is a rollback input only; it is never combined with new-release state.

## Availability and recovery

The minimum production topology has one control-plane process, one PostgreSQL instance, external TLS, and one or more remote Runners.
PostgreSQL failure makes readiness fail and stops admission.
Control-plane restart preserves desired state and lets Runners reconnect.
Runner failure leaves its Sandboxes unavailable instead of moving them without their workspaces.
Loss of a Runner workspace filesystem loses the Sandboxes homed there.

Operators back up PostgreSQL and each Runner's stable identity plus complete workspace filesystem as different recovery units.
Metrics retain fixed-cardinality labels; tenant, subject, authority, and Sandbox identifiers belong in audit and diagnostic records.

See [service boundaries](service-boundaries.md), [profiles and authorization](profiles-and-authorization.md), [security](security.md), [workspace durability](workspace-durability.md), and [deployment](../operations/deployment.md).
