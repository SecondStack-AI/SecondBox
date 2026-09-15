# Documentation

## Start here

- [Project overview and CLI examples](../README.md)
- [Guided single-host installation](operations/guided-single-host-install.md)
- [Deployment configuration and clean initialization](operations/deployment.md)
- [SDK, CLI, and Flue integration](operations/sdk-cli-and-flue.md)
- [Downstream release integration](operations/downstream-release-integration.md)

## Architecture and contracts

- [Service boundaries](design/service-boundaries.md), [domain and lifecycle](design/domain-lifecycle.md), and [recovery](design/recovery-and-reconciliation.md)
- [API conventions](design/api-conventions.md) and [SDK operation matrix](design/consumer-operation-matrix.md)
- [Profiles and authorization](design/profiles-and-authorization.md), [customer-shared tenancy](design/customer-shared-tenancy.md), and [configurable limits](design/configurable-limits.md)
- [Workspace durability](design/workspace-durability.md)
- [Networking and ports](design/networking-and-ports.md), [direct data plane](design/direct-data-plane.md), [runner protocol](design/runner-protocol.md), and [guest protocol](design/guest-agent-protocol.md)
- [Security model](design/security.md) and [threat model](design/threat-model.md)

The canonical public schema is [OpenAPI](../contracts/openapi/v1/secondbox.openapi.json).
Design documents describe the implementation in this checkout; release-specific
compatibility boundaries belong in the release notes.

## Operations

- Runners: [Firecracker](operations/firecracker-runtime.md), [gVisor](operations/gvisor-runtime.md), [experimental Microsandbox on macOS](operations/microsandbox-macos.md), and [Kubernetes boundary](operations/kubernetes-boundary.md)
- Storage: [backup and restore](operations/backup-and-restore.md), [runner decommissioning](operations/runner-decommissioning.md)
- Configuration: [declarative resources](operations/declarative-resources.md), [CLI output](operations/cli-output-contract.md)
- Diagnostics: [observability](operations/observability-and-diagnostics.md), [lifecycle benchmarks](operations/lifecycle-benchmark.md), [stress qualification](operations/stress-qualification.md), [multiple runners](operations/multirunner-qualification.md)
- Validation and releases: [scenario qualification](operations/scenario-qualification.md), [release operator setup](operations/release-operator-setup.md), [distribution](operations/release-distribution.md), [microVM image pipeline](operations/microvm-image-pipeline.md)

## History and unfinished work

[Plans](plans/README.md) separates open work from archived implementation records
and qualification evidence. Archived plans, incident reports, and measurements
record their original commits and environments; they are not deployment runbooks
or current performance guarantees. [Release notes](releases/) and the
[changelog](../CHANGELOG.md) record version-specific changes.
