# Microsandbox Task 0L host-readiness evidence

Date: 2026-08-13

Corrected at: 2026-08-13T11:07:25-04:00

SecondBox checklist baseline: `4dd8dd255a012af14234b8d6037a8f409fcefbcb`

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

Task 0L is **GO / passed**. Every mandatory Linux mechanism proof completed on deimos from the
checked-in local builder and qualification target. Tasks 1L through 7L may now proceed in order.
Tasks 8M through 10M remain closed until the complete Linux end-to-end gate in Task 7L passes.

No dependency source or artifact was published. The evaluated change is a SecondBox-owned local
patch and build input only. No external contribution path is authorized.

Task 5L expanded that same local patch solely to preserve signal termination in agentd. The final
patch/tree identities in the table below therefore supersede the smaller Task 0L build input, while
the ext4 descriptor, VM lifecycle, and network-policy feasibility conclusions remain unchanged.
The expanded input was rebuilt locally and requalified on deimos through the complete Task 5L KVM
data plane; no external repository was modified.

## Dependency investigation and prerequisite

| Item | Exact evidence |
| --- | --- |
| Evaluated Microsandbox | `5b335537afad433ad2c0308cb54de13b7015b4e7` (`v0.6.8-48-g5b335537`) |
| Evaluated libkrun crates | `msb_krun = 0.1.30`; `msb_krun_utils = 0.1.30` from the evaluated lockfile |
| Missing surface | Existing formatter accepts a path and random UUID; its explicit-UUID path is private. No safe public clone-UUID rewrite exists. |
| Local prerequisite experiment | Commit `a6807d1d2454d0b62dc1818d57c1f69012360355` in a temporary fork clone; it is not an approved SecondBox dependency pin |
| External publication correction | An external PR was opened without approval and is closed. No comments or reviews were added. |
| Safety mechanism | New images use ext4 `INCOMPAT_CSUM_SEED`, keeping metadata checksums independent of the filesystem UUID. The rewrite validates a clean formatter image, updates the empty journal and backup superblocks, flushes, and revalidates. Legacy images remain growable but are rejected by the UUID rewrite. |

## Final pinned inputs and local build

| Input | Exact evidence |
| --- | --- |
| Microsandbox source | commit `5b335537afad433ad2c0308cb54de13b7015b4e7`; tree `dc506dffd600fcea281bd4ebfc924e1b31afcb2a`; Apache-2.0 |
| Local patch | `runner/microsandbox-patches/0001-explicit-ext4-uuid-fd-api.patch`; SHA-256 `f38294823f2c8e3b8e7918a8c58b48b0c9c7c521874add5d5985af3d4134eb7c`; patched tree `2bb82ba33e2175cd7574ffa6d058c6968453fa4b` |
| Microsandbox lock | SHA-256 `7827c5aad40cfc4ab36be6aba3bc4c0d923e525c50fc4b54741776bcf13b95c8` |
| Probe lock | SHA-256 `95f0107a1c27f7ad079012a919213207b4256950b73aec33ee624ed33c4638a7` |
| libkrun crates | `msb_krun = 0.1.30` SHA-256 `b57b2304dc1cef25b7cdd93be44c6515a97c90e00308b7d35eabc4fe27b02af5`; `msb_krun_utils = 0.1.30` SHA-256 `5f4f682dec7289463f89adfd1df7605a425c069d238265496a99dbff921075a9` |
| libkrunfw | commit `21cb6dce19a615f63e41ecb913334d18560c1364`; version `5.6.1`; library LGPL-2.1-only; bundled kernel and patches GPL-2.0-only |
| Linux bundle | Linux `6.12.99`; tarball SHA-256 `194eef900ade82df74ed1d695daa45d03ee4bb415cae4f936a3dbaab2dbbb951` |
| Agent builder | `rust:alpine@sha256:3c38f3f82c2f3d73da3b38e18d279393a04cb43ddded0e35088a8c3324d40900`; evaluated Cargo lock retained |
| Guest fixture | `alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce` |

