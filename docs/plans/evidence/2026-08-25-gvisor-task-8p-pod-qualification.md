# gVisor Task 8P privileged-pod mechanism qualification

Date: 2026-08-25 (UTC)

Result: pass — every host-proven mechanism works inside a privileged containerd-managed pod on
a Kubernetes node without KVM. The pod track proceeds to packaging (Task 9P).

## Pod environment

- Cluster: K3s `v1.36.3+k3s1` (single node `gvisor-qual`, no taints), Kubernetes server
  `v1.36.3+k3s1`, downloaded against the pinned SHA-256
  `2f98a9f8fe5782479ee2d54e70a1b10a7f6fd4cae8d38ed3098452dc6eed76b5`.
- Container runtime: `containerd://2.3.2-k3s2` with the systemd cgroup-v2 driver.
- Node: the Task 7H no-KVM QEMU guest — Debian 13, kernel `6.12.101+deb13-cloud-amd64`, booted
  with `-cpu host,-vmx,-svm`; no `/dev/kvm`, no `vmx`/`svm` flags.
- Pod security context: `privileged: true`, default (pod-scoped) PID and network namespaces,
  host cgroup namespace (the containerd default for this configuration), CPU/memory requests
  equal to limits (4 CPU / 4 GiB) standing in for the node's declared sandbox budget.
- Inputs by hostPath: the pinned gVisor build directory (read-only), the compiled
  `internal/gvisor` test binary (read-only), and a node-local Btrfs reflink volume.

`scripts/test-gvisor-pod.sh` (target `just test-gvisor-pod`) reproduces the run: it applies the
qualification pod, executes the complete backend qualification suite inside it, samples the
node's cgroup table mid-flight, and fails on host mount leaks or leftover sandbox cgroups.

## Outcomes

1. **runsc launch under the pod** — the full attachment, backend, and network qualification
   suites passed inside the pod (`PASS`, exit 0): sandbox boot on systrap, agent negotiation
   over gofer Unix sockets, buffered/streaming exec, PTY, ports, idempotent start, fence
   rejection, kill reaping, and crash reconciliation all behaved exactly as on the host
   profile, inside the pod's own PID namespace.
2. **Nested cgroup enforcement** — observed mid-suite from the node:
   `/sys/fs/cgroup/kubepods.slice/kubepods-pod<uid>.slice/secondbox-gvisor/<instance>` with
   `cpu.max=100000 100000` and `memory.max=268435456`, i.e. per-sandbox limits nested inside
   the pod's slice so the pod budget caps the sum. This run drove two backend changes: the
   sandbox cgroup parent is now resolved as the nearest ancestor of the runner's own cgroup
   that delegates the cpu and memory controllers (the pod slice in Kubernetes; the visible
   root elsewhere), and teardown plus readiness now sweep Instance cgroups, because a forced
   sandbox kill leaves the directory behind (removal retries through the kernel's transient
   EBUSY while sandbox processes drain).
3. **Loop-mount containment** — Workspace attachment, crash reconciliation, and lock recovery
   passed inside the pod; the node's mount table showed no Workspace mount at any point
   (supervisor-private mount namespaces hold inside the pod's scope).
4. **Network path** — the per-Instance namespace, routed veth, DNS proxy, and the full policy
   matrix (deny-all, exact allow-list, protected classes, pinned domain rules) passed inside
   the pod's network namespace. NAT egress through the pod's `eth0` was proven separately: a
   nested instance-style namespace reached a real external destination through the backend's
   masquerade shape (an HTTP response returned end to end through pod and node NAT).

The host profile was re-qualified after the cgroup changes: the same suite passed twice in a
row directly on the node, with the sandbox cgroups nesting under the invoking session's slice
and no residue between runs.
