# Microsandbox Task 0L host-readiness evidence

Date: 2026-08-13

Corrected at: 2026-08-13T11:07:25-04:00

SecondBox revision: `d5eb9a5c2c76dde056e0277dc42e88e6fe304e9b`

## Correction

An earlier check incorrectly treated `/dev/kvm` being absent from the agent container as evidence
that the deimos host lacked KVM. That conclusion was false: the bb-managed worktree execution
container has a narrower device namespace than its host.

The authoritative host-daemon check shows that deimos exposes `/dev/kvm`, and the host user belongs
to the `kvm` group. The hard Task 0L Linux feasibility gate is therefore not blocked on host
availability.
This document supersedes the deleted no-go record.

## Qualified host candidates

| Role | Host evidence |
| --- | --- |
| Linux KVM | `deimos`: Linux `7.1.4-arch1-1`, x86_64; `/dev/kvm` is `crw-rw-rw- root:kvm`; host user is in group `kvm` |
| Apple Silicon | `mini1`: Darwin `24.1.0`, Apple M1 arm64; Hypervisor.framework present; `kern.hv_support=1`; APFS |
| Additional Apple Silicon | `callisto`: Darwin `27.0.0`, arm64 |

The local worktree is still the only authoritative implementation checkout. Real-host probe
commands must execute through explicit bb host-daemon terminals so the deimos KVM device and macOS
Hypervisor.framework are visible.

## Bounded commands and outcomes

```text
$ bb terminal create --machine deimos --command 'uname -a' --attach
Linux deimos 7.1.4-arch1-1 ... x86_64 GNU/Linux

$ bb terminal create --machine deimos --command "stat -c '%A %U %G %t:%T %n' /dev/kvm" --attach
crw-rw-rw- root kvm a:e8 /dev/kvm

$ bb terminal create --machine deimos --command 'id' --attach
uid=1000(sasha) gid=1000(sasha) groups=...,991(kvm),...

$ bb terminal create --machine mini1 --command 'uname -a' --attach
Darwin mini1.wwh.home.arpa 24.1.0 ... RELEASE_ARM64_T8103 arm64

$ bb terminal create --machine mini1 --command 'sysctl kern.hv_support' --attach
kern.hv_support: 1

$ bb terminal create --machine mini1 --command 'diskutil info /' --attach
File System Personality: APFS
Allocation Block Size: 4096 Bytes
```

## Gate status

Task 0L is **in progress**, not passed. Linux host availability is established, but none of the
mandatory Microsandbox/libkrun Linux mechanism proofs has completed yet. Tasks 1L through 7L remain
closed until every Task 0L checkbox succeeds on deimos. Tasks 8M through 10M remain closed until the
complete Linux end-to-end gate in Task 7L passes.

The evaluated Microsandbox revision does not expose the two ext4 APIs required by the reviewed
gate: formatting an already-open file with a caller-supplied UUID and safely changing the UUID of a
clone. The gate requires a reviewed dependency change to land and be pinned before the standalone
probe continues. The required changes will be maintained as a SecondBox-owned local patch and build
input. No external contribution path is authorized. No Tasks 1L through 10M implementation has
started.

## Dependency investigation and prerequisite

| Item | Exact evidence |
| --- | --- |
| Evaluated Microsandbox | `5b335537afad433ad2c0308cb54de13b7015b4e7` (`v0.6.8-48-g5b335537`) |
| Evaluated libkrun crates | `msb_krun = 0.1.30`; `msb_krun_utils = 0.1.30` from the evaluated lockfile |
| Missing surface | Existing formatter accepts a path and random UUID; its explicit-UUID path is private. No safe public clone-UUID rewrite exists. |
| Local prerequisite experiment | Commit `a6807d1d2454d0b62dc1818d57c1f69012360355` in a temporary fork clone; it is not an approved SecondBox dependency pin |
| External publication correction | An external PR was opened without approval and is closed. No comments or reviews were added. |
| Safety mechanism | New images use ext4 `INCOMPAT_CSUM_SEED`, keeping metadata checksums independent of the filesystem UUID. The rewrite validates a clean formatter image, updates the empty journal and backup superblocks, flushes, and revalidates. Legacy images remain growable but are rejected by the UUID rewrite. |

Local prerequisite experiment validation at commit `a6807d1d2454d0b62dc1818d57c1f69012360355`:

- `cargo fmt --all -- --check`: passed.
- `cargo test -p microsandbox-image --lib`: 204 passed, 0 failed, 4 ignored.
- `cargo clippy -p microsandbox-image --all-targets -- -D warnings`: passed.
- `cargo test -p microsandbox-image ext4::resizer::tests::test_e2fsck --lib -- --ignored --nocapture`:
  4 passed, including rewritten-UUID metadata verification by `e2fsck`.

## Repository validation already completed

- `just lint`: passed with task-scoped Go and golangci-lint caches.
- `just verify-generated`: passed with task-scoped Go and npm caches, including 133 passing
  TypeScript SDK tests and dry-run package validation.
- `just test-contract`: passed for the Go SDK, public contracts, runner protocol, guest, and
  Firecracker contract packages.
- `just test`: stopped at its explicit prerequisite because `SECONDBOX_TEST_DATABASE_URL` was not
  supplied for a disposable PostgreSQL test database; no implicit database authority was added.