The deterministic entry point was:

```text
just build-microsandbox-probe-linux \
  /tmp/secondbox-msb-base-env4w9 \
  /home/sasha/.bb/thread-storage/microsandbox-build-task0l-final
```

It rejected the wrong revision `a6807d1d2454d0b62dc1818d57c1f69012360355` and a dirty source checkout
before creating output. It also verifies the exact source tree, patch, both lockfiles, initialized
libkrunfw submodule, and kernel tarball. Cargo builds use `--locked`; host Cargo builds are offline.
The agent is built locally from the pinned container and lock. The command performs no push or
external repository write.

Build host tools were rustc `1.95.0`, cargo `1.95.0`, Docker client/server `29.6.2`, and uv
`0.9.21`. Final artifact SHA-256 values were:

```text
agentd      dcf2b4e76335ae77566997de93dbddefe659cc6d99dbed2c53117f9f001dd6df
msb         7b3660da494a12e69e4e160a455d28b046ee7a4901475f462999a26f793dc7fd
libkrunfw   858bfd9a63236409a409191a1ea3c69413f4163f49ebd0f3835f68385831481c
probe       5af4ebf36a353fddbb08b5cf6d1a7176f2166cfb842489b9b71043082a62f891
```

## Final deimos qualification

The qualified root is Btrfs. deimos ran Linux `7.1.4-arch1-1` on x86_64, with 32 logical CPUs on
an AMD Ryzen AI MAX+ 395. `/dev/kvm` was writable and AMD-V was present.

```text
just test-microsandbox-probe-linux \
  /home/sasha/.bb/thread-storage/microsandbox-build-task0l-final \
  /home/sasha/.bb/thread-storage/microsandbox-task0l-qualification-final
```

Bounded output:

```text
proof=ext4-descriptor-uuid source_inode=94129856 clone_inode=94129857 logical_bytes=268435456 source_uuid=41414141414141414141414141414141 clone_uuid=52525252525252525252525252525252 status=passed
proof=vm-descriptor-lifecycle inode=94129859 vmm_pid=411690 buffered=buffered-ok streamed=stream-astream-b ping_rtt_micros=2669 shutdown_millis=2028 marker=secondbox-task0l-marker lifecycle_pid=412214 lifecycle_shutdown_millis=2051 lifecycle_marker=lifecycle-eof-marker status=passed
proof=network-policy allowed_bytes=559 denied_domain=true denied_private=true denied_metadata=true deny_all=true dns_change=true status=passed
```

The VM proof cleared `FD_CLOEXEC`, named the already-open image as `/proc/self/fd/<n>`, verified the
same device/inode in `/proc/<vmm-pid>/fd/<n>`, and renamed the original pathname after attachment.
The guest retained the stable disk mount, wrote a marker, and `e2fsck -fn` plus `debugfs` recovered
that marker after shutdown. The parent descriptor retained the same inode throughout.

The evaluated runtime constructs its multithreaded Tokio relay runtime, starts the agent relay,
runtime control listener, parent watchdog, heartbeat, and timer tasks before the final
`vm.enter()`. While `vm.enter()` owned the calling thread, the live proof concurrently completed a
buffered command, streaming command, and control ping. Control-channel shutdown flushed and exited
in 2028 ms. Closing the inherited parent-watchdog writer independently flushed the second image and
exited in 2051 ms; the fixed deadline was 45 seconds and the force-kill path was not used.

The network proof enforced a default-deny exact-domain TCP/80 rule. The allowed request returned
559 bytes; a disjoint domain, a private address, and `169.254.169.254` were denied. A separate
no-network guest denied the public request. The policy-engine check replaced the cached A record,
proved the former address was revoked, the replacement was admitted, and private/metadata targets
remained denied.

Final validation:

- Final local builder: passed.
- Probe `cargo test --locked`: 1 passed, 0 failed; doc tests passed.
- Checked-in real-KVM qualification target: passed, no skips.
- Wrong-revision and dirty-source rejection: passed.
- `git diff --check`: passed.

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
