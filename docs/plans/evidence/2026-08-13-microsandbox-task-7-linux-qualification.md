# Microsandbox Task 7L Linux vertical-slice qualification

Date: 2026-08-13 (`qualifiedAt` values are UTC on 2026-08-14)

Host: deimos (`Linux 7.1.4-arch1-1`, `linux/amd64`)

Source commit: `97d065ae4c3834a656ca0fa029fe3da4a6901497`

Result: pass

The two final scenario runs started from a clean repository and recorded
`repositoryDirty: false`. No macOS work began before this gate passed. Every dependency build,
artifact, test, and scenario was local; no external repository write, pull request, issue,
comment, release, or push occurred.

## Host gate

- `/dev/kvm` is a readable and writable character device (`0666 root:kvm`).
- `/dev/net/tun` is a readable and writable character device (`0666 root:root`) for Firecracker.
- The Workspace root is Btrfs on `/home` and passed real `FICLONE` readiness and qualification.
- Go is `go1.26.5 linux/amd64`; Rust is `rustc 1.95.0`; Cargo is `1.95.0`.
- Docker client and server are `29.6.2`.
- Firecracker is `v1.16.1`.

The Microsandbox driver refuses a missing/unusable KVM device, an absent local build, a symlinked
or non-canonical build root, a wrong upstream revision, a wrong patched tree, a wrong helper lock,
an unpinned rootfs OCI digest, or a non-reflink Workspace filesystem. None of those checks skip or
fall back successfully.

## Exact local identities

Final Microsandbox build directory:

```text
/home/sasha/.bb/thread-storage/microsandbox-build-task7l-reviewed
```

```text
Microsandbox version       0.6.8
Microsandbox commit        5b335537afad433ad2c0308cb54de13b7015b4e7
Microsandbox upstream tree dc506dffd600fcea281bd4ebfc924e1b31afcb2a
Microsandbox patched tree  2bb82ba33e2175cd7574ffa6d058c6968453fa4b
Microsandbox lock SHA256   7827c5aad40cfc4ab36be6aba3bc4c0d923e525c50fc4b54741776bcf13b95c8
SecondBox patch SHA256     f38294823f2c8e3b8e7918a8c58b48b0c9c7c521874add5d5985af3d4134eb7c
Helper lock SHA256         72ba8a7f40cc17eb75386425cdb387c46cb394913d30d51b811f08e9f14d681c
Helper SHA256              096984f62f409e6d460426ee949cf66862e4aac5deea824a5680ae5d49d02fdd
agentd SHA256              b1a0ba13b7af54df89073af6909f12df737e6553ecd23bc14fcc7e30b2f68a41
msb SHA256                 54d80a34aa95b27d7578c5db60156cd77d2b380af920a1f341881ac9b63611f1
libkrunfw SHA256           858bfd9a63236409a409191a1ea3c69413f4163f49ebd0f3835f68385831481c
probe SHA256               7d102b4c8684cde1e129b4f4608c72605d2fd79ff3efc16bf1cafae00a0e3565
source OCI digest          sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
flat-root digest           sha256:6fbc63a5c35d3a6b6d56234441b26dd5419b13ac22bd397b129438a83a034e98
runtime digest             sha256:52242528763bff8eedc7d6593aacfe7e1de02e65ada8f7908a9a550b1dddc23f
toolchain digest           sha256:99ea2c40abc91c7e8a93546cdeba6597732aa1f5ae6af611860d54cb41709b17
materialization digest     sha256:2ceaf44de2223be33fd525c50758319a23b3169521d1bcb4d62e20d07480ed4a
```

Final Firecracker artifact directory:

```text
/home/sasha/.bb/thread-storage/firecracker-task7l-reviewed-artifacts
```

```text
artifact version           local-task7l-reviewed
artifact manifest digest   sha256:bebc75ef45cdf8918fa4a422db1ea1f17e280aec84082fc4f2f0a2252558226d
runtime manifest digest    sha256:7af2584672183936c362168d045f4b808cfcb1f4df0f6c296de0bc59bd264d9c
toolchain manifest digest  sha256:c732f5311cae5e82d52dee77ced93cb96f776a41b43263c410a01fd3e43a1c3b
kernel SHA256              7c48460080dd76ea685570c50f6c80028c31b48f8b14ee9afde6605933b83f14
rootfs SHA256              c670660ac1097fcec79e83ce068406cb6b0e15cb93b27f26ed499f4295ad52cd
shared image SHA256        6ec06a34cf0d0e9d8c5805ecccd9e86fa39e457c3907e84c316bb1e3e084079d
public key DER SHA256      04d2e5879b5178726adf8650a75527344ec22bc47ce2c92ed36fec58814c0ba2
```

