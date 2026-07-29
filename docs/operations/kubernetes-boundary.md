# Kubernetes boundary

SecondBox v1 does not ship or qualify Kubernetes manifests.

The unprivileged control plane has no KVM, TUN/TAP, host-cgroup, host-path, or container-engine dependency, so operators may run it in Kubernetes using ordinary platform controls, external PostgreSQL, and an external S3-compatible Artifact store. The binary never receives or mounts a Runner Workspace and never gives object credentials to Runners.

An operator-authored workload must expose the HTTP port behind TLS and the Runner gRPC port without replacing client-certificate identity. It must inject the platform token and pre-shared Runner credential, mount the Runner CA certificate and server keypair read-only, provide TLS-verified PostgreSQL, preserve the log path, and configure probes and every quota/protocol/timing setting explicitly. Each `secondboxd` process validates and applies the embedded ordered database migration lineage under one PostgreSQL advisory lock before opening stores or listeners; a separate workload must not bypass or rewrite that ledger. The Runner CA private key can issue Runner identities and must remain outside the control-plane workload.

SecondBox does not ship Helm charts, Deployments, Services, Jobs, PodDisruptionBudgets, NetworkPolicies, or storage classes. An operator-authored Kubernetes deployment is therefore outside release qualification. In particular, rolling replica replacement, migration ordering, secret rotation, and gRPC connection draining have not been proven by manifests in this repository.

Firecracker runners require qualified Linux hosts with KVM, cgroup, networking, storage, and cleanup capabilities. The supported v1 runner deployment is a standalone binary or systemd-managed service on those hosts. A future runner DaemonSet or Kubernetes-native sandbox backend requires its own qualification and does not inherit support from the control plane's portability.

See [service boundaries](../design/service-boundaries.md), [runner protocol](../design/runner-protocol.md), and [Firecracker runtime](firecracker-runtime.md).
