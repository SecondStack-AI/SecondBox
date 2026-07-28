# Sandbox Service Development Rules

- Sandbox Service owns durable Environments, workspaces, generations, leases, snapshots, artifacts, lifecycle policy, and replaceable compute Instances in the `sandbox` PostgreSQL schema.
- Public and Sandbox Host contracts use Sandbox domain language. Firecracker, KVM, container, vsock, host path, and network implementation details are forbidden outside a host adapter.
- The control-plane process is unprivileged. Only the separately deployed Sandbox Host may perform privileged compute, filesystem, and network operations.
- Cross-service references are logical strings. Do not add PostgreSQL foreign keys or CHECK constraints.
- Every required runtime setting comes from the root environment flow. Application code and deployment templates must not provide defaults.
- Run `just test` and `just verify-generated` before handing off changes.