The prepared rootfs used the pinned Debian snapshot
`https://snapshot.debian.org/archive/debian/20250701T000000Z/`, image-definition SHA-256
`dbd60037ad1bbacd92d6a2b7a16f8a53b8365269597679e09a963cf39a2d1547`, Dockerfile SHA-256
`0f7e17c374b73c051ad8f475a24cc2e2b7838038fbedce2ee7053ec028661636`, apt-input SHA-256
`2b0bc854bb16303e4852cceb722eb77d5e19655f2669c61de9af135d8cc96306`, and Python-input
SHA-256 `3f9233bf74002b6607fd818636c14e7fcc51ff8be00c70f5796b00aa386539d3`.

## Vertical-slice outcomes

The final Microsandbox scenario passed 21 top-level tests. It exercised:

- two concurrent Instances with independent command, file, terminal, Port, Workspace, and event
  traffic;
- buffered Exec, bounded output exhaustion, streaming stdin/stdout, signals, credit, cancellation,
  and concurrent operations;
- binary file and configured-boundary operations plus PTY dimensions, detach, reattach, and close;
- proxied and direct Port traffic with generation fencing;
- deny-all and exact domain/port allow-list policy through the user-space network engine;
- control-plane restart, runner loss during active Exec, helper exit, Workspace lock recovery, and
  durable restart without reconstructing empty state;
- stopped, Snapshot-free relocation between compatible Linux runners, with bytes relayed rather
  than persisted by the control plane;
- Snapshot creation, mutation, restore, retention, generation advance, and stale-generation
  rejection;
- uncached logical/materialization rejection without an Instance or compute start;
- rejection of Microsandbox `snapshot_resume` before creating a durable operation or compute;
- enrollment into the backend-sealed pool and all resource-capacity admission paths.

The final Firecracker scenario passed the same 21 top-level scenario tests. It additionally built
and admitted a real snapshot-resume template, exercised jailed KVM, TAP/nftables, cgroups, signed
artifacts and trust anchor, resumed Instances without cold-boot fallback, and retained
Firecracker-specific lifecycle and snapshot behavior. Snapshot-resume observations were:

```text
concurrency 1: create-to-ready p50/p95 204/302 ms; start-to-ready 141/172 ms
concurrency 4: create-to-ready p50/p95 249/419 ms; start-to-ready 172/302 ms
```

The first Firecracker scenario attempt exposed a heartbeat-observation race in the new rejection
assertion: the test sampled the start counter before the preceding real boot reached the next
heartbeat. The Firecracker backend already rejected mismatched assignment assets before Workspace
or compute. Commit `97d065a` made the test require two consecutive settled heartbeats before and
after the rejection window. Both complete scenarios then passed from that clean commit.

## Thirty cold starts

The 30-start observations are committed in
[2026-08-13-microsandbox-task-7-cold-starts.json](2026-08-13-microsandbox-task-7-cold-starts.json).

```text
start-to-ready p50/p95/max: 455.407 / 487.543 / 488.064 ms
peak helper RSS p50/p95/max: 82,816 / 84,996 / 85,100 KiB
```

Every stage has 30 samples. The recorded p50/p95 values in milliseconds are:

```text
artifact_verify    208.914 / 215.414
compute_launch       0.002 /   0.003
guest_negotiation  226.665 / 240.763
network_setup        0.004 /   0.017
ready                0.128 /   0.172
runner_admission     16.591 /  39.462
workspace_attach      0.154 /   0.199
```

These are observations, not a release gate.

## Exact validation commands

All commands below passed on source commit `97d065a`:

