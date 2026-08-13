# Microsandbox Task 6L network and lifecycle qualification

Date: 2026-08-13

Host: deimos (`Linux 7.1.4-arch1-1`, `linux/amd64`, real `/dev/kvm`, Btrfs Workspace root)

Result: pass

## Network enforcement boundary

- The Go adapter sends every assignment policy through the shared provider-neutral
  `networkpolicy` compiler before constructing the private helper message. This preserves the
  128-destination bound, exact normalized domain or canonical CIDR, TCP/HTTP/HTTPS classification,
  exact port, reserved-DNS-port rejection, and protected private, loopback, link-local, metadata,
  multicast, and management-address rejection. Unsupported or inexact rules fail admission.
- The helper converts the validated policy into Microsandbox's pinned smoltcp `NetworkPolicy` with
  deny-by-default ingress and egress. Each SecondBox TCP, HTTP, or HTTPS destination becomes the
  exact L4 TCP port allowed by the provider-neutral contract; the helper retains its protocol
  classification through translation. Domain policies add only the gateway DNS rule needed to
  resolve the exact domain. Deny-all and an empty allow-list install no egress rules.
- `SmoltcpNetwork::new_with_profile(..., MultiTenant)` applies Microsandbox's public-network
  isolation floor in addition to the Sandbox policy. The floor forces DNS rebinding protection,
  blocks private, loopback, link-local, metadata, and host destinations, disables custom interface,
  resolver, host-CA, and published-port overrides, and caps connections. A Sandbox policy can
  narrow this floor but cannot broaden it.
- DNS is intercepted at the user-space gateway. With no operator override, the pinned engine
  discovers upstream resolvers from the host's `/etc/resolv.conf`; domain answers populate the
  engine's bounded DNS binding set and protected answers are rejected. TLS interception remains
  disabled by the pinned default, host CA trust is not imported, and guest-to-origin TLS remains
  end-to-end. The engine still observes SNI/DNS identity for domain policy where available. These
  are documented Microsandbox backend properties, not new universal SecondBox guarantees.
- The helper owns the Tokio runtime, smoltcp backend, MAC, and guest network environment for the
  complete non-returning VM lifetime. Readiness requires `network-policy`, `network-smoltcp`, and
  `agent-relay` features after the engine is constructed and the agent relay is connected. It uses
  no TAP, bridge, nftables, host listener, or elevated network privilege.

The real-KVM test admitted HTTP only to `example.com:80`, fetched its response, and rejected a
connection to `169.254.169.254`. The preceding deny-all assignment still completed the entire
buffered/streaming Exec, binary file, PTY, and TCP-relay conformance suite without unintended
network access.

## Lifecycle and failure evidence

- A reverse cleanup stack reserves capacity, opens the Workspace attachment, and launches the
  helper-owned control socket and network in that order. Pre-ready failure unwinds helper/network/
  socket, Workspace attachment, then capacity. Ownership transferred to a running helper is
  explicitly disarmed; no local path is exposed and no empty Workspace is reconstructed.
- Unsupported shapes and policies map to incompatible-profile, unavailable resources to capacity,
  digest/materialization mismatch to artifact, and helper, hypervisor, guest-negotiation, or mount
  startup failures to the existing provider-neutral prerequisite/infrastructure classes. The
  runner-control service preserves typed backend decisions and terminals; unit tests cover both
  the rejection and post-admission startup paths.
- Ready, teardown, and unexpected-exit records contain fixed-shape correlations for runner,
  Sandbox, Instance, generation, assignment, request, operation, lease, helper PID, backend,
  platform, dependency version, materialization digest, stage, and stream. Unexpected exit adds
  exit code or signal, helper reason, and SHA-256 stderr/event-tail digests. Optional evidence text
  is capped at 256 bytes; environment values, command payloads, files, and network bodies are never
  recorded. Evidence-sink errors are returned during start or fencing rather than swallowed.
- Diagnostics and metrics expose only the fixed `microsandbox` backend kind and `linux-amd64` host
  platform dimensions, together with bounded active-instance/operation and cold-start values.
- The real helper-death qualification emitted exactly one terminal with no duplicate and validated
  the bounded unexpected-exit record.

## Exact local build

The final bundle was built only from clean local source into:

```text
/home/sasha/.bb/thread-storage/microsandbox-build-task6l-final
```

Pinned evidence:

```text
Microsandbox version      0.6.8
Microsandbox commit       5b335537afad433ad2c0308cb54de13b7015b4e7
Microsandbox source tree  dc506dffd600fcea281bd4ebfc924e1b31afcb2a
Microsandbox patched tree 2bb82ba33e2175cd7574ffa6d058c6968453fa4b
Microsandbox lock SHA256  7827c5aad40cfc4ab36be6aba3bc4c0d923e525c50fc4b54741776bcf13b95c8
SecondBox patch SHA256    f38294823f2c8e3b8e7918a8c58b48b0c9c7c521874add5d5985af3d4134eb7c
Helper lock SHA256        72ba8a7f40cc17eb75386425cdb387c46cb394913d30d51b811f08e9f14d681c
Helper SHA256             444cb9974609329c7fe19d21ac39ef62a9ea0f91060a709294f9372678a87c61
agentd SHA256             b1a0ba13b7af54df89073af6909f12df737e6553ecd23bc14fcc7e30b2f68a41
msb SHA256                1e79fca3c9de4f7d0c512c9e04cc06124e7fda8c74db5192ea8e67d91f5b481f
libkrunfw SHA256          858bfd9a63236409a409191a1ea3c69413f4163f49ebd0f3835f68385831481c
probe SHA256              9c2ed2650ad3efc601bde4d9a6dcbdb7fb665ece4b22d2a7fcc8af6b153bcdd3
```

The builder verified the pinned revision, clean source tree, local patch, patched tree, lockfiles,
libkrun 0.1.30 family, and packaged artifact digests. The helper's network and types crates are path
dependencies into that exact staged source rather than separately published crates with a different
libkrun version. No push, fork, branch, PR, issue, comment, release, or other external repository
write occurred.

## Validation

Passed:

```text
cd runner && GOCACHE=/tmp/secondbox-task6-go-cache \
  go test ./internal/microsandbox/... ./internal/networkpolicy/... -count=1
cd runner && GOCACHE=/tmp/secondbox-task6-go-cache \
  go test ./internal/runnercontrol/... ./internal/microsandbox/... \
  ./internal/networkpolicy/... ./internal/runnerevidence/... -count=1
just test-microsandbox-linux
just lint
just verify-generated
just test
just test-contract
just test-firecracker
cargo clippy --locked --manifest-path \
  /home/sasha/.bb/thread-storage/microsandbox-build-task6l-final/source/runner/microsandbox-helper/Cargo.toml \
  --all-targets -- -D warnings
```

The Microsandbox target passed 12 Rust unit/property tests, 2 inherited-descriptor process tests,
runner command tests, and its real-KVM qualification in 10.488 seconds. The complete database-backed
`just test` suite passed against the existing disposable local PostgreSQL container. Its first
sandboxed attempt correctly failed only because the pre-existing host-preflight test intentionally
creates a `/var/tmp` fixture; the same target passed with local host filesystem access.

The Firecracker regression passed independently in 25.516 seconds using Firecracker 1.16.1, the
existing signed local v0.3 artifacts, and real deimos KVM. This confirms the optional typed failure
interfaces and extended bounded evidence record did not alter Firecracker's intended path.

No macOS implementation, compilation, packaging, or qualification was started. Task 7L is the next
and final Linux gate.
