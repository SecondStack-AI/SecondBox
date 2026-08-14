# Microsandbox Task 10M qualification

Date: 2026-08-14

Result: macOS scenario pass; dual-platform closure incomplete

The verifiable Apple Silicon qualification passed. The required post-port Linux real-compute and
Firecracker reruns could not run because `/dev/kvm` is currently absent on `deimos`. Production or
repository macOS signing verification is unavailable on `mini1` and is not claimed by this result.

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

## Unavailable required checks

On `deimos`, `lscpu` reports AMD-V and `kvm_amd` plus `kvm` are loaded, but `/dev/kvm` does not
exist. No host device or kernel state was changed. Therefore these mandatory real-host validations
were not rerun:

- `just test-microsandbox-linux`
- `just test-scenario-microsandbox-linux`
- `just test-firecracker`
- `just test-scenario`

The previously reviewed Task 7L and Task 9M Linux evidence remains intact, but it does not
substitute for Task 10M's required post-macOS rerun. The spike stays experimental and Task 10M
dual-platform closure remains incomplete until a qualified Linux KVM host is available.