```text
env GOCACHE=$PWD/.tmp/task7l-go-cache npm_config_cache=$PWD/.tmp/task7l-npm-cache \
  just verify-generated

env GOCACHE=$PWD/.tmp/task7l-go-cache \
  GOLANGCI_LINT_CACHE=$PWD/.tmp/task7l-golangci-cache \
  npm_config_cache=$PWD/.tmp/task7l-npm-cache just lint

env GOCACHE=$PWD/.tmp/task7l-go-cache \
  GOLANGCI_LINT_CACHE=$PWD/.tmp/task7l-golangci-cache \
  npm_config_cache=$PWD/.tmp/task7l-npm-cache \
  SECONDBOX_TEST_DATABASE_URL='postgresql://secondbox:secondbox-task7l-local@127.0.0.1:32771/secondbox_test?sslmode=disable' \
  just test

env GOCACHE=$PWD/.tmp/task7l-go-cache npm_config_cache=$PWD/.tmp/task7l-npm-cache \
  just test-contract

env GOCACHE=$PWD/.tmp/task7l-go-cache npm_config_cache=$PWD/.tmp/task7l-npm-cache \
  SECONDBOX_TEST_DATABASE_URL='postgresql://secondbox:secondbox-task7l-local@127.0.0.1:32771/secondbox_test?sslmode=disable' \
  just test-non-kvm

env GOCACHE=$PWD/.tmp/task7l-go-cache \
  SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE=/home/sasha/.bb/thread-storage/microsandbox-build-task7l-reviewed/runtime/bin/secondbox-microsandbox-helper \
  SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM=$PWD/.tmp/microsandbox-workspaces \
  just test-workspacestore-linux

env GOCACHE=$PWD/.tmp/task7l-go-cache \
  SECONDBOX_MICROSANDBOX_LINUX_BUILD=/home/sasha/.bb/thread-storage/microsandbox-build-task7l-reviewed \
  SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM=$PWD/.tmp/microsandbox-workspaces \
  just test-microsandbox-linux

env GOCACHE=$PWD/.tmp/task7l-go-cache \
  SECONDBOX_RUNNER_QUALIFICATION_TEMP_ROOT=$PWD/.tmp/firecracker-qualification \
  SECONDBOX_RUNNER_FIRECRACKER_PATH=/home/sasha/Developer/tries/agent-manager/tmp/firecracker-smoke/bin/firecracker \
  SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH=/home/sasha/.bb/thread-storage/firecracker-task7l-reviewed-artifacts/kernel \
  SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH=/home/sasha/.bb/thread-storage/firecracker-task7l-reviewed-artifacts/rootfs.ext4 \
  SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH=/home/sasha/.bb/thread-storage/firecracker-task7l-reviewed-artifacts/shared.img \
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY=/home/sasha/.bb/thread-storage/firecracker-task7l-reviewed-signing/public.pem \
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256=04d2e5879b5178726adf8650a75527344ec22bc47ce2c92ed36fec58814c0ba2 \
  SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS='console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init' \
  SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB=256 \
  SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT=1024 \
  SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT=1025 \
  SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL=1s just test-firecracker

env GOCACHE=$PWD/.tmp/task7l-go-cache npm_config_cache=$PWD/.tmp/task7l-npm-cache \
  SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1 \
  SECONDBOX_MICROSANDBOX_LINUX_BUILD=/home/sasha/.bb/thread-storage/microsandbox-build-task7l-reviewed \
  SECONDBOX_RUNNER_WORKSPACE_ROOT=$PWD/.tmp/microsandbox-workspaces \
  SECONDBOX_SCENARIO_DIAGNOSTICS_DIR=$PWD/.tmp/task7l-ms-final-diagnostics \
  scripts/test-scenario-microsandbox-linux.sh

env GOCACHE=$PWD/.tmp/task7l-go-cache npm_config_cache=$PWD/.tmp/task7l-npm-cache \
  SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1 \
  SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR=/home/sasha/.bb/thread-storage/firecracker-task7l-reviewed-artifacts \
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY=/home/sasha/.bb/thread-storage/firecracker-task7l-reviewed-signing/public.pem \
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256=04d2e5879b5178726adf8650a75527344ec22bc47ce2c92ed36fec58814c0ba2 \
  SECONDBOX_RUNNER_WORKSPACE_ROOT=$PWD/.tmp/firecracker-workspaces \
  SECONDBOX_SCENARIO_DIAGNOSTICS_DIR=$PWD/.tmp/task7l-fc-clean-diagnostics-2 \
  scripts/test-scenario.sh
```

`just lint` reported zero issues in both Go modules. `just test-microsandbox-linux` passed 13
Rust unit/property tests, 2 inherited-descriptor/lifecycle process tests, the runner tests, and
the real-KVM qualification. `just test-firecracker` passed its three mandatory real-KVM smoke,
local Snapshot restore, and lifecycle-stop paths in 28.002 seconds.

## Bounded evidence and diagnostic integrity

- [Microsandbox scenario evidence](2026-08-13-microsandbox-task-7-linux-scenario.json) records 21
  passes, 379 wall-clock seconds, clean source, KVM, and Btrfs.
- [Firecracker scenario evidence](2026-08-13-firecracker-task-7-linux-scenario.json) records 21
  passes, 197 wall-clock seconds, clean source, KVM, TUN, and Btrfs.
- The complete local diagnostic capture is preserved under
  `/home/sasha/.bb/thread-storage/microsandbox-task7l-evidence/final-97d065a`.

```text
51fad3b8cccb8cfce27d44974f75e673c3c2c0b6909ee3ce2e0b2f476f5eaaec  2026-08-13-microsandbox-linux-cold-starts.json
078442de147843a864084be5702520fb1b3b21fac15e7135437c99f7e29ea7a8  microsandbox-linux-scenario-qualification-evidence.json
256ddb0de0ccba475ffedd0fe5917d16ed65996d47ecc6ad5576b5b38da172be  scenario-qualification-evidence.json
b3fa37538465eed239bf7b796dad6e86a1f9d073cce64e129878ef63f2dc1db7  microsandbox-diagnostics/compose.log
3e0f1eab1a8b1bcf990f2423262defcc73f1b0c66cb2334129b4a1db6b1bf07d  microsandbox-diagnostics/runner.jsonl
5cef68c1c0e34512017618a8c055f5f8e61adfa6b9eac8eeb3c95d1eb3a54354  firecracker-diagnostics/compose.log
f02ede0f577ce381c540c7710a793d7915dfc8a860b0f2a9892e6e69cfc040a1  firecracker-diagnostics/runner.jsonl
```

Task 7L passes. This satisfies the hard Linux gate and permits Task 8M to begin; it does not make
Microsandbox non-experimental or qualify macOS.
