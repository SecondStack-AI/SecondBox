# Kubernetes Sandbox Host Boundary

SecondStack does not currently ship a supported Kubernetes deployment for Sandbox Service or Sandbox Host. The supported production boundary is the root Compose deployment, where:

- Sandbox Service is an unprivileged Go control plane with exclusive access to the `sandbox` schema.
- Sandbox Host is the only process with KVM, TUN, cgroup, network-administration, workspace-image, jailer, or launcher authority.
- Agent Service is an unprivileged Sandbox client. It stores logical Environment, lease, generation, workspace-version, snapshot, and Artifact references only.
- Sandbox Service owns recovery of its schema together with opaque Host workspace, checkpoint, and Artifact state.

A future Kubernetes topology must preserve those process and authority boundaries. It must deploy Sandbox Service independently from the privileged Host, authenticate and generation-fence every Host command, keep Agent Service off Host sockets and privileged mounts, and provide storage fencing that prevents two Hosts from attaching the same mutable workspace state.

Do not adapt the Compose launcher socket into a public Service, mount `/dev/kvm`, `/dev/net/tun`, cgroups, Host workspace state, or a container-engine socket into Agent Service, or claim transparent workspace migration or active/active Host operation. Kubernetes sandbox capability remains unavailable until checked-in manifests and live qualification tests prove the same isolation, generation, recovery, and storage invariants as the supported Compose topology.

See [Firecracker Runtime Operations](firecracker-runtime.md) for Host requirements and [SecondStack deployment artifacts](../../../../agent-manager/docs/operations/secondstack-artifacts.md) for rendered-Compose validation.
