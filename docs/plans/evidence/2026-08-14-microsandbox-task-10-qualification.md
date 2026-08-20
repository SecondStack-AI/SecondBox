# Microsandbox Task 10M qualification

Date: 2026-08-14

Result: verifiable dual-platform functional qualification pass

The Apple Silicon, post-port Linux Microsandbox, and unchanged Firecracker qualifications passed.
Production or repository macOS signing verification is unavailable on `mini1` and is not claimed
by this result. Microsandbox remains experimental.

## macOS scenario

Host: `mini1`, Apple M1, Darwin 24.1.0, macOS 15.1.1, APFS, `kern.hv_support=1`, Go 1.25.13,
Rust/Cargo 1.95.0, Docker client 27.4.0 and server 27.3.1.

The task-owned SecondBox snapshot was
`/Users/alex/Developer/SecondBox-task10m-e38600d`. The local execution bundle was
`/Users/alex/Developer/microsandbox-task10m-dev1`. The exact command was:

```sh
SECONDBOX_MICROSANDBOX_MACOS_BUILD=/Users/alex/Developer/microsandbox-task10m-dev1 \
  just test-scenario-microsandbox-macos
```

The final bounded log is `/Users/alex/Developer/task10m-aggregate-final.log`. The suite passed 22
tests in 344 seconds. It covered concurrent-instance isolation; terminal and binary filesystem
flows; direct and proxied Ports; buffered and streaming exec; lifecycle and control-plane restart;
runner loss; leases and generation fencing; deny-all and allow-list networking; idle expiry;
compatible stopped-Sandbox relocation; runner enrollment and admission rejection; real compute
boot; unsupported architecture and materialization rejection; unsupported snapshot resume; and
Snapshot durability, restore, limit, and retention.

The final Port observation was 26.498 ms proxied mean echo latency and 13.146 ms direct mean echo
latency. The direct interactive session was 286.590 ms versus 536.632 ms proxied.

Thirty real Hypervisor.framework cold starts recorded 351.896/363.035 ms start-to-ready p50/p95
and 103072/103328 KiB peak helper RSS p50/p95. The complete stage breakdown and immutable
materialization identity are in the adjacent JSON evidence.

Two races exposed by the aggregate run were corrected and rechecked before the passing run:

- the BusyBox echo server now uses persistent listen mode, eliminating the listener rebind window;
- lifecycle qualification accepts either observable `draining` or already-advanced `stopped`, and
  accepts a Lease already fenced by generation advance rather than waiting forever for a transient
  state.

The control-plane session now serializes inbound and outbound stream-state mutation, and runner
terminal streams close their control queue ownership without blocking on late input or credit.
Focused race tests and three repeated streaming sequences passed before the aggregate suite.

## Build identity and signing boundary

The execution bundle records Microsandbox commit
`5b335537afad433ad2c0308cb54de13b7015b4e7`, patched tree
`daf8457b13e5f124a63e23a12edbd8482d7da43a`, helper lock
`72ba8a7f40cc17eb75386425cdb387c46cb394913d30d51b811f08e9f14d681c`, libkrunfw commit
`21cb6dce19a615f63e41ecb913334d18560c1364`, and scenario materialization
`sha256:fe4e11d11c799488dff5c8eff1a5ded50988d62c7dcf1ddb8cebea6079c1871b`.

The bundle reports `signing_mode=adhoc` and a helper entitlement plist. That was a mechanical local
execution prerequisite only. `mini1` cannot independently verify the repository signing workflow,
an operator-selected production identity, Developer ID distribution, or notarization. Those items
remain unavailable external evidence and this qualification makes no production-signing claim.

## Local and Linux validation

The following passed on `deimos` after the macOS changes:

- `just verify-generated`
- `just lint` with a writable task cache
- `just test-contract`
- `just test-non-kvm`
- `cd runner && go test ./... -count=1`
- helper Cargo tests staged against the pinned source graph: 14 unit/property and 2 process tests
- `just test-workspacestore-linux`
- `GOOS=darwin GOARCH=arm64 go test -c ./cmd/secondbox-runner`
- focused `go test -race` for both control-plane and runner protocol stream state
- scenario-tag compilation, shell syntax checks, and `git diff --check`

`just test-non-kvm` included the PostgreSQL-backed Go suite, 133 TypeScript tests, SDK clean-project
validation, Compose smoke checks, and image policy checks. An isolated task database was used and
stopped afterward.

The direct helper command requires staging beneath the pinned Microsandbox source because its path
dependencies are intentional. A temporary task-owned staging tree passed the helper suite without
modifying the retained dependency checkout.

## Post-macOS Linux real-host qualification

After `/dev/kvm` became available again, a deimos host terminal verified it as a readable and
writable character device. The Task 7 bundle correctly failed the Task 10 scenario preflight
because it recorded the older `2bb82ba3...` patched tree. It was not used as Task 10 evidence.

A clean pinned checkout at `/tmp/secondbox-msb-base-task5-spawnfix` was passed to the
repository-owned builder. It did not modify that checkout or any external repository. The new
task-owned build is retained at:

```text
/home/sasha/.bb/thread-storage/microsandbox-build-task10m-final
```

It records the same pinned upstream commit and lock identities as macOS, patched tree
`daf8457b13e5f124a63e23a12edbd8482d7da43a`, Linux libkrunfw
`858bfd9a63236409a409191a1ea3c69413f4163f49ebd0f3835f68385831481c`, and helper
`394a54ff67d5323b8bda0d0bb97919bb60b0e07c63daac1c54de7e6084aec4f6`.

The following real-host gates passed using that build and the reviewed Firecracker artifacts:

```text
just test-microsandbox-linux             pass; real KVM boot and 14+2 helper tests
just test-firecracker                    pass; 3 real-KVM tests in 26.724 s
just test-scenario-microsandbox-linux    pass; 22 scenarios in 393 s
just test-scenario                       pass; 22 Firecracker scenarios in 222 s
```

The Linux Microsandbox suite exercised the same normal-control-plane vertical slice as macOS. Its
30 cold starts recorded 471.701/520.480 ms start-to-ready p50/p95 and 70468/72240 KiB peak helper
RSS p50/p95. The Firecracker suite also published and admitted the real snapshot-resume template,
then measured resume arrivals at concurrency one and four with no refusals.

Bounded build and test logs, including diagnostics, are retained beneath:

```text
/home/sasha/.bb/thread-storage/task10m-linux-final-81dddd3
```

The final log SHA-256 values are:

```text
8b94b89b4b981a20767d1ce2a5b6365feb628d82da577ac24ffbb18dc9f41d07  build-microsandbox-linux.log
d0afa8d99fb9f07a2de68238b7cf6a0cf595021fa1a410da04aa68e6c091b343  test-microsandbox-linux-final.log
61be5f88250aad0dec4894721a8ce9d97ca7fb98982fa8f3bf9ff0e52f5c8f0b  test-firecracker-final.log
34f2ab39bd5ffada1b7543eb687064f7c735373f08e9f499e487af5b3c488d07  test-scenario-microsandbox-linux-final.log
304cf252cf6d75107c642ee97f8c27c45dd497877d853cf3d3d16e09dc2d1da2  test-scenario-firecracker-final.log
```

The earlier preflight failure against the stale Task 7 build and the first Firecracker invocation
that omitted the required heartbeat interval are retained as diagnostic logs but are not presented
as product failures or qualification evidence.

## Unavailable external evidence

The remaining unchecked plan items concern production/repository macOS signing and its effective
entitlement evidence. `mini1` cannot meaningfully verify that external distribution authority.
Local ad-hoc signing remains only a mechanical execution prerequisite and does not prevent the
functional dual-platform qualification from passing.
