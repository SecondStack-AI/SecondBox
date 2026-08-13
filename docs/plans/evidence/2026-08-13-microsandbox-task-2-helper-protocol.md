# Task 2L evidence: pinned helper and private protocol

Date: 2026-08-13  
Host: deimos (`linux/amd64`)  
Result: pass

## Implemented boundary

- Added the independently locked `secondbox-microsandbox-helper` Rust process. Its lock graph uses
  the exact `msb_krun` 0.1.30 family qualified in Task 0L; it is never linked into the Go runner.
- Added one private protobuf v1 schema and generated the Go and Rust bindings from it. The schema
  covers start/readiness evidence, exec, file operations, PTY, TCP, cancellation, shutdown,
  terminal results, diagnostics, formatting, data frames, and byte credit.
- Added 1 MiB big-endian frames, request/stream ordering, explicit credit, duplicate/stale/EOF
  rejection, bounded diagnostic text, fuzz/property tests, and correct short-write handling.
- Reserved inherited fd 3 for the Unix control socket, fd 4 for the Workspace image, and fd 5 for
  the parent-lifecycle pipe. Production libkrun configuration attaches `/proc/self/fd/4` as the
  writable raw block device with stable ID `workspace` and full flush semantics.
- Added deterministic ext4 formatting through the already-open Workspace descriptor, including
  explicit UUID/label, non-lazy initialization, `fsync`, and `e2fsck` before success.
- Added validated translation for the exact flat-root path and digest, host architecture, integer
  VCPU count, whole-MiB memory, startup environment, Workspace identity, and typed default-deny or
  allow-list network policy. `VmBuilder::build` validates the resulting libkrun configuration.
- Readiness evidence carries helper/dependency versions, host platform, agent protocol generation,
  features, supported operations, and the active materialization digest.
- Parent EOF starts a Workspace flush with a four-second hard bound and terminates the helper; the
  helper has no daemon mode or recovery/adoption path.

## Locked inputs and local artifact

The final local-only build was produced by:

```text
runner/scripts/build-microsandbox-local.sh \
  --source /tmp/secondbox-msb-base-env4w9 \
  --output /home/sasha/.bb/thread-storage/microsandbox-build-task2l-final
```

Pinned evidence:

```text
Microsandbox commit       5b335537afad433ad2c0308cb54de13b7015b4e7
Microsandbox patched tree 972fd637a835c175a4aa5b11fd52ccd0ab087f95
Microsandbox lock SHA256  7827c5aad40cfc4ab36be6aba3bc4c0d923e525c50fc4b54741776bcf13b95c8
SecondBox patch SHA256    943e728067cce9f0efe9ed578c74f6323bd6c1cf8407822a1e8ee998f64564de
Helper lock SHA256        f8e07fd675fd194c9ba571547a05e122ad77e96829b3ff00e4455008b52eb5a8
libkrunfw commit          21cb6dce19a615f63e41ecb913334d18560c1364
Helper binary SHA256      d4422b2ae330e3038af1ed80724d9674c20a9f3ed4f9503605540ed8ad01b784
```

The staged build compiled and tested the pinned patched Microsandbox source, probe, all-0.1.30
libkrun family, protobuf helper, and agentd, then packaged the helper beside the local runtime. It
performed no push, fork, PR, issue, release, or other external repository write.

## Validation

All required Task 2L commands passed:

```text
cargo fmt --manifest-path runner/microsandbox-helper/Cargo.toml -- --check
cargo clippy --manifest-path runner/microsandbox-helper/Cargo.toml --all-targets --locked -- -D warnings
cargo test --manifest-path runner/microsandbox-helper/Cargo.toml --locked
cd runner && go test ./internal/microsandboxprotocol/... -count=1
just verify-generated
```

Rust results: 10 unit/property tests, 2 inherited-descriptor process tests, and doc tests passed.
Go protocol tests and fuzz seed coverage passed. Generated-contract verification, Go SDK checks,
TypeScript type/tests/build/package verification, and protobuf regeneration comparison passed.
