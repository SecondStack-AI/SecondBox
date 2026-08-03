# Multi-runner qualification

`just test-multirunner` has two deliberate layers:

1. A PostgreSQL-backed two-fake-runner integration test proves deterministic initial placement, capacity-aware distribution, durable workspace-create receipts, stable ordinary-lifecycle home assignment, and typed unavailability without automatic relocation during drain or runner loss.
2. An opt-in filesystem qualification test initializes two independent `WorkspaceStore` roots, executes each store's real `FICLONE` readiness probe, creates one ext4 Workspace per root, and proves reconciliation never observes the other runner's local data.

The ordinary command requires only the disposable PostgreSQL test database used by the rest of the Go suite:

```sh
export SECONDBOX_TEST_DATABASE_URL='postgresql://secondbox:password@127.0.0.1:5432/secondbox_test?sslmode=disable'
just test-multirunner
```

The filesystem layer skips unless its fixture is explicitly configured. A qualified run must fail rather than skip, so set all five variables:

```sh
export SECONDBOX_REQUIRE_QUALIFIED_MULTIRUNNER=1
export SECONDBOX_MULTIRUNNER_RUNNER_A_ID='runner-qualified-a'
export SECONDBOX_MULTIRUNNER_RUNNER_B_ID='runner-qualified-b'
export SECONDBOX_MULTIRUNNER_FILESYSTEM_A='/srv/secondbox-qualification/runner-a'
export SECONDBOX_MULTIRUNNER_FILESYSTEM_B='/srv/secondbox-qualification/runner-b'
just test-multirunner
```

The two identity values must be the stable logical identities of the fixture runners. The two filesystem paths must be distinct existing directories on XFS or Btrfs mounts. They are parent directories used only to create operation-scoped temporary WorkspaceStore roots; never point either variable at a live runner's `SECONDBOX_RUNNER_WORKSPACE_ROOT`.

For the final two-machine qualification, run this target from the qualification coordinator after mounting or otherwise exposing dedicated scratch directories from both runner hosts. Record the printed `findmnt` lines, source commit, and Go version together with the two runner build versions. Separately run `just test-firecracker` on each host using its real qualified KVM, cgroup, network, jailer, and reflink configuration. A successful local fake run is not evidence for the qualified KVM/XFS or KVM/Btrfs gate.

## Recorded single-host evidence

The 2026-07-29 qualification on host `deimos` proves the complete local
Firecracker lifecycle and two-root isolation on one Btrfs/KVM machine.

- Source base: `c7b9bbe06ecaa5dae911081cd8ed9285f8a386e6`; the exact working-tree
  implementation also passed every non-KVM gate from disposable clean
  validation commit `239574b51c3b02cdbd97aceaef22d7ed1f4d32fc`.
- Toolchain: `go version go1.26.5 linux/amd64`.
- Workspace mount:
  `/mnt/agents /dev/mapper/990pro[/@agents] btrfs rw,noatime,ssd,space_cache=v2,subvolid=260,subvol=/@agents`.
- KVM device: character device `/dev/kvm`, major/minor `10,232`, readable and
  writable by the qualification user.
- Firecracker: `v1.16.1`.
- Signed artifact version:
  `secondbox-local-cow-c7b9bbe-20260729-ext4-entropy2`.
- Manifest SHA-256:
  `9eb58e3d8f3a030b1522c88b450de09702e002da85dcf8185659da08a5122b60`.
- Kernel SHA-256:
  `ea5e7d5cf494a8c4ba043259812fc018b44880d70bcbbfc4d57d2760631b1cd6`.
- Rootfs SHA-256:
  `610ced0455903857459ffde52ceb6bf3430cdcd00c772edd156158b0ee25de76`.
- Shared-image SHA-256:
  `7f3257f4bb0137811c53b3ca21111cd65bc63eeda0c8b25fd0672e176d57198f`.
- `just test-firecracker` passed the boot, Snapshot restore, and all-stop-path
  lifecycle matrix in 48.588 seconds after removing the final legacy
  no-attachment workspace constructor.
- Qualified `just test-multirunner` passed with distinct logical identities
  `runner-deimos-a` and `runner-deimos-b`, distinct scratch roots, two real
  `FICLONE` probes, and isolated Workspace reconciliation.

Both qualified scratch roots above resolve to the same Btrfs mount on `deimos`.
No second qualified runner machine or XFS-backed KVM host was available for
this run. The project owner accepted this single-host evidence and explicitly
waived the independent second-host run for the local-COW implementation plan's
closeout on 2026-07-29. This record does not claim that a two-machine
qualification occurred.
