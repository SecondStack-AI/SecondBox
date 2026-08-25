# gVisor Task 7H host vertical-slice qualification

Date: 2026-08-25 (UTC)

Host: dedicated no-KVM QEMU guest (`Linux 6.12.101+deb13-cloud-amd64`, Debian 13, `linux/amd64`,
booted with `-cpu host,-vmx,-svm` so the guest exposes no `/dev/kvm` and no virtualization CPU
flags)

Source commit: `d1787b517394b4cc1fe6a7cabf4f0f249d48b6fb`

Result: pass

The final scenario run started from a clean repository and recorded `repositoryDirty: false`.
Every build, artifact, test, and scenario was local to the operator's machines; no external
repository write occurred from the qualification host.

## Host gate

- `/dev/kvm` is absent and the CPU exposes neither `vmx` nor `svm`; the wrapper refuses hosts
  that expose `/dev/kvm`.
- `/dev/loop-control` is present; the Workspace root is Btrfs (loop-backed volume at
  `/var/lib/secondbox-reflink`) and passed real reflink qualification.
- `nftables` and `iproute2` are installed; Docker server is `29.7.2` with its default
  drop-policy firewall active, exercising the `DOCKER-USER` admission path.
- Go is `go1.25.12 linux/amd64`.

## Exact local identities

```text
runsc release              20260817.0
runsc SHA-512 (leading 32) 84936438d583ec976800f464e75a83e1
guest agent SHA-256        9429066d1b30c08b95794ec84089231b2257b090a55b7ee25bece49a30645eea
flat root                  pinned alpine extract (digest recorded in the derived materialization)
materialization digest     sha256:6ba6deda93b10023f4fd672890f45fcc8e36fe72ee98e9bb9b9f1b87ed2ca3f3
```

The full runsc SHA-512 pin lives in `runner/scripts/fetch-runsc.sh`; the wrapper refuses a
binary whose digest differs.

## Scenario suite

`just test-scenario-gvisor` drove `scripts/test-scenario.sh` with the normal control plane,
authenticated runner protocol, both data-plane transports, and durable WorkspaceStore against
the privileged gVisor runner container. 22 scenarios passed in 234 seconds, covering:

- durable Sandbox lifecycle with buffered and streaming exec, binary files, PTY, and Port relay;
- stop/restart with Workspace preservation, Snapshot create/mutate/restore;
- stale-generation rejection during active work, runner loss with mount/loop/lock reconciliation;
- digest, sealed-pool, and unsupported `snapshot_resume` rejections before compute creation;
- two concurrent Instances with independent operations, streams, mounts, and terminal events;
- deny-all plus exact allow-list enforcement including a pinned domain rule fetching a real
  external destination through the routed-veth NAT path and the Docker firewall;
- Workspace relocation between two gVisor runners sharing the host network namespace on
  distinct network profiles.

Machine-readable evidence:

- `2026-08-25-gvisor-task-7h-linux-scenario.json` (suite `test-scenario-gvisor`,
  `repositoryDirty: false`, KVM neither required nor present)
- `2026-08-25-gvisor-task-7h-cold-starts.json`

## Cold-start observations (no gate)

30 cold boots, start-to-ready milliseconds: p50 483.8, p95 645.6, max 966.4. Largest stages at
p50: guest negotiation 173.0, artifact verify 133.1, ready 88.7, network setup 50.9. Peak
supervised helper RSS: p50 14148 KiB, max 14280 KiB.
