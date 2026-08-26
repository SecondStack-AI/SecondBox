# gVisor Task 9P pod packaging and spike close-out

Date: 2026-08-26 (UTC)

Source commit: `c5a6bd676` (clean tree; both scenario evidence files record
`repositoryDirty: false` at this commit, re-run after the branch was rebased onto main)

Result: pass — the spike closes with both environments qualified.

## What shipped

- `runner/Dockerfile.gvisor`: the gVisor runner image with the pinned `runsc` release
  (SHA-512 verified by `runner/scripts/fetch-runsc.sh` inside the build) and the
  ext4/loop/nftables toolchain. CI builds it and records the image digest
  (`gvisor-runner-image` job). Verified locally: the image carries
  `runsc release-20260817.0` and a functional runner binary.
- `runner/deploy/gvisor-runner-pod.yaml`: the reference privileged, node-pinned runner pod —
  dedicated tainted node pool, node-local reflink volume, per-runner identity Secret, pod
  budget equal to the node's sandbox budget, proxied-only data plane by default with hostPort
  as the qualified direct-transport option. Documented as the qualified Kubernetes surface in
  the Kubernetes-boundary and gVisor runtime documents.
- Pod scenario placement: `just test-scenario-gvisor-pod` runs the identical scenario driver
  with the runner as a privileged pod (postgres and control plane stay in Compose), through
  the same service-control seam the native macOS placement uses.

## Environment suites (identical scenario driver, clean tree)

On the no-KVM QEMU node (Debian 13, kernel `6.12.101+deb13-cloud-amd64`, K3s `v1.36.3+k3s1`,
containerd `2.3.2-k3s2`):

- `test-scenario-gvisor` (host placement): 22 scenarios passed —
  `2026-08-25-gvisor-task-9p-linux-scenario.json`. 30 cold starts: start-to-ready p50 413.2 ms,
  p95 468.8 ms — `2026-08-25-gvisor-task-9p-cold-starts.json`.
- `test-scenario-gvisor-pod` (pod placement): the same 22 scenarios passed, including
  Snapshot/restore, runner-kill reconciliation, concurrency, the rejection matrix, the network
  policy matrix with real external egress through pod and node NAT, and Workspace relocation
  between two runner pods on distinct network profiles —
  `2026-08-25-gvisor-task-9p-pod-scenario.json`. 30 cold starts inside the pod: p50 398.7 ms,
  p95 456.8 ms — `2026-08-25-gvisor-task-9p-pod-cold-starts.json`.
- `test-gvisor-pod` and the backend qualification suites (`TestQualified|TestAttachment`) also
  passed at the same commit, with sandbox cgroups observed nested at
  `kubepods-pod<uid>.slice/secondbox-gvisor-p0/<instance>` under per-sandbox limits.

Qualification surfaced two fixes recorded on this branch: sandbox cgroup directories are
scoped per network profile (runners sharing a host and one delegating cgroup ancestor must not
sweep each other's live Instances), and the pod service control pins kubectl's discovery cache
outside the repository so evidence stays commit-exact.

## Existing backends unchanged

- `just test-firecracker`: passed on a real-KVM host (deimos).
- `just test-workspacestore-linux` and `just test-microsandbox-linux`: passed on deimos
  against a freshly rebuilt pinned Microsandbox bundle (helper, agentd, libkrunfw, flat root).
- `just test-scenario` (Firecracker): 22 of 23 scenarios passed on deimos;
  `TestScenarioNetworkPolicyDenyAndAllowList` failed with the guest unable to resolve the
  allow-listed domain. The identical focused test fails identically at the pre-rebase branch base commit
  `dd3f8e76e` on the same host, so the failure is environmental (this host also runs a live
  SecondBox deployment) and not introduced by this branch.
- `just verify-generated`, `just lint`, `just test`, `just test-contract`,
  `just test-compose`, `just test-image-policy`, and `scripts/build-artifacts.sh` passed
  locally. `scripts/test-sdk-packages.sh` (and therefore the `test-non-kvm` aggregate) fails
  on this workstation because npm 12 changed the `npm pack --json` output shape; that is
  pre-existing environment drift unrelated to this branch (CI runs an older npm and passes).

## Spike posture

The gVisor backend remains experimental. Removing that label requires a later decision with
production distribution, sustained stress, upgrade/recovery evidence, and an explicit support
policy; none of those are spike exit criteria, and `snapshot_resume` stays absent from gVisor
capabilities.
