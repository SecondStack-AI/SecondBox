# Kubernetes boundary

SecondBox v1 does not ship or qualify Kubernetes manifests.

The unprivileged control plane has no KVM, TUN/TAP, host-cgroup, host-path, or container-engine dependency, so operators may run it in Kubernetes using ordinary platform controls, external PostgreSQL, and an external S3-compatible store. The binary composes that store into checkpoint and Artifact lifecycle operations without giving object credentials to Runners.

An operator-authored workload must expose the HTTP port behind TLS and the Runner gRPC port without replacing client-certificate identity. It must inject the bootstrap and enrollment hash secrets, mount the Runner CA private key and server private key read-only, provide TLS-verified PostgreSQL, preserve the log path, and configure probes and every quota/protocol/timing setting explicitly. Each `secondboxd` process validates and applies the embedded ordered database migration lineage under one PostgreSQL advisory lock before opening stores or listeners; a separate workload must not bypass or rewrite that ledger. The Runner CA private key can issue workload-host credentials and belongs in a tightly controlled secret-management boundary, not a ConfigMap or image layer.

SecondBox does not ship Helm charts, Deployments, Services, Jobs, PodDisruptionBudgets, NetworkPolicies, or storage classes. An operator-authored Kubernetes deployment is therefore outside release qualification. In particular, rolling replica replacement, migration ordering, secret rotation, and gRPC connection draining have not been proven by manifests in this repository.

Firecracker runners require qualified Linux hosts with KVM, cgroup, networking, storage, and cleanup capabilities. The supported v1 runner deployment is a standalone binary or systemd-managed service on those hosts. A future runner DaemonSet or Kubernetes-native sandbox backend requires its own qualification and does not inherit support from the control plane's portability.

See [service boundaries](../design/service-boundaries.md), [runner protocol](../design/runner-protocol.md), and [qualification gates](qualification.md).
