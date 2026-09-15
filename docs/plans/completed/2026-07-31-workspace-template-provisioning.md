---
title: Reflinked Workspace Template Provisioning
date: 2026-07-31
status: implemented
owner: SecondStack
provenance: Qualified lifecycle follow-up after event-driven control-plane wakeups
---

# Plan: Reflinked Workspace Template Provisioning

## Outcome

Remove `mke2fs` from the normal Sandbox-create path without weakening the
runner-local durability boundary. The WorkspaceStore owns an immutable ext4
template for each logical capacity, creates a Workspace only with `FICLONE`,
and rewrites the child filesystem UUID before atomic publication.

The Runner prewarms the already-required maximum Sandbox disk capacity before
registration. An explicitly requested capacity not yet present is formatted
once under a process-local generation lock and then reused. There is no
byte-copy fallback, shared writable template, or public template identity.

## Fixed design

- Templates live under `templates/ext4` inside the authoritative reflink root.
- A template key is its exact logical capacity. Its filesystem UUID is
  deterministic from that capacity and its mode is read-only by policy.
- Template publication is sparse-create, deterministic format, fsync, atomic
  rename, and directory fsync. A restart removes only the known temporary file.
- Existing templates must be regular files with the exact capacity, ext4
  magic, expected template UUID, and exact mode. Invalid evidence is corrupt
  state and is never silently regenerated.
- Workspace creation reflinks the template, rewrites the child to the
  deterministic Workspace UUID, fsyncs it, and atomically publishes it before
  the existing manifest and receipt sequence.
- Cross-Sandbox Snapshot cloning also rewrites the target Workspace UUID.
  Snapshot and same-Sandbox restore reflinks retain the source UUID.
- The WorkspaceStore remains the only component that resolves these paths.

## Validation Commands

- `go test ./runner/internal/workspacestore`
- `just verify-generated`
- `just test`
- `just test-contract`
- `just test-compose`
- `git diff --check`
- `just test-scenario` on the qualified KVM/Btrfs host
- concurrency-1 and burst lifecycle benchmarks against the recorded wakeup
  candidate

## Result

The qualified runner preformatted the 1 GiB maximum-capacity template during
startup in 18 ms plus 4 ms of fsync. Every measured Workspace create then used
a zero-millisecond `FICLONE` followed by its isolated UUID rewrite. The
uncontended UUID rewrite measured 28/40 ms p50/p95; at burst concurrency it
measured 59/60 ms.

Compared with the final event-driven wakeup candidate:

| metric | wakeup candidate | template candidate |
|---|---:|---:|
| concurrency-1 `create_to_ready` p50/p95 | 701/1,027 ms | 657/823 ms |
| concurrency-1 `workspace_provision` p50/p95 | 151/229 ms | 148/192 ms |
| burst-32 `create_to_ready` p50/p95 | 3,020/3,463 ms | 2,797/3,195 ms |
| burst-32 `workspace_provision` p50/p95 | 804/1,510 ms | 681/1,256 ms |
| burst-32 completion rate | 9.07/s | 9.79/s |

Both lifecycle runs completed without refusals or failures. The unfiltered
KVM/Btrfs scenario suite also passed all tests in 117.8 seconds, including
ordinary lifecycle, control-plane restart, runner-loss recovery, Snapshot
durability, and in-place restore. That suite used a 64 MiB profile beneath a
10 GiB runner maximum and proved the first-use exact-capacity template path as
well as the startup-prewarmed path.

Machine-readable evidence:

- `.tmp/lifecycle-workspace-template-c1-result.json`
- `.tmp/lifecycle-workspace-template-burst32-result.json`
- `.tmp/workspace-template-scenario.diagnostics/runner.jsonl`
